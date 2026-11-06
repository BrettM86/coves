//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labelHoldPoint is the step after which a held decision transaction blocks,
// still open, until the test releases it.
type labelHoldPoint int

const (
	// holdAfterLabelInsert blocks once the decision's label row is written,
	// with the label allocation lock held.
	holdAfterLabelInsert labelHoldPoint = iota
	// holdAfterRemovalDeactivation blocks once a restore has deactivated the
	// removal decision, before it writes its label.
	holdAfterRemovalDeactivation
)

// holdingLabelStore is the Postgres moderation store whose decision
// transactions on subjectURI block at point until release is called.
type holdingLabelStore struct {
	*postgres.ModerationRepository
	point       labelHoldPoint
	subjectURI  string
	reached     chan struct{}
	released    chan struct{}
	reachedOnce sync.Once
	releaseOnce sync.Once
}

// newHoldingLabelStore returns a holding store over db. The test's cleanup
// releases any transaction still held.
func newHoldingLabelStore(t *testing.T, db *sql.DB, point labelHoldPoint, subjectURI string) *holdingLabelStore {
	t.Helper()
	store := &holdingLabelStore{
		ModerationRepository: postgres.NewModerationRepository(db),
		point:                point, subjectURI: subjectURI,
		reached: make(chan struct{}), released: make(chan struct{}),
	}
	t.Cleanup(store.release)
	return store
}

func (s *holdingLabelStore) release() {
	s.releaseOnce.Do(func() { close(s.released) })
}

func (s *holdingLabelStore) hold(ctx context.Context) error {
	s.reachedOnce.Do(func() { close(s.reached) })
	select {
	case <-s.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *holdingLabelStore) InTransaction(ctx context.Context, fn func(ctx context.Context, tx moderation.Transaction) error) error {
	return s.ModerationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, holdingLabelTransaction{Transaction: tx, store: s})
	})
}

type holdingLabelTransaction struct {
	moderation.Transaction
	store *holdingLabelStore
}

func (tx holdingLabelTransaction) InsertLabel(ctx context.Context, issue labels.Issue) error {
	if err := tx.Transaction.InsertLabel(ctx, issue); err != nil {
		return err
	}
	if tx.store.point == holdAfterLabelInsert && issue.URI == tx.store.subjectURI {
		return tx.store.hold(ctx)
	}
	return nil
}

func (tx holdingLabelTransaction) SetRemovalDecision(ctx context.Context, authorityDID, subjectURI, actionID string, active bool) error {
	if err := tx.Transaction.SetRemovalDecision(ctx, authorityDID, subjectURI, actionID, active); err != nil {
		return err
	}
	if tx.store.point == holdAfterRemovalDeactivation && !active && subjectURI == tx.store.subjectURI {
		return tx.store.hold(ctx)
	}
	return nil
}

// runInBackground runs call on its own goroutine, tracked by wait, and
// delivers its error on the returned channel.
func runInBackground(wait *sync.WaitGroup, call func() error) <-chan error {
	done := make(chan error, 1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		done <- call()
	}()
	return done
}

// waitForHold waits until the held transaction reaches its hold point,
// failing if the call finishes first.
func waitForHold(t *testing.T, store *holdingLabelStore, call <-chan error, description string) {
	t.Helper()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case <-store.reached:
			return true, nil
		case err := <-call:
			return false, fmt.Errorf("the held call finished before its hold point: %v", err)
		default:
			return false, nil
		}
	}, testkit.WithDescription("%s", description))
}

