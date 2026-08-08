//go:build integration

package posts_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The acceptance engine's outer contract: one post's whole life inside one
// community, driven through the engine and read back out of a REAL community
// repository on a REAL PDS (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6).
//
// The routing matrix is proven against fakes in engine_matrix_test.go, at the
// width the matrix has. What is unprovable there — and what this file exists
// for — is everything that only a real repo can contradict:
//
//   - that the acceptance lands in the COMMUNITY's repo, at the deterministic
//     rkey, pinning the CID the AppView indexed. A writer that computed the key
//     differently, or wrote to the wrong repo, passes every fake.
//   - that RE-FIRING MINTS NOTHING. This is the assertion with teeth. Three
//     independent writers converge on this record (§3.2), and every one of them
//     retries; a writer that re-put an identical record would produce a fresh
//     record CID on every attempt, invalidating every reference to the
//     acceptance it just rewrote. No fake can catch that, because the fake is
//     the thing that would have to notice.
//   - that the removal is ONE commit. §3.3 requires the acceptance's deletion
//     and the removal's write to reach the firehose together, and the PDS is
//     the only participant that can refuse a badly shaped batch — it answers a
//     delete of a missing record, or a create of an existing one, with a 500.
//   - that a lost swapRecord race CONVERGES rather than throws. Forced here by
//     committing a competing write between the writer's pre-read and its put,
//     which is a real InvalidSwap from a real PDS rather than an injected
//     error value.

const (
	// postv2Collection is the author-repo collection posts live in under
	// author-owned posts. Real records are written here so the acceptance pins
	// a CID the PDS actually minted, rather than a plausible-looking string.
	postv2Collection = "social.coves.community.postv2"
)

// engineFixture is the acceptance engine over a real community repo, a real
// author repo, and the real admissions table.
type engineFixture struct {
	*postFixture

	engine      *posts.AcceptanceEngine
	writer      posts.CommunityRecordWriter
	admissions  posts.AdmissionRepository
	decider     *scriptedDecider
	refreshes   *countingRefresher
	communityAt *testkit.Account
}

// scriptedDecider is the admission policy, scripted per case. The policy itself
// has its own tests (admit_matrix_test.go); what matters here is which repo
// write each verdict produces.
type scriptedDecider struct {
	code posts.DecisionCode
	err  error
}

func (d *scriptedDecider) DecideAdmission(_ context.Context, _, _ string) (posts.AdmissionDecision, error) {
	if d.err != nil {
		return posts.AdmissionDecision{Cause: d.err}, d.err
	}
	return posts.AdmissionDecision{Code: d.code}, nil
}

// countingRefresher records forced credential renewals. The community's token
// is freshly minted here, so a non-zero count means the writer met a 401 it
// should not have.
type countingRefresher struct{ calls int }

func (r *countingRefresher) RefreshCommunityCredentials(_ context.Context, _ string) error {
	r.calls++
	return nil
}

// newEngineFixture provisions the community (with its real PDS credentials) and
// points the engine at it.
func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()

	base := newPostFixture(t)
	account := base.communityAccount(t)

	generic, err := pds.NewFromAccessToken(base.pds.URL(), account.DID, account.AccessToken)
	require.NoError(t, err)

	repo, ok := generic.(pds.CommitClient)
	require.Truef(t, ok, "the PDS client must implement pds.CommitClient — the community-repo "+
		"writers need the commit rev and applyWrites, and neither is on the base Client")

	admissions := postgres.NewAdmissionRepository(base.db)
	decider := &scriptedDecider{}
	refreshes := &countingRefresher{}

	writer := posts.NewCommunityRecordWriter(
		func(_ context.Context, communityDID string) (posts.CommunityRepo, error) {
			require.Equalf(t, base.community.DID, communityDID,
				"the writer asked for credentials to a repo that is not the subject's community")
			return repo, nil
		},
		func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) },
	)

	return &engineFixture{
		postFixture: base,
		engine:      posts.NewAcceptanceEngine(admissions, decider, writer, refreshes),
		writer:      writer,
		admissions:  admissions,
		decider:     decider,
		refreshes:   refreshes,
		communityAt: account,
	}
}

// publishPost writes a real post record into the AUTHOR's repo and returns it.
// Using a real record means the acceptance pins a CID the PDS minted, so a
// strongRef that fails to round-trip fails here rather than in production.
func (f *engineFixture) publishPost(t *testing.T, title string) testkit.Record {
	t.Helper()
	return f.author.CreateRecord(t, postv2Collection, map[string]any{
		"$type":     postv2Collection,
		"community": f.community.DID,
		"title":     title,
		"content":   "a body seeking admission",
		"createdAt": "2026-07-01T12:00:00Z",
	})
}

