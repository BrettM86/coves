//go:build integration

package routes_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

const ownerlessUnmappableDirectory = "did_web_example.test_alice"
const ownerlessOwnerC = "did:web:example.test"

type ownerlessCDNFixture struct {
	h        *moderationMediaHarness
	subject  moderation.StrongRef
	cid      string
	boundary *ownerlessDeletionBoundary
}

func newOwnerlessCDNFixture(t *testing.T, label string, endpoint *testkit.CloudflarePurgeEndpoint, clock *moderationCDNClock, configure ...func(*moderationMediaHarnessConfig)) ownerlessCDNFixture {
	t.Helper()
	imageCID := mediaImageCID(label)
	var boundary *ownerlessDeletionBoundary
	withBoundary := func(options *moderationMediaHarnessConfig) {
		options.wrapCache = func(cache *imageproxy.DiskCache, db *sql.DB, root string) imageproxy.Cache {
			boundary = &ownerlessDeletionBoundary{DiskCache: cache, db: db, root: root, cid: imageCID}
			return boundary
		}
	}
	configuration := append([]func(*moderationMediaHarnessConfig){withBoundary}, configure...)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, configuration...)
	boundary.owners = []string{h.ownerB, ownerlessOwnerC}
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	require.Equal(t, h.ownerA, ownerA)
	for _, preset := range []string{"content_preview", "content_full"} {
		require.Equal(t, http.StatusOK, h.request(t, preset, ownerA, imageCID))
		for _, owner := range []string{h.ownerB, ownerlessOwnerC} {
			require.NoError(t, h.cache.Set(preset, owner, imageCID, testkit.TestPNG(32, 32)))
		}
		for _, owner := range []string{ownerA, h.ownerB, ownerlessOwnerC} {
			_, err := os.Stat(h.cachePath(preset, owner, imageCID))
			require.NoError(t, err)
		}
	}
	return ownerlessCDNFixture{h: h, subject: subject, cid: imageCID, boundary: boundary}
}

func requireOwnerlessCachedEntries(t *testing.T, h *moderationMediaHarness, cid string, owners ...string) {
	t.Helper()
	for _, preset := range []string{"content_preview", "content_full"} {
		for _, owner := range owners {
			_, err := os.Stat(h.cachePath(preset, owner, cid))
			require.NoError(t, err, "cached entry for %s under %s must remain", owner, preset)
		}
	}
}

func seedOwnerlessUnmappableEntry(t *testing.T, h *moderationMediaHarness, cid string) string {
	t.Helper()
	path := filepath.Join(h.cacheDir, "content_preview", ownerlessUnmappableDirectory, cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, testkit.TestPNG(32, 32), 0644))
	_, err := os.Stat(path)
	require.NoError(t, err)
	return path
}

// This wraps the real cache at the deletion boundary, before delegating any
// operation that can remove a cached copy belonging to B or C.
type ownerlessDeletionBoundary struct {
	*imageproxy.DiskCache
	db         *sql.DB
	root       string
	cid        string
	owners     []string
	mu         sync.Mutex
	violations []string
}

func (boundary *ownerlessDeletionBoundary) check(cid string, removes func(preset, owner string) bool) {
	if cid != boundary.cid {
		return
	}
	var violations []string
	err := filepath.WalkDir(boundary.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != cid {
			return nil
		}
		ownerDir := filepath.Base(filepath.Dir(path))
		preset := filepath.Base(filepath.Dir(filepath.Dir(path)))
		for _, owner := range boundary.owners {
			if ownerDir != strings.ReplaceAll(owner, ":", "_") || !removes(preset, owner) {
				continue
			}
			var exists bool
			queryErr := boundary.db.QueryRowContext(context.Background(), `
				SELECT EXISTS (SELECT 1 FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2)
			`, owner, cid).Scan(&exists)
			if queryErr != nil || !exists {
				violations = append(violations, owner+"/"+preset+": missing committed target or query failed: "+errorText(queryErr))
			}
		}
		return nil
	})
	if err != nil {
		violations = append(violations, "cache scan: "+err.Error())
	}
	boundary.mu.Lock()
	boundary.violations = append(boundary.violations, violations...)
	boundary.mu.Unlock()
}

