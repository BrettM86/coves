package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cdnBootLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (handler *cdnBootLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (handler *cdnBootLogHandler) Handle(_ context.Context, record slog.Record) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.records = append(handler.records, record.Clone())
	return nil
}
func (handler *cdnBootLogHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *cdnBootLogHandler) WithGroup(string) slog.Handler      { return handler }
func (handler *cdnBootLogHandler) snapshot() []slog.Record {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]slog.Record(nil), handler.records...)
}

func TestBuildCDNPurgerDisabledAndConfigured(t *testing.T) {
	previous := slog.Default()
	handler := &cdnBootLogHandler{}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	purger, err := buildCDNPurger(config.CDNPurgeConfig{}, cloudflareAPIBase)
	require.NoError(t, err)
	require.True(t, purger == nil, "disabled purger must be an untyped nil interface")
	records := handler.snapshot()
	require.Len(t, records, 1)
	assert.Equal(t, slog.LevelInfo, records[0].Level)

	purger, err = buildCDNPurger(config.CDNPurgeConfig{ZoneID: "zone-abc", APIToken: "tok", BaseURLs: []string{"https://img.example.test"}}, cloudflareAPIBase)
	require.NoError(t, err)
	assert.NotNil(t, purger)
}

// cdnWiringStore runs a removal against one indexed comment with no prior
// moderation state. Transaction methods a removal never calls are left to the
// embedded nil interface, so reaching one panics instead of passing silently.
type cdnWiringStore struct {
	moderation.Store
	transaction *cdnWiringTransaction
}

func (store *cdnWiringStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	return fn(ctx, store.transaction)
}

type cdnWiringTransaction struct {
	moderation.Transaction
	comment      moderation.IndexedComment
	purgeTargets []imageproxy.BlockedBlob
}

func (*cdnWiringTransaction) LockActor(context.Context, string) error { return nil }
func (*cdnWiringTransaction) LiveIdempotencyRecord(context.Context, string, string, string, time.Time) (*moderation.IdempotencyRecord, error) {
	return nil, nil
}
func (*cdnWiringTransaction) CountLiveIdempotencyKeys(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (*cdnWiringTransaction) SaveIdempotencyRecord(context.Context, moderation.IdempotencyRecord) error {
	return nil
}
func (*cdnWiringTransaction) LockSubject(context.Context, string) (int64, error) { return 0, nil }
func (transaction *cdnWiringTransaction) ReadIndexedComment(context.Context, string) (*moderation.IndexedComment, error) {
	comment := transaction.comment
	return &comment, nil
}
func (*cdnWiringTransaction) ActiveRemoval(context.Context, string, string) (*moderation.Action, error) {
	return nil, nil
}
func (*cdnWiringTransaction) ActiveLabels(context.Context, string, string) ([]moderation.Action, error) {
	return nil, nil
}
func (*cdnWiringTransaction) InsertAction(_ context.Context, action moderation.Action) (*moderation.Action, error) {
	action.ID = "cdn-wiring-action"
	return &action, nil
}
func (*cdnWiringTransaction) SetRemovalDecision(context.Context, string, string, string, bool) error {
	return nil
}
func (*cdnWiringTransaction) SetSubjectVersion(context.Context, string, int64) error { return nil }
func (*cdnWiringTransaction) InsertMediaBlocks(context.Context, []moderation.MediaBlock) error {
	return nil
}

func (transaction *cdnWiringTransaction) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) error {
	transaction.purgeTargets = append(transaction.purgeTargets, blobs...)
	return nil
}

func (store *cdnWiringStore) ReadSubject(_ context.Context, uri string) (*moderation.IndexedRecord, error) {
	return &moderation.IndexedRecord{URI: uri, CID: store.transaction.comment.CID}, nil
}

type cdnWiringMediaTransaction struct {
	moderation.MediaTransaction
	comment      moderation.IndexedComment
	purgeTargets []imageproxy.BlockedBlob
}

func (*cdnWiringMediaTransaction) ActiveRemoval(context.Context, string, string) (*moderation.Action, error) {
	return &moderation.Action{ID: "cdn-wiring-action"}, nil
}

func (transaction *cdnWiringMediaTransaction) ReadIndexedComment(context.Context, string) (*moderation.IndexedComment, error) {
	return &transaction.comment, nil
}

func (*cdnWiringMediaTransaction) InsertNewMediaBlocks(_ context.Context, blocks []moderation.MediaBlock) ([]moderation.MediaBlock, error) {
	return blocks, nil
}

