package moderation_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedCDNPurge struct {
	blobs            []imageproxy.BlockedBlob
	contextLive      bool
	removalCommitted bool
}

type recordingCDNPurger struct {
	mu     sync.Mutex
	store  *inMemoryModerationStore
	result imageproxy.CDNPurgeResult
	calls  []recordedCDNPurge
}

func (purger *recordingCDNPurger) PurgeBlobs(ctx context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	call := recordedCDNPurge{blobs: append([]imageproxy.BlockedBlob(nil), blobs...), contextLive: ctx.Err() == nil}
	if purger.store != nil && purger.store.mu.TryLock() {
		call.removalCommitted = purger.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, removeRulesURI}] != ""
		for _, blob := range blobs {
			found := false
			for block, active := range purger.store.state.mediaBlocks {
				if active && block.OwnerDID == blob.OwnerDID && block.BlobCID == blob.CID {
					found = true
				}
			}
			call.removalCommitted = call.removalCommitted && found
		}
		purger.store.mu.Unlock()
	}
	purger.mu.Lock()
	defer purger.mu.Unlock()
	purger.calls = append(purger.calls, call)
	return purger.result
}

func (purger *recordingCDNPurger) snapshot() []recordedCDNPurge {
	purger.mu.Lock()
	defer purger.mu.Unlock()
	return append([]recordedCDNPurge(nil), purger.calls...)
}

type afterBlocksFailureStore struct {
	*inMemoryModerationStore
	err error
}

func (store *afterBlocksFailureStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	return store.inMemoryModerationStore.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, &afterBlocksFailureTransaction{Transaction: tx, err: store.err})
	})
}

type afterBlocksFailureTransaction struct {
	moderation.Transaction
	err      error
	inserted bool
}

func (tx *afterBlocksFailureTransaction) InsertMediaBlocks(ctx context.Context, blocks []moderation.MediaBlock) error {
	if err := tx.Transaction.InsertMediaBlocks(ctx, blocks); err != nil {
		return err
	}
	tx.inserted = true
	return nil
}

func (tx *afterBlocksFailureTransaction) SaveIdempotencyRecord(ctx context.Context, record moderation.IdempotencyRecord) error {
	if tx.inserted {
		return tx.err
	}
	return tx.Transaction.SaveIdempotencyRecord(ctx, record)
}

type cancelAfterCommitStore struct {
	*inMemoryModerationStore
	cancel context.CancelFunc
}

func (store *cancelAfterCommitStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	err := store.inMemoryModerationStore.InTransaction(ctx, fn)
	if err == nil {
		store.cancel()
	}
	return err
}

type recordingCDNLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*recordingCDNLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (handler *recordingCDNLogHandler) Handle(_ context.Context, record slog.Record) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.records = append(handler.records, record.Clone())
	return nil
}
func (handler *recordingCDNLogHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *recordingCDNLogHandler) WithGroup(string) slog.Handler      { return handler }
func (handler *recordingCDNLogHandler) snapshot() []slog.Record {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]slog.Record(nil), handler.records...)
}

