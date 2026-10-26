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
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerlessUnmappableDirectory = "did_web_example.test_alice"
	ownerlessOwnerC              = "did:web:example.test"
)

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
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, testkit.TestPNG(32, 32), 0o644))
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
	boundary.check(cid, func(_, candidateOwner string) bool { return candidateOwner == owner })
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

func seedOwnerlessBlobReference(t *testing.T, db *sql.DB, owner, cid, subject, kind string, active bool) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
		VALUES ($1, $2, $3, $4, $5)
	`, owner, cid, subject, kind, active)
	require.NoError(t, err)
}

func installOwnerlessBlobIndex(options *moderationMediaHarnessConfig) {
	options.installBlobOwnerIndex = true
}

func TestModerationCDNPurgeOwnerlessIndexNamesUncachedOwners(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex)
	imageCID := mediaImageCID("ownerless index cold")
	otherCID := mediaImageCID("ownerless index other")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	ownerF := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxf"))
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", true)
	seedOwnerlessBlobReference(t, h.db, ownerF, otherCID,
		"at://"+ownerF+"/app.bsky.actor.profile/self", "avatar", true)
	h.assertNoCachedBlob(t, imageCID, "")

	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")
	waitModerationCDN(t, h.queue)
	require.ElementsMatch(t, []string{ownerA, h.ownerB, ownerC}, ownerlessTargetOwners(t, h.db, imageCID))
	var otherOwnerTargets int
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_media_purges WHERE owner_did = $1`, ownerF).Scan(&otherOwnerTargets))
	assert.Zero(t, otherOwnerTargets)
}

func TestModerationCDNPurgeOwnerlessIndexUnionsCachedOwnersBeforeDeletion(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	imageCID := mediaImageCID("ownerless index warm")
	var boundary *ownerlessDeletionBoundary
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex,
		func(options *moderationMediaHarnessConfig) {
			options.wrapCache = func(cache *imageproxy.DiskCache, db *sql.DB, root string) imageproxy.Cache {
				boundary = &ownerlessDeletionBoundary{DiskCache: cache, db: db, root: root, cid: imageCID}
				return boundary
			}
		})
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	ownerD := ownerlessOwnerC
	boundary.owners = []string{h.ownerB, ownerD}
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	otherPost, _ := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, otherPost.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", true)
	for _, preset := range []string{"content_preview", "content_full"} {
		for _, owner := range []string{h.ownerB, ownerD} {
			require.NoError(t, h.cache.Set(preset, owner, imageCID, testkit.TestPNG(32, 32)))
		}
	}
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB, ownerD)

	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")
	waitModerationCDN(t, h.queue)
	require.ElementsMatch(t, []string{ownerA, h.ownerB, ownerC, ownerD}, ownerlessTargetOwners(t, h.db, imageCID))
	h.assertNoCachedBlob(t, imageCID, "")
	assert.Empty(t, boundary.snapshot(), "every deletion of B/D must follow a committed target")
}

func TestModerationCDNPurgeOwnerlessIndexIncludesInactiveOwners(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex)
	imageCID := mediaImageCID("ownerless index inactive")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", false)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", false)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", false)
	var total, active int
	require.NoError(t, h.db.QueryRowContext(t.Context(), `
		SELECT count(*), count(*) FILTER (WHERE active) FROM blob_references WHERE blob_cid = $1
	`, imageCID).Scan(&total, &active))
	require.Equal(t, 3, total)
	require.Zero(t, active)
	h.assertNoCachedBlob(t, imageCID, "")

	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")
	waitModerationCDN(t, h.queue)
	require.ElementsMatch(t, []string{ownerA, h.ownerB, ownerC}, ownerlessTargetOwners(t, h.db, imageCID))
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

type failingOwnerlessBlobIndex struct {
	imageproxy.BlobOwnerIndex
	failing *atomic.Bool
}

