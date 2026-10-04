package moderation_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cdnAttemptReschedule struct {
	blob          imageproxy.BlockedBlob
	nextAttemptAt time.Time
	failureCode   string
}

// cdnAttemptStore hands out its targets on the first claim and records how
// each outcome is written.
type cdnAttemptStore struct {
	mu          sync.Mutex
	targets     []moderation.CDNPurgeTarget
	claimed     bool
	reschedules []cdnAttemptReschedule
	completed   []imageproxy.BlockedBlob
}

func (store *cdnAttemptStore) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) ([]imageproxy.BlockedBlob, error) {
	return blobs, nil
}

func (store *cdnAttemptStore) ClaimDueCDNPurgeTargets(context.Context, moderation.CDNPurgeClaim) ([]moderation.CDNPurgeTarget, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimed {
		return nil, nil
	}
	store.claimed = true
	return store.targets, nil
}

func (store *cdnAttemptStore) CompleteCDNPurgeTarget(_ context.Context, target moderation.CDNPurgeTarget) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.completed = append(store.completed, target.Blob)
	return nil
}

func (store *cdnAttemptStore) RescheduleCDNPurgeTarget(_ context.Context, target moderation.CDNPurgeTarget, nextAttemptAt time.Time, failureCode string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.reschedules = append(store.reschedules, cdnAttemptReschedule{blob: target.Blob, nextAttemptAt: nextAttemptAt, failureCode: failureCode})
	return nil
}

type cdnAttemptPurgerFunc func(context.Context, []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult

func (purge cdnAttemptPurgerFunc) PurgeBlobs(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	return purge(ctx, blobs)
}

var cdnAttemptNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func cdnAttemptQueue(store *cdnAttemptStore, purger moderation.CDNPurger) *moderation.CDNPurgeQueue {
	return moderation.NewCDNPurgeQueue(store, purger, moderation.CDNPurgeQueueConfig{
		WriteTimeout: time.Second,
		Now:          func() time.Time { return cdnAttemptNow },
	})
}

// A target the sweep's context cut off was never refused by the edge, so it
// must neither count as a failed attempt nor back off.
func TestCDNPurgeQueueInterruptedTargetIsDueAgainWithoutCountingAnAttempt(t *testing.T) {
	refused := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordCanonicalCID}
	cutOff := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordSecondCID}
	store := &cdnAttemptStore{targets: []moderation.CDNPurgeTarget{
		{Blob: refused, Attempts: 2},
		{Blob: cutOff, Attempts: 3},
	}}
	sweepCtx, cancelSweep := context.WithCancel(t.Context())
	defer cancelSweep()
	queue := cdnAttemptQueue(store, cdnAttemptPurgerFunc(func(context.Context, []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		cancelSweep()
		return imageproxy.CDNPurgeResult{Failed: []imageproxy.CDNPurgeFailure{
			{Blob: refused, Code: "http_500"},
			{Blob: cutOff, Code: "transport"},
		}}
	}))

	assert.ErrorIs(t, queue.Sweep(sweepCtx), context.Canceled)

	assert.ElementsMatch(t, []cdnAttemptReschedule{
		{blob: refused, nextAttemptAt: cdnAttemptNow.Add(4 * time.Minute), failureCode: "http_500"},
		{blob: cutOff, nextAttemptAt: cdnAttemptNow, failureCode: ""},
	}, store.reschedules)
}

// Each claimed batch's lease runs cdnPurgeLease from its claim, so the request
// for that batch must end before the lease can pass to another worker even when
// the caller's context carries no deadline.
func TestCDNPurgeQueueSweepBoundsEachAttempt(t *testing.T) {
	blob := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordCanonicalCID}
	store := &cdnAttemptStore{targets: []moderation.CDNPurgeTarget{{Blob: blob}}}
	var deadline time.Time
	var hasDeadline bool
	queue := cdnAttemptQueue(store, cdnAttemptPurgerFunc(func(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		deadline, hasDeadline = ctx.Deadline()
		return imageproxy.CDNPurgeResult{Acknowledged: blobs}
	}))

	require.NoError(t, queue.Sweep(context.Background()))

	require.True(t, hasDeadline, "a sweep attempt must run under its own deadline")
	assert.WithinDuration(t, time.Now(), deadline, 2*time.Minute, "the attempt deadline must fall inside the claim lease")
}

type cdnAttemptLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*cdnAttemptLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (capture *cdnAttemptLogCapture) Handle(_ context.Context, record slog.Record) error {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.records = append(capture.records, record.Clone())
	return nil
}

func (capture *cdnAttemptLogCapture) WithAttrs([]slog.Attr) slog.Handler { return capture }
func (capture *cdnAttemptLogCapture) WithGroup(string) slog.Handler      { return capture }

func (capture *cdnAttemptLogCapture) errors() []slog.Record {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var errors []slog.Record
	for _, record := range capture.records {
		if record.Level == slog.LevelError {
			errors = append(errors, record)
		}
	}
	return errors
}

// A refused credential fails every target of every attempt. One line per
// attempt that names the cause is a signal; one line per target is noise.
//
// Not parallel: this test replaces slog.Default while the sweep runs.
func TestCDNPurgeQueueLogsOneAggregatedErrorPerFailureCode(t *testing.T) {
	blobs := []imageproxy.BlockedBlob{
		{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordCanonicalCID},
		{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordSecondCID},
		{OwnerDID: "did:plc:cdnrecordsecondowner", CID: cdnRecordCanonicalCID},
	}
	targets := make([]moderation.CDNPurgeTarget, len(blobs))
	for i, blob := range blobs {
		targets[i] = moderation.CDNPurgeTarget{Blob: blob}
	}
	store := &cdnAttemptStore{targets: targets}
	queue := cdnAttemptQueue(store, cdnAttemptPurgerFunc(func(_ context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
		var result imageproxy.CDNPurgeResult
		for _, blob := range blobs {
			result.Failed = append(result.Failed, imageproxy.CDNPurgeFailure{Blob: blob, Code: "http_403"})
		}
		return result
	}))
	logs := &cdnAttemptLogCapture{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	require.NoError(t, queue.Sweep(t.Context()))

	errors := logs.errors()
	require.Len(t, errors, 1, "one error line per failure code per attempt")
	attrs := make(map[string]slog.Value)
	errors[0].Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	assert.Equal(t, "http_403", attrs["code"].String())
	assert.Equal(t, int64(3), attrs["count"].Int64())
	assert.NotContains(t, attrs, "cid")
	assert.Contains(t, strings.ToLower(errors[0].Message), "credentials")
}
