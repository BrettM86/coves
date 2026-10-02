//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const moderationConcurrencyReason = "social.coves.moderation.defs#reasonSpam"

type moderationConcurrentOutcome struct {
	result *moderation.MutationResult
	err    error
}

func startConcurrentModerationCalls(first, second func() (*moderation.MutationResult, error)) <-chan moderationConcurrentOutcome {
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan moderationConcurrentOutcome, 2)
	for _, call := range []func() (*moderation.MutationResult, error){first, second} {
		go func() {
			ready <- struct{}{}
			<-start
			result, err := call()
			results <- moderationConcurrentOutcome{result: result, err: err}
		}()
	}
	<-ready
	<-ready
	close(start)
	return results
}

// Keep the first action insert in flight while the other mutation attempts to
// lock the same existing subject row. Both actors are distinct, so the actor
// advisory lock cannot accidentally serialize this race.
func holdModerationActionInsert(t *testing.T, db *sql.DB) (*sql.Conn, func()) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		CREATE FUNCTION hold_moderation_action_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(hashtextextended(current_database(), 0));
			RETURN NEW;
		END;
		$$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		CREATE TRIGGER hold_moderation_action_insert BEFORE INSERT ON moderation_actions
		FOR EACH ROW EXECUTE FUNCTION hold_moderation_action_insert();
	`)
	require.NoError(t, err)
	connection, err := db.Conn(t.Context())
	require.NoError(t, err)
	_, err = connection.ExecContext(t.Context(), `SELECT pg_advisory_lock(hashtextextended(current_database(), 0))`)
	require.NoError(t, err)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		var unlocked bool
		assert.NoError(t, connection.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended(current_database(), 0))`).Scan(&unlocked))
		assert.True(t, unlocked)
		assert.NoError(t, connection.Close())
	}
	t.Cleanup(release)
	return connection, release
}

func waitForSubjectLockContention(t *testing.T, connection *sql.Conn) {
	t.Helper()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		var inserting, waitingForSubject int
		err := connection.QueryRowContext(t.Context(), `
			SELECT count(*) FILTER (WHERE query LIKE '%INSERT INTO moderation_actions%'),
			       count(*) FILTER (WHERE query LIKE '%SELECT version FROM moderation_subjects%'
			                           AND query LIKE '%FOR UPDATE%')
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
		`).Scan(&inserting, &waitingForSubject)
		return inserting == 1 && waitingForSubject == 1, err
	}, testkit.WithDescription("one action insert gated while the other request waits for the subject row lock"))
}

