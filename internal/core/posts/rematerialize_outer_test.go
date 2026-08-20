//go:build integration

package posts_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The re-materialization OUTER contract, against a REAL PDS and a REAL ledger:
// the whole tool driven end to end over records a real PDS minted the CIDs for
// (docs/PRD_AUTHOR_OWNED_POSTS.md §11).
//
// # WHY THIS IS T1 AND NOT T2 (the tier the task named)
//
// The plan asked for this outer proof in tests/e2e, driving the tool's package
// in-process. It cannot live there. The e2e package's own constitution
// (tests/e2e/contracts_test.go, "NEVER open a testkit.DB clone to assert on
// AppView state", and community_contract_test.go, "writing to the AppView's own
// database … the one thing the package doc forbids") bars the DB access the tool
// structurally requires: its ledger IS a table in that database. A prior test
// (error_recovery_test.go) was DELETED for exactly the DB-touching this would
// reintroduce. So the real-infra outer proof belongs where real-infra behaviour
// is sanctioned — the integration tier, over a real PDS and a real Postgres — and
// this file is that proof. The pipeline leg the task also wanted (postv2 →
// pending → accepted via getStatus) is already proven for ANY producer of
// postv2/acceptance records by tests/e2e/author_post_contract_test.go, and the
// tool is just such a producer; re-asserting it here would duplicate that
// coverage while breaking the tier it lives in. See the RED report's scope flag.
//
// What only a REAL PDS can prove, and what this file is therefore for:
//
//   - createAuthorRecord's converge-by-read against the PDS's ACTUAL create-only
//     guard: a re-run at the deterministic rkey meets ErrSwapConflict / ErrNoCommit
//     and reads the standing postv2 back, minting no second record.
//   - WriteAcceptance's skip against a REAL standing record: a re-run pins no new
//     CID.
//   - the CID the acceptance pins is one the PDS minted for the postv2, so a
//     strongRef that fails to round-trip fails here rather than in production.
//   - the old community.post is REALLY gone from the community repo afterward.

// realLegacySource lists the staged legacy records and deletes them from the real
// community repo. The delete is idempotent — a not-found is success — because it
// is the step a crash after the migrated checkpoint retries.
type realLegacySource struct {
	community pds.Client
	staged    []posts.LegacyPost
}

func (s *realLegacySource) ListLegacyPosts(_ context.Context) ([]posts.LegacyPost, error) {
	return s.staged, nil
}

// ReadLegacyPost re-reads the record from the REAL community repo — the read the
// pre-delete CID check is made against.
func (s *realLegacySource) ReadLegacyPost(ctx context.Context, uri string) (posts.LegacyPost, bool, error) {
	rkey := uri[strings.LastIndex(uri, "/")+1:]
	record, err := s.community.GetRecord(ctx, postCollection, rkey)
	if err != nil {
		if testkit.IsNotFound(err) {
			return posts.LegacyPost{}, false, nil
		}
		return posts.LegacyPost{}, false, err
	}
	for _, staged := range s.staged {
		if staged.URI == uri {
			// The staged shape, re-stamped with what the repo says NOW: the CID is
			// the whole reason for re-reading.
			staged.CID = record.CID
			staged.RawRecord = record.Value
			return staged, true, nil
		}
	}
	return posts.LegacyPost{}, false, nil
}

// DeleteLegacyPost deletes UNDER THE SWAP GUARD, exactly as production does: the
// PDS refuses the delete if the record no longer carries swapCID, so a
// concurrent edit cannot be destroyed.
func (s *realLegacySource) DeleteLegacyPost(ctx context.Context, legacy posts.LegacyPost, swapCID string) error {
	if swapCID == "" {
		return fmt.Errorf("refusing to delete %s without a swap guard", legacy.URI)
	}
	rkey := legacy.URI[strings.LastIndex(legacy.URI, "/")+1:]
	guarded, ok := s.community.(pds.GuardedDeleter)
	if !ok {
		return fmt.Errorf("the PDS client does not support the swap-guarded delete")
	}
	err := guarded.DeleteRecordWithSwap(ctx, postCollection, rkey, swapCID)
	if err != nil && testkit.IsNotFound(err) {
		return nil
	}
	return err
}