// editPost rewrites the post in place, producing a new content CID.
func (f *engineFixture) editPost(t *testing.T, post testkit.Record, title string) testkit.Record {
	t.Helper()
	edited := f.author.PutRecord(t, postv2Collection, post.RKey, map[string]any{
		"$type":     postv2Collection,
		"community": f.community.DID,
		"title":     title,
		"content":   "a body the community has not judged",
		"createdAt": "2026-07-01T12:00:00Z",
	})
	require.NotEqualf(t, post.CID, edited.CID,
		"the edit produced the same content CID, so this test would prove nothing about re-acceptance")
	return edited
}

// seedPending records the AppView's observation of the post's content, which is
// what puts a row in the engine's way.
func (f *engineFixture) seedPending(t *testing.T, postURI, contentCID string) *posts.Admission {
	t.Helper()
	result, err := f.admissions.UpsertPending(context.Background(), posts.UpsertPendingCommand{
		CommunityDID: f.community.DID,
		PostURI:      postURI,
		EvaluatedCID: contentCID,
	})
	require.NoError(t, err)
	require.NotNil(t, result.Admission)
	return result.Admission
}

func (f *engineFixture) process(t *testing.T, postURI string) (posts.EngineOutcome, error) {
	t.Helper()
	return f.engine.ProcessAdmission(context.Background(), f.community.DID, postURI)
}

// acceptanceOf reads the community's acceptance record for a subject.
func (f *engineFixture) acceptanceOf(t *testing.T, postURI string) testkit.RecordValue {
	t.Helper()
	return f.communityAt.GetRecord(t, posts.AcceptanceCollection, posts.SubjectRkey(postURI))
}

// assertSubject checks a record's strongRef points at exactly this version of
// exactly this post.
func assertSubject(t *testing.T, record testkit.RecordValue, wantURI, wantCID string) {
	t.Helper()
	subject, ok := record.Value["subject"].(map[string]any)
	require.Truef(t, ok, "the record has no strongRef subject: %#v", record.Value)
	assert.Equal(t, wantURI, subject["uri"])
	assert.Equalf(t, wantCID, subject["cid"],
		"the strongRef must pin the exact version that was judged; pinning anything else means "+
			"content nobody evaluated renders under this record")
}

// assertRecordAbsent asserts a record is not in the community's repo.
func (f *engineFixture) assertRecordAbsent(t *testing.T, collection, rkey, what string) {
	t.Helper()
	err := getRecordErr(context.Background(), f.communityAt, collection, rkey)
	require.Errorf(t, err, "%s is still in the community's repo", what)
	assert.Truef(t, testkit.IsNotFound(err), "%s: expected the record to be gone, got: %v", what, err)
}

// ---------------------------------------------------------------------------

