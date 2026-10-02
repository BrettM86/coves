//go:build integration

package routes_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cdnPurgeSpam = "social.coves.moderation.defs#reasonSpam"

type moderationCDNClock struct {
	mu sync.Mutex
	at time.Time
}

func (clock *moderationCDNClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.at
}

func (clock *moderationCDNClock) Set(at time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.at = at
}

func newModerationCDNPurgeHarness(t *testing.T, endpoint *testkit.CloudflarePurgeEndpoint, clock *moderationCDNClock, writeTimeout time.Duration, wrapStore ...func(moderation.Store) moderation.Store) *moderationMediaHarness {
	var configure []func(*moderationMediaHarnessConfig)
	if len(wrapStore) != 0 {
		configure = append(configure, func(options *moderationMediaHarnessConfig) { options.wrapStore = wrapStore[0] })
	}
	return newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, writeTimeout, configure...)
}

func newModerationCDNPurgeHarnessConfigured(t *testing.T, endpoint *testkit.CloudflarePurgeEndpoint, clock *moderationCDNClock, writeTimeout time.Duration, configure ...func(*moderationMediaHarnessConfig)) *moderationMediaHarness {
	t.Helper()
	purger := newMediaCloudflarePurger(t, endpoint, "route-cdn-token-SENTINEL")
	configureHarness := func(options *moderationMediaHarnessConfig) {
		options.now = clock.Now
		options.targets = func(db *sql.DB, now func() time.Time) moderation.CDNPurgeTargets {
			var targetStore moderation.CDNPurgeTargetStore = postgres.NewModerationRepository(db)
			if options.wrapTargetStore != nil {
				targetStore = options.wrapTargetStore(targetStore)
			}
			return moderation.NewCDNPurgeQueue(targetStore, purger, moderation.CDNPurgeQueueConfig{
				WriteTimeout: writeTimeout, Now: now, BatchSize: 100,
			})
		}
	}
	configuration := append([]func(*moderationMediaHarnessConfig){configureHarness}, configure...)
	h, _ := newModerationMediaHarness(t, false, configuration...)
	require.NotNil(t, h.queue)
	return h
}

func moderationCDNTarget(t *testing.T, db *sql.DB, owner, cid string) (target struct {
	state, failureCode string
	attempts           int
	nextAttemptAt      time.Time
}) {
	t.Helper()
	var failure sql.NullString
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT state, attempts, next_attempt_at, last_failure_code
		FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2
	`, owner, cid).Scan(&target.state, &target.attempts, &target.nextAttemptAt, &failure))
	target.failureCode = failure.String
	return target
}

func moderationCDNTargetCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_media_purges`).Scan(&count))
	return count
}

// These are the Cloudflare files for the six configured image presets, written
// from the fixture base, owner DID and CID rather than using the URL hydrator.
func moderationCDNFiles(owner, cid string) []string {
	return []string{
		"https://img.example.test/img/avatar/plain/" + owner + "/" + cid,
		"https://img.example.test/img/avatar_small/plain/" + owner + "/" + cid,
		"https://img.example.test/img/banner/plain/" + owner + "/" + cid,
		"https://img.example.test/img/content_preview/plain/" + owner + "/" + cid,
		"https://img.example.test/img/content_full/plain/" + owner + "/" + cid,
		"https://img.example.test/img/embed_thumbnail/plain/" + owner + "/" + cid,
	}
}

func awaitModerationCDN[T any](t *testing.T, event string, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
		var zero T
		return zero
	}
}

func waitModerationCDN(t *testing.T, queue *moderation.CDNPurgeQueue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, queue.Wait(ctx))
}

