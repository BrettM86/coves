//go:build integration

package jetstream

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ingesting the two records a COMMUNITY writes about a post:
// social.coves.community.acceptance and social.coves.community.removal
// (docs/PRD_AUTHOR_OWNED_POSTS.md §5.4, §5.2).
//
// These are the records that decide what a community shows. A post claiming a
// community and lacking an acceptance is never rendered in it (§2), so an
// acceptance event is the moment a post becomes visible and a removal event is
// the moment it stops — which makes two properties non-negotiable here:
//
//   - THE REPO DID IS THE COMMUNITY, and it must be a community this AppView has
//     indexed. Nothing else in the event says which community decided. An
//     acceptance from an arbitrary repo, taken at face value, is a stranger
//     publishing into someone else's feed.
//   - THE PINNED CID IS PART OF THE DECISION. An acceptance is a strongRef, and
//     agreeing to at://x/postv2/y is not agreeing to whatever that URI holds
//     later. A consumer that dropped the CID comparison would let an author edit
//     content past moderation after approval.
//
// AND CONVERGENCE IS NOT FREE. An acceptance can arrive for a post this AppView
// has never seen, and redrive alone cannot fix that: bounded retries cannot
// manufacture a post event that a relay-coverage gap will never deliver. §5.4's
// direct fetch is the mechanism, and the second half of this file is about the
// ways that fetch must refuse rather than the way it succeeds — it is an
// outbound request whose destination is chosen by a stranger's record.

const (
	accPrefix    = "did:plc:acc"
	accCommunity = accPrefix + "community"
	accAuthor    = accPrefix + "author"
	accOutsider  = accPrefix + "outsider"
)

// accFixture is a consumer wired for community-repo events, plus the stores the
// assertions read.
type accFixture struct {
	consumer   *PostEventConsumer
	admissions posts.AdmissionRepository
	db         *sql.DB
}

func newAccFixture(t *testing.T, db *sql.DB, opts ...PostEventConsumerOption) accFixture {
	t.Helper()

	insertBridgedUser(t, db, accAuthor, "accauthor.test")
	insertBridgedCommunity(t, db, accCommunity, "acccommunity.test", accAuthor)

	us := newMockUserService()
	us.users[accAuthor] = &users.User{DID: accAuthor, Handle: "accauthor.test"}

	admissions := postgres.NewAdmissionRepository(db)
	wired := append([]PostEventConsumerOption{
		WithAdmissions(admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
	}, opts...)

	return accFixture{
		consumer: NewPostEventConsumer(
			postgres.NewPostRepository(db),
			postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
			us,
			db,
			wired...,
		),
		admissions: admissions,
		db:         db,
	}
}

// accPostURI is the author-repo URI an acceptance points at.
func accPostURI(rkey string) string { return "at://" + accAuthor + "/" + PostV2Collection + "/" + rkey }

// indexPV2 puts a post in the index the ordinary way — through the consumer —
// so these tests start from a state the pipeline actually produces.
func (f accFixture) indexPV2(t *testing.T, rkey, cid string, timeUS int64) string {
	t.Helper()
	uri := accPostURI(rkey)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), pv2Event(
		accAuthor, "create", rkey, testkit.TID(), cid, timeUS,
		pv2Record(accCommunity, "a post awaiting a decision", "body"),
	)), "fixture: indexing the subject post")
	return uri
}

// acceptanceEvent builds a community-repo acceptance commit.
//
// The rkey is derived from the subject rather than invented, because that is
// what the writers do (§3.2): one post has exactly one acceptance rkey per
// community, forever, which is what makes three independent writers converge on
// putRecord of the same record instead of allocating duplicate TIDs.
func acceptanceEvent(communityDID, postURI, pinnedCID, rev string, timeUS int64) *JetstreamEvent {
	return revCommitEvent(communityDID, posts.AcceptanceCollection, "create",
		posts.SubjectRkey(postURI), rev, "bafyreiacceptancerecord", timeUS,
		map[string]interface{}{
			"$type":     posts.AcceptanceCollection,
			"subject":   map[string]interface{}{"uri": postURI, "cid": pinnedCID},
			"createdAt": "2026-03-01T00:00:00Z",
		})
}