func errorText(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

func (boundary *ownerlessDeletionBoundary) Delete(preset, owner, cid string) error {
	boundary.check(cid, func(candidatePreset, candidateOwner string) bool {
		return candidatePreset == preset && candidateOwner == owner
	})
	return boundary.DiskCache.Delete(preset, owner, cid)
}

func (boundary *ownerlessDeletionBoundary) DeleteOwner(owner, cid string) error {
	boundary.check(cid, func(_ string, candidateOwner string) bool { return candidateOwner == owner })
	return boundary.DiskCache.DeleteOwner(owner, cid)
}

func (boundary *ownerlessDeletionBoundary) DeleteCID(cid string) error {
	boundary.check(cid, func(string, string) bool { return true })
	return boundary.DiskCache.DeleteCID(cid)
}

func (boundary *ownerlessDeletionBoundary) snapshot() []string {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	return append([]string(nil), boundary.violations...)
}

func ownerlessTargetOwners(t *testing.T, db *sql.DB, cid string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT owner_did FROM moderation_media_purges WHERE blob_cid = $1`, cid)
	require.NoError(t, err)
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		require.NoError(t, rows.Scan(&owner))
		owners = append(owners, owner)
	}
	require.NoError(t, rows.Err())
	return owners
}

// Not parallel: this test replaces slog.Default while removal logs unmappable directories.
func TestModerationCDNPurgeOwnerlessRemovalNamesCachedOwnersBeforeDeletion(t *testing.T) {
	const ownerD = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	fixture := newOwnerlessCDNFixture(t, "ownerless cdn removal", endpoint, clock)
	h, subject, imageCID, boundary := fixture.h, fixture.subject, fixture.cid, fixture.boundary
	ownerA := h.ownerA
	unmappablePath := seedOwnerlessUnmappableEntry(t, h, imageCID)

	logs := &postMediaLogCapture{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")

	// Check the durable result first, so missing B/C rows fail here.
	require.ElementsMatch(t, []string{ownerA, h.ownerB, ownerlessOwnerC}, ownerlessTargetOwners(t, h.db, imageCID), "cached owners must be named")
	assert.NotContains(t, ownerlessTargetOwners(t, h.db, imageCID), ownerD)
	assert.NotContains(t, ownerlessTargetOwners(t, h.db, imageCID), ownerlessUnmappableDirectory)
	assert.Empty(t, boundary.snapshot(), "every deletion of B/C must follow a committed purge target")
	h.assertNoCachedBlob(t, imageCID, "")
	_, err := os.Stat(unmappablePath)
	assert.True(t, os.IsNotExist(err), "unmappable cache entry must also be deleted")
	var warned bool
	for _, record := range logs.snapshot() {
		if record.Level < slog.LevelWarn {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Value.String() == ownerlessUnmappableDirectory {
				warned = true
			}
			return true
		})
	}
	assert.True(t, warned, "warn-level log attribute must name the unmappable directory")

	waitModerationCDN(t, h.queue)
	files := make(map[string]bool)
	for _, request := range endpoint.Requests() {
		for _, file := range request.Files {
			files[file] = true
			assert.NotContains(t, file, ownerD)
		}
	}
	var received []string
	for file := range files {
		received = append(received, file)
	}
	for _, owner := range []string{ownerA, h.ownerB, ownerlessOwnerC} {
		assert.Subset(t, received, moderationCDNFiles(owner, imageCID), "all six preset URLs must reach the endpoint for %s", owner)
	}
}

type failingOwnerlessRecordStore struct {
	moderation.CDNPurgeTargetStore
	failing *atomic.Bool
}

func (store failingOwnerlessRecordStore) RecordCDNPurgeTargets(ctx context.Context, blobs []imageproxy.BlockedBlob) ([]imageproxy.BlockedBlob, error) {
	if store.failing.Load() {
		return nil, errors.New("ownerless target recording unavailable")
	}
	return store.CDNPurgeTargetStore.RecordCDNPurgeTargets(ctx, blobs)
}

func TestModerationCDNPurgeOwnerlessRecordingFailureRetainsBytesUntilSweep(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	var failing atomic.Bool
	failing.Store(true)
	fixture := newOwnerlessCDNFixture(t, "ownerless recording failure", endpoint, clock, func(options *moderationMediaHarnessConfig) {
		options.wrapTargetStore = func(store moderation.CDNPurgeTargetStore) moderation.CDNPurgeTargetStore {
			return failingOwnerlessRecordStore{CDNPurgeTargetStore: store, failing: &failing}
		}
	})
	h, imageCID := fixture.h, fixture.cid
	seedOwnerlessUnmappableEntry(t, h, imageCID)
	h.remove(t, fixture.subject, "social.coves.moderation.defs#reasonIllegalContent")
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB, ownerlessOwnerC)
	require.Equal(t, http.StatusNotFound, h.request(t, "content_preview", h.ownerB, imageCID))
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	repo := postgres.NewModerationRepository(h.db)
	require.Error(t, h.proxyService.PurgeActiveBlocks(ctx, repo))
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB, ownerlessOwnerC)
	failing.Store(false)
	require.NoError(t, h.proxyService.PurgeActiveBlocks(ctx, repo))
	assert.ElementsMatch(t, []string{h.ownerA, h.ownerB, ownerlessOwnerC}, ownerlessTargetOwners(t, h.db, imageCID))
	assert.Empty(t, fixture.boundary.snapshot(), "every deletion of B/C must follow a committed target")
	h.assertNoCachedBlob(t, imageCID, "")
}

type noOpOwnerlessMediaPurger struct{}

func (noOpOwnerlessMediaPurger) PurgeOwnerBlob(context.Context, string, string) error { return nil }
func (noOpOwnerlessMediaPurger) PurgeBlob(context.Context, string) error              { return nil }

func TestModerationCDNPurgeOwnerlessRestartRecordsTargetsBeforeDeleting(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	fixture := newOwnerlessCDNFixture(t, "ownerless restart sweep", endpoint, clock, func(options *moderationMediaHarnessConfig) {
		options.purger = noOpOwnerlessMediaPurger{}
	})
	h, imageCID := fixture.h, fixture.cid
	fixture.boundary.owners = []string{h.ownerA, h.ownerB, ownerlessOwnerC}
	h.remove(t, fixture.subject, "social.coves.moderation.defs#reasonIllegalContent")
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerA, h.ownerB, ownerlessOwnerC)

	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	repo := postgres.NewModerationRepository(h.db)
	restarted, err := imageproxy.NewService(fixture.boundary, processor,
		imageproxy.NewPDSFetcher(30*time.Second, 10, imageproxy.WithPrivateHostsAllowed()),
		repo, imageproxy.DefaultConfig(), imageproxy.WithCDNPurgeRecorder(h.queue))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, restarted.PurgeActiveBlocks(ctx, repo))
	require.ElementsMatch(t, []string{h.ownerA, h.ownerB, ownerlessOwnerC}, ownerlessTargetOwners(t, h.db, imageCID))
	assert.Empty(t, fixture.boundary.snapshot(), "every deletion of A/B/C must follow a committed target")
	h.assertNoCachedBlob(t, imageCID, "")
	ensureQueueIdle, cancelWait := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWait()
	require.NoError(t, h.queue.Wait(ensureQueueIdle))
	files := make(map[string]bool)
	for _, request := range endpoint.Requests() {
		for _, file := range request.Files {
			files[file] = true
		}
	}
	var received []string
	for file := range files {
		received = append(received, file)
	}
	for _, owner := range []string{h.ownerA, h.ownerB, ownerlessOwnerC} {
		assert.Subset(t, received, moderationCDNFiles(owner, imageCID), "all six preset URLs must reach the endpoint for %s", owner)
	}
}

func TestModerationCDNPurgeOwnerlessUnconfiguredDeletesWithoutRecording(t *testing.T) {
	const ownerC = "did:web:example.test"
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h, _ := newModerationMediaHarness(t, false)
	imageCID := mediaImageCID("ownerless cdn unconfigured")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	owners := []string{ownerA, h.ownerB, ownerC}
	for _, preset := range []string{"content_preview", "content_full"} {
		for _, owner := range owners {
			require.NoError(t, h.cache.Set(preset, owner, imageCID, testkit.TestPNG(32, 32)))
			_, err := os.Stat(h.cachePath(preset, owner, imageCID))
			require.NoError(t, err)
		}
	}
	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")
	h.assertNoCachedBlob(t, imageCID, "")
	assert.Zero(t, moderationCDNTargetCount(t, h.db))
	assert.Empty(t, endpoint.Requests())

	for _, owner := range owners {
		require.NoError(t, h.cache.Set("content_preview", owner, imageCID, testkit.TestPNG(32, 32)))
	}
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	restarted, err := imageproxy.NewService(h.cache, processor,
		imageproxy.NewPDSFetcher(30*time.Second, 10, imageproxy.WithPrivateHostsAllowed()),
		postgres.NewModerationRepository(h.db), imageproxy.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.PurgeActiveBlocks(t.Context(), postgres.NewModerationRepository(h.db)))
	h.assertNoCachedBlob(t, imageCID, "")
	assert.Zero(t, moderationCDNTargetCount(t, h.db))
	assert.Empty(t, endpoint.Requests())
}
