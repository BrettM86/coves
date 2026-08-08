//go:build integration

package jetstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// How the §5.4 direct fetch decides the bytes it got are the bytes the
// acceptance pinned.
//
// # THE ENVELOPE IS NOT EVIDENCE
//
// com.atproto.repo.getRecord answers with JSON: {"uri": ..., "cid": ..., "value":
// {...}}. The `cid` in it is a CLAIM BY THE SERVER, and the server is the
// author's PDS — chosen by a DID document, reached because a stranger wrote an
// acceptance record naming that subject. Comparing the pinned CID against that
// field asks the attacker whether the attacker is lying.
//
// The consequence is the worst one available in this design. The AppView indexes
// whatever `value` contains, under a community's SIGNED acceptance of a CID that
// content does not have. The community attested to one thing; every reader is
// shown another; and the attestation is what the whole trust model rests on.
//
// com.atproto.sync.getRecord answers with a CAR instead — the actual repo blocks
// — so the CID can be RECOMPUTED from the bytes rather than read off a label. A
// server cannot lie about a hash of what it just sent.
//
// # WHY THE POSITIVE CASE NEEDS A REAL REPO
//
// The negatives below are servable by hand. The positive is not: a CAR carries a
// commit root and the MST blocks proving the record's membership, and a fixture
// built here would encode this file's guesses about how the verification walks
// them — passing or failing for reasons unrelated to the property. So the
// positive drives the fetch against a record genuinely written to a genuine repo
// on the test PDS, which is why this package now carries a PDS floor
// (harness_test.go).

// pinnedResolver points the fetcher at one PDS for one DID.
func pinnedResolver(did, pdsURL string) identity.Resolver {
	return &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		did: {DID: did, Handle: "verify.test", PDSURL: pdsURL},
	}}
}

func TestDirectFetch_RecomputesTheCIDFromARealRepo(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	pdsServer := testkit.NewPDS(t)

	// A real account, a real record, a real commit. The CID the PDS reports is
	// one it derived from bytes it stored, so a verifier that recomputes and one
	// that trusts the label agree here — which is exactly what makes this the
	// positive control for the negatives below.
	author := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("vf"))
	insertBridgedUser(t, db, accAuthor, "verifyowner.test")
	insertBridgedCommunity(t, db, accCommunity, "verifycommunity.test", accAuthor)

	record := author.CreateRecord(t, PostV2Collection, map[string]any{
		"$type":     PostV2Collection,
		"community": accCommunity,
		"title":     "written into a real repo",
		"content":   "bytes the PDS actually holds",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})

	fetcher := NewDevDirectPostFetcher(pinnedResolver(author.DID, pdsServer.URL()))
	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db),
		newMockUserService(), db,
		WithAdmissions(postgres.NewAdmissionRepository(db)),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostRecordFetcher(fetcher),
	)

	require.NoError(t, consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, record.URI, record.CID, testkit.TID(), time.Now().UnixMicro())),
		"a record whose recomputed CID matches the pin must converge; if this fails while the negatives pass, the "+
			"verification is refusing everything rather than verifying anything")

	_, _, storedCID, _, _ := readPV2Post(t, db, record.URI)
	assert.Equal(t, record.CID, storedCID, "the indexed CID must be the one the repo minted")

	row, err := postgres.NewAdmissionRepository(db).Get(ctx, accCommunity, record.URI)
	require.NoError(t, err)
	assert.Equal(t, "accepted", string(row.Status))
}

func TestDirectFetch_UsesSyncGetRecordNotTheJSONEnvelope(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	uri := accPostURI("verifyendpoint")

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := newAccFixture(t, db, WithPostRecordFetcher(
		NewDevDirectPostFetcher(pinnedResolver(accAuthor, srv.URL))))

	_ = f.consumer.HandleEvent(context.Background(),
		acceptanceEvent(accCommunity, uri, "bafyreiverifyendpoint", testkit.TID(), time.Now().UnixMicro()))

	require.NotEmpty(t, paths, "the fetch must have reached the PDS at all")
	assert.Containsf(t, paths, "/xrpc/com.atproto.sync.getRecord",
		"the fetch asked %v. repo.getRecord returns a server-authored JSON envelope whose `cid` is a claim; "+
			"sync.getRecord returns the repo's own blocks, which is the only answer a hostile PDS cannot fabricate", paths)
	assert.NotContainsf(t, paths, "/xrpc/com.atproto.repo.getRecord",
		"the fetch still used repo.getRecord (%v); as long as it does, the CID check compares the pin against a number "+
			"the same server chose", paths)
}

func TestDirectFetch_RefusesAPDSThatLiesAboutTheCID(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	uri := accPostURI("verifyliar")
	const pinned = "bafyreiverifypinnedversion"

	// THE ATTACK, in its simplest form: the server echoes the pinned CID back
	// and serves whatever content it likes underneath. Against an envelope-
	// trusting verifier this succeeds completely and silently — the post indexes,
	// the admission goes to accepted, and the community's signed acceptance now
	// covers content it never saw.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		serveRecord(t, w, uri, pinned,
			pv2Record(accCommunity, "content the community never evaluated", "substituted after acceptance"))
	}))
	defer srv.Close()

	f := newAccFixture(t, db, WithPostRecordFetcher(
		NewDevDirectPostFetcher(pinnedResolver(accAuthor, srv.URL))))

	err := f.consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, uri, pinned, testkit.TID(), time.Now().UnixMicro()))

	require.Error(t, err,
		"a PDS claiming the pinned CID over arbitrary content was believed. Nothing about that response is verifiable: "+
			"the CID is a field the same server wrote, so trusting it lets the author's host substitute any content under "+
			"the community's signed attestation")
	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"the substituted content must not be indexed")
	assert.Zero(t, countRows(t, db,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1 AND status = 'accepted'`, uri),
		"and no acceptance may be recorded for it")
}