// removalEvent builds a community-repo removal commit.
func removalEvent(communityDID, postURI, pinnedCID, code, rev string, timeUS int64) *JetstreamEvent {
	return revCommitEvent(communityDID, posts.RemovalCollection, "create",
		posts.SubjectRkey(postURI), rev, "bafyreiremovalrecord", timeUS,
		map[string]interface{}{
			"$type":     posts.RemovalCollection,
			"subject":   map[string]interface{}{"uri": postURI, "cid": pinnedCID},
			"code":      code,
			"createdAt": "2026-03-01T00:00:00Z",
		})
}

func TestAcceptanceConsumer_MatchingCID_AcceptsAndStampsTheWatermark(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newAccFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiaccmatch"
	uri := f.indexPV2(t, "accmatch", cid, base)

	rev := testkit.TID()
	require.NoError(t, f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, cid, rev, base+1_000_000)))

	admission, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)

	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status,
		"an acceptance pinning the CID the AppView has indexed is the community agreeing to exactly this content")
	assertNullableStringPV2(t, cid, admission.AcceptedCID, "accepted_cid must be the CID the acceptance pinned")
	assertNullableStringPV2(t, posts.SubjectRkey(uri), admission.AcceptanceRkey,
		"the acceptance rkey is deterministic from the subject; storing a different one breaks the one-record-per-subject convergence the writers rely on")

	// The §5.2 tuple, and specifically its second half. The rank is derived from
	// the OPERATION by the repository, never taken from the wire: a put ranks
	// above a delete so that the removal commit {acceptance-delete, removal-put}
	// converges on removed and the restore commit {removal-delete,
	// acceptance-put} converges on accepted, whichever half the consumer sees
	// first. A caller-supplied rank would let one mislabelled event reorder a
	// commit permanently.
	require.NotNil(t, admission.LastCommunityEvent, "a community event must stamp the subject-scoped watermark")
	assert.Equal(t, rev, admission.LastCommunityEvent.Rev,
		"the watermark rev must be the COMMIT's rev — it is the only clock that orders acceptance against removal, since they are different record URIs about the same subject")
	assert.Equal(t, posts.CommunityOpPut, admission.LastCommunityEvent.OpRank,
		"a record write ranks as a put; the rank is the operation's kind, derived repo-side")
}

func TestAcceptanceConsumer_ReplayIsANoOpAndLeavesTheRowUntouched(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newAccFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiaccreplay"
	uri := f.indexPV2(t, "accreplay", cid, base)

	// The redelivery is guaranteed, not hypothetical: the connector rewinds its
	// cursor after every reconnect, the AppView consumes overlapping feeds, and
	// the dead-letter redriver replays. This exact commit WILL arrive twice.
	event := acceptanceEvent(accCommunity, uri, cid, testkit.TID(), base+1_000_000)
	require.NoError(t, f.consumer.HandleEvent(ctx, event))

	before := readAdmissionRow(t, db, accCommunity, uri)

	require.NoError(t, f.consumer.HandleEvent(ctx, event),
		"an equal watermark is a replay, and a replay is a no-op — not an error the connector logs as a failure")

	after := readAdmissionRow(t, db, accCommunity, uri)
	assert.Equal(t, before, after,
		"a replayed acceptance must leave the row byte-identical. Re-stamping decision_at or updated_at would make the moderation audit trail a function of how many feeds happened to carry the event")
}

func TestAcceptanceConsumer_FromANonCommunityRepo_IsRefusedAndRedrivable(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newAccFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiaccoutsider"
	uri := f.indexPV2(t, "accoutsider", cid, base)

	// accOutsider is a DID with no communities row. Nothing in the record says
	// which community decided — the repo IS the claim — so accepting this would
	// let anyone with a PDS publish into any feed by writing a record that names
	// someone else's post.
	err := f.consumer.HandleEvent(ctx, acceptanceEvent(accOutsider, uri, cid, testkit.TID(), base+1_000_000))
	require.Error(t, err, "an acceptance from a repo that is not an indexed community must not be applied")

	// Transient, not permanent, and the reason is delivery order rather than
	// leniency: BigSky preserves order within a repo, not across repos, so a
	// community's first acceptance can genuinely outrun its own profile event.
	// Marking this permanent would spend the redrive budget that resolves the
	// race and discard a legitimate decision.
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"an unindexed community repo is an ordering failure; the redrive resolves it once the community profile arrives")

	assert.Zero(t, countRows(t, db,
		`SELECT count(*) FROM community_post_admissions WHERE post_uri = $1 AND community_did = $2`, uri, accOutsider),
		"the refused acceptance must not have opened an admission row for the outsider repo")
}