// The second handoff blocks after the removal commits, before its immediate
// attempt can claim the newly re-pended target.
type gatedModerationCDNTargets struct {
	queue   *moderation.CDNPurgeQueue
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (targets *gatedModerationCDNTargets) queueForRecorder() *moderation.CDNPurgeQueue {
	return targets.queue
}

func (targets *gatedModerationCDNTargets) PurgeAfterCommit(ctx context.Context, blobs []imageproxy.BlockedBlob) {
	targets.mu.Lock()
	targets.calls++
	second := targets.calls == 2
	targets.mu.Unlock()
	if second {
		close(targets.entered)
		<-targets.release
	}
	targets.queue.PurgeAfterCommit(ctx, blobs)
}

func TestModerationCDNPurgeRependDuringInflightClaim(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "held success cannot complete new generation", status: http.StatusOK, body: `{"success":true}`},
		{name: "held failure cannot reschedule new claim", status: http.StatusInternalServerError, body: `{"success":false}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
			endpoint := testkit.NewCloudflarePurgeEndpoint(t)
			gate := &gatedModerationCDNTargets{entered: make(chan struct{}), release: make(chan struct{})}
			var releaseGate sync.Once
			releaseFresh := func() { releaseGate.Do(func() { close(gate.release) }) }
			t.Cleanup(releaseFresh)
			purger := newMediaCloudflarePurger(t, endpoint, "route-cdn-token-SENTINEL")
			h, _ := newModerationMediaHarness(t, false, func(options *moderationMediaHarnessConfig) {
				options.now = clock.Now
				options.targets = func(db *sql.DB, now func() time.Time) moderation.CDNPurgeTargets {
					gate.queue = moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(db), purger, moderation.CDNPurgeQueueConfig{
						WriteTimeout: 90 * time.Second, Now: now, BatchSize: 100,
					})
					return gate
				}
			})
			h.queue = gate.queue
			imageCID := mediaImageCID("cdn repend during claim " + testCase.name)
			subject := h.comment(t, h.ownerA, imageCID)
			removed := h.remove(t, subject, cdnPurgeSpam)
			waitModerationCDN(t, h.queue)
			initial := moderationCDNTarget(t, h.db, h.ownerA, imageCID)
			require.Equal(t, "pending", initial.state)
			require.Equal(t, 0, initial.attempts)
			require.Equal(t, time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC), initial.nextAttemptAt)
			require.Len(t, endpoint.Requests(), 1)
			assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[0].Files)

			clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
			endpoint.QueueResponse(testCase.status, testCase.body)
			entered, releaseHeld := endpoint.HoldNextRequest(t)
			swept := make(chan error, 1)
			go func() { swept <- h.queue.Sweep(t.Context()) }()
			awaitModerationCDN(t, "held sweep request", entered)
			clock.Set(time.Date(2026, 9, 28, 12, 3, 0, 0, time.UTC))
			restored := h.restore(t, subject, removed)
			fresh := make(chan error, 1)
			go func() {
				result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
					Subject: subject, ExpectedVersion: restored.State.Version,
					IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: cdnPurgeSpam,
				})
				if err == nil && (result == nil || result.Outcome != moderation.OutcomeApplied) {
					err = errors.New("fresh removal did not apply")
				}
				fresh <- err
			}()
			awaitModerationCDN(t, "committed fresh removal before immediate attempt", gate.entered)
			// A second connection sees the reset target while the new handoff is gated.
			var state, nextAttempt, failureCode string
			var attempts int
			require.NoError(t, h.db.QueryRowContext(t.Context(), `
				SELECT state, attempts, next_attempt_at::text, COALESCE(last_failure_code, '')
				FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2
			`, h.ownerA, imageCID).Scan(&state, &attempts, &nextAttempt, &failureCode))
			require.Equal(t, "pending", state)
			require.Equal(t, 0, attempts)
			require.Equal(t, "-infinity", nextAttempt)
			require.Empty(t, failureCode)
			require.Len(t, endpoint.Requests(), 2, "the fresh immediate attempt is still gated")
			assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[1].Files)

			releaseHeld()
			require.NoError(t, awaitModerationCDN(t, "stale sweep to return", swept))
			require.NoError(t, h.db.QueryRowContext(t.Context(), `
				SELECT state, attempts, next_attempt_at::text, COALESCE(last_failure_code, '')
				FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2
			`, h.ownerA, imageCID).Scan(&state, &attempts, &nextAttempt, &failureCode))
			assert.Equal(t, "pending", state)
			assert.Equal(t, 0, attempts)
			assert.Equal(t, "-infinity", nextAttempt)
			assert.Empty(t, failureCode)
			assert.Len(t, endpoint.Requests(), 2)

			releaseFresh()
			require.NoError(t, awaitModerationCDN(t, "fresh removal to return", fresh))
			waitModerationCDN(t, h.queue)
			repended := moderationCDNTarget(t, h.db, h.ownerA, imageCID)
			require.Equal(t, "pending", repended.state)
			assert.Equal(t, 0, repended.attempts)
			assert.Empty(t, repended.failureCode)
			assert.Equal(t, time.Date(2026, 9, 28, 12, 4, 30, 0, time.UTC), repended.nextAttemptAt)
			require.Len(t, endpoint.Requests(), 3)
			assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[2].Files)
			clock.Set(time.Date(2026, 9, 28, 12, 4, 30, 0, time.UTC))
			require.NoError(t, h.queue.Sweep(t.Context()))
			require.Len(t, endpoint.Requests(), 4)
			assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[3].Files)
			assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
		})
	}
}

func TestModerationCDNPurgeAfterCommittedRemoval(t *testing.T) {
	imageCID := mediaImageCID("cdn committed removal")
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	entered, release := endpoint.HoldNextRequest(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	subject := h.comment(t, h.ownerA, imageCID)
	callerCtx, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	removed := make(chan error, 1)
	actorDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin"))
	idempotencyKey := "remove-" + testkit.UniqueID(t)
	go func() {
		result, err := h.moderation.RemoveContent(callerCtx, actorDID, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: idempotencyKey, Reason: cdnPurgeSpam,
		})
		if err == nil && (result == nil || result.Outcome != moderation.OutcomeApplied) {
			err = errors.New("removal did not apply")
		}
		removed <- err
	}()
	require.NoError(t, awaitModerationCDN(t, "removal to return while the CDN request is held", removed))
	awaitModerationCDN(t, "immediate CDN purge request", entered)
	blocked, err := postgres.NewModerationRepository(h.db).IsBlocked(t.Context(), h.ownerA, imageCID)
	require.NoError(t, err)
	assert.True(t, blocked, "a separate connection must see the committed block before Cloudflare responds")
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
	cancelCaller()
	release()
	waitModerationCDN(t, h.queue)
	require.Len(t, endpoint.Requests(), 1)
	assert.NoError(t, endpoint.Requests()[0].DecodeError)
	assert.Equal(t, http.MethodPost, endpoint.Requests()[0].Method)
	assert.Equal(t, "/zones/zone-abc/purge_cache", endpoint.Requests()[0].Path)
	assert.Equal(t, "Bearer route-cdn-token-SENTINEL", endpoint.Requests()[0].Authorization)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[0].Files)
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 29, 999999000, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 1)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 2)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[1].Files)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
	clock.Set(time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 2)
}

func TestModerationCDNPurgeWaitHonorsCallerDeadline(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	entered, release := endpoint.HoldNextRequest(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	imageCID := mediaImageCID("cdn bounded wait")
	h.remove(t, h.comment(t, h.ownerA, imageCID), cdnPurgeSpam)
	awaitModerationCDN(t, "immediate CDN purge request", entered)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waited := make(chan error, 1)
	go func() { waited <- h.queue.Wait(ctx) }()
	assert.ErrorIs(t, awaitModerationCDN(t, "cancelled Wait to return", waited), context.Canceled)
	release()
	waitModerationCDN(t, h.queue)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
	assert.Len(t, endpoint.Requests(), 2)
}

func TestModerationCDNPurgeRemovalAndRestoreLifecycle(t *testing.T) {
	t.Run("stale version", func(t *testing.T) {
		clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
		endpoint := testkit.NewCloudflarePurgeEndpoint(t)
		h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
		imageCID := mediaImageCID("cdn stale version")
		subject := h.comment(t, h.ownerA, imageCID)
		result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v7", IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: cdnPurgeSpam,
		})
		require.ErrorIs(t, err, moderation.ErrStateConflict)
		assert.Nil(t, result)
		assert.Zero(t, moderationCDNTargetCount(t, h.db))
		assert.Empty(t, endpoint.Requests())
	})
	t.Run("pending restore, completion, and fresh removal", func(t *testing.T) {
		clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
		endpoint := testkit.NewCloudflarePurgeEndpoint(t)
		endpoint.QueueResponse(http.StatusInternalServerError, `{"success":false}`)
		h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
		imageCID := mediaImageCID("cdn restore pending")
		subject := h.comment(t, h.ownerA, imageCID)
		removed := h.remove(t, subject, cdnPurgeSpam)
		waitModerationCDN(t, h.queue)
		failed := moderationCDNTarget(t, h.db, h.ownerA, imageCID)
		assert.Equal(t, "pending", failed.state)
		assert.Equal(t, 1, failed.attempts)
		assert.Equal(t, "http_500", failed.failureCode)
		assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), failed.nextAttemptAt)
		require.Len(t, endpoint.Requests(), 1)
		assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[0].Files)
		restored := h.restore(t, subject, removed)
		assert.Len(t, endpoint.Requests(), 1, "restore makes no request")
		clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
		require.NoError(t, h.queue.Sweep(t.Context()))
		require.Len(t, endpoint.Requests(), 2, "a pending target remains eligible after restore")
		assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[1].Files)
		assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
		result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: restored.State.Version,
			IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: cdnPurgeSpam,
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeApplied, result.Outcome)
		waitModerationCDN(t, h.queue)
		assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
		require.Len(t, endpoint.Requests(), 3)
		assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[2].Files)
	})
	t.Run("restore after completion", func(t *testing.T) {
		clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
		endpoint := testkit.NewCloudflarePurgeEndpoint(t)
		h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
		imageCID := mediaImageCID("cdn restore completed")
		subject := h.comment(t, h.ownerA, imageCID)
		removed := h.remove(t, subject, cdnPurgeSpam)
		waitModerationCDN(t, h.queue)
		clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
		require.NoError(t, h.queue.Sweep(t.Context()))
		assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
		h.restore(t, subject, removed)
		assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
		assert.Len(t, endpoint.Requests(), 2)
	})
}

func TestModerationCDNPurgeIllegalContentRecordsOnlyOwner(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	imageCID := mediaImageCID("cdn illegal content")
	h.remove(t, h.comment(t, h.ownerA, imageCID), "social.coves.moderation.defs#reasonIllegalContent")
	waitModerationCDN(t, h.queue)
	assert.Equal(t, 1, moderationCDNTargetCount(t, h.db))
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
	require.Len(t, endpoint.Requests(), 1)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[0].Files)
}

type failingCDNRecordStore struct{ moderation.Store }
type failingCDNRecordTransaction struct{ moderation.Transaction }

func (store failingCDNRecordStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	return store.Store.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, failingCDNRecordTransaction{tx})
	})
}

func (tx failingCDNRecordTransaction) RecordCDNPurgeTargets(ctx context.Context, blobs []imageproxy.BlockedBlob) error {
	if err := tx.Transaction.RecordCDNPurgeTargets(ctx, blobs); err != nil {
		return err
	}
	return errors.New("record targets failed after insert")
}

func TestModerationCDNPurgeRecordFailureRollsBackRemoval(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second, func(store moderation.Store) moderation.Store {
		return failingCDNRecordStore{store}
	})
	imageCID := mediaImageCID("cdn atomicity")
	subject := h.comment(t, h.ownerA, imageCID)
	result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: cdnPurgeSpam,
	})
	require.Error(t, err)
	assert.Nil(t, result)
	blocked, err := postgres.NewModerationRepository(h.db).IsBlocked(t.Context(), h.ownerA, imageCID)
	require.NoError(t, err)
	assert.False(t, blocked)
	assert.Zero(t, moderationCDNTargetCount(t, h.db))
	assert.Empty(t, endpoint.Requests())
}

func TestModerationCDNPurgeImmediateAttemptIsRestrictedToRemoval(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	endpoint.QueueResponse(http.StatusInternalServerError, `{"success":false}`)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	imageCID := mediaImageCID("cdn shared cid")
	h.remove(t, h.comment(t, h.ownerB, imageCID), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	otherBefore := moderationCDNTarget(t, h.db, h.ownerB, imageCID)
	assert.Equal(t, "pending", otherBefore.state)
	assert.Equal(t, 1, otherBefore.attempts)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), otherBefore.nextAttemptAt)
	require.Len(t, endpoint.Requests(), 1)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerB, imageCID), endpoint.Requests()[0].Files)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC))
	h.remove(t, h.comment(t, h.ownerA, imageCID), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	require.Len(t, endpoint.Requests(), 2)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[1].Files)
	assert.Equal(t, otherBefore, moderationCDNTarget(t, h.db, h.ownerB, imageCID))
	h.remove(t, h.comment(t, h.ownerA), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	assert.Len(t, endpoint.Requests(), 2, "a removal without an image makes no purge request")
}

func TestModerationCDNPurgeUnconfiguredWritesNoTargets(t *testing.T) {
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h, _ := newModerationMediaHarness(t, false)
	imageCID := mediaImageCID("cdn unconfigured restore")
	subject := h.comment(t, h.ownerA, imageCID)
	removed := h.remove(t, subject, cdnPurgeSpam)
	h.restore(t, subject, removed)
	assert.Zero(t, moderationCDNTargetCount(t, h.db))
	assert.Empty(t, endpoint.Requests())
}

// assertModerationCDNUnclaimed checks a recorded target no attempt has claimed:
// still pending, no attempts, and due immediately.
func assertModerationCDNUnclaimed(t *testing.T, db *sql.DB, owner, cid string) {
	t.Helper()
	var state, nextAttempt string
	var attempts int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT state, attempts, next_attempt_at::text
		FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2
	`, owner, cid).Scan(&state, &attempts, &nextAttempt))
	assert.Equal(t, "pending", state)
	assert.Zero(t, attempts)
	assert.Equal(t, "-infinity", nextAttempt)
}

