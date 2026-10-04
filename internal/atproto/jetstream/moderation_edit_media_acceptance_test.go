//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	imagehandler "Coves/internal/api/handlers/imageproxy"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const editMediaIllegalReason = "social.coves.moderation.defs#reasonIllegalContent"

func editMediaCID(label string) string {
	digest, err := multihash.Sum([]byte(label), multihash.SHA2_256, -1)
	if err != nil {
		panic(err)
	}
	return cid.NewCidV1(cid.Raw, digest).String()
}

type editMediaPDS struct {
	mu    sync.Mutex
	known map[string]map[string]bool
}

func (p *editMediaPDS) know(owner string, blobCIDs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.known[owner] == nil {
		p.known[owner] = make(map[string]bool)
	}
	for _, blobCID := range blobCIDs {
		p.known[owner][blobCID] = true
	}
}

func (p *editMediaPDS) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/xrpc/com.atproto.sync.getBlob" {
		http.NotFound(w, r)
		return
	}
	parsed, err := cid.Decode(r.URL.Query().Get("cid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	known := p.known[r.URL.Query().Get("did")][parsed.String()]
	p.mu.Unlock()
	if !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(testkit.TestPNG(32, 32))
}

type editMediaResolver struct{ pdsURL string }

func (r editMediaResolver) ResolveDID(_ context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{DID: did, Service: []identity.Service{{
		Type: "AtprotoPersonalDataServer", ServiceEndpoint: r.pdsURL,
	}}}, nil
}

func (editMediaResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	return nil, errors.New("blob owner must be resolved by DID")
}

func (editMediaResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", errors.New("blob owner must be resolved by DID")
}
func (editMediaResolver) Purge(context.Context, string) error { return nil }

type editMediaHarness struct {
	db         *sql.DB
	proxy      *httptest.Server
	pds        *editMediaPDS
	moderator  moderation.Service
	posts      *PostEventConsumer
	comments   *CommentEventConsumer
	otherOwner string
}

func newEditMediaHarness(t *testing.T) *editMediaHarness {
	t.Helper()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	otherOwner := fixtures.DID(testkit.UniqueIDWithPrefix(t, "othermedia"))
	pds := &editMediaPDS{known: make(map[string]map[string]bool)}
	pdsServer := httptest.NewServer(http.HandlerFunc(pds.serve))
	t.Cleanup(pdsServer.Close)
	cache, err := imageproxy.NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	processor, err := imageproxy.NewProcessor(imageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	store := postgres.NewModerationRepository(db)
	proxyService, err := imageproxy.NewService(cache, processor,
		imageproxy.NewPDSFetcher(30*time.Second, 10, imageproxy.WithPrivateHostsAllowed()),
		store, imageproxy.DefaultConfig())
	require.NoError(t, err)
	router := chi.NewRouter()
	routes.RegisterImageProxyRoutes(router, imagehandler.NewHandler(proxyService, editMediaResolver{pdsURL: pdsServer.URL}))
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)
	reconciler := moderation.NewMediaReconciler(store, fixtures.InstanceDID(), proxyService)
	moderator := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		store, moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour,
			MaxLiveIdempotencyKeys: 1000, Purger: proxyService,
		},
	)
	return &editMediaHarness{
		db: db, proxy: proxy, pds: pds, moderator: moderator, otherOwner: otherOwner,
		posts: NewPostEventConsumer(postgres.NewPostRepository(db),
			postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), fixture.users, db,
			WithAdmissions(fixture.admissions), WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
			WithPostMediaReconciler(reconciler)),
		comments: NewCommentEventConsumer(postgres.NewCommentRepository(db), db, WithCommentMediaReconciler(reconciler)),
	}
}

func (h *editMediaHarness) imageStatus(t *testing.T, owner, blobCID string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		h.proxy.URL+"/img/content_preview/plain/"+owner+"/"+blobCID, nil)
	require.NoError(t, err)
	response, err := h.proxy.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if response.StatusCode == http.StatusOK {
		require.NotEmpty(t, body, "the image proxy must serve image bytes")
		require.Equal(t, "image/jpeg", response.Header.Get("Content-Type"))
	}
	return response.StatusCode
}