func TestEngine_AcceptanceLandsInTheCommunityRepoAndSurvivesRefiring(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx := context.Background()
	post := f.publishPost(t, "a post the community will accept")
	f.seedPending(t, post.URI, post.CID)

	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	assert.Equal(t, posts.EngineAccepted, outcome)

	rkey := posts.SubjectRkey(post.URI)
	acceptance := f.acceptanceOf(t, post.URI)

	// The authority half of the URI is the COMMUNITY. An acceptance written
	// into the author's repo would be an author vouching for themselves.
	assert.Equal(t, "at://"+f.community.DID+"/"+posts.AcceptanceCollection+"/"+rkey, acceptance.URI)
	assert.Equal(t, posts.AcceptanceCollection, acceptance.Value["$type"])
	assert.NotEmpty(t, acceptance.Value["createdAt"])
	assertSubject(t, acceptance, post.URI, post.CID)

	// The row now agrees: an acceptance pinning the indexed CID is `accepted`.
	row, err := f.admissions.Get(ctx, f.community.DID, post.URI)
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionStatusAccepted, row.Status)
	require.NotNil(t, row.AcceptanceRkey)
	assert.Equalf(t, rkey, *row.AcceptanceRkey,
		"the rkey the AppView recorded must be the one the record actually lives at, or getStatus "+
			"hands clients a URI that resolves to nothing")
	require.NotNil(t, row.LastCommunityEvent)
	assert.NotEmptyf(t, row.LastCommunityEvent.Rev,
		"the row must carry the rev the acceptance COMMITTED in — that is the §5.2 watermark the "+
			"firehose copy of this same event will be compared against")

	firstRecordCID := acceptance.CID

	// FIRE THE ENGINE AGAIN. A redrive, an overlapping feed, a notify racing
	// the firehose — the queue hands the engine the same subject constantly.
	outcome, err = f.process(t, post.URI)
	require.NoError(t, err)
	assert.Equalf(t, posts.EngineDeferred, outcome,
		"a settled row is a defensive skip; re-deciding it is how a moderated post gets laundered")

	// FIRE THE WRITER AGAIN, which is where the skip-write rule actually lives:
	// the engine's defensive skip above never reaches it, and the fast path and
	// the notify endpoint call it with no row check at all.
	result, err := f.writer.WriteAcceptance(ctx, posts.CommunityWriteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
		PostCID:      post.CID,
	})
	require.NoError(t, err)
	assert.Truef(t, result.Skipped,
		"the repo already held this exact acceptance, so the writer must write NOTHING")
	assert.Emptyf(t, result.Rev,
		"nothing committed, so there is no revision to report — and a caller that stamped one "+
			"would write a watermark no commit ever had")

	// THE ASSERTION WITH TEETH.
	assert.Equalf(t, firstRecordCID, f.acceptanceOf(t, post.URI).CID,
		"re-firing minted a NEW record CID for an identical acceptance; every reference to the "+
			"acceptance record just became stale, and it will happen again on every retry")
	assert.Zerof(t, f.refreshes.calls, "the community's token was fresh; nothing should have forced a renewal")
}

func TestEngine_FailedReacceptanceRemovesInOneCommit(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post the community will accept, then remove")
	f.seedPending(t, post.URI, post.CID)

	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	require.Equal(t, posts.EngineAccepted, outcome)

	// The author edits. The acceptance no longer pins the content the AppView
	// holds, so the row moves to pending_reacceptance and §5.5 forbids
	// rendering the new content under the old acceptance.
	edited := f.editPost(t, post, "a title the community would never have accepted")
	row := f.seedPending(t, post.URI, edited.CID)
	require.Equalf(t, posts.AdmissionStatusPendingReacceptance, row.Status,
		"an edit to an accepted post must leave the row awaiting re-acceptance")

	f.decider.code = posts.DecisionRuleViolation

	outcome, err = f.process(t, post.URI)
	require.NoError(t, err)
	assert.Equalf(t, posts.EngineRemoved, outcome,
		"§5.5: a failed re-acceptance is a REMOVAL — a local rejection would leave the published "+
			"acceptance standing, so federated peers would keep rendering what this AppView hid")

	rkey := posts.SubjectRkey(post.URI)

	// Both halves of the one commit, from the repo's point of view.
	f.assertRecordAbsent(t, posts.AcceptanceCollection, rkey, "the acceptance record")

	removal := f.communityAt.GetRecord(t, posts.RemovalCollection, rkey)
	assert.Equal(t, "at://"+f.community.DID+"/"+posts.RemovalCollection+"/"+rkey, removal.URI)
	assert.Equal(t, posts.RemovalCollection, removal.Value["$type"])
	assert.Equalf(t, string(posts.DecisionRuleViolation), removal.Value["code"],
		"the removal's code is what a client renders in #removedPost and what the author is told")
	assert.NotEmpty(t, removal.Value["createdAt"])
	assertSubject(t, removal, post.URI, edited.CID)

	// Exactly ONE removal record for the subject, ever. The rkey is derived
	// from the subject, so a second removal is an update of this one; a writer
	// that allocated a TID instead would leave a trail of them.
	assert.Equalf(t, []string{rkey}, listRecordKeys(t, f.communityAt, posts.RemovalCollection),
		"the community's removal collection must hold exactly one record, at the deterministic rkey")

	// And the row followed the repo.
	stored, err := f.admissions.Get(ctx, f.community.DID, post.URI)
	require.NoError(t, err)
	assert.Equal(t, posts.AdmissionStatusRemoved, stored.Status)
	require.NotNil(t, stored.DecisionCode)
	assert.Equal(t, string(posts.DecisionRuleViolation), *stored.DecisionCode)
	assert.Nilf(t, stored.AcceptanceURI,
		"the acceptance is gone from the repo, so the row must not still advertise it")
}