// interruptingCDNPurger acknowledges one pair, then holds until the caller's
// context ends and reports every other pair as a transport failure, the way a
// Cloudflare request cut off by the sweep's cycle deadline does.
type interruptingCDNPurger struct {
	acknowledged imageproxy.BlockedBlob
	entered      chan struct{}
}

func (purger *interruptingCDNPurger) PurgeBlobs(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	close(purger.entered)
	<-ctx.Done()
	var result imageproxy.CDNPurgeResult
	for _, blob := range blobs {
		if blob == purger.acknowledged {
			result.Acknowledged = append(result.Acknowledged, blob)
			continue
		}
		result.Failed = append(result.Failed, imageproxy.CDNPurgeFailure{Blob: blob, Code: "transport"})
	}
	return result
}

// Not parallel: this test replaces slog.Default while the sweep runs.
func TestModerationCDNPurgeSweepRecordsOutcomesAfterContextEnds(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	first, second := mediaImageCID("cdn interrupted first"), mediaImageCID("cdn interrupted second")
	h.remove(t, moderationCDNTwoImagePost(t, h, first, second), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	require.Equal(t, time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC), moderationCDNTarget(t, h.db, h.ownerA, second).nextAttemptAt)

	logs := &postMediaLogCapture{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	purger := &interruptingCDNPurger{acknowledged: imageproxy.BlockedBlob{OwnerDID: h.ownerA, CID: first}, entered: make(chan struct{})}
	queue := moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(h.db), purger,
		moderation.CDNPurgeQueueConfig{Now: clock.Now, WriteTimeout: 90 * time.Second, BatchSize: 100})
	sweepCtx, cancelSweep := context.WithCancel(t.Context())
	defer cancelSweep()
	swept := make(chan error, 1)
	go func() { swept <- queue.Sweep(sweepCtx) }()
	awaitModerationCDN(t, "sweep purge request", purger.entered)
	cancelSweep()
	awaitModerationCDN(t, "interrupted sweep to return", swept)

	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, first).state,
		"an acknowledged pair completes even though the sweep's context ended")
	interrupted := moderationCDNTarget(t, h.db, h.ownerA, second)
	assert.Equal(t, "pending", interrupted.state)
	assert.Equal(t, 1, interrupted.attempts)
	assert.Equal(t, "interrupted", interrupted.failureCode)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 2, 30, 0, time.UTC), interrupted.nextAttemptAt)
	var warnings int
	for _, record := range logs.snapshot() {
		attrs := make(map[string]slog.Value)
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value
			return true
		})
		if record.Level == slog.LevelError {
			assert.NotEqual(t, second, attrs["cid"].String(), "an interrupted pair is not logged as a purge failure")
		}
		if record.Level == slog.LevelWarn && attrs["count"].Kind() == slog.KindInt64 && attrs["count"].Int64() == 1 {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings, "one warning counts the interrupted pairs")

	clock.Set(time.Date(2026, 9, 28, 12, 2, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 2)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, second), endpoint.Requests()[1].Files)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, second).state)
}