func (transaction *cdnWiringMediaTransaction) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) error {
	transaction.purgeTargets = append(transaction.purgeTargets, blobs...)
	return nil
}

type cdnWiringMediaBinder struct{ transaction *cdnWiringMediaTransaction }

func (binder cdnWiringMediaBinder) BindTransaction(*sql.Tx) moderation.MediaTransaction {
	return binder.transaction
}

type cdnWiringPurgeTargetStore struct {
	mu             sync.Mutex
	claims         []moderation.CDNPurgeClaim
	recorded       []imageproxy.BlockedBlob
	recordedSignal chan struct{}
}

func (store *cdnWiringPurgeTargetStore) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) ([]imageproxy.BlockedBlob, error) {
	store.mu.Lock()
	store.recorded = append(store.recorded, blobs...)
	store.mu.Unlock()
	if store.recordedSignal != nil {
		select {
		case store.recordedSignal <- struct{}{}:
		default:
		}
	}
	return blobs, nil
}

func (store *cdnWiringPurgeTargetStore) recordedBlobs() []imageproxy.BlockedBlob {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]imageproxy.BlockedBlob(nil), store.recorded...)
}

func (store *cdnWiringPurgeTargetStore) ClaimDueCDNPurgeTargets(_ context.Context, claim moderation.CDNPurgeClaim) ([]moderation.CDNPurgeTarget, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.claims = append(store.claims, claim)
	return nil, nil
}

func (*cdnWiringPurgeTargetStore) CompleteCDNPurgeTarget(context.Context, moderation.CDNPurgeTarget) error {
	return nil
}

func (*cdnWiringPurgeTargetStore) RescheduleCDNPurgeTarget(context.Context, moderation.CDNPurgeTarget, time.Time, string) error {
	return nil
}

func (store *cdnWiringPurgeTargetStore) snapshot() []moderation.CDNPurgeClaim {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]moderation.CDNPurgeClaim(nil), store.claims...)
}

func TestBuildModerationRecordsCDNPurgeTargetsAndConfiguresQueue(t *testing.T) {
	const (
		ownerDID       = "did:plc:cdnwiringauthor"
		subjectURI     = "at://did:plc:cdnwiringauthor/social.coves.community.comment/3kcdnwiring"
		removedImage   = "bafyreicdnwiringremoved"
		reconciledBlob = "bafyreicdnwiringreconciled"
		imageBaseURL   = "https://img.cdn-wiring.test"
	)
	for _, configured := range []bool{true, false} {
		name := "unconfigured"
		if configured {
			name = "configured"
		}
		t.Run(name, func(t *testing.T) {
			endpoint := testkit.NewCloudflarePurgeEndpoint(t)
			cfg := &config.Config{
				Instance:   config.InstanceConfig{DID: "did:web:cdn-wiring.test"},
				Moderation: config.ModerationConfig{IdempotencyRetention: time.Hour, MaxLiveIdempotencyKeys: 10},
				Media:      config.MediaConfig{ImageProxy: imageproxy.Config{BaseURL: imageBaseURL}},
			}
			cfg.Server.WriteTimeout = 77 * time.Second
			if configured {
				cfg.Media.CDNPurge = config.CDNPurgeConfig{ZoneID: "zone-cdn-wiring", APIToken: "token-cdn-wiring", BaseURLs: []string{imageBaseURL}}
			}
			comment := moderation.IndexedComment{URI: subjectURI, CID: "bafyreicdnwiringsubject", OwnerDID: ownerDID, ImageCIDs: []string{removedImage}}
			store := &cdnWiringStore{transaction: &cdnWiringTransaction{comment: comment}}
			media := &cdnWiringMediaTransaction{comment: moderation.IndexedComment{URI: subjectURI, OwnerDID: ownerDID, ImageCIDs: []string{reconciledBlob}}}
			targets := &cdnWiringPurgeTargetStore{}
			queue, err := buildCDNPurgeQueue(cfg, endpoint.URL(), targets)
			require.NoError(t, err)
			if configured {
				require.NotNil(t, queue, "configured CDN purge needs a durable queue")
				t.Cleanup(queue.Close)
			} else {
				require.True(t, queue == nil)
			}
			service, reconciler, err := buildModeration(cfg, moderationDependencies{
				subjectReader: store, store: store, mediaBinder: cdnWiringMediaBinder{transaction: media}, cdnPurgeQueue: queue,
			})
			require.NoError(t, err)

			result, err := service.RemoveContent(t.Context(), "did:plc:cdnwiringadmin", moderation.RemoveContentRequest{
				Subject:         moderation.StrongRef{URI: subjectURI, CID: comment.CID},
				ExpectedVersion: moderation.InitialVersion, IdempotencyKey: "cdn-wiring-removal",
				Reason: "social.coves.moderation.defs#reasonSpam",
			})
			require.NoError(t, err)
			require.Equal(t, moderation.OutcomeApplied, result.Outcome)
			blocks, err := reconciler.ReconcileTx(t.Context(), nil, subjectURI)
			require.NoError(t, err)
			require.Equal(t, []moderation.MediaBlock{{OwnerDID: ownerDID, BlobCID: reconciledBlob, ActionID: "cdn-wiring-action"}}, blocks)
			if configured {
				assert.Equal(t, []imageproxy.BlockedBlob{{OwnerDID: ownerDID, CID: removedImage}}, store.transaction.purgeTargets)
				assert.Equal(t, []imageproxy.BlockedBlob{{OwnerDID: ownerDID, CID: reconciledBlob}}, media.purgeTargets)
				queueCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				require.NoError(t, queue.Wait(queueCtx))
				beforeSweep := len(targets.snapshot())
				require.NoError(t, queue.Sweep(queueCtx))
				claims := targets.snapshot()
				require.Greater(t, len(claims), beforeSweep)
				assert.Equal(t, 77*time.Second, claims[beforeSweep].WriteTimeout)
				assert.Equal(t, 2*time.Minute, claims[beforeSweep].Lease)
			} else {
				assert.Empty(t, store.transaction.purgeTargets)
				assert.Empty(t, media.purgeTargets)
			}
		})
	}
	t.Run("configured without target store", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Media.CDNPurge = config.CDNPurgeConfig{ZoneID: "zone-cdn-wiring", APIToken: "token-cdn-wiring", BaseURLs: []string{"https://img.cdn-wiring.test"}}
		queue, err := buildCDNPurgeQueue(cfg, "https://api.cdn-wiring.test", nil)
		require.Error(t, err)
		require.True(t, queue == nil)
	})
}