func TestRemoveContentCDNPurgeAfterCommit(t *testing.T) {
	for _, test := range []struct {
		name     string
		images   []string
		reason   string
		want     []imageproxy.BlockedBlob
		rollback bool
		cancel   bool
		failures bool
	}{
		{
			name: "spam three images", reason: removeRulesSpam,
			images: []string{"bafyreifirstimage", "bafyreisecondimage", "bafyreithirdimage"},
			want: []imageproxy.BlockedBlob{
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreifirstimage"},
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreisecondimage"},
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreithirdimage"},
			},
		},
		{
			name: "illegal content excludes ownerless blocks", reason: removeRulesIllegal,
			images: []string{"bafyreifirstimage", "bafyreisecondimage"},
			want: []imageproxy.BlockedBlob{
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreifirstimage"},
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreisecondimage"},
			},
		},
		{name: "no images", reason: removeRulesSpam},
		{
			name: "transaction rolls back after blocks", reason: removeRulesSpam,
			images: []string{"bafyreifirstimage"}, rollback: true,
		},
		{
			name: "caller cancelled after commit", reason: removeRulesSpam,
			images: []string{"bafyreifirstimage"}, cancel: true,
			want: []imageproxy.BlockedBlob{{OwnerDID: "did:plc:commentauthor", CID: "bafyreifirstimage"}},
		},
		{
			name: "two failed pairs leave mutation and local purge intact", reason: removeRulesSpam,
			images: []string{"bafyreifirstimage", "bafyreisecondimage", "bafyreithirdimage"}, failures: true,
			want: []imageproxy.BlockedBlob{
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreifirstimage"},
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreisecondimage"},
				{OwnerDID: "did:plc:commentauthor", CID: "bafyreithirdimage"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newRemoveRulesScenario()
			comment := scenario.store.state.indexedComments[removeRulesURI]
			comment.ImageCIDs = test.images
			scenario.store.state.indexedComments[removeRulesURI] = comment
			scenario.request.Reason = test.reason
			cdn := &recordingCDNPurger{store: scenario.store, result: imageproxy.CDNPurgeResult{Acknowledged: test.want}}
			var store moderation.Store = scenario.store
			var ctx context.Context = t.Context()
			if test.rollback {
				store = &afterBlocksFailureStore{inMemoryModerationStore: scenario.store, err: errCDNRollback}
			}
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(t.Context())
				t.Cleanup(cancel)
				store = &cancelAfterCommitStore{inMemoryModerationStore: scenario.store, cancel: cancel}
			}
			if test.failures {
				cdn.result = imageproxy.CDNPurgeResult{
					Acknowledged: []imageproxy.BlockedBlob{{OwnerDID: "did:plc:commentauthor", CID: "bafyreithirdimage"}},
					Failed: []imageproxy.CDNPurgeFailure{
						{Blob: imageproxy.BlockedBlob{OwnerDID: "did:plc:commentauthor", CID: "bafyreifirstimage"}, Code: "http_500"},
						{Blob: imageproxy.BlockedBlob{OwnerDID: "did:plc:commentauthor", CID: "bafyreisecondimage"}, Code: "transport"},
					},
				}
				logs := &recordingCDNLogHandler{}
				previous := slog.Default()
				slog.SetDefault(slog.New(logs))
				t.Cleanup(func() { slog.SetDefault(previous) })
				scenario.withCDNPurger(store, cdn)
				result, err := scenario.service.RemoveContent(ctx, removeRulesAdminDID, scenario.request)
				require.NoError(t, err)
				baseline := newRemoveRulesScenario()
				baselineComment := baseline.store.state.indexedComments[removeRulesURI]
				baselineComment.ImageCIDs = test.images
				baseline.store.state.indexedComments[removeRulesURI] = baselineComment
				baselineCDN := &recordingCDNPurger{store: baseline.store, result: imageproxy.CDNPurgeResult{Acknowledged: test.want}}
				baseline.withCDNPurger(baseline.store, baselineCDN)
				baselineResult, baselineErr := baseline.service.RemoveContent(t.Context(), removeRulesAdminDID, baseline.request)
				require.NoError(t, baselineErr)
				require.Equal(t, moderation.OutcomeApplied, baselineResult.Outcome)
				require.Equal(t, moderation.OutcomeApplied, result.Outcome)
				assert.Equal(t, baselineResult, result)
				assert.ElementsMatch(t, []removeRulesOwnerPurge{
					{ownerDID: "did:plc:commentauthor", blobCID: "bafyreifirstimage"},
					{ownerDID: "did:plc:commentauthor", blobCID: "bafyreisecondimage"},
					{ownerDID: "did:plc:commentauthor", blobCID: "bafyreithirdimage"},
				}, scenario.purger.ownerPurges)
				assertCDNPurgeCalls(t, cdn.snapshot(), test.want)
				assertCDNFailureLogs(t, logs.snapshot())
				return
			}
			scenario.withCDNPurger(store, cdn)
			result, err := scenario.service.RemoveContent(ctx, removeRulesAdminDID, scenario.request)
			if test.rollback {
				require.ErrorIs(t, err, errCDNRollback)
				assert.Nil(t, result)
				assert.Empty(t, scenario.store.state.mediaBlocks)
				assert.Empty(t, scenario.store.state.activeRemovals)
				assert.Empty(t, scenario.purger.ownerPurges)
				assert.Empty(t, cdn.snapshot())
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			if test.cancel {
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			if len(test.want) == 0 {
				assert.Empty(t, cdn.snapshot())
				return
			}
			assertCDNPurgeCalls(t, cdn.snapshot(), test.want)
		})
	}
}

var errCDNRollback = errors.New("injected after media insertion")

func assertCDNPurgeCalls(t *testing.T, calls []recordedCDNPurge, want []imageproxy.BlockedBlob) {
	t.Helper()
	require.Len(t, calls, 1)
	assert.ElementsMatch(t, want, calls[0].blobs)
	assert.True(t, calls[0].removalCommitted, "removal and blocks must be visible outside the transaction")
	assert.True(t, calls[0].contextLive, "purge must have a live context even after caller cancellation")
}

func assertCDNFailureLogs(t *testing.T, records []slog.Record) {
	t.Helper()
	var errorsOnly []slog.Record
	for _, record := range records {
		if record.Level == slog.LevelError {
			errorsOnly = append(errorsOnly, record)
		}
	}
	require.Len(t, errorsOnly, 2)
	want := []struct{ cid, code string }{{"bafyreifirstimage", "http_500"}, {"bafyreisecondimage", "transport"}}
	seen := make(map[string]int)
	for _, record := range errorsOnly {
		values := make([]string, 0)
		record.Attrs(func(attr slog.Attr) bool {
			values = append(values, attr.Value.String())
			return true
		})
		assert.Contains(t, values, "did:plc:commentauthor")
		assert.NotContains(t, values, "bafyreithirdimage")
		matched := false
		for _, pair := range want {
			if containsCDNLogValue(values, pair.cid) {
				matched = true
				seen[pair.cid]++
				assert.Contains(t, values, pair.code)
			}
		}
		assert.True(t, matched, "unexpected CDN failure record attributes: %v", values)
	}
	assert.Equal(t, map[string]int{"bafyreifirstimage": 1, "bafyreisecondimage": 1}, seen)
}

func containsCDNLogValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestRestoreContentDoesNotPurgeCDN(t *testing.T) {
	scenario := newRemoveRulesScenario()
	cdn := &recordingCDNPurger{store: scenario.store}
	scenario.withCDNPurger(scenario.store, cdn)
	removed, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.NotNil(t, removed.Action)
	before := cdn.snapshot()
	require.Len(t, before, 1, "removal must exercise the configured purger")
	restored, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &moderation.StrongRef{URI: removeRulesURI, CID: removeRulesCID},
		ExpectedVersion: removed.State.Version, IdempotencyKey: "restore-cdn", Reason: removeRulesSpam,
	})
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	assert.Equal(t, before, cdn.snapshot())
}

