//go:build integration

package posts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole journey of an author-owned post through the write path, against a
// real PDS and the real admissions table (docs/PRD_AUTHOR_OWNED_POSTS.md §4.2).
//
// service_writeforward_test.go proves WHERE the record lands. This file proves
// the four things that only become true once it lands there, and each one is a
// claim the AppView makes to a client that nothing else in the suite can check:
//
//   - THE COMMUNITY'S ANSWER ARRIVES WITH THE RESPONSE, for a community this
//     AppView hosts. §4.2 step 4 promises the UX is identical to the pre-flip
//     one: the client gets URI, CID and an accepted post, without waiting for
//     its own write to come back around the firehose.
//   - A RETRY IS THE SAME POST. §4.2 names the lost-response asymmetry the
//     deterministic rkey exists to close, and closing it means a retry produces
//     the same URI, the same CID, and NO second commit — because a second commit
//     is a second firehose event, and consumers would index an edit that never
//     happened.
//   - A COMMUNITY WE DO NOT HOST, AND AN ACCEPTANCE THAT FAILED, ARE THE SAME
//     ANSWER TO THE AUTHOR: their record stands, the response succeeds, the post
//     is pending. The author's record is NEVER rolled back — §4.2 is explicit
//     that a failed acceptance is degraded latency, not data loss.
//   - AN EDIT IS NOT A SUBMISSION. It preserves what the lexicon calls
//     immutable, it is guarded against a concurrent edit, and it leaves the
//     submission ledger completely alone.

// hostedRkeyOf is the acceptance record key for a subject — the SAME derivation
// the engine and the firehose consumer use, asserted here so that a fast path
// which computed its own would be caught rather than merely producing a record
// nobody looks up.
func hostedRkeyOf(postURI string) string { return posts.SubjectRkey(postURI) }

// admissionOf reads the row the fast path seeded and settled.
func (f *postFixture) admissionOf(t *testing.T, postURI string) *posts.Admission {
	t.Helper()

	row, err := f.admissions.Get(context.Background(), f.community.DID, postURI)
	require.NoErrorf(t, err, "reading the admission of %s", postURI)
	require.NotNilf(t, row, "no admission row exists for %s — the write path must seed one before it "+
		"asks the engine to settle it, and the URI it seeds has to be byte-identical to the one the "+
		"firehose consumer will build, or the two will index the same post as two subjects", postURI)
	return row
}

