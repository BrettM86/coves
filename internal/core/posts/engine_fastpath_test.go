//go:build integration

package posts_test

import (
	"context"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AcceptSubmission's guards, from the engine's side of the seam.
//
// # WHY THIS EXISTS SEPARATELY FROM THE WRITE PATH'S PINS
//
// AcceptSubmission is the one entry point in the system that writes a community
// acceptance WITHOUT re-running the admission policy. That is correct and
// deliberate — CreatePost has already made the decision, and the production
// decider reads an index the post is not in yet, so a fast path that re-decided
// would refuse every post it was handed. But it means the guard on this method
// is the entire safety argument: everything that normally stands between a post
// and a published attestation has already happened somewhere else, and this
// method is trusting that it did.
//
// So the guard has to hold against a caller that is WRONG, not merely against a
// caller that is late. It has two halves and they answer different mistakes:
//
//   - THE CID MUST MATCH what the row currently holds. The caller is asking the
//     community to attest to a specific version, and the row is the AppView's
//     record of which version it has actually evaluated. A mismatch means the
//     two disagree about what the post says — and an acceptance settles that
//     disagreement by publishing a signed attestation to the caller's version.
//   - THE ROW MUST BE PENDING. `rejected` and `removed` are terminal (§5.5), and
//     an acceptance written over either is a moderation decision being reversed
//     by a write path that never asked a moderator anything.
//
// # THE DEFENCE-IN-DEPTH ARGUMENT, STATED HONESTLY
//
// service_writeflip_test.go's seed-rewind pin proves the WRITE PATH must not
// hand this method a stale CID. This file proves the engine refuses one anyway.
// Those are not the same test and neither makes the other redundant: the rewind
// pin fails if the seed is fixed and the guard is broken, this one fails if the
// guard is fixed and the seed is broken, and the bug the review found needed
// BOTH to be wrong at once. It got there because the seed rewound the row into
// agreement with a stale CID — which is precisely the shape of failure a guard
// checking "do these two agree" cannot catch on its own, and precisely why the
// two layers are worth having separately.
//
// Every case below asserts the community's repo as well as the outcome value. A
// method that returned EngineDeferred and wrote a record anyway would pass an
// outcome-only assertion, and the record is the thing that federates.

// acceptSubmission drives the fast path against the fixture's community.
//
// The row states these tests need are reached through the REPOSITORY rather
// than by driving the engine into them, and that is deliberate: what is under
// test is how AcceptSubmission READS a row, so the row is the input, and how it
// came to look that way is somebody else's contract (admission_repo_test.go's).
func acceptSubmission(t *testing.T, f *engineFixture, postURI, postCID string) (posts.EngineOutcome, error) {
	t.Helper()
	return f.engine.AcceptSubmission(context.Background(), f.community.DID, postURI, postCID)
}

// assertNoAcceptance asserts the community published nothing about this subject.
func assertNoAcceptance(t *testing.T, f *engineFixture, postURI, why string) {
	t.Helper()

	assert.Emptyf(t, listRecordKeys(t, f.communityAt, posts.AcceptanceCollection),
		"the community's repo holds an acceptance record: %s", why)
	f.assertRecordAbsent(t, posts.AcceptanceCollection, posts.SubjectRkey(postURI),
		"an acceptance for a subject the engine refused to accept")
}

func TestEngine_AcceptSubmissionRefusesACIDTheRowDoesNotHold(t *testing.T) {
	t.Parallel()

	// The row has been advanced to the EDITED content — the ordinary case, and
	// the one the firehose produces constantly: an author edits between the
	// write path's PDS call and its call to this method. The caller is asking
	// the community to accept the version it wrote, which is no longer the
	// version the AppView holds.
	f := newEngineFixture(t)
	post := f.publishPost(t, "a post accepted at the wrong version")
	edited := f.editPost(t, post, "the version the AppView has actually evaluated")

	f.seedPending(t, post.URI, edited.CID)

	outcome, err := acceptSubmission(t, f, post.URI, post.CID)
	require.NoErrorf(t, err, "a CID mismatch is the engine working, not a failure to report: the caller "+
		"is simply late, and the firehose will drive the subject again with the content that stands")
	assert.Equalf(t, posts.EngineDeferred, outcome,
		"accepting the caller's CID would publish an attestation to content the AppView has already "+
			"superseded — the exact outcome §5.5 exists to prevent")

	assertNoAcceptance(t, f, post.URI,
		"the pinned CID is not the one the row holds, so there is nothing the community can honestly attest to")

	// And the row was not moved into agreement with the caller. This is the
	// assertion that ties the two layers together: the review's bug reached a
	// published acceptance precisely because something upstream rewound the row
	// until the guard agreed, so the guard must not do that itself.
	row := f.admissionRow(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusPending, row.Status)
	require.NotNil(t, row.EvaluatedCID)
	assert.Equalf(t, edited.CID, *row.EvaluatedCID,
		"AcceptSubmission rewrote the row's evaluated content to match the CID it was handed; a guard "+
			"that adjusts the thing it is checking is not a guard")
}

func TestEngine_AcceptSubmissionRefusesARowWithNoEvaluatedContent(t *testing.T) {
	t.Parallel()

	// No row at all — the caller's seed failed, or the subject was swept. An
	// acceptance here would attest to a post this AppView has no record of
	// having looked at.
	f := newEngineFixture(t)
	post := f.publishPost(t, "a post with no admission row")

	outcome, err := acceptSubmission(t, f, post.URI, post.CID)
	assert.Equalf(t, posts.EngineDeferred, outcome,
		"there is no row, so there is no evaluated content, so there is nothing to accept")
	if err != nil {
		// An error is acceptable here and a deferral is the required half: the
		// caller must not treat this as accepted whichever way it is reported.
		assert.ErrorIsf(t, err, posts.ErrNotFound, "an absent row must report itself as one: %v", err)
	}

	assertNoAcceptance(t, f, post.URI, "no admission row exists for this subject")
}

func TestEngine_AcceptSubmissionRefusesATerminalRow(t *testing.T) {
	t.Parallel()

	// `rejected` and `removed` are terminal against everything except a
	// moderator restore at a strictly greater watermark (§5.5). An acceptance
	// written over either is a moderation decision reversed by a write path that
	// never asked a moderator anything — and unlike a rejection, which is
	// AppView-local, the acceptance is PUBLISHED, so every federated peer would
	// see the removed post come back.
	for _, tc := range []struct {
		settle func(t *testing.T, f *engineFixture, postURI, contentCID string)
		name   string
		status posts.AdmissionStatus
		why    string
	}{
		{
			name:   "rejected",
			status: posts.AdmissionStatusRejected,
			why: "a rejected submission was refused by this community's policy; accepting it here " +
				"would overturn that refusal with no decision behind it",
			settle: func(t *testing.T, f *engineFixture, postURI, contentCID string) {
				t.Helper()
				result, err := f.admissions.RecordRejection(context.Background(), posts.RecordRejectionCommand{
					CommunityDID: f.community.DID,
					PostURI:      postURI,
					DecisionCode: string(posts.DecisionSpam),
					JudgedCID:    contentCID,
					Redrivable:   false,
				})
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionApplied, result.Outcome)
			},
		},
		{
			name:   "removed",
			status: posts.AdmissionStatusRemoved,
			why: "a removed post was taken down by a moderator and the removal was published; " +
				"an acceptance written over it would restore the post for every federated peer",
			settle: func(t *testing.T, f *engineFixture, postURI, _ string) {
				t.Helper()
				result, err := f.admissions.ApplyRemoval(context.Background(), posts.ApplyRemovalCommand{
					CommunityDID: f.community.DID,
					PostURI:      postURI,
					DecisionCode: string(posts.DecisionRuleViolation),
					Watermark:    posts.CommunityWatermark{Rev: testkit.TID()},
				})
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionApplied, result.Outcome)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newEngineFixture(t)
			post := f.publishPost(t, "a post settled before the fast path reached it")
			f.seedPending(t, post.URI, post.CID)
			tc.settle(t, f, post.URI, post.CID)

			// The CID MATCHES. That is the point of this case: the content guard
			// is satisfied, so the status guard is the only thing left, and a
			// method that checked only the CID would write the acceptance here.
			outcome, err := acceptSubmission(t, f, post.URI, post.CID)
			require.NoError(t, err)
			assert.Equalf(t, posts.EngineDeferred, outcome, "%s", tc.why)

			assertNoAcceptance(t, f, post.URI, tc.why)

			row := f.admissionRow(t, post.URI)
			assert.Equalf(t, tc.status, row.Status,
				"the terminal row was moved by a pass that was supposed to decline it")
		})
	}
}