func TestRemovalConsumer_RemovesWithTheCodeItCarries(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newAccFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiaccremove"
	uri := f.indexPV2(t, "accremove", cid, base)

	revs := increasingTIDs(t, 2)
	require.NoError(t, f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, cid, revs[0], base+1_000_000)))
	require.NoError(t, f.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, uri, cid, string(posts.DecisionRuleViolation), revs[1], base+2_000_000)))

	admission, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)

	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status)
	assertNullableStringPV2(t, string(posts.DecisionRuleViolation), admission.DecisionCode,
		"the removal's code is what #removedPost renders to the author; dropping it turns a moderation act into an unexplained disappearance")
	assert.NotNil(t, admission.DecisionAt, "a removal must record when it happened")
}

func TestRemovalConsumer_PreemptiveRemovalCreatesTheRow(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := newAccFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiaccpreempt"
	uri := f.indexPV2(t, "accpreempt", cid, base)

	// A removal with no prior acceptance is VALID (§5.4). A community that has
	// decided in advance about a post — an author it is about to ban, content it
	// has already seen elsewhere — must be able to say so, and a consumer that
	// required an acceptance first would drop exactly the decisions a community
	// most wants to make early.
	require.NoError(t, f.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, uri, cid, string(posts.DecisionSpam), testkit.TID(), base+1_000_000)))

	admission, err := f.admissions.Get(ctx, accCommunity, uri)
	require.NoErrorf(t, err, "a pre-emptive removal must create the admission row it decides")
	require.NotNil(t, admission)
	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status)
	assertNullableStringPV2(t, string(posts.DecisionSpam), admission.DecisionCode, "decision_code")
}

// ---------------------------------------------------------------------------
// §5.4 direct fetch: acceptance before post
// ---------------------------------------------------------------------------

// fakeAuthorPDS is an httptest server answering com.atproto.sync.getRecord for
// the author's postv2 record, standing in for the PDS a DID document points at.
//
// It asserts the request shape as it serves, because the fetch is the one place
// the AppView reads a record without the firehose: a fetch aimed at the wrong
// repo or collection would return someone else's record, and the verification
// downstream would happily confirm it.
//
// sync.getRecord, and its parameter is `did` rather than `repo`. Both changed
// together and neither is cosmetic: repo.getRecord answers with a JSON envelope
// whose `cid` is a claim by the server being interrogated, while sync.getRecord
// answers with the repo's own blocks, which is what makes the CID something the
// AppView can RECOMPUTE instead of read off a label. A helper still asserting
// the old endpoint would keep passing against a fetcher that had regressed to
// trusting the envelope.
func fakeAuthorPDS(t *testing.T, expectRepo string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/xrpc/com.atproto.sync.getRecord", r.URL.Path)
		assert.Equal(t, expectRepo, r.URL.Query().Get("did"))
		assert.Equal(t, PostV2Collection, r.URL.Query().Get("collection"))
		handler(w, r)
	}))
}

// serveRecord writes a getRecord response body.
func serveRecord(t *testing.T, w http.ResponseWriter, uri, cid string, value map[string]interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
		"uri": uri, "cid": cid, "value": value,
	}))
}

// newFetcherAt builds a DirectPostFetcher pointed at srv, with the SSRF guard
// stood down.
//
// Standing it down is REQUIRED and is the whole reason the seam exists:
// httptest listens on loopback, which the guard blocks by design, so a fetcher
// that could not be relaxed could not be tested against a fake PDS at all. The
// guard's default is asserted separately, and behaviourally, in
// TestDirectPostFetcher_RefusesAPrivateHostByDefault.
func newFetcherAt(t *testing.T, authorDID, pdsURL string) *DirectPostFetcher {
	t.Helper()
	fetcher := NewDirectPostFetcher(&mockIdentityResolverForUser{
		identities: map[string]*identity.Identity{
			authorDID: {DID: authorDID, Handle: "accauthor.test", PDSURL: pdsURL},
		},
	})
	fetcher.allowPrivateHosts = true
	return fetcher
}

