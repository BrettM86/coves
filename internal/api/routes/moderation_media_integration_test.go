//go:build integration

package routes_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	imagehandler "Coves/internal/api/handlers/imageproxy"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mediaImageCID returns a real, canonical CIDv1 for label. The proxy decodes
// every CID, so a fixture must be a decodable CID rather than a lookalike.
func mediaImageCID(label string) string {
	digest, err := multihash.Sum([]byte("moderation media "+label), multihash.SHA2_256, -1)
	if err != nil {
		panic(err)
	}
	return cid.NewCidV1(cid.Raw, digest).String()
}

// base58MediaCID re-encodes a canonical CID in base58btc: the same CID in a
// different multibase string.
func base58MediaCID(t *testing.T, canonical string) string {
	t.Helper()
	parsed, err := cid.Decode(canonical)
	require.NoError(t, err)
	encoded, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	require.NotEqual(t, canonical, encoded)
	return encoded
}

type mediaBlobKey struct{ did, cid string }

type mediaPDS struct {
	mu     sync.Mutex
	counts map[mediaBlobKey]int
	known  map[mediaBlobKey]bool
}

func (p *mediaPDS) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/xrpc/com.atproto.sync.getBlob" {
		http.NotFound(w, r)
		return
	}
	// Like the reference PDS, look the blob up by its parsed CID, so every
	// multibase encoding of one CID names the same blob.
	parsed, err := cid.Decode(r.URL.Query().Get("cid"))
	if err != nil {
		http.Error(w, "invalid cid", http.StatusBadRequest)
		return
	}
	key := mediaBlobKey{r.URL.Query().Get("did"), parsed.String()}
	p.mu.Lock()
	known := p.known[key]
	if known {
		p.counts[key]++
	}
	p.mu.Unlock()
	if !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(testkit.TestPNG(32, 32))
}

func (p *mediaPDS) count(did, cid string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[mediaBlobKey{did, cid}]
}

type mediaPDSResolver struct{ url string }

func (r mediaPDSResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{DID: did, Service: []identity.Service{{
		Type: "AtprotoPersonalDataServer", ServiceEndpoint: r.url,
	}}}, nil
}
func (mediaPDSResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	return nil, fmt.Errorf("image proxy must resolve the blob owner DID")
}
func (mediaPDSResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", fmt.Errorf("image proxy must resolve the blob owner DID")
}
func (mediaPDSResolver) Purge(context.Context, string) error { return nil }

type waitingMediaFetcher struct {
	upstream imageproxy.Fetcher
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (f *waitingMediaFetcher) Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error) {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
		return f.upstream.Fetch(ctx, pdsURL, did, cid)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type moderationMediaHarness struct {
	db         *sql.DB
	cache      *imageproxy.DiskCache
	cacheDir   string
	proxy      *httptest.Server
	pds        *mediaPDS
	moderation moderation.Service
	postURI    string
	postCID    string
	ownerA     string
	ownerB     string

	// proxyService is the purger a consumer's media reconciler shares with the proxy.
	proxyService *imageproxy.ImageProxyService
}

func newModerationMediaHarness(t *testing.T, blockFetch bool) (*moderationMediaHarness, *waitingMediaFetcher) {
	t.Helper()
	db := testkit.DB(t)
	ownerName := testkit.UniqueIDWithPrefix(t, "mediaowner")
	otherName := testkit.UniqueIDWithPrefix(t, "otherowner")
	ownerA, ownerB := fixtures.DID(ownerName), fixtures.DID(otherName)
	fixtures.User(t, db, ownerName+".test", ownerA)
	fixtures.User(t, db, otherName+".test", ownerB)
	communityName := testkit.UniqueIDWithPrefix(t, "mediapost")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, ownerA, "moderation media", 0, time.Now())
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)

	cacheDir := t.TempDir()
	cache, err := imageproxy.NewDiskCache(cacheDir, 1, 0)
	require.NoError(t, err)
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	pds := &mediaPDS{counts: make(map[mediaBlobKey]int), known: make(map[mediaBlobKey]bool)}
	pdsServer := httptest.NewServer(http.HandlerFunc(pds.serve))
	t.Cleanup(pdsServer.Close)
	var fetcher imageproxy.Fetcher = imageproxy.NewPDSFetcher(30*time.Second, 10, imageproxy.WithPrivateHostsAllowed())
	var waiting *waitingMediaFetcher
	if blockFetch {
		waiting = &waitingMediaFetcher{upstream: fetcher, entered: make(chan struct{}), release: make(chan struct{})}
		fetcher = waiting
		t.Cleanup(func() {
			select {
			case <-waiting.release:
			default:
				close(waiting.release)
			}
		})
	}
	store := postgres.NewModerationRepository(db)
	proxyService, err := imageproxy.NewService(cache, processor, fetcher, store, imageproxy.DefaultConfig())
	require.NoError(t, err)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		store, moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000, Purger: proxyService},
	)
	router := chi.NewRouter()
	routes.RegisterImageProxyRoutes(router, imagehandler.NewHandler(proxyService, mediaPDSResolver{url: pdsServer.URL}))
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)
	return &moderationMediaHarness{db: db, cache: cache, cacheDir: cacheDir, proxy: proxy, proxyService: proxyService, pds: pds,
		moderation: service, postURI: postURI, postCID: post.CID, ownerA: ownerA, ownerB: ownerB}, waiting
}