func TestMediaReconcilerPurgesOwnerScopedBlobsFromCDN(t *testing.T) {
	local := &removeRulesPurger{}
	cdn := &recordingCDNPurger{}
	reconciler := moderation.NewMediaReconciler(postRulesMediaBinder{&postRulesMediaTransaction{}}, removeRulesInstanceDID, local, moderation.WithCDNPurger(cdn))
	reconciler.Purge(nil)
	reconciler.Purge([]moderation.MediaBlock{{BlobCID: "bafyreiownerlessonly"}})
	assert.Empty(t, cdn.snapshot())
	assert.Equal(t, []string{"bafyreiownerlessonly"}, local.blobPurges)
	reconciler.Purge([]moderation.MediaBlock{
		{OwnerDID: "did:plc:postauthor", BlobCID: "bafyreifirstimage"},
		{BlobCID: "bafyreithirdimage"},
		{OwnerDID: "did:plc:postauthor", BlobCID: "bafyreisecondimage"},
	})
	calls := cdn.snapshot()
	require.Len(t, calls, 1)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{
		{OwnerDID: "did:plc:postauthor", CID: "bafyreifirstimage"},
		{OwnerDID: "did:plc:postauthor", CID: "bafyreisecondimage"},
	}, calls[0].blobs)
	assert.True(t, calls[0].contextLive)
	assert.ElementsMatch(t, []removeRulesOwnerPurge{
		{ownerDID: "did:plc:postauthor", blobCID: "bafyreifirstimage"},
		{ownerDID: "did:plc:postauthor", blobCID: "bafyreisecondimage"},
	}, local.ownerPurges)
	assert.Equal(t, []string{"bafyreiownerlessonly", "bafyreithirdimage"}, local.blobPurges)
}