// realRepoFixture is the acceptance consumer pointed at a REAL author repo on
// the test PDS.
//
// The three arcs below all turn on what the fetch VERIFIES, and verification is
// recomputation from the repo's own blocks (§5.4) — so a hand-served response
// cannot exercise them at all: it would either be rejected as malformed, which
// proves nothing about the CID, or require this file to fabricate a CAR and
// thereby encode its own guesses about how the verification walks one.
//
// The DirectFetch trio in direct_fetch_verification_test.go proves the
// COMPONENT. These prove the WIRING: that the consumer reaches for it on an
// unindexed subject, and that what it does with each answer — index and accept,
// or refuse permanently — is what the admission row ends up saying.
type realRepoFixture struct {
	accFixture

	pds    *testkit.PDS
	author *testkit.Account
}

func newRealRepoFixture(t *testing.T, db *sql.DB) *realRepoFixture {
	t.Helper()

	pdsServer := testkit.NewPDS(t)
	author := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("rr"))

	insertBridgedUser(t, db, accAuthor, "realowner.test")
	insertBridgedCommunity(t, db, accCommunity, "realcommunity.test", accAuthor)

	admissions := postgres.NewAdmissionRepository(db)
	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		newMockUserService(), db,
		WithAdmissions(admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostRecordFetcher(NewDirectPostFetcher(pinnedResolver(author.DID, pdsServer.URL()),
			PrivatePostFetcherOptions(true)...)),
	)

	return &realRepoFixture{
		accFixture: accFixture{consumer: consumer, admissions: admissions, db: db},
		pds:        pdsServer,
		author:     author,
	}
}

// publish writes a real postv2 record into the author's repo and returns it, so
// the CID an acceptance pins is one the PDS minted from bytes it stored.
func (f *realRepoFixture) publish(t *testing.T, communityDID, title string) testkit.Record {
	t.Helper()
	return f.author.CreateRecord(t, PostV2Collection, map[string]any{
		"$type":     PostV2Collection,
		"community": communityDID,
		"title":     title,
		"content":   "a body only the repo can vouch for",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
}

func TestAcceptanceConsumer_UnindexedPost_IsFetchedDirectlyAndAccepted(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newRealRepoFixture(t, db)

	// The post exists in its author's repo and the AppView has never seen it —
	// no create event arrived, and none ever will if the relay does not crawl
	// that PDS. This is the case §5.4 says redrive cannot solve: bounded retries
	// cannot manufacture an event nobody is going to send. Without the fetch,
	// convergence is a bet on full relay coverage.
	record := f.publish(t, accCommunity, "never delivered by any relay")

	require.NoError(t, f.consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, record.URI, record.CID, testkit.TID(), time.Now().UnixMicro())))

	authorDID, communityDID, storedCID, _, _ := readPV2Post(t, db, record.URI)
	assert.Equal(t, f.author.DID, authorDID, "the fetched post is attributed to the repo it was read from")
	assert.Equal(t, accCommunity, communityDID)
	assert.Equal(t, record.CID, storedCID, "the indexed CID must be the one the repo minted")

	admission, err := f.admissions.Get(ctx, accCommunity, record.URI)
	require.NoError(t, err)
	require.NotNil(t, admission)
	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status,
		"the fetch exists so the acceptance can be APPLIED; indexing the post and leaving the decision pending would "+
			"solve half the problem and leave the post invisible")
}

