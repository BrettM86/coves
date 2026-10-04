//go:build integration

package routes_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type moderationCDNScriptedPurger struct {
	mu      sync.Mutex
	calls   [][]imageproxy.BlockedBlob
	result  func([]imageproxy.BlockedBlob) imageproxy.CDNPurgeResult
	enter   chan struct{}
	release chan struct{}
}

func (purger *moderationCDNScriptedPurger) PurgeBlobs(_ context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	copyOfBlobs := append([]imageproxy.BlockedBlob(nil), blobs...)
	purger.mu.Lock()
	purger.calls = append(purger.calls, copyOfBlobs)
	purger.mu.Unlock()
	if purger.enter != nil {
		close(purger.enter)
		<-purger.release
	}
	return purger.result(copyOfBlobs)
}

func (purger *moderationCDNScriptedPurger) Calls() [][]imageproxy.BlockedBlob {
	purger.mu.Lock()
	defer purger.mu.Unlock()
	calls := make([][]imageproxy.BlockedBlob, len(purger.calls))
	for i, blobs := range purger.calls {
		calls[i] = append([]imageproxy.BlockedBlob(nil), blobs...)
	}
	return calls
}

func moderationCDNRetryQueue(h *moderationMediaHarness, clock *moderationCDNClock, purger moderation.CDNPurger, timeout time.Duration, batchSize int) *moderation.CDNPurgeQueue {
	return moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(h.db), purger,
		moderation.CDNPurgeQueueConfig{Now: clock.Now, WriteTimeout: timeout, BatchSize: batchSize})
}

func moderationCDNTwoImagePost(t *testing.T, h *moderationMediaHarness, first, second string) moderation.StrongRef {
	t.Helper()
	subject, owner := h.indexedImagePost(t, moderation.PostV2Collection, first)
	images := make([]any, 0, 2)
	for _, cid := range []string{first, second} {
		images = append(images, map[string]any{"alt": "test image", "image": map[string]any{
			"$type": "blob", "ref": map[string]any{"$link": cid}, "mimeType": "image/png", "size": 10,
		}})
	}
	embed, err := json.Marshal(map[string]any{"$type": "social.coves.embed.images", "images": images})
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, subject.URI, string(embed))
	require.NoError(t, err)
	h.pds.mu.Lock()
	h.pds.known[mediaBlobKey{owner, second}] = true
	h.pds.mu.Unlock()
	return subject
}

func TestModerationCDNPurgeSweepDrainsAllDueBatches(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	endpoint.SetResponse(http.StatusInternalServerError, `{"success":false}`)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 30*time.Second)
	first, second := mediaImageCID("cdn batch first"), mediaImageCID("cdn batch second")
	h.remove(t, moderationCDNTwoImagePost(t, h, first, second), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	require.Len(t, endpoint.Requests(), 1)
	assert.ElementsMatch(t, append(moderationCDNFiles(h.ownerA, first), moderationCDNFiles(h.ownerA, second)...), endpoint.Requests()[0].Files)
	for _, cid := range []string{first, second} {
		row := moderationCDNTarget(t, h.db, h.ownerA, cid)
		assert.Equal(t, "pending", row.state)
		assert.Equal(t, 1, row.attempts)
		assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), row.nextAttemptAt)
	}
	endpoint.SetResponse(http.StatusOK, `{"success":true}`)
	clock.Set(time.Date(2026, 9, 28, 12, 2, 0, 0, time.UTC))
	before := len(endpoint.Requests())
	queue := moderationCDNRetryQueue(h, clock, newMediaCloudflarePurger(t, endpoint, "batch-token"), 30*time.Second, 1)
	require.NoError(t, queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), before+2, "one Sweep must drain both batches")
	assert.Len(t, endpoint.Requests()[before].Files, 6)
	assert.Len(t, endpoint.Requests()[before+1].Files, 6)
	assert.ElementsMatch(t, append(moderationCDNFiles(h.ownerA, first), moderationCDNFiles(h.ownerA, second)...),
		append(endpoint.Requests()[before].Files, endpoint.Requests()[before+1].Files...))
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, first).state)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, second).state)
}