func (h *moderationMediaHarness) comment(t *testing.T, owner string, imageCIDs ...string) moderation.StrongRef {
	t.Helper()
	images := make([]any, 0, len(imageCIDs))
	for _, cid := range imageCIDs {
		images = append(images, map[string]any{"alt": "test image", "image": map[string]any{
			"$type": "blob", "ref": map[string]any{"$link": cid}, "mimeType": "image/png", "size": 10,
		}})
	}
	return h.commentWithEmbed(t, owner, map[string]any{"$type": "social.coves.embed.images", "images": images}, imageCIDs...)
}

// commentWithEmbed indexes a comment carrying embed verbatim, and registers
// servedCIDs as blobs the owner's PDS serves.
func (h *moderationMediaHarness) commentWithEmbed(t *testing.T, owner string, embedValue any, servedCIDs ...string) moderation.StrongRef {
	t.Helper()
	rkey := testkit.TID()
	subject := moderation.StrongRef{URI: "at://" + owner + "/" + moderation.CommentCollection + "/" + rkey, CID: mediaImageCID("comment record")}
	for _, cid := range servedCIDs {
		h.pds.known[mediaBlobKey{owner, cid}] = true
	}
	embed, err := json.Marshal(embedValue)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'moderated comment', $7::jsonb, NOW())
	`, subject.URI, subject.CID, rkey, owner, h.postURI, h.postCID, string(embed))
	require.NoError(t, err)
	return subject
}

func (h *moderationMediaHarness) request(t *testing.T, preset, owner, cid string) int {
	t.Helper()
	url := h.proxy.URL + "/img/" + preset + "/plain/" + owner + "/" + cid
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	response, err := h.proxy.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s: %d %s", url, response.StatusCode, body)
	}
	return response.StatusCode
}

func (h *moderationMediaHarness) remove(t *testing.T, subject moderation.StrongRef, reason string) *moderation.MutationResult {
	t.Helper()
	result, err := h.moderation.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: reason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	return result
}

func (h *moderationMediaHarness) restore(t *testing.T, subject moderation.StrongRef, removed *moderation.MutationResult) {
	t.Helper()
	result, err := h.moderation.RestoreContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "restore-" + testkit.UniqueID(t), Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
}

func (h *moderationMediaHarness) cachePath(preset, owner, cid string) string {
	return filepath.Join(h.cacheDir, preset, strings.ReplaceAll(owner, ":", "_"), cid)
}

func (h *moderationMediaHarness) assertNoCachedBlob(t *testing.T, cid string, owner string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(h.cacheDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == cid && (owner == "" || filepath.Base(filepath.Dir(path)) == strings.ReplaceAll(owner, ":", "_")) {
			assert.Failf(t, "blocked image remains on disk", "path: %s", path)
		}
		return nil
	}))
}

func TestModerationMediaServing(t *testing.T) {
	const spam = "social.coves.moderation.defs#reasonSpam"
	const illegal = "social.coves.moderation.defs#reasonIllegalContent"
	const preset = "content_preview"
	var cid1, cid2, cid5, cid6, cid7 = mediaImageCID("a"), mediaImageCID("b"), mediaImageCID("e"), mediaImageCID("f"), mediaImageCID("g")

	t.Run("spam purges warm images across presets and blocks cold fetches", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		subject := h.comment(t, h.ownerA, cid1, cid2)
		for _, name := range []string{preset, "content_full", "avatar_small"} {
			for _, cid := range []string{cid1, cid2} {
				require.Equal(t, http.StatusOK, h.request(t, name, h.ownerA, cid))
				_, err := os.Stat(h.cachePath(name, h.ownerA, cid))
				require.NoError(t, err, "image must really be warm on disk before removal")
			}
		}
		before1, before2 := h.pds.count(h.ownerA, cid1), h.pds.count(h.ownerA, cid2)
		assert.Equal(t, 3, before1)
		assert.Equal(t, 3, before2)
		h.remove(t, subject, spam)
		for _, cid := range []string{cid1, cid2} {
			h.assertNoCachedBlob(t, cid, h.ownerA)
			for i := 0; i < 2; i++ {
				require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, cid))
			}
		}
		assert.Equal(t, before1, h.pds.count(h.ownerA, cid1), "blocked image must not be refetched")
		assert.Equal(t, before2, h.pds.count(h.ownerA, cid2), "blocked image must not be refetched")
	})

	t.Run("same CID under an unremoved owner remains fetchable", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		subject := h.comment(t, h.ownerA, cid1)
		h.comment(t, h.ownerB, cid1)
		h.remove(t, subject, spam)
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, cid1))
		require.Equal(t, http.StatusOK, h.request(t, preset, h.ownerB, cid1))
		assert.Equal(t, 1, h.pds.count(h.ownerB, cid1), "another owner's blob must still be fetched")
	})

	t.Run("illegal content purges every owner's cached copy", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		subject := h.comment(t, h.ownerA, cid5)
		h.comment(t, h.ownerB, cid5)
		for _, name := range []string{preset, "content_full"} {
			for _, owner := range []string{h.ownerA, h.ownerB} {
				require.Equal(t, http.StatusOK, h.request(t, name, owner, cid5))
				_, err := os.Stat(h.cachePath(name, owner, cid5))
				require.NoError(t, err)
			}
		}
		h.remove(t, subject, illegal)
		h.assertNoCachedBlob(t, cid5, "")
		for _, owner := range []string{h.ownerA, h.ownerB} {
			require.Equal(t, http.StatusNotFound, h.request(t, preset, owner, cid5))
			assert.Equal(t, 2, h.pds.count(owner, cid5), "a blocked owner must not be fetched again")
		}
	})

	t.Run("in flight fetch cannot publish after removal", func(t *testing.T) {
		h, waiting := newModerationMediaHarness(t, true)
		subject := h.comment(t, h.ownerA, cid6)
		result := make(chan int, 1)
		go func() {
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				h.proxy.URL+"/img/"+preset+"/plain/"+h.ownerA+"/"+cid6, nil)
			if err != nil {
				result <- 0
				return
			}
			response, err := h.proxy.Client().Do(request)
			if err != nil {
				result <- 0
				return
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			result <- response.StatusCode
		}()
		testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
			select {
			case <-waiting.entered:
				return true, nil
			default:
				return false, nil
			}
		})
		h.remove(t, subject, spam)
		close(waiting.release)
		var status int
		testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
			select {
			case status = <-result:
				return true, nil
			default:
				return false, nil
			}
		})
		require.Equal(t, http.StatusNotFound, status, "an in-flight fetch must not publish blocked bytes")
		testkit.Holds(t, 300*time.Millisecond, func() (bool, error) {
			_, err := os.Stat(h.cachePath(preset, h.ownerA, cid6))
			return os.IsNotExist(err), nil
		})
		assert.Equal(t, 1, h.pds.count(h.ownerA, cid6), "fetch must have reached the PDS before the post-fetch block check")
	})

	t.Run("shared blob stays blocked until last removal is restored then refetches", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		first := h.comment(t, h.ownerA, cid7)
		second := h.comment(t, h.ownerA, cid7)
		require.Equal(t, http.StatusOK, h.request(t, preset, h.ownerA, cid7))
		before := h.pds.count(h.ownerA, cid7)
		require.Equal(t, 1, before)
		removedFirst := h.remove(t, first, spam)
		removedSecond := h.remove(t, second, spam)
		h.assertNoCachedBlob(t, cid7, h.ownerA)
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, cid7))
		h.restore(t, first, removedFirst)
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, cid7))
		assert.Equal(t, before, h.pds.count(h.ownerA, cid7))
		h.restore(t, second, removedSecond)
		require.Equal(t, http.StatusOK, h.request(t, preset, h.ownerA, cid7))
		assert.Equal(t, before+1, h.pds.count(h.ownerA, cid7), "last restore must permit a fresh real PDS fetch")
	})
}

func TestModerationMediaBlocksEveryServedEncoding(t *testing.T) {
	const spam = "social.coves.moderation.defs#reasonSpam"
	const preset = "content_preview"

	t.Run("base58btc encoding of a blocked CID is refused without a fetch", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		blocked := mediaImageCID("re-encoded")
		subject := h.comment(t, h.ownerA, blocked)
		h.remove(t, subject, spam)
		alias := base58MediaCID(t, blocked)
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, alias))
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, blocked))
		assert.Zero(t, h.pds.count(h.ownerA, blocked), "a re-encoded blocked CID must not reach the PDS")
		h.assertNoCachedBlob(t, alias, "")
		h.assertNoCachedBlob(t, blocked, "")
	})

	t.Run("legacy cid-encoded image is blocked on removal", func(t *testing.T) {
		h, _ := newModerationMediaHarness(t, false)
		legacy := mediaImageCID("legacy blob")
		subject := h.commentWithEmbed(t, h.ownerA, map[string]any{
			"$type": "social.coves.embed.images",
			"images": []any{map[string]any{"alt": "legacy image", "image": map[string]any{
				"cid": legacy, "mimeType": "image/png",
			}}},
		}, legacy)
		require.Equal(t, http.StatusOK, h.request(t, preset, h.ownerA, legacy), "the legacy blob must really be servable before removal")
		before := h.pds.count(h.ownerA, legacy)
		h.remove(t, subject, spam)
		h.assertNoCachedBlob(t, legacy, h.ownerA)
		require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, legacy))
		assert.Equal(t, before, h.pds.count(h.ownerA, legacy), "a blocked legacy blob must not be refetched")
	})

	t.Run("malformed embeds never prevent removal", func(t *testing.T) {
		valid := mediaImageCID("valid beside malformed")
		for _, test := range []struct {
			name    string
			embed   any
			blocked []string
		}{
			{name: "images is a string", embed: map[string]any{"$type": "social.coves.embed.images", "images": "x"}},
			{name: "image is a string", embed: map[string]any{"$type": "social.coves.embed.images", "images": []any{map[string]any{"image": "x"}}}},
			{name: "ref is a string", embed: map[string]any{"$type": "social.coves.embed.images", "images": []any{map[string]any{"image": map[string]any{"ref": mediaImageCID("string ref")}}}}},
			{name: "type is not a string", embed: map[string]any{"$type": 5, "images": []any{map[string]any{"image": map[string]any{"ref": map[string]any{"$link": mediaImageCID("numeric type")}}}}}},
			{name: "embed is an array", embed: []any{"x"}},
			{name: "undecodable link", embed: map[string]any{"$type": "social.coves.embed.images", "images": []any{map[string]any{"image": map[string]any{"ref": map[string]any{"$link": "bafynotacid"}}}}}},
			{
				name: "valid image beside malformed entries",
				embed: map[string]any{"$type": "social.coves.embed.images", "images": []any{
					"x", map[string]any{"image": 7},
					map[string]any{"image": map[string]any{"ref": map[string]any{"$link": valid}}},
				}},
				blocked: []string{valid},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				h, _ := newModerationMediaHarness(t, false)
				subject := h.commentWithEmbed(t, h.ownerA, test.embed, test.blocked...)
				removed := h.remove(t, subject, spam)
				var blocks []string
				rows, err := h.db.QueryContext(t.Context(), `
					SELECT blob_cid FROM moderation_media_blocks WHERE action_id = $1 AND active ORDER BY blob_cid
				`, removed.Action.ID)
				require.NoError(t, err)
				defer rows.Close()
				for rows.Next() {
					var blobCID string
					require.NoError(t, rows.Scan(&blobCID))
					blocks = append(blocks, blobCID)
				}
				require.NoError(t, rows.Err())
				assert.Equal(t, test.blocked, blocks)
				for _, cid := range test.blocked {
					require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, cid))
					assert.Zero(t, h.pds.count(h.ownerA, cid))
				}
			})
		}
	})
}