func TestAcceptanceConsumer_FetchedCIDMismatch_IsPermanentlyRefused(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newRealRepoFixture(t, db)

	subject := f.publish(t, accCommunity, "the post the acceptance is about")
	other := f.publish(t, accCommunity, "a different post in the same repo")
	require.NotEqual(t, subject.CID, other.CID, "fixture: the two records must have distinct CIDs")

	// BOTH CIDs ARE REAL, and that is what makes this the sharp version. The
	// acceptance names one record and pins another's CID — a perfectly
	// well-formed strongRef that simply does not describe its subject. Nothing
	// about the response is malformed, so the refusal can only come from the
	// verification actually comparing what it recomputed against what was
	// pinned.
	err := f.consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, subject.URI, other.CID, testkit.TID(), time.Now().UnixMicro()))

	require.Error(t, err, "an acceptance pinning a CID the subject does not have must never be applied")
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"the pinned version is not what that URI holds, and no retry changes which bytes are in the repo; re-fetching "+
			"the same mismatch ten times is pure noise")

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, subject.URI),
		"the unverified record must not be indexed")
}

func TestAcceptanceConsumer_FetchedRecordNamingAnotherCommunity_IsPermanentlyRefused(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	f := newRealRepoFixture(t, db)

	const elsewhere = accPrefix + "othercommunity"
	insertBridgedCommunity(t, db, elsewhere, "otherrealcommunity.test", accAuthor)

	// A genuine record, correctly signed, whose community field names someone
	// else. The CID verifies; the CLAIM does not. Cross-community acceptance is
	// the privileged fork/import flow and §10.2 is explicit that it is
	// deliberately not built — until it is, a community accepting a post that
	// names another is a community pulling someone else's content into its feed
	// on its own say-so.
	record := f.publish(t, elsewhere, "submitted to a different community")

	err := f.consumer.HandleEvent(ctx,
		acceptanceEvent(accCommunity, record.URI, record.CID, testkit.TID(), time.Now().UnixMicro()))

	require.Error(t, err, "a community may not accept a post whose record names a different community")
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"the record's community field is immutable across updates (§3.1), so this can never become valid")

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, record.URI),
		"the refused record must not be indexed")
}