func TestRematerialize_OuterContract_RealPDS_MovesPostAndIsIdempotent(t *testing.T) {
	t.Parallel()

	pdsServer := testkit.NewPDS(t)
	communityAcct := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("remc"))
	authorAcct := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("rema"))
	ctx := context.Background()

	// Seed a REAL deprecated community.post into the community's repo, signed with
	// the community's own credentials — exactly how the pre-flip write path put it
	// there, `author` field and all.
	oldRkey := testkit.TID()
	title := "legacy " + testkit.UniqueID(t)
	seeded := communityAcct.PutRecord(t, postCollection, oldRkey, map[string]any{
		"$type":     postCollection,
		"community": communityAcct.DID,
		"author":    authorAcct.DID,
		"title":     title,
		"content":   "words the author is accountable for",
		"createdAt": "2026-01-02T03:04:05Z",
	})

	legacy := posts.LegacyPost{
		URI:          seeded.URI,
		CID:          seeded.CID,
		CommunityDID: communityAcct.DID,
		AuthorDID:    authorAcct.DID,
		Record: posts.PostRecord{
			Type:      postCollection,
			Community: communityAcct.DID,
			Author:    authorAcct.DID,
			Title:     strPtr(title),
			Content:   strPtr("words the author is accountable for"),
			CreatedAt: "2026-01-02T03:04:05Z",
		},
		RawRecord: map[string]any{
			"$type":     postCollection,
			"community": communityAcct.DID,
			"author":    authorAcct.DID,
			"title":     title,
			"content":   "words the author is accountable for",
			"createdAt": "2026-01-02T03:04:05Z",
		},
	}

	// Real author-repo credentials for the author, over the real PDS.
	authorFactory := func(_ context.Context, authorDID string, _ *oauth.ClientSessionData) (posts.AuthorRepo, error) {
		require.Equalf(t, authorAcct.DID, authorDID, "the tool asked for a repo other than the post's author")
		generic, err := pds.NewFromAccessToken(pdsServer.URL(), authorAcct.DID, authorAcct.AccessToken, pds.PrivateHostOptions(true)...)
		require.NoError(t, err)
		repo, ok := generic.(posts.AuthorRepo)
		require.True(t, ok, "the PDS client must implement the author-repo write surface")
		return repo, nil
	}

	// Real community-repo credentials, and the DIRECT acceptance writer over them.
	communityGeneric, err := pds.NewFromAccessToken(pdsServer.URL(), communityAcct.DID, communityAcct.AccessToken, pds.PrivateHostOptions(true)...)
	require.NoError(t, err)
	communityRepo, ok := communityGeneric.(posts.CommunityRepo)
	require.True(t, ok, "the PDS client must implement the community-repo write surface")
	writer := posts.NewCommunityRecordWriter(
		func(_ context.Context, _ string) (posts.CommunityRepo, error) { return communityRepo, nil },
		time.Now,
	)

	source := &realLegacySource{community: communityGeneric, staged: []posts.LegacyPost{legacy}}
	ledger := postgres.NewRematerializeLedger(testkit.DB(t))
	communityRepos := func(_ context.Context, _ string) (posts.CommunityRepo, error) { return communityRepo, nil }
	tool := &posts.Rematerializer{Source: source, Ledger: ledger, AuthorRepos: authorFactory, Acceptances: writer, CommunityRepos: communityRepos}

	// ---- run -----------------------------------------------------------------
	state, err := tool.RematerializeOne(ctx, legacy)
	require.NoError(t, err)
	require.Equal(t, posts.RematerializeDone, state)

	wantRkey := posts.RematerializeRkey(legacy.URI)
	newURI := "at://" + authorAcct.DID + "/" + postv2Collection + "/" + wantRkey

	// The postv2 stands in the AUTHOR's repo, with NO author field (authorship is
	// the repo now), carrying the original community and title.
	postV2 := authorAcct.GetRecord(t, postv2Collection, wantRkey)
	newCID := postV2.CID
	assert.Equalf(t, communityAcct.DID, postV2.Value["community"], "the postv2 must keep its original community")
	assert.Equal(t, title, postV2.Value["title"])
	_, hasAuthor := postV2.Value["author"]
	assert.Falsef(t, hasAuthor, "the re-materialized postv2 must NOT carry an author field — the repo signature is the authorship anchor")

	// The community's acceptance stands, pinning the NEW postv2 URI and the CID the
	// PDS minted for it.
	acceptance := communityAcct.GetRecord(t, posts.AcceptanceCollection, posts.SubjectRkey(newURI))
	assertSubject(t, acceptance, newURI, newCID)

	// The old community.post is REALLY gone.
	require.Truef(t, testkit.IsNotFound(getRecordErr(ctx, communityAcct, postCollection, oldRkey)),
		"the old community.post must be deleted from the community repo once the postv2 and acceptance are verified")

	row, found, err := ledger.Get(ctx, legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, posts.RematerializeDone, row.State)
	assert.Equal(t, newURI, row.NewURI)
	assert.Equal(t, newCID, row.NewCID)

	// ---- re-run: pure no-op --------------------------------------------------
	stateAgain, err := tool.RematerializeOne(ctx, legacy)
	require.NoError(t, err)
	assert.Equal(t, posts.RematerializeDone, stateAgain)

	postV2Again := authorAcct.GetRecord(t, postv2Collection, wantRkey)
	assert.Equalf(t, newCID, postV2Again.CID,
		"a re-run minted a new postv2 CID; the deterministic rkey must converge on the first record via createAuthorRecord's read-back")

	acceptanceAgain := communityAcct.GetRecord(t, posts.AcceptanceCollection, posts.SubjectRkey(newURI))
	assertSubject(t, acceptanceAgain, newURI, newCID)
	assert.Equalf(t, acceptance.CID, acceptanceAgain.CID,
		"a re-run rewrote the acceptance record; WriteAcceptance must SKIP when the standing record already pins the target CID")

	require.Truef(t, testkit.IsNotFound(getRecordErr(ctx, communityAcct, postCollection, oldRkey)),
		"the old community.post must stay gone across a re-run")
}