func TestModerationCDNPurgeAttemptUsesClockBeforeHeldResponse(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	endpoint.QueueResponse(http.StatusInternalServerError, `{"success":false}`)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	cid := mediaImageCID("cdn crossed anchor")
	h.remove(t, h.comment(t, h.ownerA, cid), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	row := moderationCDNTarget(t, h.db, h.ownerA, cid)
	assert.Equal(t, 1, row.attempts)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), row.nextAttemptAt)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC))
	entered, release := endpoint.HoldNextRequest(t)
	finished := make(chan error, 1)
	go func() { finished <- h.queue.Sweep(t.Context()) }()
	awaitModerationCDN(t, "early purge request", entered)
	clock.Set(time.Date(2026, 9, 28, 12, 2, 0, 0, time.UTC))
	release()
	require.NoError(t, awaitModerationCDN(t, "early sweep to finish", finished))
	require.Len(t, endpoint.Requests(), 2, "one Sweep attempts the target once")
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, cid), endpoint.Requests()[1].Files)
	row = moderationCDNTarget(t, h.db, h.ownerA, cid)
	assert.Equal(t, "pending", row.state, "an early attempt cannot complete merely because its response arrives after the anchor")
	assert.Equal(t, 1, row.attempts)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC), row.nextAttemptAt)
	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 3)
	assert.ElementsMatch(t, moderationCDNFiles(h.ownerA, cid), endpoint.Requests()[2].Files)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
}

func TestModerationCDNPurgeMixedResultsRetryOnlyFailedPair(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	first, second := mediaImageCID("cdn mixed first"), mediaImageCID("cdn mixed second")
	h.remove(t, moderationCDNTwoImagePost(t, h, first, second), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	require.Len(t, endpoint.Requests(), 1)
	assert.ElementsMatch(t, append(moderationCDNFiles(h.ownerA, first), moderationCDNFiles(h.ownerA, second)...), endpoint.Requests()[0].Files)
	for _, cid := range []string{first, second} {
		row := moderationCDNTarget(t, h.db, h.ownerA, cid)
		assert.Equal(t, "pending", row.state)
		assert.Zero(t, row.attempts)
		assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC), row.nextAttemptAt)
	}
	var phase atomic.Int32
	purger := &moderationCDNScriptedPurger{result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		if phase.Load() == 0 {
			return imageproxy.CDNPurgeResult{
				Acknowledged: []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: first}},
				Failed:       []imageproxy.CDNPurgeFailure{{Blob: imageproxy.BlockedBlob{OwnerDID: h.ownerA, CID: second}, Code: "http_500"}},
			}
		}
		return imageproxy.CDNPurgeResult{Acknowledged: blobs}
	}}
	queue := moderationCDNRetryQueue(h, clock, purger, 90*time.Second, 100)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	require.Len(t, purger.Calls(), 1)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: first}, {OwnerDID: h.ownerA, CID: second}}, purger.Calls()[0])
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, first).state)
	failed := moderationCDNTarget(t, h.db, h.ownerA, second)
	assert.Equal(t, "pending", failed.state)
	assert.Equal(t, 1, failed.attempts)
	assert.Equal(t, "http_500", failed.failureCode)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 2, 30, 0, time.UTC), failed.nextAttemptAt)
	phase.Store(1)
	clock.Set(time.Date(2026, 9, 28, 12, 2, 30, 0, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	require.Len(t, purger.Calls(), 2)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: second}}, purger.Calls()[1])
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, second).state)
}