func TestEngine_AcceptSubmissionDoesNotReMintAStandingAcceptance(t *testing.T) {
	t.Parallel()

	// An already-accepted row: the retry case, which the write path reaches
	// whenever a client resubmits a settled post. This one legitimately HAS an
	// acceptance in the repo, so "untouched" cannot mean "absent" — it means the
	// standing record keeps its CID.
	//
	// Re-minting would not look like a failure anywhere. The record would still
	// pin the right post at the right version; it would simply be a NEW record
	// CID, which invalidates every reference built from the old one and emits a
	// firehose event describing a moderation action nobody took.
	//
	// WHAT THIS CASE ACTUALLY GUARDS, measured rather than assumed. Disabling
	// AcceptSubmission's status guard does NOT fail this test — the three cases
	// above catch that — because with the guard gone the write still reaches the
	// state-shaped acceptance writer, which pre-reads, finds its exact target
	// already standing, and skips. So this is a regression guard on the WRITER's
	// idempotence reached through the fast path, not on the engine's status
	// check. Both are worth having and they are not the same property; recording
	// which one this is stops a future reader trusting it for the other.
	f := newEngineFixture(t)
	post := f.publishPost(t, "a post accepted once and submitted again")
	f.seedPending(t, post.URI, post.CID)

	f.decider.code = ""
	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	require.Equal(t, posts.EngineAccepted, outcome)

	before := f.acceptanceOf(t, post.URI)

	// The same subject, the same CID, through the fast path this time.
	fastOutcome, err := acceptSubmission(t, f, post.URI, post.CID)
	require.NoErrorf(t, err, "a subject that is already accepted is a converged state, not a failure")
	assert.Contains(t, []posts.EngineOutcome{posts.EngineAccepted, posts.EngineDeferred}, fastOutcome,
		"an already-accepted subject may report either convergence or deferral, but never a new verdict")

	after := f.acceptanceOf(t, post.URI)
	assert.Equalf(t, before.CID, after.CID,
		"the standing acceptance was re-minted: same subject, same pinned CID, NEW record CID — which "+
			"dangles every reference to the acceptance and emits a firehose event for a moderation "+
			"action nobody took")

	assert.Equal(t, []string{posts.SubjectRkey(post.URI)},
		listRecordKeys(t, f.communityAt, posts.AcceptanceCollection),
		"exactly one acceptance, at the derived key")
}

// admissionRow reads the engine fixture's admission row for a subject.
func (f *engineFixture) admissionRow(t *testing.T, postURI string) *posts.Admission {
	t.Helper()

	row, err := f.admissions.Get(context.Background(), f.community.DID, postURI)
	require.NoErrorf(t, err, "reading the admission of %s", postURI)
	require.NotNilf(t, row, "no admission row exists for %s", postURI)
	return row
}