func TestModerationCDNPurgeSaturatedImmediateAttemptsLeaveTargetToSweep(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	held := make([]func(), 0, 4)
	for slot := range 4 {
		entered, release := endpoint.HoldNextRequest(t)
		held = append(held, release)
		h.remove(t, h.comment(t, h.ownerA, mediaImageCID(fmt.Sprintf("cdn saturated held %d", slot))), cdnPurgeSpam)
		awaitModerationCDN(t, "held immediate attempt", entered)
	}
	extra := mediaImageCID("cdn saturated extra")
	h.remove(t, h.comment(t, h.ownerA, extra), cdnPurgeSpam)
	for _, release := range held {
		release()
	}
	waitModerationCDN(t, h.queue)
	require.Len(t, endpoint.Requests(), 4, "a saturated queue makes no immediate request")
	assertModerationCDNUnclaimed(t, h.db, h.ownerA, extra)

	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 5, "the sweep purges the target the saturated queue skipped")
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, extra), endpoint.Requests()[4].Files)
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, extra).state)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, extra).state)
}

func TestModerationCDNPurgeClosedQueueLeavesTargetsToSweep(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	h.queue.Close()
	imageCID := mediaImageCID("cdn closed queue")
	h.remove(t, h.comment(t, h.ownerA, imageCID), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	assert.Empty(t, endpoint.Requests(), "a closed queue makes no immediate request")
	assertModerationCDNUnclaimed(t, h.db, h.ownerA, imageCID)

	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 1)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, imageCID), endpoint.Requests()[0].Files)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 2)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, imageCID).state)
}