func TestEngine_RestoreIsTheRemovalCommitRunBackwards(t *testing.T) {
	t.Parallel()

	// §5.5: `removed` is exited only by an explicit restore — one commit
	// deleting the removal and writing a fresh acceptance. There is no distinct
	// restore operation on the wire; it is the same shape as the removal with
	// the two halves swapped, which is exactly why it is worth proving the
	// symmetry holds against a real repo.
	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post that will be removed and restored")
	f.seedPending(t, post.URI, post.CID)
	require.NoError(t, firstErr(f.process(t, post.URI)))

	f.decider.code = posts.DecisionModeratorDiscretion
	edited := f.editPost(t, post, "an edit that fails re-acceptance")
	f.seedPending(t, post.URI, edited.CID)
	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	require.Equal(t, posts.EngineRemoved, outcome)

	rkey := posts.SubjectRkey(post.URI)

	result, err := f.writer.RestoreAcceptance(ctx, posts.CommunityWriteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
		PostCID:      edited.CID,
	})
	require.NoError(t, err)
	assert.Falsef(t, result.Skipped, "there was no acceptance standing; the restore had work to do")
	assert.NotEmptyf(t, result.Rev, "a commit happened, so it has a revision")

	f.assertRecordAbsent(t, posts.RemovalCollection, rkey, "the removal record")

	restored := f.acceptanceOf(t, post.URI)
	assertSubject(t, restored, post.URI, edited.CID)
	assert.Equal(t, "at://"+f.community.DID+"/"+posts.AcceptanceCollection+"/"+rkey, restored.URI,
		"the restored acceptance must reuse the subject's rkey, not allocate a new one")
	assert.Equalf(t, []string{rkey}, listRecordKeys(t, f.communityAt, posts.AcceptanceCollection),
		"exactly one acceptance record for the subject, at the deterministic rkey")
}

func TestEngine_ApplyAcceptanceTwiceAtTheSameRevIsASkipThatChangesNothing(t *testing.T) {
	t.Parallel()

	// The engine stamps the row optimistically and the firehose delivers the
	// same event moments later. Both write the SAME rev, so the second must be
	// a skip that leaves the row byte-identical — not a re-stamped decision
	// timestamp, and certainly not an error into the dead-letter queue.
	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post whose acceptance arrives twice")
	f.seedPending(t, post.URI, post.CID)
	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	require.Equal(t, posts.EngineAccepted, outcome)

	before, err := f.admissions.Get(ctx, f.community.DID, post.URI)
	require.NoError(t, err)
	require.NotNil(t, before.LastCommunityEvent)

	replay := posts.ApplyAcceptanceCommand{
		CommunityDID:   f.community.DID,
		PostURI:        post.URI,
		AcceptanceURI:  *before.AcceptanceURI,
		AcceptanceRkey: *before.AcceptanceRkey,
		PinnedCID:      post.CID,
		Watermark:      posts.CommunityWatermark{Rev: before.LastCommunityEvent.Rev, OpRank: posts.CommunityOpPut},
	}

	result, err := f.admissions.ApplyAcceptance(ctx, replay)
	require.NoErrorf(t, err, "a replayed event is the ordering gate WORKING and must not be an error")
	assert.Equalf(t, posts.AdmissionSkippedStale, result.Outcome,
		"the watermark must be STRICTLY greater; an equal rev is the same event arriving again")

	after, err := f.admissions.Get(ctx, f.community.DID, post.URI)
	require.NoError(t, err)
	assert.Equalf(t, before, after,
		"a skipped event must leave the row untouched, down to updated_at — a re-stamped row is "+
			"how a replay looks like a fresh decision in the moderation log")
}

// ---------------------------------------------------------------------------
// Swap conflicts
// ---------------------------------------------------------------------------

// racingRepo commits a competing write between the writer's pre-read and its
// put, so the swapRecord CID the writer computed is already stale when it
// arrives. The conflict is therefore a real InvalidSwap from a real PDS, not an
// error value a fake handed back.
type racingRepo struct {
	posts.CommunityRepo

	race func()
	done bool
}

func (r *racingRepo) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	if !r.done {
		r.done = true
		r.race()
	}
	return r.CommunityRepo.PutRecordWithCommit(ctx, collection, rkey, record, swapRecord)
}