// P4 — embed blob BYTES must be copied to the author's repo, not just referenced
// (whole-branch review, P4).
//
// A blob ref names a CID and not a repository, so a reader resolves it against
// the repo it believes owns the record — after the flip, the AUTHOR's. The legacy
// post's images live in the COMMUNITY's blob store; if the tool copies only the
// embed REFERENCE and then deletes the legacy record, the postv2's media resolves
// against an author repo where the bytes never landed (broken image), and the
// community's now-unreferenced blob becomes garbage-collectable — the only copy,
// gone. The bytes must be uploaded into the author's repo, and the old record must
// not be deleted until they are verified present.
func TestRematerialize_OuterContract_CopiesEmbedBlobToAuthorRepo(t *testing.T) {
	t.Parallel()

	pdsServer := testkit.NewPDS(t)
	communityAcct := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("rbmc"))
	authorAcct := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("rbma"))
	ctx := context.Background()

	// A real blob uploaded into the COMMUNITY's blob store, referenced by a legacy
	// post's images embed — exactly where a pre-flip post's media lives.
	blob := communityAcct.UploadBlob(t, []byte("PNGDATA-re-materialization-blob-bytes"), "image/png")
	embed := map[string]any{
		"$type":  "social.coves.embed.images",
		"images": []any{map[string]any{"image": blob, "alt": "a picture"}},
	}

	oldRkey := testkit.TID()
	title := "legacy with media " + testkit.UniqueID(t)
	seeded := communityAcct.PutRecord(t, postCollection, oldRkey, map[string]any{
		"$type":     postCollection,
		"community": communityAcct.DID,
		"author":    authorAcct.DID,
		"title":     title,
		"embed":     embed,
		"createdAt": "2026-01-02T03:04:05Z",
	})

	legacy := posts.LegacyPost{
		URI:          seeded.URI,
		CID:          seeded.CID,
		CommunityDID: communityAcct.DID,
		AuthorDID:    authorAcct.DID,
		Record: posts.PostRecord{
			Type:      postCollection,
			Community: communityAcct.DID,
			Author:    authorAcct.DID,
			Title:     strPtr(title),
			Embed:     embed,
			CreatedAt: "2026-01-02T03:04:05Z",
		},
		RawRecord: map[string]any{
			"$type":     postCollection,
			"community": communityAcct.DID,
			"author":    authorAcct.DID,
			"title":     title,
			"embed":     embed,
			"createdAt": "2026-01-02T03:04:05Z",
		},
	}

	authorFactory := func(_ context.Context, _ string, _ *oauth.ClientSessionData) (posts.AuthorRepo, error) {
		generic, err := pds.NewFromAccessToken(pdsServer.URL(), authorAcct.DID, authorAcct.AccessToken, pds.PrivateHostOptions(true)...)
		require.NoError(t, err)
		repo, ok := generic.(posts.AuthorRepo)
		require.True(t, ok)
		return repo, nil
	}
	communityGeneric, err := pds.NewFromAccessToken(pdsServer.URL(), communityAcct.DID, communityAcct.AccessToken, pds.PrivateHostOptions(true)...)
	require.NoError(t, err)
	communityRepo, ok := communityGeneric.(posts.CommunityRepo)
	require.True(t, ok)
	writer := posts.NewCommunityRecordWriter(
		func(_ context.Context, _ string) (posts.CommunityRepo, error) { return communityRepo, nil }, time.Now)

	source := &realLegacySource{community: communityGeneric, staged: []posts.LegacyPost{legacy}}
	ledger := postgres.NewRematerializeLedger(testkit.DB(t))
	communityRepos := func(_ context.Context, _ string) (posts.CommunityRepo, error) { return communityRepo, nil }

	// THE HATCH IS OPEN, AND THIS IS THE ONLY TEST IN THE TREE THAT NEEDS IT.
	//
	// Rematerializer.blobClient() falls back to the GUARDED default, which refuses
	// private, loopback and link-local addresses — and the CI stack's PDS is on
	// loopback, which is exactly what it refuses. Every other Rematerializer test
	// stages records carrying no blobs, so the fallback client is constructed and
	// never dialled; this one copies real bytes through it.
	//
	// It is passed as an explicit Blobs client rather than reached by loosening
	// the fallback, for the reason blobs and imageproxy pass PrivateHostOptions at
	// their construction: the decision to dial a private address is made ONCE, in
	// the open, by whoever built the thing — and the guarded spelling stays the
	// one a caller gets by omission. Nothing about the state machine's own
	// behaviour changes; this replaces the client, not the path.
	tool := &posts.Rematerializer{
		Source: source, Ledger: ledger, AuthorRepos: authorFactory, Acceptances: writer,
		CommunityRepos: communityRepos,
		Blobs:          posts.DefaultRematerializeBlobClient(true),
	}

	_, err = tool.RematerializeOne(ctx, legacy)
	require.NoError(t, err)

	// The blob's BYTES must now be fetchable from the AUTHOR's repo. sync.getBlob
	// serves a blob only from a repo that actually holds it, so a 200 here proves
	// the bytes were copied — a 404 (the current behaviour) proves only the
	// reference was, and the post's media is broken the moment the old record goes.
	require.Equalf(t, 200, getBlobStatus(t, pdsServer.URL(), authorAcct.DID, blob.CID()),
		"the embed blob %s was not copied into the author's repo: the postv2 references a CID whose bytes live only in the community's blob store, "+
			"which is now garbage-collectable and about to be the only copy lost", blob.CID())
}

// getBlobStatus fetches a blob from a repo via com.atproto.sync.getBlob and
// returns the HTTP status — 200 if the repo holds the blob, 404 if it does not.
func getBlobStatus(t *testing.T, pdsURL, did, cid string) int {
	t.Helper()
	req := pdsURL + "/xrpc/com.atproto.sync.getBlob?did=" + url.QueryEscape(did) + "&cid=" + url.QueryEscape(cid)
	resp, err := http.Get(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