func countModerationActions(t *testing.T, db *sql.DB, subjectURI, action string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM moderation_actions WHERE subject_uri = $1 AND action = $2
	`, subjectURI, action).Scan(&count))
	return count
}

func TestModerationConcurrentRemovesRejectStaleVersion(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, "")
	service := newPostgresModerationService(db)
	seed, err := service.RemoveContent(t.Context(), fixtures.DID("seedremoveadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "seed-remove",
		Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, seed)
	require.NotNil(t, seed.Action)
	restored, err := service.RestoreContent(t.Context(), fixtures.DID("seedrestoreadmin"), moderation.RestoreContentRequest{
		ActionID: seed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1",
		IdempotencyKey: "seed-restore", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Equal(t, "v2", restored.State.Version)
	require.Equal(t, moderation.ModerationStateClear, restored.State.Moderation.State)
	connection, release := holdModerationActionInsert(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request := moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v2", Reason: moderationConcurrencyReason,
	}
	first := request
	first.IdempotencyKey = "concurrent-remove-first"
	second := request
	second.IdempotencyKey = "concurrent-remove-second"
	results := startConcurrentModerationCalls(
		func() (*moderation.MutationResult, error) {
			return service.RemoveContent(ctx, fixtures.DID("concurrentadminone"), first)
		},
		func() (*moderation.MutationResult, error) {
			return service.RemoveContent(ctx, fixtures.DID("concurrentadmintwo"), second)
		},
	)
	waitForSubjectLockContention(t, connection)
	release()
	var applied, conflicts int
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			assert.ErrorIs(t, outcome.err, moderation.ErrStateConflict)
			assert.Nil(t, outcome.result)
			if errors.Is(outcome.err, moderation.ErrStateConflict) {
				conflicts++
			}
			continue
		}
		require.NotNil(t, outcome.result)
		assert.Equal(t, moderation.OutcomeApplied, outcome.result.Outcome)
		if outcome.result.Outcome == moderation.OutcomeApplied {
			applied++
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, 2, countModerationActions(t, db, subject.URI, moderation.ActionRemove), "one seed removal and exactly one racing removal")
}

// gatedMediaReconciler runs the real reconciliation inside the consumer's
// transaction, then holds that transaction open until the test releases it.
type gatedMediaReconciler struct {
	inner      *moderation.MediaReconciler
	reconciled chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (g *gatedMediaReconciler) ReconcileTx(ctx context.Context, tx *sql.Tx, subjectURI string) ([]moderation.MediaBlock, error) {
	blocks, err := g.inner.ReconcileTx(ctx, tx, subjectURI)
	g.once.Do(func() { close(g.reconciled) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return blocks, err
}

func (g *gatedMediaReconciler) Purge(ctx context.Context, blocks []moderation.MediaBlock) {
	g.inner.Purge(ctx, blocks)
}

// holdRestoreBeforeCommit parks a mutation at its idempotency-record insert,
// its last statement, after the decision update and media-block deactivation.
// It returns the gate's own connection, which stays usable for observation
// while the test's small pool is held by the parked transactions.
func holdRestoreBeforeCommit(t *testing.T, db *sql.DB) (*sql.Conn, func()) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		CREATE FUNCTION hold_moderation_idempotency_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(hashtextextended(current_database(), 1));
			RETURN NEW;
		END;
		$$;
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		CREATE TRIGGER hold_moderation_idempotency_insert BEFORE INSERT ON moderation_idempotency_keys
		FOR EACH ROW EXECUTE FUNCTION hold_moderation_idempotency_insert();
	`)
	require.NoError(t, err)
	connection, err := db.Conn(t.Context())
	require.NoError(t, err)
	_, err = connection.ExecContext(t.Context(), `SELECT pg_advisory_lock(hashtextextended(current_database(), 1))`)
	require.NoError(t, err)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		var unlocked bool
		assert.NoError(t, connection.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended(current_database(), 1))`).Scan(&unlocked))
		assert.True(t, unlocked)
		assert.NoError(t, connection.Close())
	}
	t.Cleanup(release)
	return connection, release
}

// countLockWaiters counts backends of this database waiting on a heavyweight
// lock, split into advisory-lock waits (the test's own gates) and all others.
func countLockWaiters(ctx context.Context, observer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (advisory, other int, err error) {
	err = observer.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE wait_event = 'advisory'),
		       count(*) FILTER (WHERE wait_event <> 'advisory')
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
	`).Scan(&advisory, &other)
	return advisory, other, err
}