// repoHead is a repo's current commit revision. A write that committed moves it;
// one that was correctly skipped does not.
func repoHead(t *testing.T, account *testkit.Account) string {
	t.Helper()

	var resp struct {
		Rev string `json:"rev"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, account.XRPC().Query(ctx, "com.atproto.sync.getLatestCommit",
		map[string][]string{"did": {account.DID}}, &resp))
	require.NotEmpty(t, resp.Rev)
	return resp.Rev
}

func TestService_CreateAcceptsSynchronouslyInAHostedCommunity(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	resp := f.createPost(t, f.author.DID, "accepted on the way in", "body")

	// 1. THE RESPONSE SAYS ACCEPTED. This is the field a client renders the
	// difference from: an accepted post appears in the community immediately,
	// a pending one shows its author a "waiting for the community" state.
	assert.Equal(t, posts.PostStatusAccepted, resp.Status,
		"this AppView holds the community's credentials, so it IS the admission authority (§4.2 step 4) "+
			"and must settle the post before answering rather than leaving the author to poll")

	// 2. THE ADMISSION ROW IS ACCEPTED, pinning the exact CID that committed.
	// The row is what every read path consults, so a response claiming accepted
	// over a row that says pending would render a post the AppView believes is
	// invisible.
	row := f.admissionOf(t, resp.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, row.Status)
	require.NotNil(t, row.AcceptedCID)
	assert.Equal(t, resp.CID, *row.AcceptedCID,
		"the acceptance must pin the version that was actually written, not a later or earlier one")
	require.NotNil(t, row.EvaluatedCID)
	assert.Equal(t, resp.CID, *row.EvaluatedCID)

	// 3. AND A REAL ACCEPTANCE RECORD STANDS IN THE COMMUNITY'S REPO, at the
	// deterministic rkey. Read from the PDS rather than from the row, because
	// the row is the AppView quoting itself: a fast path that stamped the row
	// without writing the record would leave the post visible here and invisible
	// to every federated peer, which is the exact split the acceptance record
	// exists to prevent.
	rkey := hostedRkeyOf(resp.URI)
	community := f.communityAccount(t)
	acceptance := community.GetRecord(t, posts.AcceptanceCollection, rkey)

	subject, ok := acceptance.Value["subject"].(map[string]any)
	require.Truef(t, ok, "the acceptance record has no subject strongRef: %#v", acceptance.Value)
	assert.Equal(t, resp.URI, subject["uri"])
	assert.Equal(t, resp.CID, subject["cid"],
		"an acceptance without the CID pins nothing, which is the one guarantee it exists to make")

	assert.Equal(t, []string{rkey}, listRecordKeys(t, community, posts.AcceptanceCollection),
		"exactly one acceptance, at the derived key")

	// 4. The fast path was actually taken. Asserted because every other
	// assertion here would also pass if the firehose had settled the row — and
	// in this fixture there is no firehose, so a zero here means the test is
	// proving something other than what it claims.
	assert.Equal(t, 1, f.acceptor.callCount())
}

func TestService_CreateRetryIsTheSamePostAndCommitsNothingNew(t *testing.T) {
	t.Parallel()

	// The lost-response case of §4.2, reproduced honestly: the client's first
	// attempt succeeded but its answer never arrived, so it sends the identical
	// submission again. Dedupe is NOT what saves it here — this fixture's ledger
	// is unmetered on purpose, because dedupe is the first line of defence and
	// the one that is gone in exactly the situation that matters (a reservation
	// released by the failure handler, or a retry aimed at a different AppView).
	// The deterministic rkey is the second, and it is the one under test.
	f := newPostFixture(t)

	first := f.createPost(t, f.author.DID, "a submission whose response was lost", "body")

	authorHeadBefore := repoHead(t, f.author)
	communityHeadBefore := repoHead(t, f.communityAccount(t))

	second := f.createPost(t, f.author.DID, "a submission whose response was lost", "body")

	assert.Equal(t, first.URI, second.URI,
		"a byte-identical retry must land on the SAME record key, or the author gets two posts")
	assert.Equal(t, first.CID, second.CID,
		"the retry must report the CID that is actually standing — a fresh CID means the record was "+
			"rewritten, and every strongRef a client built from the first response now dangles")

	assert.Equal(t, []string{rkeyOf(t, first.URI)},
		listRecordKeys(t, f.author, posts.PostV2Collection),
		"the retry left a second post record in the author's repo")

	// NO NEW COMMIT, IN EITHER REPO. This is the assertion with teeth. A retry
	// that re-PUT an identical record would satisfy the URI check, produce a new
	// CID, and — worse — emit a second firehose commit, which every consumer
	// reads as an EDIT: the admission row would move to pending_reacceptance and
	// the post would drop out of the community it was already accepted into.
	assert.Equal(t, authorHeadBefore, repoHead(t, f.author),
		"the retry committed to the author's repo; a create-only write must report the standing "+
			"record instead of rewriting it")
	assert.Equal(t, communityHeadBefore, repoHead(t, f.communityAccount(t)),
		"the retry committed to the community's repo; the acceptance already pinned this CID, so the "+
			"writer had nothing to do")

	assert.Equal(t, posts.PostStatusAccepted, second.Status,
		"the retry describes the state of the post that exists, which is accepted")
}

func TestService_CreateLeavesThePostPendingInACommunityThisAppViewDoesNotHost(t *testing.T) {
	t.Parallel()

	// §4.2 step 5: the author's server does not run admission for a community it
	// does not host — it has no authoritative view of that community's bans,
	// visibility or quotas, and a stale or hostile home server must not be able
	// to fake either an admission or a rejection. So the write succeeds and the
	// post waits.
	f := newPostFixture(t)
	remote := f.unhostedCommunity(t)

	resp, err := f.createPostIn(t, remote.DID, "submitted to a community we do not host", "body")
	require.NoErrorf(t, err, "not hosting a community is not a reason to refuse its author's post — "+
		"the record belongs to the author either way. A failure here is the retained community-token "+
		"step (service.go step 6): EnsureFreshToken cannot parse an empty access token, so it errors "+
		"for EVERY firehose-indexed community and blocks the remote pending flow the flip exists to "+
		"deliver. The post is written to the AUTHOR's repo now; the community's token buys a link "+
		"preview at most, and must not be able to refuse the post: %v", err)

	assert.Equal(t, posts.PostStatusPending, resp.Status)

	// The record is in the author's repo exactly as it would be for a local
	// community. Nothing about the author's half of the write depends on who
	// decides.
	f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))

	// And NO acceptance was invented on the community's behalf. This is the
	// security half of the case: an AppView that wrote an acceptance for a
	// community whose keys it does not hold could not, but one that STAMPED the
	// row accepted anyway would show every local reader a post the community
	// never admitted.
	row, err := f.admissions.Get(context.Background(), remote.DID, resp.URI)
	require.NoError(t, err)
	if row != nil {
		assert.Equal(t, posts.AdmissionStatusPending, row.Status,
			"a community we cannot write for must be left owing a decision, never marked accepted")
		assert.Nil(t, row.AcceptedCID)
	}
}

func TestService_CreateSurvivesAnAcceptanceThatFails(t *testing.T) {
	t.Parallel()

	// The failure mode §4.2 names by hand: "author-repo write succeeds,
	// acceptance write fails → post stays pending; the firehose engine retries
	// idempotently (same rkey). Degraded latency, not data loss. NEVER roll back
	// the author's record."
	//
	// Rolling back would be the tempting repair and it is the wrong one twice
	// over: the record is the AUTHOR's, not the AppView's, to withdraw — and a
	// rollback whose own delete failed would leave a post nobody has a row for.
	f := newPostFixture(t)
	f.acceptor.fail(errors.New("the community's PDS is unreachable"))

	resp, err := f.submitPost(t, f.author.DID, "written while acceptance was broken", "body")
	require.NoErrorf(t, err, "a failed acceptance must not fail the author's post — the record is "+
		"theirs and it committed")

	assert.Equal(t, posts.PostStatusPending, resp.Status,
		"the response must tell the truth about a post no community has accepted yet")

	// THE AUTHOR'S RECORD STANDS.
	record := f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	assert.Equal(t, resp.CID, record.CID)
	assert.Equal(t, "written while acceptance was broken", record.Value["title"])

	// And the row is pending with the content CID recorded, which is what makes
	// the firehose engine's retry possible at all: without an evaluated CID
	// there is nothing for an acceptance to pin.
	row := f.admissionOf(t, resp.URI)
	assert.Equal(t, posts.AdmissionStatusPending, row.Status)
	require.NotNil(t, row.EvaluatedCID)
	assert.Equal(t, resp.CID, *row.EvaluatedCID)

	assert.Empty(t, listRecordKeys(t, f.communityAccount(t), posts.AcceptanceCollection),
		"no acceptance may stand for a post the acceptance writer never managed to accept")
}

func TestService_UpdateEditsInPlaceAndPreservesWhatIsImmutable(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	created := f.createPost(t, f.author.DID, "the original title", "the original body")
	rkey := rkeyOf(t, created.URI)

	before := f.author.GetRecord(t, posts.PostV2Collection, rkey)
	originalCreatedAt, ok := before.Value["createdAt"].(string)
	require.True(t, ok)

	edited := "the corrected body"
	updated, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &edited})
	require.NoError(t, err)

	assert.Equal(t, created.URI, updated.URI,
		"an edit is the same post — a new URI would orphan every comment, vote and link to it")
	assert.NotEqual(t, created.CID, updated.CID,
		"the edit committed new content, so it has a new CID; the acceptance that pinned the old one "+
			"is what the engine re-decides against (§5.5)")

	after := f.author.GetRecord(t, posts.PostV2Collection, rkey)
	assert.Equal(t, updated.CID, after.CID, "the reported CID must be the one that committed")
	assert.Equal(t, "the corrected body", after.Value["content"])

	// COMMUNITY AND createdAt SURVIVE THE EDIT, and both come from the STANDING
	// RECORD rather than from the request. The community because the lexicon
	// calls it immutable — a consumer discards an update event that changes it,
	// so an edit that dropped it would be discarded entire and the post would
	// freeze at its pre-edit content everywhere but here. createdAt because
	// every feed orders by it: re-stamping it on an edit would jump a
	// three-year-old post corrected for a typo to the top of every sort.
	assert.Equal(t, f.community.DID, after.Value["community"])
	assert.Equal(t, originalCreatedAt, after.Value["createdAt"])

	assert.NotContains(t, after.Value, "author",
		"the edit reintroduced an author field the postv2 lexicon does not declare")
}

func TestService_UpdateRefusesRetargetingTheCommunity(t *testing.T) {
	t.Parallel()

	// Refused at the SERVICE, as a validation error, rather than silently
	// ignored. Both leave the record's community intact, but only one tells the
	// client that the thing it asked for did not happen — and a client that
	// believed it had moved a post would show its author a community the post is
	// not in. §3.1: retargeting means writing a NEW post record.
	f := newPostFixture(t)
	created := f.createPost(t, f.author.DID, "a post in one community", "body")
	elsewhere := f.unhostedCommunity(t)

	body := "still here"
	_, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &body, Community: elsewhere.DID})
	require.Error(t, err)
	assert.Truef(t, posts.IsValidationError(err),
		"expected a validation error the handler turns into a 400 naming the field, got: %v", err)

	after := f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, created.URI))
	assert.Equal(t, f.community.DID, after.Value["community"])
	assert.Equal(t, "body", after.Value["content"],
		"the refused update applied its content change anyway — a refusal must be total")
}

func TestService_UpdateRefusesAConcurrentEdit(t *testing.T) {
	t.Parallel()

	// The read-then-write window. UpdatePost reads the standing record to
	// preserve community and createdAt, then writes the edit guarded by the CID
	// it read. A second device's edit landing in between must make this one FAIL
	// rather than overwrite it: the edit was composed against content that no
	// longer stands, and silently re-applying it would erase a change its author
	// never saw.
	f := newPostFixture(t)
	created := f.createPost(t, f.author.DID, "edited from two devices", "the original body")
	rkey := rkeyOf(t, created.URI)

	f.authorRepos.raceAfterRead(f.author.DID, func() {
		f.author.PutRecord(t, posts.PostV2Collection, rkey, map[string]any{
			"$type":     posts.PostV2Collection,
			"community": f.community.DID,
			"title":     "edited from two devices",
			"content":   "the other device got there first",
			"createdAt": "2026-07-01T12:00:00Z",
		})
	})

	body := "the edit that was composed against stale content"
	_, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &body})
	require.Error(t, err)
	assert.ErrorIsf(t, err, posts.ErrConcurrentModification,
		"the boundary maps this to a 409 so the client can re-read and decide; got: %v", err)

	after := f.author.GetRecord(t, posts.PostV2Collection, rkey)
	assert.Equal(t, "the other device got there first", after.Value["content"],
		"the losing edit overwrote the winner, which is the whole failure the swap guard exists to stop")
}

func TestService_UpdateRefusesEveryoneButTheAuthor(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	created := f.createPost(t, f.author.DID, "the author's post", "body")
	attacker := f.authorRepos.register(f.pds.CreateAccount(t, testkit.WithHandlePrefix("aup")))

	body := "an edit by someone who does not own this repo"
	_, err := f.service.UpdatePost(context.Background(), sessionFor(t, attacker, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &body})
	assert.ErrorIs(t, err, posts.ErrNotAuthorized)

	after := f.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, created.URI))
	assert.Equal(t, "body", after.Value["content"])
}

func TestService_UpdateLeavesTheSubmissionLedgerUntouched(t *testing.T) {
	t.Parallel()

	// AN EDIT IS NOT A SUBMISSION, and the ledger is where that has to be true.
	//
	// The ledger is both the dedupe gate and the per-author quota (§8). If an
	// edit wrote to it, two things break at once: an author who fixed a typo
	// would spend a quota slot for it, and — because the edit's fingerprint is
	// the EDITED content — resubmitting that same content later would be refused
	// as a duplicate of an edit rather than of a post.
	//
	// The pin is the mirror image: after an edit, the ORIGINAL content is still
	// on the ledger, so resubmitting it inside the window is still a duplicate.
	// An implementation that moved or rewrote the ledger row would admit it.
	f := newLedgerFixture(t)

	created := f.submit(t, "a post that will be edited", "the original body")
	require.Equal(t, 1, f.ledgerRows(t))

	edited := "the corrected body"
	_, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.base.author, f.base.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &edited})
	require.NoError(t, err)

	assert.Equal(t, 1, f.ledgerRows(t),
		"the edit wrote to the submission ledger; an edit consumes no quota and creates no dedupe key")

	_, err = f.submitErr(t, "a post that will be edited", "the original body")
	assert.ErrorIsf(t, err, posts.ErrDuplicateSubmission,
		"the ORIGINAL submission's ledger row must survive the edit — an edit that overwrote it with "+
			"the edited fingerprint would let the original content be posted a second time; got: %v", err)

	// And the refused resubmission changed nothing about the post that exists.
	after := f.base.author.GetRecord(t, posts.PostV2Collection, rkeyOf(t, created.URI))
	assert.Equal(t, "the corrected body", after.Value["content"])
	assert.Equal(t, 1, f.ledgerRows(t), "a refused submission consumes no quota (§8)")
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// unhostedCommunity provisions a community and then takes away the two things
// that make it ours: BOTH stored tokens.
//
// CLEARING ONLY THE REFRESH TOKEN WAS NOT ENOUGH, and the gap hid a real bug.
// Hosting is credential presence — NewCommunityRepoFactory refuses to consult
// hosted_by_did, because that column comes from a profile record anyone can
// write — so an empty refresh token is what makes the factory answer
// ErrCommunityNotHosted, and that much was right. But a community indexed from
// someone else's firehose has NO access token either, and leaving a live one
// here let the write path's EnsureFreshToken step succeed on credentials no real
// remote community has. The fixture was quietly propping up a step that fails
// for every genuine remote community, and the pending flow the whole flip exists
// to deliver was untested against the shape it will actually meet.
func (f *postFixture) unhostedCommunity(t *testing.T) *communities.Community {
	t.Helper()

	name := testkit.UniqueIDWithPrefix(t, "uh")
	require.LessOrEqual(t, len("c-"+name), testkit.MaxIDLength)

	community, err := f.communityService.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:         name,
		DisplayName:  "Hosted elsewhere",
		Description:  "a community whose credentials this AppView does not hold",
		Visibility:   "public",
		CreatedByDID: f.author.DID,
	})
	require.NoError(t, err)

	result, err := f.db.ExecContext(context.Background(), `
		UPDATE communities
		   SET pds_refresh_token_encrypted = NULL,
		       pds_access_token_encrypted  = NULL
		 WHERE did = $1`, community.DID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equalf(t, int64(1), affected,
		"the fixture cleared no credentials for %s, so the community is still hosted and the test "+
			"would prove nothing", community.DID)

	return community
}

// createPostIn submits to a named community rather than the fixture's own.
func (f *postFixture) createPostIn(t *testing.T, communityDID, title, content string) (*posts.CreatePostResponse, error) {
	t.Helper()
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), f.author.DID),
		sessionFor(t, f.author, f.pds.URL()),
		posts.CreatePostRequest{
			Community: communityDID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.author.DID,
		})
}

// ledgerFixture is the write path over a REAL submission ledger, which the
// default fixture deliberately opts out of.
type ledgerFixture struct {
	base    *postFixture
	service posts.Service
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()

	base := newPostFixture(t)
	return &ledgerFixture{
		base: base,
		service: posts.NewPostService(
			postgres.NewPostRepository(base.db), base.communityService,
			nil, nil, nil, nil, base.pds.URL(),
			append(base.writePathOptions(),
				posts.WithAdmissionPolicy(posts.AdmissionPolicy{
					Ledger: postgres.NewSubmissionLedger(base.db),
					Bans:   base.communityService,
					Limits: posts.SubmissionLimits{
						MaxPerAuthorPerCommunity: 10,
						Window:                   time.Hour,
						DedupeWindow:             time.Hour,
					},
					// The real clock: this fixture never crosses a window, and
					// the ledger stamps created_at server-side, so an injected
					// clock would only introduce disagreement between the two.
					Now: time.Now,
				}))...),
	}
}

func (f *ledgerFixture) submit(t *testing.T, title, content string) *posts.CreatePostResponse {
	t.Helper()
	resp, err := f.submitErr(t, title, content)
	require.NoError(t, err)
	return resp
}

func (f *ledgerFixture) submitErr(t *testing.T, title, content string) (*posts.CreatePostResponse, error) {
	t.Helper()
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), f.base.author.DID),
		sessionFor(t, f.base.author, f.base.pds.URL()),
		posts.CreatePostRequest{
			Community: f.base.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.base.author.DID,
		})
}

func (f *ledgerFixture) ledgerRows(t *testing.T) int {
	t.Helper()
	return countSubmissions(t, f.base.db, f.base.author.DID, f.base.community.DID)
}

func TestService_UpdateIsIdempotentOnAnIdenticalReEdit(t *testing.T) {
	t.Parallel()

	// THE PDS ANSWERS A NO-OP PUT WITH A 200 AND NO COMMIT. GREEN verified that
	// against a live PDS on the create path (pds.ErrNoCommit): a put of bytes
	// identical to what already stands changes no record, so there is no commit
	// to report, and the sentinel exists because the response would otherwise
	// read as a malformed body.
	//
	// The edit path can reach exactly the same state and currently turns it into
	// a 500. Three ordinary things produce it: a client retrying after a lost
	// response, a UI that fires save on blur whether or not anything changed,
	// and a user who opens the editor and saves without typing. In every one of
	// them the record already holds precisely what the client asked for, which
	// is the definition of the request having succeeded — answering 500 tells
	// the author their edit failed while showing them the edited post.
	f := newPostFixture(t)
	created := f.createPost(t, f.author.DID, "a post edited twice, identically", "the original body")

	edited := "the corrected body"
	first, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &edited})
	require.NoError(t, err)

	second, err := f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &edited})
	require.NoErrorf(t, err, "an identical re-edit is a state the record is ALREADY in, which is what "+
		"the client asked for; a PDS 200-without-commit must not surface as a failure")

	assert.Equal(t, first.URI, second.URI)
	assert.Equalf(t, first.CID, second.CID,
		"the re-edit must report the CID that is standing — a different one would mean the record was "+
			"rewritten, and every strongRef built from the first response now dangles")

	// And nothing committed. The put changed no bytes, so the repo's revision
	// must not have moved: a second commit here is a firehose event describing
	// an edit that did not happen, which every consumer would apply as one.
	head := repoHead(t, f.author)
	_, err = f.service.UpdatePost(context.Background(), sessionFor(t, f.author, f.pds.URL()),
		posts.UpdatePostRequest{URI: created.URI, Content: &edited})
	require.NoError(t, err)
	assert.Equal(t, head, repoHead(t, f.author),
		"the identical re-edit committed to the author's repo — a no-op put must report what stands, "+
			"not rewrite it")
}

func TestService_TheSeedNeverRewindsContentTheFirehoseHasAlreadyAdvanced(t *testing.T) {
	t.Parallel()

	// THE RACE, CONSTRUCTED. CreatePost writes the record and then seeds the
	// admission row with the CID it just wrote. Between those two steps the
	// firehose is live, and it can deliver BOTH the create and a subsequent edit
	// — an author correcting a typo immediately, a bridge that creates then
	// updates, or simply an AppView busy enough that its own seed runs late.
	//
	// The seed is an UpsertPending, whose guard is `evaluated_cid IS DISTINCT
	// FROM excluded`. That guard is right for the firehose, where a differing
	// CID always means newer content. It is wrong for the seed, which carries
	// content that may already be OLD: a row advanced to the edit's CID_B meets
	// a seed carrying CID_A, the CIDs differ, and the row is written BACKWARDS
	// to CID_A.
	//
	// What that costs is not a stale column. AcceptSubmission's guard is "row
	// pending AND evaluated_cid == the CID I am accepting", so the rewind makes
	// the guard pass against superseded content and the community publishes an
	// acceptance pinning a version the author has already replaced — the exact
	// outcome §5.5 exists to prevent, reached without any moderator being wrong
	// about anything.
	//
	// The ordering is forced through the author-repo seam rather than by timing:
	// the hook fires once the record has committed and its CID is known, which
	// is precisely the window the firehose would land in.
	f := newPostFixture(t)
	ctx := context.Background()

	const editedCID = "bafyreicaneditedversionthefirehosealreadyindexed"

	var seededURI string
	f.authorRepos.afterPut(f.author.DID, func(uri, _ string) {
		// The firehose, arriving first: it indexed the create and then the edit,
		// so the row already holds the LATER content.
		seededURI = uri
		_, err := f.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: f.community.DID,
			PostURI:      uri,
			EvaluatedCID: editedCID,
		})
		require.NoError(t, err)
	})

	resp, err := f.submitPost(t, f.author.DID, "a post edited before its own seed ran", "body")
	require.NoError(t, err)
	require.Equalf(t, resp.URI, seededURI,
		"the hook and the response must describe one record, or this test is racing something else")

	row := f.admissionOf(t, resp.URI)
	require.NotNil(t, row.EvaluatedCID)

	// THE ROW MUST STILL HOLD THE NEWER CONTENT. A seed that overwrote it has
	// moved the AppView's belief about this post backwards in time.
	assert.Equalf(t, editedCID, *row.EvaluatedCID,
		"the seed rewound the row from the edit's CID back to the create's: UpsertPending writes on any "+
			"DISTINCT cid, so a seed carrying content the firehose has already superseded moves the "+
			"AppView's view of the post backwards")

	// AND NO ACCEPTANCE PINS THE SUPERSEDED VERSION. This is what the rewind
	// actually buys an attacker or an unlucky author: with the row rewound,
	// AcceptSubmission's CID guard passes and the community publishes an
	// attestation to content nobody is reading any more.
	acceptances := listRecordKeys(t, f.communityAccount(t), posts.AcceptanceCollection)
	if len(acceptances) > 0 {
		acceptance := f.communityAccount(t).GetRecord(t, posts.AcceptanceCollection, acceptances[0])
		subject, ok := acceptance.Value["subject"].(map[string]any)
		require.True(t, ok)
		assert.NotEqualf(t, resp.CID, subject["cid"],
			"the community accepted the CREATE's content after the firehose had already recorded an "+
				"edit — an acceptance must never pin a version the row no longer holds")
	}

	// The author is told the truth: the AppView has not evaluated the content it
	// currently holds, so the post is pending, not accepted.
	assert.Equalf(t, posts.PostStatusPending, resp.Status,
		"the row holds content this request never evaluated, so the community cannot have accepted it")
}
