package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/blobs"
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
	comment moderation.IndexedComment
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

// TestBuildModerationPurgesCloudflareOnRemovalAndReconcile pins the production
// wiring: with CDN purge configured, both the removal path (moderation service)
// and the consumer reconcile path (media reconciler) must reach Cloudflare.
func TestBuildModerationPurgesCloudflareOnRemovalAndReconcile(t *testing.T) {
	const (
		ownerDID       = "did:plc:cdnwiringauthor"
		subjectURI     = "at://did:plc:cdnwiringauthor/social.coves.community.comment/3kcdnwiring"
		removedImage   = "bafyreicdnwiringremoved"
		reconciledBlob = "bafyreicdnwiringreconciled"
		imageBaseURL   = "https://img.cdn-wiring.test"
	)
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	cfg := &config.Config{
		Instance:   config.InstanceConfig{DID: "did:web:cdn-wiring.test"},
		Moderation: config.ModerationConfig{IdempotencyRetention: time.Hour, MaxLiveIdempotencyKeys: 10},
		Media: config.MediaConfig{
			ImageProxy: imageproxy.Config{BaseURL: imageBaseURL},
			CDNPurge:   config.CDNPurgeConfig{ZoneID: "zone-cdn-wiring", APIToken: "token-cdn-wiring", BaseURLs: []string{imageBaseURL}},
		},
	}
	store := &cdnWiringStore{transaction: &cdnWiringTransaction{comment: moderation.IndexedComment{
		URI: subjectURI, CID: "bafyreicdnwiringsubject", OwnerDID: ownerDID, ImageCIDs: []string{removedImage},
	}}}

	service, reconciler, err := buildModeration(cfg, endpoint.URL(), moderationDependencies{store: store})
	require.NoError(t, err)

	presets := imageproxy.ListPresets()
	require.NotEmpty(t, presets)
	assertPurged := func(t *testing.T, requests []testkit.CloudflarePurgeRequest, cid string) {
		t.Helper()
		require.Len(t, requests, 1)
		assert.Equal(t, "/zones/zone-cdn-wiring/purge_cache", requests[0].Path)
		assert.Equal(t, "Bearer token-cdn-wiring", requests[0].Authorization)
		require.NoError(t, requests[0].DecodeError)
		assert.Contains(t, requests[0].Files, blobs.HydrateImageProxyURL(imageBaseURL, presets[0].Name, ownerDID, cid))
	}

	result, err := service.RemoveContent(t.Context(), "did:plc:cdnwiringadmin", moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: subjectURI, CID: "bafyreicdnwiringsubject"},
		ExpectedVersion: moderation.InitialVersion, IdempotencyKey: "cdn-wiring-removal",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	assertPurged(t, endpoint.Requests(), removedImage)

	reconciler.Purge([]moderation.MediaBlock{{OwnerDID: ownerDID, BlobCID: reconciledBlob, ActionID: "cdn-wiring-action"}})
	requests := endpoint.Requests()
	require.Len(t, requests, 2, "the media reconciler must purge Cloudflare after a reconcile commit")
	assertPurged(t, requests[1:], reconciledBlob)
}