func moderationConsumerCommentEvent(subject moderation.StrongRef, authorDID, rootURI, rootCID, operation, recordCID, imageCID string) *jetstream.JetstreamEvent {
	rkey := subject.URI[strings.LastIndex(subject.URI, "/")+1:]
	return &jetstream.JetstreamEvent{
		Did: authorDID, Kind: "commit", TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: testkit.TID(), Operation: operation, Collection: moderation.CommentCollection,
			RKey: rkey, CID: recordCID,
			Record: map[string]interface{}{
				"$type": moderation.CommentCollection, "content": "author rewrite",
				"reply": map[string]interface{}{
					"root":   map[string]interface{}{"uri": rootURI, "cid": rootCID},
					"parent": map[string]interface{}{"uri": rootURI, "cid": rootCID},
				},
				"embed": map[string]interface{}{
					"$type": "social.coves.embed.images",
					"images": []interface{}{map[string]interface{}{"alt": "rewritten image", "image": map[string]interface{}{
						"$type": "blob", "ref": map[string]interface{}{"$link": imageCID}, "mimeType": "image/png", "size": 10,
					}}},
				},
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	}
}

// A restore and the comment consumer's in-transaction media reconciliation
// race on one removed comment. Whichever commits first, no active media block
// may be left tied to a restored action, and a consumer write that commits
// first must be seen by the restore. The comment row may be absent — purged by
// account deletion and then recreated by a fresh insert — in which case no
// content-row lock orders the two.
func TestModerationRestoreRacesCommentConsumer(t *testing.T) {
	for _, test := range []struct {
		name          string
		rowPresent    bool
		consumerFirst bool
	}{
		{name: "consumer edit commits first", rowPresent: true, consumerFirst: true},
		{name: "restore commits before consumer edit", rowPresent: true},
		{name: "consumer reinsertion of a purged row commits first", consumerFirst: true},
		{name: "restore commits before consumer reinsertion of a purged row"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			subject, authorDID, _ := indexedModerationComment(t, db, true, "")
			var rootURI, rootCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT root_uri, root_cid FROM comments WHERE uri = $1`, subject.URI).Scan(&rootURI, &rootCID))
			service := newPostgresModerationService(db)
			removed, err := service.RemoveContent(t.Context(), fixtures.DID("consumerraceadmin"), moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "consumer-race-remove",
				Reason: moderationConcurrencyReason,
			})
			require.NoError(t, err)
			require.NotNil(t, removed.Action)
			operation := "update"
			if !test.rowPresent {
				// Account deletion purges the row outright (user_repo), leaving
				// the moderation decision and no content row to lock.
				_, err := db.ExecContext(t.Context(), `DELETE FROM comments WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
				operation = "create"
			}

			gate := &gatedMediaReconciler{
				inner:      moderation.NewMediaReconciler(postgres.NewModerationRepository(db), fixtures.InstanceDID(), nil),
				reconciled: make(chan struct{}), release: make(chan struct{}),
			}
			consumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db, jetstream.WithCommentMediaReconciler(gate))
			event := moderationConsumerCommentEvent(subject, authorDID, rootURI, rootCID, operation, moderationImageCIDOne, moderationImageCIDTwo)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			consumerDone := make(chan error, 1)
			restoreDone := make(chan moderationConcurrentOutcome, 1)
			startConsumer := func() {
				go func() { consumerDone <- consumer.HandleEvent(ctx, event) }()
			}
			startRestore := func() {
				go func() {
					result, restoreErr := service.RestoreContent(ctx, fixtures.DID("consumerraceadmin"), moderation.RestoreContentRequest{
						ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1",
						IdempotencyKey: "consumer-race-restore", Reason: moderationConcurrencyReason,
					})
					restoreDone <- moderationConcurrentOutcome{result: result, err: restoreErr}
				}()
			}

			if test.consumerFirst {
				startConsumer()
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					select {
					case <-gate.reconciled:
						return true, nil
					case consumerErr := <-consumerDone:
						return false, fmt.Errorf("consumer finished before reconciling: %v", consumerErr)
					default:
						return false, nil
					}
				}, testkit.WithDescription("consumer holding its reconciled write open"))
				startRestore()
				// The restore either blocks behind the open consumer write or,
				// if nothing orders them, runs to completion.
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					if len(restoreDone) > 0 {
						return true, nil
					}
					_, waiting, err := countLockWaiters(t.Context(), db)
					return waiting > 0, err
				}, testkit.WithDescription("restore waiting on the consumer's locks or finished"))
				close(gate.release)
			} else {
				close(gate.release)
				observer, releaseRestore := holdRestoreBeforeCommit(t, db)
				startRestore()
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					if len(restoreDone) > 0 {
						return false, fmt.Errorf("restore finished before reaching its commit gate")
					}
					parked, _, err := countLockWaiters(t.Context(), observer)
					return parked == 1, err
				}, testkit.WithDescription("restore parked before commit"))
				startConsumer()
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					if len(consumerDone) > 0 {
						return true, nil
					}
					_, waiting, err := countLockWaiters(t.Context(), observer)
					return waiting > 0, err
				}, testkit.WithDescription("consumer waiting on the restore's locks or finished"))
				releaseRestore()
			}

			require.NoError(t, <-consumerDone)
			restored := <-restoreDone
			var indexedCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT cid FROM comments WHERE uri = $1`, subject.URI).Scan(&indexedCID))
			assert.Equal(t, moderationImageCIDOne, indexedCID, "the consumer write must be indexed")
			var activeBlocks int
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT count(*) FROM moderation_media_blocks WHERE action_id = $1 AND active
			`, removed.Action.ID).Scan(&activeBlocks))
			var decisionActive bool
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT active FROM moderation_decisions WHERE subject_uri = $1 AND kind = 'removal'
			`, subject.URI).Scan(&decisionActive))

			if test.rowPresent && test.consumerFirst {
				// The restore waited for the edit and saw the reviewed CID go stale.
				require.ErrorIs(t, restored.err, moderation.ErrContentChanged)
				assert.True(t, decisionActive)
				assert.Equal(t, 1, activeBlocks, "the edit's new image stays blocked under the still-active removal")
				assert.Zero(t, countModerationActions(t, db, subject.URI, moderation.ActionRestore))
				return
			}
			require.NoError(t, restored.err)
			require.NotNil(t, restored.result)
			assert.Equal(t, moderation.OutcomeApplied, restored.result.Outcome)
			assert.False(t, decisionActive)
			assert.Zero(t, activeBlocks, "no active media block may remain tied to a restored action")
			blocked, err := postgres.NewModerationRepository(db).IsBlocked(t.Context(), authorDID, moderationImageCIDTwo)
			require.NoError(t, err)
			assert.False(t, blocked, "the restored comment's image must be servable")
		})
	}
}

func TestModerationConcurrentRestoresReverseActionOnlyOnce(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, "")
	service := newPostgresModerationService(db)
	removed, err := service.RemoveContent(t.Context(), fixtures.DID("restoreraceadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "restore-race-remove",
		Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	require.Equal(t, "v1", removed.State.Version)
	connection, release := holdModerationActionInsert(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request := moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1", Reason: moderationConcurrencyReason,
	}
	first := request
	first.IdempotencyKey = "restore-race-first"
	second := request
	second.IdempotencyKey = "restore-race-second"
	firstAdminDID := fixtures.DID("restoreraceone")
	secondAdminDID := fixtures.DID("restoreracetwo")
	require.NotEqual(t, firstAdminDID, secondAdminDID)
	results := startConcurrentModerationCalls(
		func() (*moderation.MutationResult, error) {
			return service.RestoreContent(ctx, firstAdminDID, first)
		},
		func() (*moderation.MutationResult, error) {
			return service.RestoreContent(ctx, secondAdminDID, second)
		},
	)
	waitForSubjectLockContention(t, connection)
	release()
	var applied, rejected int
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			assert.Truef(t, errors.Is(outcome.err, moderation.ErrStateConflict) || errors.Is(outcome.err, moderation.ErrInvalidDecision), "unexpected restore error: %v", outcome.err)
			assert.Nil(t, outcome.result)
			if errors.Is(outcome.err, moderation.ErrStateConflict) || errors.Is(outcome.err, moderation.ErrInvalidDecision) {
				rejected++
			}
			continue
		}
		require.NotNil(t, outcome.result)
		assert.Equal(t, moderation.OutcomeApplied, outcome.result.Outcome)
		if outcome.result.Outcome == moderation.OutcomeApplied {
			applied++
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, rejected)
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRestore))
}

func TestModerationConcurrentNewKeysCannotExceedActorCap(t *testing.T) {
	db := testkit.DB(t)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 2},
	)
	actor := fixtures.DID("capraceadmin")
	seed, _, _ := indexedModerationComment(t, db, true, "")
	firstSubject, _, _ := indexedModerationComment(t, db, true, "")
	secondSubject, _, _ := indexedModerationComment(t, db, true, "")
	seedResult, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
		Subject: seed, ExpectedVersion: "v0", IdempotencyKey: "cap-seed",
		Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	require.NotNil(t, seedResult)
	require.Equal(t, moderation.OutcomeApplied, seedResult.Outcome)
	// Hold the first admitted request inside its action insert until the other
	// request is blocked, so both overlap instead of running one after another.
	connection, release := holdModerationActionInsert(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pending := startConcurrentModerationCalls(
		func() (*moderation.MutationResult, error) {
			return service.RemoveContent(ctx, actor, moderation.RemoveContentRequest{
				Subject: firstSubject, ExpectedVersion: "v0", IdempotencyKey: "cap-first",
				Reason: moderationConcurrencyReason,
			})
		},
		func() (*moderation.MutationResult, error) {
			return service.RemoveContent(ctx, actor, moderation.RemoveContentRequest{
				Subject: secondSubject, ExpectedVersion: "v0", IdempotencyKey: "cap-second",
				Reason: moderationConcurrencyReason,
			})
		},
	)
	waitForGatedInsertAndOneWaiter(t, connection)
	release()
	var applied, rejected int
	for _, outcome := range [2]moderationConcurrentOutcome{<-pending, <-pending} {
		if outcome.err != nil {
			assert.ErrorIs(t, outcome.err, moderation.ErrInvalidRequest)
			assert.Nil(t, outcome.result)
			if errors.Is(outcome.err, moderation.ErrInvalidRequest) {
				rejected++
			}
			continue
		}
		require.NotNil(t, outcome.result)
		assert.Equal(t, moderation.OutcomeApplied, outcome.result.Outcome)
		if outcome.result.Outcome == moderation.OutcomeApplied {
			applied++
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, rejected)
	var liveKeys int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1 AND expires_at > NOW()
	`, actor).Scan(&liveKeys))
	assert.Equal(t, 2, liveKeys)
	firstActions := countModerationActions(t, db, firstSubject.URI, moderation.ActionRemove)
	secondActions := countModerationActions(t, db, secondSubject.URI, moderation.ActionRemove)
	assert.Equal(t, 1, firstActions+secondActions, "only one new subject may be removed")
	assert.LessOrEqual(t, firstActions, 1)
	assert.LessOrEqual(t, secondActions, 1)
}