// racingWriter returns a writer whose first put loses a swap race to fn.
func (f *engineFixture) racingWriter(t *testing.T, fn func()) posts.CommunityRecordWriter {
	t.Helper()

	generic, err := pds.NewFromAccessToken(f.pds.URL(), f.communityAt.DID, f.communityAt.AccessToken)
	require.NoError(t, err)
	repo, ok := generic.(pds.CommitClient)
	require.True(t, ok)

	return posts.NewCommunityRecordWriter(
		func(_ context.Context, _ string) (posts.CommunityRepo, error) {
			return &racingRepo{CommunityRepo: repo, race: fn}, nil
		},
		func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) },
	)
}

// writeAcceptanceDirectly commits an acceptance through a SECOND client on the
// same repo — another AppView instance, or the synchronous fast path racing the
// firehose engine.
func (f *engineFixture) writeAcceptanceDirectly(t *testing.T, postURI, pinnedCID string) {
	t.Helper()
	f.communityAt.PutRecord(t, posts.AcceptanceCollection, posts.SubjectRkey(postURI), map[string]any{
		"$type":     posts.AcceptanceCollection,
		"subject":   map[string]any{"uri": postURI, "cid": pinnedCID},
		"createdAt": "2026-07-01T11:59:00Z",
	})
}

func TestEngine_LostSwapRaceToTheSameCIDIsAlreadyDone(t *testing.T) {
	t.Parallel()

	// The common race: two writers admit the same post at the same moment and
	// aim at the same CID. The loser re-reads, finds the record it wanted
	// already standing, and STOPS. Retrying would mint a new record CID for an
	// acceptance that is already correct — the exact churn deterministic rkeys
	// exist to prevent.
	f := newEngineFixture(t)
	post := f.publishPost(t, "a post two writers accept at once")

	writer := f.racingWriter(t, func() {
		f.writeAcceptanceDirectly(t, post.URI, post.CID)
	})

	result, err := writer.WriteAcceptance(context.Background(), posts.CommunityWriteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
		PostCID:      post.CID,
	})
	require.NoErrorf(t, err, "a lost race to the same outcome is convergence, not a failure")
	assert.Truef(t, result.Skipped,
		"the winner pinned the CID we wanted, so there is nothing left to write")

	acceptance := f.acceptanceOf(t, post.URI)
	assertSubject(t, acceptance, post.URI, post.CID)
}

func TestEngine_LostSwapRaceToADifferentCIDRetriesAndConverges(t *testing.T) {
	t.Parallel()

	// The harder race: the winner pinned a DIFFERENT version — an acceptance of
	// the pre-edit content landing while this writer is accepting the edit. The
	// loser must re-read and try again against what is actually there, and the
	// repo must end up holding OUR target.
	f := newEngineFixture(t)
	post := f.publishPost(t, "a post accepted at two different versions")
	edited := f.editPost(t, post, "the version this writer is accepting")

	writer := f.racingWriter(t, func() {
		f.writeAcceptanceDirectly(t, post.URI, post.CID)
	})

	result, err := writer.WriteAcceptance(context.Background(), posts.CommunityWriteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
		PostCID:      edited.CID,
	})
	require.NoErrorf(t, err, "a bounded retry after a lost swap must converge, not surface the conflict")
	assert.Falsef(t, result.Skipped, "the standing record pinned the wrong CID, so there WAS work to do")
	assert.NotEmpty(t, result.Rev)

	acceptance := f.acceptanceOf(t, post.URI)
	assertSubject(t, acceptance, post.URI, edited.CID)
	assert.Equalf(t, []string{posts.SubjectRkey(post.URI)},
		listRecordKeys(t, f.communityAt, posts.AcceptanceCollection),
		"the race must leave ONE acceptance record, not one per contender")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// listRecordKeys returns the rkeys in a collection of the account's repo, so a
// test can assert that a collection holds exactly the records it should.
func listRecordKeys(t *testing.T, account *testkit.Account, collection string) []string {
	t.Helper()

	var resp struct {
		Records []struct {
			URI string `json:"uri"`
		} `json:"records"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := account.XRPC().Query(ctx, "com.atproto.repo.listRecords", map[string][]string{
		"repo":       {account.DID},
		"collection": {collection},
		"limit":      {"100"},
	}, &resp)
	require.NoErrorf(t, err, "listing %s in %s", collection, account.DID)

	keys := make([]string, 0, len(resp.Records))
	for _, record := range resp.Records {
		keys = append(keys, rkeyOf(t, record.URI))
	}
	return keys
}

// firstErr drops an outcome and keeps the error, for the setup steps whose
// outcome a test has already proven elsewhere.
func firstErr(_ posts.EngineOutcome, err error) error { return err }