func TestModerationCDNPurgeExpiredLeaseFencesStaleWorker(t *testing.T) {
	for _, lateFailure := range []bool{false, true} {
		name := "stale failure before completion"
		if lateFailure {
			name = "late failure after completion"
		}
		t.Run(name, func(t *testing.T) {
			clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
			endpoint := testkit.NewCloudflarePurgeEndpoint(t)
			h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
			cid := mediaImageCID("cdn fenced " + name)
			h.remove(t, h.comment(t, h.ownerA, cid), cdnPurgeSpam)
			waitModerationCDN(t, h.queue)
			assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
			clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
			first := &moderationCDNScriptedPurger{enter: make(chan struct{}), release: make(chan struct{}),
				result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
					return imageproxy.CDNPurgeResult{Failed: []imageproxy.CDNPurgeFailure{{Blob: blobs[0], Code: "http_500"}}}
				}}
			var releaseFirst sync.Once
			t.Cleanup(func() { releaseFirst.Do(func() { close(first.release) }) })
			firstFinished := make(chan error, 1)
			go func() {
				firstFinished <- moderationCDNRetryQueue(h, clock, first, 90*time.Second, 100).Sweep(t.Context())
			}()
			awaitModerationCDN(t, "first worker to claim", first.enter)
			assert.ElementsMatch(t, []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: cid}}, first.Calls()[0])
			clock.Set(time.Date(2026, 9, 28, 12, 3, 30, 0, time.UTC))
			second := &moderationCDNScriptedPurger{enter: make(chan struct{}), release: make(chan struct{}),
				result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
					return imageproxy.CDNPurgeResult{Acknowledged: blobs}
				}}
			var releaseSecond sync.Once
			t.Cleanup(func() { releaseSecond.Do(func() { close(second.release) }) })
			secondFinished := make(chan error, 1)
			go func() {
				secondFinished <- moderationCDNRetryQueue(h, clock, second, 90*time.Second, 100).Sweep(t.Context())
			}()
			awaitModerationCDN(t, "replacement worker to claim expired lease", second.enter)
			third := &moderationCDNScriptedPurger{result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
				return imageproxy.CDNPurgeResult{Acknowledged: blobs}
			}}
			if !lateFailure {
				releaseFirst.Do(func() { close(first.release) })
				require.NoError(t, awaitModerationCDN(t, "stale failure", firstFinished))
			}
			require.NoError(t, moderationCDNRetryQueue(h, clock, third, 90*time.Second, 100).Sweep(t.Context()))
			assert.Empty(t, third.Calls(), "the stale failure cannot steal the replacement worker's lease")
			releaseSecond.Do(func() { close(second.release) })
			require.NoError(t, awaitModerationCDN(t, "replacement worker success", secondFinished))
			assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
			if lateFailure {
				releaseFirst.Do(func() { close(first.release) })
				require.NoError(t, awaitModerationCDN(t, "late failure", firstFinished))
			}
			assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
			clock.Set(time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
			require.NoError(t, moderationCDNRetryQueue(h, clock, third, 90*time.Second, 100).Sweep(t.Context()))
			assert.Empty(t, third.Calls())
		})
	}
}

func TestModerationCDNPurgeRestartRecoversAbandonedLeaseAtDeadline(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	cid := mediaImageCID("cdn abandoned lease")
	h.remove(t, h.comment(t, h.ownerA, cid), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	first := &moderationCDNScriptedPurger{enter: make(chan struct{}), release: make(chan struct{}),
		result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
			return imageproxy.CDNPurgeResult{Acknowledged: blobs}
		}}
	var releaseFirst sync.Once
	firstFinished := make(chan error, 1)
	t.Cleanup(func() {
		releaseFirst.Do(func() { close(first.release) })
		require.NoError(t, awaitModerationCDN(t, "abandoned worker cleanup", firstFinished))
	})
	go func() {
		firstFinished <- moderationCDNRetryQueue(h, clock, first, 90*time.Second, 100).Sweep(context.Background())
	}()
	awaitModerationCDN(t, "abandoned worker to claim", first.enter)
	restarted := &moderationCDNScriptedPurger{result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		return imageproxy.CDNPurgeResult{Acknowledged: blobs}
	}}
	queue := moderationCDNRetryQueue(h, clock, restarted, 90*time.Second, 100)
	clock.Set(time.Date(2026, 9, 28, 12, 3, 29, 999999000, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	assert.Empty(t, restarted.Calls())
	clock.Set(time.Date(2026, 9, 28, 12, 3, 30, 0, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: cid}}, restarted.Calls()[0])
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
}