func TestDirectPostFetcher_RefusesAnOversizedBody(t *testing.T) {
	t.Parallel()

	uri := accPostURI("accoversized")

	// A post record has a lexicon-bounded size; a PDS streaming megabytes is
	// either broken or hostile. The cap has to be enforced by the FETCHER
	// because the DID document that chose this host is attacker-controlled — any
	// stranger who writes an acceptance record picks where this request goes,
	// and an unbounded read there is a memory-exhaustion primitive handed to the
	// public.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uri":"` + uri + `","cid":"bafyreiaccbig","value":{"$type":"` +
			PostV2Collection + `","community":"` + accCommunity + `","createdAt":"2026-03-01T00:00:00Z","content":"`))
		chunk := strings.Repeat("A", 64*1024)
		for i := 0; i < 64; i++ { // 4 MiB of content
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}}`))
	}))
	defer srv.Close()

	fetched, err := newFetcherAt(t, accAuthor, srv.URL).FetchPost(context.Background(), uri)
	require.Error(t, err, "a response past the size cap must be an error, not a truncated record parsed as if it were whole")
	assert.Nil(t, fetched)
}

func TestDirectPostFetcher_RefusesAPrivateHostByDefault(t *testing.T) {
	t.Parallel()

	uri := accPostURI("accssrf")

	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		serveRecord(t, w, uri, "bafyreiaccssrf", pv2Record(accCommunity, "should never be read", "body"))
	}))
	defer srv.Close()

	// The default constructor, with nothing stood down. httptest listens on
	// loopback, which is precisely the class of address the guard blocks.
	//
	// This is asserted behaviourally rather than by reading the flag because the
	// flag is not the protection — a fetcher that stored allowPrivateHosts and
	// then built its client from a hardcoded false (or true) would pass a field
	// check and fail this.
	fetcher := NewDirectPostFetcher(&mockIdentityResolverForUser{
		identities: map[string]*identity.Identity{
			accAuthor: {DID: accAuthor, Handle: "accauthor.test", PDSURL: srv.URL},
		},
	})
	require.Falsef(t, fetcher.allowPrivateHosts,
		"NewDirectPostFetcher must default to SSRF protection ON: the PDS this dials is named by a DID document anyone can publish")

	fetched, err := fetcher.FetchPost(context.Background(), uri)
	require.Error(t, err,
		"a PDS resolving to a private address must be refused: an acceptance record is public and unauthenticated input, so this fetch is a request forger pointed at whatever the AppView can reach on its own network")
	assert.Nil(t, fetched)
	assert.Falsef(t, reached, "the guard must refuse before the request is made, not after the server has already answered")
}

// ---------------------------------------------------------------------------
// §5.4 direct fetch: classifying what the author's PDS answered
// ---------------------------------------------------------------------------

// How a failed fetch is CLASSIFIED, which decides what the connector does next.
//
// The consumer returns an error and the connector reads its shape: an error
// wrapping ErrPermanentEvent is dead-lettered with the redrive budget already
// spent, and anything else is retried inline (~4.2 seconds of blocking, per
// event) and then redriven ten times. So the classification is not a label — it
// is the difference between one forensic row and forty pointless refetches
// against somebody else's PDS while this consumer stops indexing.
//
// A non-200 from getRecord is currently one undifferentiated failure, and the
// three cases below need three different answers:
//
//   - A GENUINE XRPC RecordNotFound is a definite fact about the repo: the PDS
//     was reached, it understood the question, and the record is not there. No
//     retry changes that.
//   - A BARE 404 — no XRPC error envelope — usually means the request never
//     reached a PDS at all: a stale pds_url pointing at a reverse proxy or a
//     generic web server, which answers 404 for everything. Treating that as
//     proof the record does not exist would permanently discard a post over a
//     misconfigured hostname. users.FetchProfileRecord already draws exactly
//     this distinction, and for exactly this reason.
//   - A 5xx is the PDS saying it is having a bad time. Definitionally transient.
//
// WHAT IS ASSERTED HERE, AND WHAT IS NOT. These drive HandleEvent directly, so
// no dead-letter row is written and no redrive runs — the consumer never touches
// that table; the connector does. What these pin is the input the connector
// switches on (the ErrPermanentEvent wrapping) plus the request count, which
// proves the consumer and fetcher hold no retry loop of their own. The
// connector's half — that a permanent error is dead-lettered exhausted and a
// transient one is retried — is TestConnector_DeadLettersAfterRetryExhaustion.

// countingAuthorPDS is fakeAuthorPDS with a request tally, so a test can prove
// one event produced exactly one outbound fetch.
func countingAuthorPDS(t *testing.T, expectRepo string, requests *int, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return fakeAuthorPDS(t, expectRepo, func(w http.ResponseWriter, r *http.Request) {
		*requests++
		handler(w, r)
	})
}

func TestAcceptanceConsumer_GenuineRecordNotFound_IsPermanentlyRefusedAfterOneFetch(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	base := time.Now().UnixMicro()
	uri := accPostURI("accgone")

	var requests int
	srv := countingAuthorPDS(t, accAuthor, &requests, func(w http.ResponseWriter, r *http.Request) {
		// The reference PDS' answer for a repo that exists and a record that
		// does not: 400 with an XRPC error envelope naming RecordNotFound.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"RecordNotFound","message":"Could not locate record: ` + uri + `"}`))
	})
	defer srv.Close()

	f := newAccFixture(t, db, WithPostRecordFetcher(newFetcherAt(t, accAuthor, srv.URL)))

	err := f.consumer.HandleEvent(context.Background(),
		acceptanceEvent(accCommunity, uri, "bafyreiaccgonepinned", testkit.TID(), base))

	require.Error(t, err, "an acceptance whose subject the PDS says does not exist cannot be applied")
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"a genuine RecordNotFound is a definite fact about the repo — the PDS was reached and answered — so no retry can change it. "+
			"Left transient, every one of these costs ~4.2s of inline blocking plus ten redrives, and a community can mint them at will "+
			"by writing acceptances for URIs nobody wrote")

	assert.Equalf(t, 1, requests,
		"one event produced %d fetches: the consumer or the fetcher is retrying internally. Retries belong to the connector, "+
			"which can classify and budget them; a loop in here is invisible to it and unbounded", requests)

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
		"nothing may be indexed for a subject the PDS says does not exist")
}