func (index failingOwnerlessBlobIndex) ListBlobOwners(ctx context.Context, cid string) ([]string, error) {
	if index.failing.Load() {
		return nil, errors.New("blob owner index unavailable")
	}
	return index.BlobOwnerIndex.ListBlobOwners(ctx, cid)
}

func TestModerationCDNPurgeOwnerlessIndexFailureRetainsBytesUntilSweep(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	var failing atomic.Bool
	failing.Store(true)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex,
		func(options *moderationMediaHarnessConfig) {
			options.wrapBlobOwnerIndex = func(index imageproxy.BlobOwnerIndex) imageproxy.BlobOwnerIndex {
				return failingOwnerlessBlobIndex{BlobOwnerIndex: index, failing: &failing}
			}
		})
	imageCID := mediaImageCID("ownerless index lookup failure")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", true)
	for _, preset := range []string{"content_preview", "content_full"} {
		require.NoError(t, h.cache.Set(preset, h.ownerB, imageCID, testkit.TestPNG(32, 32)))
	}
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)

	h.remove(t, subject, "social.coves.moderation.defs#reasonIllegalContent")
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.Error(t, h.proxyService.PurgeBlob(ctx, imageCID))
	require.Error(t, h.proxyService.PurgeActiveBlocks(ctx, postgres.NewModerationRepository(h.db)))
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)
	assert.NotContains(t, ownerlessTargetOwners(t, h.db, imageCID), ownerC)

	failing.Store(false)
	require.NoError(t, h.proxyService.PurgeActiveBlocks(ctx, postgres.NewModerationRepository(h.db)))
	require.ElementsMatch(t, []string{ownerA, h.ownerB, ownerC}, ownerlessTargetOwners(t, h.db, imageCID))
	h.assertNoCachedBlob(t, imageCID, "")
}

func TestModerationCDNPurgeOwnerScopedIndexKeepsOtherOwnersCached(t *testing.T) {
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex)
	imageCID := mediaImageCID("owner scoped index guard")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", true)
	for _, preset := range []string{"content_preview", "content_full"} {
		for _, owner := range []string{ownerA, h.ownerB} {
			require.NoError(t, h.cache.Set(preset, owner, imageCID, testkit.TestPNG(32, 32)))
		}
	}
	requireOwnerlessCachedEntries(t, h, imageCID, ownerA, h.ownerB)

	h.remove(t, subject, "social.coves.moderation.defs#reasonSpam")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, h.proxyService.PurgeActiveBlocks(ctx, postgres.NewModerationRepository(h.db)))
	require.ElementsMatch(t, []string{ownerA}, ownerlessTargetOwners(t, h.db, imageCID))
	h.assertNoCachedBlob(t, imageCID, ownerA)
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)
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
		repo, allowMediaReferences{}, imageproxy.DefaultConfig(), imageproxy.WithCDNPurgeRecorder(h.queue))
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
		postgres.NewModerationRepository(h.db), allowMediaReferences{}, imageproxy.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.PurgeActiveBlocks(t.Context(), postgres.NewModerationRepository(h.db)))
	h.assertNoCachedBlob(t, imageCID, "")
	assert.Zero(t, moderationCDNTargetCount(t, h.db))
	assert.Empty(t, endpoint.Requests())
}

const ownerlessIllegalContent = "social.coves.moderation.defs#reasonIllegalContent"