func (h *editMediaHarness) remove(t *testing.T, subject moderation.StrongRef, version string) *moderation.MutationResult {
	t.Helper()
	result, err := h.moderator.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: version, IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: editMediaIllegalReason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	return result
}

func (h *editMediaHarness) restore(t *testing.T, subject moderation.StrongRef, removed *moderation.MutationResult) string {
	t.Helper()
	result, err := h.moderator.RestoreContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "mediaadmin")), moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "restore-" + testkit.UniqueID(t), Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	return result.State.Version
}

func editMediaCommentEvent(root moderation.StrongRef, operation, rkey, rev, recordCID string, eventTime time.Time, blobCIDs ...string) *JetstreamEvent {
	var record map[string]interface{}
	if operation != "delete" {
		record = map[string]interface{}{
			"$type": moderation.CommentCollection, "content": "image comment", "createdAt": eventTime.Format(time.RFC3339),
			"reply": map[string]interface{}{
				"root":   map[string]interface{}{"uri": root.URI, "cid": root.CID},
				"parent": map[string]interface{}{"uri": root.URI, "cid": root.CID},
			},
			"embed": postModerationRecord(blobCIDs...)["embed"],
		}
	}
	return revCommitEvent(pv2Author, moderation.CommentCollection, operation, rkey, rev, recordCID, eventTime.UnixMicro(), record)
}