// ungrantedAdvisoryLockWaiters counts backends of this test database waiting
// for an advisory lock another backend holds.
func ungrantedAdvisoryLockWaiters(ctx context.Context, db *sql.DB) (int, error) {
	var waiters int
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	`).Scan(&waiters)
	return waiters, err
}

// awaitCall waits for a background call's error.
func awaitCall(t *testing.T, call <-chan error, description string) error {
	t.Helper()
	var result error
	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		select {
		case result = <-call:
			return true, nil
		default:
			return false, nil
		}
	}, testkit.WithDescription("%s", description))
	return result
}

type servedLabel struct {
	uri, val string
	neg      bool
}

func servedLabels(rows []labels.Label) []servedLabel {
	var served []servedLabel
	for _, row := range rows {
		served = append(served, servedLabel{uri: row.URI, val: row.Val, neg: row.Neg})
	}
	return served
}

func TestLabelCommittedPrefixReaderNeverSkipsAnEarlierSequence(t *testing.T) {
	db := testkit.DB(t)
	var background sync.WaitGroup
	t.Cleanup(background.Wait)
	heldSubject, laterSubject := indexedLabelPost(t, db), indexedLabelPost(t, db)
	heldStore := newHoldingLabelStore(t, db, holdAfterLabelInsert, heldSubject.URI)
	heldService := newPostgresModerationServiceWith(db, heldStore, issuanceLabelSigningKey(t))
	laterService := newLabelPublishingModerationService(t, db)
	ctx := context.Background()

	held := runInBackground(&background, func() error {
		_, err := heldService.RemoveContent(ctx, fixtures.DID("labelprefixadmina"), moderation.RemoveContentRequest{
			Subject: heldSubject, ExpectedVersion: "v0", IdempotencyKey: "prefix-held-remove", Reason: moderationConcurrencyReason,
		})
		return err
	})
	waitForHold(t, heldStore, held, "decision A holding open with its label row written")

	later := runInBackground(&background, func() error {
		_, err := laterService.RemoveContent(ctx, fixtures.DID("labelprefixadminb"), moderation.RemoveContentRequest{
			Subject: laterSubject, ExpectedVersion: "v0", IdempotencyKey: "prefix-later-remove", Reason: moderationConcurrencyReason,
		})
		return err
	})
	laterFinished := false
	var laterErr error
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case laterErr = <-later:
			laterFinished = true
			return true, nil
		default:
		}
		waiters, err := ungrantedAdvisoryLockWaiters(t.Context(), db)
		return waiters > 0, err
	}, testkit.WithDescription("decision B committed or waiting on an ungranted advisory lock"))

	repository := postgres.NewLabelRepository(db)
	query := labels.Query{URIPatterns: []string{heldSubject.URI, laterSubject.URI}, Limit: 50}
	firstRead, err := repository.Query(t.Context(), query)
	require.NoError(t, err)

	heldStore.release()
	require.NoError(t, awaitCall(t, held, "decision A to commit"))
	if !laterFinished {
		laterErr = awaitCall(t, later, "decision B to commit")
	}
	require.NoError(t, laterErr)

	query.Cursor = firstRead.Cursor
	secondRead, err := repository.Query(t.Context(), query)
	require.NoError(t, err)

	served := append(servedLabels(firstRead.Labels), servedLabels(secondRead.Labels)...)
	assert.Equal(t, []servedLabel{
		{uri: heldSubject.URI, val: "!takedown", neg: false},
		{uri: laterSubject.URI, val: "!takedown", neg: false},
	}, served, "a reader continuing from its cursor must see every committed label exactly once")
}

func TestLabelExportConcurrentRestoreEndsNegated(t *testing.T) {
	for _, test := range []struct {
		name string
		hold labelHoldPoint
		// exportFinishesWhileHeld: the export completes while the restore is
		// held; otherwise it must be seen waiting on the label allocation lock.
		exportFinishesWhileHeld bool
		want                    []servedLabel
	}{
		{
			name: "restore holds the allocation lock after its label insert while export waits",
			hold: holdAfterLabelInsert,
			want: []servedLabel{{val: "!takedown", neg: true}},
		},
		{
			name: "restore holds after deactivating the removal while export completes",
			hold: holdAfterRemovalDeactivation, exportFinishesWhileHeld: true,
			want: []servedLabel{{val: "!takedown", neg: false}, {val: "!takedown", neg: true}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			var background sync.WaitGroup
			t.Cleanup(background.Wait)
			subject := indexedLabelPost(t, db)
			actor := fixtures.DID("labelexportraceadmin")
			removed, err := newPostgresModerationService(db).RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "export-race-remove", Reason: moderationConcurrencyReason,
			})
			require.NoError(t, err)
			require.Empty(t, allStoredLabels(t, db), "a removal made with publication disabled publishes nothing")

			heldStore := newHoldingLabelStore(t, db, test.hold, subject.URI)
			restoringService := newPostgresModerationServiceWith(db, heldStore, issuanceLabelSigningKey(t))
			publisher, err := labels.NewPublisher(labels.Config{
				SourceDID: fixtures.InstanceDID(), LabelerEndpoint: bootPublisherLabelerEndpoint, Key: issuanceLabelSigningKey(t),
			})
			require.NoError(t, err)
			ctx := context.Background()

			restore := runInBackground(&background, func() error {
				_, err := restoringService.RestoreContent(ctx, actor, moderation.RestoreContentRequest{
					ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1",
					IdempotencyKey: "export-race-restore", Reason: issuanceRestoreReason,
				})
				return err
			})
			waitForHold(t, heldStore, restore, "the restore holding open at its hold point")

			export := runInBackground(&background, func() error {
				return publisher.Start(ctx, postgres.NewLabelRepository(db))
			})
			if test.exportFinishesWhileHeld {
				require.NoError(t, awaitCall(t, export, "the export to complete while the restore is held"))
			} else {
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					select {
					case err := <-export:
						return false, fmt.Errorf("the export finished while the restore held the allocation lock: %v", err)
					default:
					}
					waiters, err := ungrantedAdvisoryLockWaiters(t.Context(), db)
					return waiters > 0, err
				}, testkit.WithDescription("the export waiting on the label allocation lock"))
			}

			heldStore.release()
			require.NoError(t, awaitCall(t, restore, "the restore to commit"))
			if !test.exportFinishesWhileHeld {
				require.NoError(t, awaitCall(t, export, "the export to commit"))
			}

			rows := storedLabels(t, db, subject.URI)
			var got []servedLabel
			for _, row := range rows {
				got = append(got, servedLabel{val: row.Val, neg: row.Neg})
			}
			assert.Equal(t, test.want, got, "labels for the subject in seq order")
			require.NotEmpty(t, rows)
			latest := rows[len(rows)-1]
			assert.Equal(t, "!takedown", latest.Val)
			assert.True(t, latest.Neg, "the subject's latest label must negate the takedown")
		})
	}
}

func TestLabelRotationWaitsForAHeldDecisionAndResignsItsLabel(t *testing.T) {
	db := testkit.DB(t)
	var background sync.WaitGroup
	t.Cleanup(background.Wait)
	seedIssuanceKeyLabels(t, db)
	heldSubject := indexedLabelPost(t, db)
	heldStore := newHoldingLabelStore(t, db, holdAfterLabelInsert, heldSubject.URI)
	heldService := newPostgresModerationServiceWith(db, heldStore, issuanceLabelSigningKey(t))
	publisher, err := labels.NewPublisher(labels.Config{
		SourceDID: fixtures.InstanceDID(), LabelerEndpoint: bootPublisherLabelerEndpoint,
		Key: labelPrivateKey(t, rotationLabelKeyPrivateMultibase), RetiringKey: issuanceLabelKeyPublicMultibase,
	})
	require.NoError(t, err)
	ctx := context.Background()

	held := runInBackground(&background, func() error {
		_, err := heldService.RemoveContent(ctx, fixtures.DID("labelrotationholdadmin"), moderation.RemoveContentRequest{
			Subject: heldSubject, ExpectedVersion: "v0", IdempotencyKey: "rotation-held-remove", Reason: moderationConcurrencyReason,
		})
		return err
	})
	waitForHold(t, heldStore, held, "the decision holding open with its label written under the retiring key")

	boot := runInBackground(&background, func() error {
		return publisher.Start(ctx, postgres.NewLabelRepository(db))
	})
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case err := <-boot:
			return false, fmt.Errorf("the rotating boot finished while a decision held the label allocation lock: %v", err)
		default:
		}
		waiters, err := ungrantedAdvisoryLockWaiters(t.Context(), db)
		return waiters > 0, err
	}, testkit.WithDescription("the rotating boot waiting on the label allocation lock"))

	heldStore.release()
	require.NoError(t, awaitCall(t, held, "the held decision to commit"))
	require.NoError(t, awaitCall(t, boot, "the rotating boot to complete"))

	rows := allStoredLabels(t, db)
	require.Len(t, rows, 5, "the four seeded labels and the held decision's label")
	for _, row := range rows {
		assert.Equal(t, rotationLabelKeyPublicMultibase, row.SignerKey, "seq %d must be signed by the new key", row.Seq)
		assert.NoError(t, labelSignatureError(row, labelPublicKey(t, rotationLabelKeyPublicMultibase)), "seq %d must verify with the new key", row.Seq)
	}
}