type moderationCDNNoopTargets struct{}

func (moderationCDNNoopTargets) PurgeAfterCommit(context.Context, []imageproxy.BlockedBlob) {}

func TestModerationCDNPurgeAnchorIsSampledAfterRemovalCommit(t *testing.T) {
	firstInstant := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	committedInstant := time.Date(2026, 9, 28, 12, 0, 10, 0, time.UTC)
	clock := &moderationCDNClock{at: firstInstant}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	var armed atomic.Bool
	var h *moderationMediaHarness
	var subject moderation.StrongRef
	now := func() time.Time {
		if armed.Swap(false) {
			clock.Set(committedInstant)
			h.remove(t, subject, cdnPurgeSpam)
			return firstInstant
		}
		return clock.Now()
	}
	h, _ = newModerationMediaHarness(t, false, func(config *moderationMediaHarnessConfig) {
		config.now = now
		config.targets = func(*sql.DB, func() time.Time) moderation.CDNPurgeTargets { return moderationCDNNoopTargets{} }
	})
	cid := mediaImageCID("cdn after commit anchor")
	subject = h.comment(t, h.ownerA, cid)
	queue := moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(h.db),
		newMediaCloudflarePurger(t, endpoint, "anchor-token"),
		moderation.CDNPurgeQueueConfig{Now: now, WriteTimeout: 90 * time.Second, BatchSize: 100})
	armed.Store(true)
	require.NoError(t, queue.Sweep(t.Context()))
	require.False(t, armed.Load(), "the first Sweep must invoke the clock hook")
	assert.Equal(t, "pending", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 39, 999999000, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	assert.NotEqual(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state,
		"a post-commit target cannot complete before C2+90s")
	clock.Set(time.Date(2026, 9, 28, 12, 1, 40, 0, time.UTC))
	require.NoError(t, queue.Sweep(t.Context()))
	if moderationCDNTarget(t, h.db, h.ownerA, cid).state != "completed" {
		clock.Set(time.Date(2026, 9, 28, 12, 3, 40, 0, time.UTC))
		require.NoError(t, queue.Sweep(t.Context()))
	}
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
}

func TestModerationCDNPurgeSweepSkipsRowLockedByConcurrentClaim(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 90*time.Second)
	cid := mediaImageCID("cdn locked by concurrent claim")
	h.remove(t, h.comment(t, h.ownerA, cid), cdnPurgeSpam)
	waitModerationCDN(t, h.queue)
	row := moderationCDNTarget(t, h.db, h.ownerA, cid)
	require.Equal(t, "pending", row.state)
	require.Equal(t, time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC), row.nextAttemptAt)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))

	holder, err := h.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	var releaseHolder sync.Once
	rollbackHolder := func() { releaseHolder.Do(func() { require.NoError(t, holder.Rollback()) }) }
	t.Cleanup(rollbackHolder)
	var lockedCID string
	require.NoError(t, holder.QueryRowContext(t.Context(), `
		SELECT blob_cid FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2 FOR UPDATE
	`, h.ownerA, cid).Scan(&lockedCID))

	purger := &moderationCDNScriptedPurger{result: func(blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		return imageproxy.CDNPurgeResult{Acknowledged: blobs}
	}}
	queue := moderationCDNRetryQueue(h, clock, purger, 90*time.Second, 100)
	sweepCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, queue.Sweep(sweepCtx), "a sweep must skip a row another claim holds instead of waiting on it")
	assert.Empty(t, purger.Calls(), "a row held by a concurrent claim must not be purged twice")

	rollbackHolder()
	require.NoError(t, queue.Sweep(t.Context()))
	require.Len(t, purger.Calls(), 1, "the row was due, so only the lock kept the first sweep away")
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{{OwnerDID: h.ownerA, CID: cid}}, purger.Calls()[0])
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerA, cid).state)
}