// A consumer may block an edit-introduced blob only for its author. The original
// admin removal retains its ownerless block, while a later removal covers the
// blobs indexed at that later instant.
func TestModerationEditIntroducedMediaIsOwnerScoped(t *testing.T) {
	for _, variant := range []struct {
		name string
		kind string
		// indexesEdit is true when the consumer indexes B, so the stored row
		// names it at the second removal.
		indexesEdit bool
	}{
		{name: "comment edit", kind: "comment", indexesEdit: true},
		{name: "postv2 edit", kind: "post edit", indexesEdit: true},
		{name: "postv2 delete and recreate", kind: "post recreate"},
		{name: "postv2 already-indexed create", kind: "post conflict"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			h := newEditMediaHarness(t)
			imageA := editMediaCID(variant.name + " image A")
			imageB := editMediaCID(variant.name + " image B")
			for _, owner := range []string{pv2Author, h.otherOwner} {
				h.pds.know(owner, imageA, imageB)
			}
			revs := increasingTIDs(t, 4)
			started := time.Now().Add(-time.Minute)
			postRkey := testkit.TID()
			postSubject := moderation.StrongRef{URI: pv2URI(pv2Author, postRkey), CID: editMediaCID(variant.name + " post record")}
			require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey, revs[0],
				postSubject.CID, started.UnixMicro(), postModerationRecord(imageA))))
			subject := postSubject
			var commentRkey string
			if variant.kind == "comment" {
				commentRkey = testkit.TID()
				subject = moderation.StrongRef{
					URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + commentRkey,
					CID: editMediaCID(variant.name + " comment record"),
				}
				require.NoError(t, h.comments.HandleEvent(t.Context(), editMediaCommentEvent(postSubject,
					"create", commentRkey, revs[0], subject.CID, started, imageA)))
			}
			// Warm the other owner's copy of B before reconciliation: a block must
			// win over disk cache, and a scoped purge must leave this copy intact.
			require.Equal(t, http.StatusOK, h.imageStatus(t, h.otherOwner, imageB))
			removed := h.remove(t, subject, "v0")
			switch variant.kind {
			case "comment":
				require.NoError(t, h.comments.HandleEvent(t.Context(), editMediaCommentEvent(postSubject,
					"update", commentRkey, revs[1], editMediaCID(variant.name+" edited record"), started.Add(time.Second), imageA, imageB)))
			case "post edit":
				require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", postRkey, revs[1],
					editMediaCID(variant.name+" edited record"), started.Add(time.Second).UnixMicro(), postModerationRecord(imageA, imageB))))
			case "post recreate":
				require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "delete", postRkey, revs[1],
					"", started.Add(time.Second).UnixMicro(), nil)))
				require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", postRkey, revs[2],
					editMediaCID(variant.name+" recreated record"), started.Add(2*time.Second).UnixMicro(), postModerationRecord(imageB))))
			case "post conflict":
				// Exercise the real create/ON CONFLICT path, whose incoming embed
				// never replaces the already-indexed post row.
				embed, err := json.Marshal(postModerationRecord(imageB)["embed"])
				require.NoError(t, err)
				embedJSON := string(embed)
				applied, err := h.posts.indexPostIfRevWins(t.Context(), &posts.Post{
					URI: subject.URI, CID: editMediaCID(variant.name + " incoming record"), RKey: postRkey,
					AuthorDID: pv2Author, CommunityDID: pv2Community, Embed: &embedJSON,
					CreatedAt: started, IndexedAt: started.Add(time.Second),
				}, revs[1])
				require.NoError(t, err)
				require.False(t, applied, "an existing post must take the ON CONFLICT path")
			default:
				t.Fatalf("unknown variant kind %q", variant.kind)
			}
			var indexedEmbed string
			if variant.kind == "comment" {
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT embed::text FROM comments WHERE uri = $1`, subject.URI).Scan(&indexedEmbed))
			} else {
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT embed::text FROM posts WHERE uri = $1`, subject.URI).Scan(&indexedEmbed))
			}
			if variant.indexesEdit {
				require.Contains(t, indexedEmbed, imageB, "the consumer must index the edited image")
			} else {
				require.Contains(t, indexedEmbed, imageA, "the original post row must remain indexed")
			}
			assert.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_media_blocks
				WHERE owner_did = $1 AND blob_cid = $2 AND active`, pv2Author, imageB),
				"the new image must have exactly one active author-owned block")
			assert.Equal(t, 1, countRows(t, h.db, `SELECT count(*) FROM moderation_media_blocks
				WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active`, removed.Action.ID, pv2Author, imageB),
				"the author-owned block belongs to the removal being restored")
			assert.Zero(t, countRows(t, h.db, `SELECT count(*) FROM moderation_media_blocks
				WHERE blob_cid = $1 AND owner_did IS NULL AND active`, imageB),
				"edit-introduced B must have no ownerless block")
			assert.Equal(t, http.StatusOK, h.imageStatus(t, h.otherOwner, imageB), "another owner's warm B stays available")
			assert.Equal(t, http.StatusNotFound, h.imageStatus(t, pv2Author, imageB), "the author's B is blocked")
			assert.Equal(t, http.StatusNotFound, h.imageStatus(t, h.otherOwner, imageA), "the initial removal blocks A for every owner")

			currentCID := subject.CID
			if variant.indexesEdit {
				currentCID = editMediaCID(variant.name + " edited record")
			}
			currentSubject := moderation.StrongRef{URI: subject.URI, CID: currentCID}
			version := h.restore(t, currentSubject, removed)
			for _, owner := range []string{pv2Author, h.otherOwner} {
				for _, blobCID := range []string{imageA, imageB} {
					assert.Equal(t, http.StatusOK, h.imageStatus(t, owner, blobCID), "restore must release both blobs for %s", owner)
				}
			}
			h.remove(t, currentSubject, version)
			indexedImages := []string{imageA}
			if variant.indexesEdit {
				indexedImages = append(indexedImages, imageB)
			}
			for _, indexedImage := range indexedImages {
				assert.Equal(t, http.StatusNotFound, h.imageStatus(t, h.otherOwner, indexedImage),
					"a new illegal-content removal blocks each currently indexed CID for every owner, even after a warm cache hit")
			}
			if !variant.indexesEdit {
				assert.Equal(t, http.StatusOK, h.imageStatus(t, h.otherOwner, imageB),
					"a new removal must not block another owner's B, which the stored row does not name")
			}
		})
	}
}