func TestAcceptanceConsumer_BareNotFound_IsUnresolved(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	base := time.Now().UnixMicro()
	uri := accPostURI("accbare404")

	var requests int
	srv := countingAuthorPDS(t, accAuthor, &requests, func(w http.ResponseWriter, r *http.Request) {
		// No XRPC envelope. This is what a reverse proxy, a load balancer or a
		// generic web server answers — which is what a stale pds_url in a DID
		// document points at.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html><body>404 Not Found</body></html>"))
	})
	defer srv.Close()

	f := newAccFixture(t, db, WithPostRecordFetcher(newFetcherAt(t, accAuthor, srv.URL)))

	err := f.consumer.HandleEvent(context.Background(),
		acceptanceEvent(accCommunity, uri, "bafyreiaccbarepinned", testkit.TID(), base))

	require.Error(t, err, "a fetch that did not produce a record cannot apply the acceptance")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"a bare 404 carries no XRPC error envelope, which means the request most likely never reached a PDS at all — a stale pds_url "+
			"pointing at a proxy. Reading it as proof the record does not exist permanently discards a real post over a misconfigured "+
			"hostname; users.FetchProfileRecord draws the same distinction for the same reason")
	assert.ErrorIs(t, err, ErrUnresolvedReference,
		"a remote fetch failure must skip the connector's in-line retry sleeps and converge on the redriver")

	assert.Equal(t, 1, requests, "one event, one fetch — the connector owns retries")
}

func TestAcceptanceConsumer_PDSServerError_IsUnresolved(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	base := time.Now().UnixMicro()
	uri := accPostURI("accpds5xx")

	var requests int
	srv := countingAuthorPDS(t, accAuthor, &requests, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"InternalServerError","message":"upstream unavailable"}`))
	})
	defer srv.Close()

	f := newAccFixture(t, db, WithPostRecordFetcher(newFetcherAt(t, accAuthor, srv.URL)))

	err := f.consumer.HandleEvent(context.Background(),
		acceptanceEvent(accCommunity, uri, "bafyreiacc5xxpinned", testkit.TID(), base))

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"a 5xx is the author's PDS saying it is unwell, which is the definition of transient; discarding the acceptance permanently "+
			"would lose a post because somebody else's server restarted")
	assert.ErrorIs(t, err, ErrUnresolvedReference,
		"an attacker-controlled PDS failure must not buy in-line retries on the posts lane")

	assert.Equal(t, 1, requests, "one event, one fetch — the connector owns retries")
}

// readAdmissionRow returns every mutable column of one admission row as a
// comparable value, so "the row did not change" can be asserted as a whole
// rather than field by field — a new column added later is covered without
// anyone remembering to extend an assertion.
func readAdmissionRow(t *testing.T, db *sql.DB, communityDID, postURI string) []interface{} {
	t.Helper()

	var (
		status                                  string
		acceptanceURI, acceptanceRkey, accepted *string
		decisionCode, evaluatedCID, rev         *string
		decisionAt                              *time.Time
		opRank                                  *int16
		redrivable                              bool
		createdAt, updatedAt                    time.Time
	)
	require.NoError(t, db.QueryRow(`
		SELECT status, acceptance_uri, acceptance_rkey, accepted_cid, decision_code, decision_at,
		       evaluated_cid, redrivable, last_community_rev, last_community_op_rank, created_at, updated_at
		FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2
	`, communityDID, postURI).Scan(
		&status, &acceptanceURI, &acceptanceRkey, &accepted, &decisionCode, &decisionAt,
		&evaluatedCID, &redrivable, &rev, &opRank, &createdAt, &updatedAt,
	))

	deref := func(p *string) interface{} {
		if p == nil {
			return nil
		}
		return *p
	}
	var decidedAt interface{}
	if decisionAt != nil {
		decidedAt = decisionAt.UTC()
	}
	var rank interface{}
	if opRank != nil {
		rank = *opRank
	}
	return []interface{}{
		status, deref(acceptanceURI), deref(acceptanceRkey), deref(accepted), deref(decisionCode),
		decidedAt, deref(evaluatedCID), redrivable, deref(rev), rank, createdAt.UTC(), updatedAt.UTC(),
	}
}