type cdnWiringSweeper struct{ called chan struct{} }

func (sweeper *cdnWiringSweeper) Sweep(context.Context) error {
	sweeper.called <- struct{}{}
	return nil
}

func TestStartCDNPurgeSweepJobNilSweeper(t *testing.T) {
	for _, test := range []struct {
		name    string
		sweeper cdnPurgeSweeper
	}{
		{name: "nil interface"},
		{name: "typed nil queue", sweeper: (*moderation.CDNPurgeQueue)(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var waitGroup sync.WaitGroup
			startCDNPurgeSweepJob(t.Context(), &waitGroup, test.sweeper, time.Hour)
			requireCDNPurgeSweepJobStops(t, &waitGroup)
		})
	}
}

func TestStartCDNPurgeSweepJobBootAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	sweeper := &cdnWiringSweeper{called: make(chan struct{}, 2)}
	startCDNPurgeSweepJob(ctx, &waitGroup, sweeper, time.Hour)
	select {
	case <-sweeper.called:
	case <-time.After(10 * time.Second):
		t.Fatal("CDN purge sweep did not run on boot")
	}
	cancel()
	requireCDNPurgeSweepJobStops(t, &waitGroup)
	assert.Empty(t, sweeper.called)
}

func requireCDNPurgeSweepJobStops(t *testing.T, waitGroup *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CDN purge sweep job did not stop")
	}
}

// cdnInterruptedSweeper fails the way lib/pq reports a statement cancelled by
// the caller's context: with an error that does not wrap context.Canceled.
type cdnInterruptedSweeper struct{ entered chan struct{} }

func (sweeper *cdnInterruptedSweeper) Sweep(ctx context.Context) error {
	close(sweeper.entered)
	<-ctx.Done()
	return errors.New("pq: canceling statement due to user request")
}

// Not parallel: this test replaces slog.Default while the job runs.
func TestStartCDNPurgeSweepJobEndedContextIsNotAnError(t *testing.T) {
	previous := slog.Default()
	handler := &cdnBootLogHandler{}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waitGroup sync.WaitGroup
	sweeper := &cdnInterruptedSweeper{entered: make(chan struct{})}
	startCDNPurgeSweepJob(ctx, &waitGroup, sweeper, time.Hour)
	select {
	case <-sweeper.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CDN purge sweep did not run on boot")
	}
	cancel()
	requireCDNPurgeSweepJobStops(t, &waitGroup)
	for _, record := range handler.snapshot() {
		assert.NotEqual(t, slog.LevelError, record.Level, "a sweep stopped by its context is not a failure")
	}
}