func TestModerationCDNPurgeOwnerlessSkipsNonCanonicalIndexOwner(t *testing.T) {
	const nonCanonicalOwner = "did:web:Example.test"
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex)
	imageCID := mediaImageCID("ownerless index non-canonical owner")
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	seedOwnerlessBlobReference(t, h.db, nonCanonicalOwner, imageCID,
		"at://"+nonCanonicalOwner+"/app.bsky.actor.profile/self", "avatar", true)
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	for _, preset := range []string{"content_preview", "content_full"} {
		require.NoError(t, h.cache.Set(preset, h.ownerB, imageCID, testkit.TestPNG(32, 32)))
	}
	requireOwnerlessCachedEntries(t, h, imageCID, h.ownerB)

	h.remove(t, subject, ownerlessIllegalContent)
	waitModerationCDN(t, h.queue)
	assert.ElementsMatch(t, []string{ownerA, h.ownerB}, ownerlessTargetOwners(t, h.db, imageCID),
		"a non-canonical indexed owner must not stop the valid owners from being recorded")
	h.assertNoCachedBlob(t, imageCID, "")

	for _, preset := range []string{"content_preview", "content_full"} {
		require.NoError(t, h.cache.Set(preset, h.ownerB, imageCID, testkit.TestPNG(32, 32)))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, h.proxyService.PurgeActiveBlocks(ctx, postgres.NewModerationRepository(h.db)))
	h.assertNoCachedBlob(t, imageCID, "")
	assert.NotContains(t, ownerlessTargetOwners(t, h.db, imageCID), nonCanonicalOwner)
}

// ownerlessCompletedBlock is an illegal-content removal whose every-owner
// block has indexed owners A (the author), B and C, none of them cached, and
// whose purge targets have all completed.
type ownerlessCompletedBlock struct {
	h        *moderationMediaHarness
	endpoint *testkit.CloudflarePurgeEndpoint
	clock    *moderationCDNClock
	subject  moderation.StrongRef
	removed  *moderation.MutationResult
	cid      string
	owners   []string
}

func newOwnerlessCompletedBlock(t *testing.T, label string) ownerlessCompletedBlock {
	t.Helper()
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	h := newModerationCDNPurgeHarnessConfigured(t, endpoint, clock, 90*time.Second, installOwnerlessBlobIndex)
	imageCID := mediaImageCID(label)
	subject, ownerA := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	ownerC := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxc"))
	seedOwnerlessBlobReference(t, h.db, ownerA, imageCID, subject.URI, "post_image", true)
	seedOwnerlessBlobReference(t, h.db, h.ownerB, imageCID,
		"at://"+h.ownerB+"/"+moderation.CommentCollection+"/"+testkit.TID(), "comment_image", true)
	seedOwnerlessBlobReference(t, h.db, ownerC, imageCID,
		"at://"+ownerC+"/app.bsky.actor.profile/self", "avatar", true)
	h.assertNoCachedBlob(t, imageCID, "")

	removed := h.remove(t, subject, ownerlessIllegalContent)
	waitModerationCDN(t, h.queue)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	owners := []string{ownerA, h.ownerB, ownerC}
	require.ElementsMatch(t, owners, ownerlessTargetOwners(t, h.db, imageCID))
	for _, owner := range owners {
		require.Equal(t, "completed", moderationCDNTarget(t, h.db, owner, imageCID).state, "target for %s", owner)
	}
	return ownerlessCompletedBlock{h: h, endpoint: endpoint, clock: clock, subject: subject, removed: removed, cid: imageCID, owners: owners}
}

// sweep runs the blocked media sweep at the given time, waits for its
// immediate purge attempts, then runs the queue sweep once the write timeout
// has passed so every target purged at that time can complete.
func (block ownerlessCompletedBlock) sweep(t *testing.T, at time.Time) {
	t.Helper()
	block.clock.Set(at)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, block.h.proxyService.PurgeActiveBlocks(ctx, postgres.NewModerationRepository(block.h.db)))
	waitModerationCDN(t, block.h.queue)
	block.clock.Set(at.Add(90 * time.Second))
	require.NoError(t, block.h.queue.Sweep(ctx))
}

// requestedFileCounts counts how often each file was sent from request index from on.
func (block ownerlessCompletedBlock) requestedFileCounts(from int) map[string]int {
	counts := make(map[string]int)
	for _, request := range block.endpoint.Requests()[from:] {
		for _, file := range request.Files {
			counts[file]++
		}
	}
	return counts
}

func TestModerationCDNPurgeOwnerlessSweepLeavesCompletedIndexTargets(t *testing.T) {
	block := newOwnerlessCompletedBlock(t, "ownerless sweep completed index")
	before := len(block.endpoint.Requests())

	block.sweep(t, time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC))
	block.sweep(t, time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
	assert.Empty(t, block.requestedFileCounts(before), "sweeps must not purge completed index-only targets again")
	for _, owner := range block.owners {
		assert.Equal(t, "completed", moderationCDNTarget(t, block.h.db, owner, block.cid).state, "target for %s", owner)
	}
}

func TestModerationCDNPurgeOwnerlessSweepRecordsNewlyIndexedOwner(t *testing.T) {
	block := newOwnerlessCompletedBlock(t, "ownerless sweep new index owner")
	ownerE := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxe"))
	seedOwnerlessBlobReference(t, block.h.db, ownerE, block.cid,
		"at://"+ownerE+"/app.bsky.actor.profile/self", "avatar", true)
	before := len(block.endpoint.Requests())

	block.sweep(t, time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC))
	counts := block.requestedFileCounts(before)
	for _, file := range moderationCDNFiles(ownerE, block.cid) {
		assert.Positive(t, counts[file], "the sweep must purge %s", file)
	}
	assert.Equal(t, "completed", moderationCDNTarget(t, block.h.db, ownerE, block.cid).state)
}

func TestModerationCDNPurgeOwnerlessRemovalAfterRestoreRependsIndexOwners(t *testing.T) {
	block := newOwnerlessCompletedBlock(t, "ownerless restore re-removal")
	h := block.h
	restored := h.restore(t, block.subject, block.removed)
	block.clock.Set(time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC))
	before := len(block.endpoint.Requests())
	result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
		Subject: block.subject, ExpectedVersion: restored.State.Version,
		IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: ownerlessIllegalContent,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	waitModerationCDN(t, h.queue)
	counts := block.requestedFileCounts(before)
	for _, owner := range block.owners {
		assert.Equal(t, "pending", moderationCDNTarget(t, h.db, owner, block.cid).state, "re-removal must re-pend %s", owner)
		for _, file := range moderationCDNFiles(owner, block.cid) {
			assert.Equal(t, 1, counts[file], "the re-removal's immediate attempt must purge %s once", file)
		}
	}

	block.clock.Set(time.Date(2026, 9, 28, 13, 1, 30, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	counts = block.requestedFileCounts(before)
	for _, owner := range block.owners {
		assert.Equal(t, "completed", moderationCDNTarget(t, h.db, owner, block.cid).state, "target for %s", owner)
		for _, file := range moderationCDNFiles(owner, block.cid) {
			assert.Equal(t, 2, counts[file], "the completing sweep must purge %s once more", file)
		}
	}

	afterCompletion := len(block.endpoint.Requests())
	block.sweep(t, time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
	assert.Empty(t, block.requestedFileCounts(afterCompletion), "a later sweep must not purge the re-removal's targets again")
	for _, owner := range block.owners {
		assert.Equal(t, "completed", moderationCDNTarget(t, h.db, owner, block.cid).state, "target for %s", owner)
	}
}

func TestModerationCDNPurgeOwnerlessSweepRependsRecachedOwner(t *testing.T) {
	block := newOwnerlessCompletedBlock(t, "ownerless sweep recached owner")
	h := block.h
	for _, preset := range []string{"content_preview", "content_full"} {
		require.NoError(t, h.cache.Set(preset, h.ownerB, block.cid, testkit.TestPNG(32, 32)))
	}
	requireOwnerlessCachedEntries(t, h, block.cid, h.ownerB)
	before := len(block.endpoint.Requests())

	block.sweep(t, time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC))
	h.assertNoCachedBlob(t, block.cid, "")
	counts := block.requestedFileCounts(before)
	for _, file := range moderationCDNFiles(h.ownerB, block.cid) {
		assert.Positive(t, counts[file], "re-cached bytes must purge %s again", file)
	}
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, h.ownerB, block.cid).state)
}
