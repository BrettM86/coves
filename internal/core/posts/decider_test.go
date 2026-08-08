package posts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production AdmissionDecider (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6): the
// adapter between "decide about this indexed post" and the policy matrix
// admit_matrix_test.go already pins.
//
// THIS IS T0 AND NOT T1, deliberately, though the driver and repository halves
// of this task are T1. Every collaborator here is an interface — a post lookup,
// an aggregator lookup, the injected policy — and the tier is chosen by what a
// test NEEDS out of process (docs/TEST_ARCHITECTURE.md), not by which task it
// belongs to. Nothing below opens a socket, and a Postgres-backed version of
// these cases would prove the same things more slowly while making the actor
// matrix awkward to enumerate.
//
// What is asserted here is exactly the seam the policy matrix cannot see: which
// AdmissionRequest gets built. The matrix takes a request and proves the
// verdict; this proves the request is the right one, which is where the two
// genuinely dangerous mistakes live.
//
//   - THE ACTOR CLASS IS A PRIVILEGE DECISION. A trusted aggregator skips
//     visibility, ban and authorization entirely. Anything that guesses UPWARD
//     when it is unsure hands the widest privileges in the system to whoever can
//     make a lookup fail.
//   - A DELETED POST HAS NOTHING TO DECIDE. Admitting one writes an acceptance
//     for content that no longer exists, and the host-side tombstone sweep then
//     deletes that acceptance — two components looping against each other, each
//     correct alone.

const (
	deciderCommunityDID = "did:plc:decidercommunity"
	deciderAuthorDID    = "did:plc:deciderauthor"
	deciderTrustedDID   = "did:plc:decidertrustedbot"
	deciderPostURI      = "at://" + deciderAuthorDID + "/social.coves.community.postv2/decided"
)

// stubPostLookup answers with one post, or with a failure.
type stubPostLookup struct {
	post *Post
	err  error

	calls int
}

func (s *stubPostLookup) GetByURI(_ context.Context, _ string) (*Post, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.post == nil {
		return nil, ErrNotFound
	}
	return s.post, nil
}

// stubAggregatorLookup is the IsAggregator classification lookup.
type stubAggregatorLookup struct {
	registered map[string]bool
	err        error

	calls int
}

func (s *stubAggregatorLookup) IsAggregator(_ context.Context, did string) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.registered[did], nil
}

// deciderHarness is the default world: a live post by an ordinary author in a
// public community, with the same policy stubs the admit matrix uses.
type deciderHarness struct {
	*admitHarness

	posts       *stubPostLookup
	aggregators *stubAggregatorLookup
	trusted     map[string]bool
}

func newDeciderHarness() *deciderHarness {
	base := newAdmitHarness()
	base.communities.community.DID = deciderCommunityDID
	return &deciderHarness{
		admitHarness: base,
		posts: &stubPostLookup{post: &Post{
			URI:          deciderPostURI,
			CID:          "bafyreidecided",
			AuthorDID:    deciderAuthorDID,
			CommunityDID: deciderCommunityDID,
		}},
		aggregators: &stubAggregatorLookup{registered: map[string]bool{}},
		trusted:     map[string]bool{},
	}
}

func (h *deciderHarness) decider() *AdmissionEngineDecider {
	return NewAdmissionEngineDecider(DeciderDeps{
		Posts:       h.posts,
		Communities: h.communities,
		Authorizer:  h.aggregators2(),
		Aggregators: h.aggregators,
		Policy: AdmissionPolicy{
			Ledger: h.ledger,
			Bans:   h.bans,
			Limits: h.limits,
			Now:    h.clock(),
		},
		TrustedAggregatorDIDs: h.trusted,
	})
}

// aggregators2 is the admit harness's authorization stub, named apart from the
// classification lookup because the two answer different questions and only
// this file holds both at once.
func (h *deciderHarness) aggregators2() *stubAggregatorAuthorizer { return h.admitHarness.aggregators }

func (h *deciderHarness) decide(t *testing.T) (AdmissionDecision, error) {
	t.Helper()
	return h.decider().DecideAdmission(context.Background(), deciderCommunityDID, deciderPostURI)
}

func TestDecider_AdmitsAnOrdinaryPostInAPublicCommunity(t *testing.T) {
	t.Parallel()

	h := newDeciderHarness()
	decision, err := h.decide(t)

	require.NoError(t, err)
	assert.True(t, decision.Admitted(), "a live post by an unbanned author in a public community is admitted: %+v", decision)

	// An admission has to be EARNED, not defaulted. AdmissionDecision's zero
	// value reports Admitted() true — deliberately, since a decision is an
	// admission only when there is neither a code nor a cause — so a decider
	// that returned early, or was never implemented, produces exactly the
	// verdict above. These two assertions are what separate "the policy ran and
	// said yes" from "nothing ran at all".
	assert.Positivef(t, h.posts.calls,
		"the decision was reached without reading the post it is about (%d lookups)", h.posts.calls)
	assert.Positivef(t, h.communities.resolveCalls,
		"the decision was reached without resolving the community (%d lookups): an admission nobody evaluated is the zero value, not a verdict", h.communities.resolveCalls)

	// The ledger is what separates this from admitPost, and the separation is
	// the whole reason task 3 split the policy out. The engine is not a
	// submission — it is re-deciding a post that already exists, often one it
	// has decided before — so reserving here would charge the author's quota for
	// a firehose redelivery and then refuse the redecision as a duplicate of the
	// very post it is redeciding.
	assert.Emptyf(t, h.ledger.reserveCalls,
		"the decider must NOT reserve a ledger slot: it re-decides existing posts, and a redelivery would consume quota and then be refused as its own duplicate")
	assert.Nil(t, decision.Reservation, "a decision that reserved nothing must not report a reservation")
}

func TestDecider_RefusesATombstonedPostWithoutRunningPolicy(t *testing.T) {
	t.Parallel()

	h := newDeciderHarness()
	deleted := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	h.posts.post.DeletedAt = &deleted

	decision, err := h.decide(t)

	// Not an admission, whichever way it is spelled. The driver already excludes
	// tombstoned subjects, but a post can be deleted between the listing and the
	// decision, and this is the guard that closes that window.
	assert.Falsef(t, decision.Admitted(),
		"a tombstoned post must never be admitted: the acceptance would pin content that no longer exists, and the tombstone sweep would then delete the acceptance — forever, once per pass: %+v", decision)
	if err == nil {
		assert.NotEmpty(t, decision.Code,
			"a refusal that is not an error must carry a code; a decision with neither reads as an admission to Admitted()")
	}

	// And the policy never ran. Evaluating a deleted post costs a community
	// resolve, a ban lookup and a quota count to answer a question that has no
	// content behind it — and worse, it can produce a REJECTION code that gets
	// written onto the row as this community's verdict about a post nobody can
	// read.
	assert.Zerof(t, h.communities.resolveCalls,
		"the tombstone check must run BEFORE the policy: %d community lookups happened for a post that no longer exists", h.communities.resolveCalls)
	assert.Zero(t, h.bans.calls, "no ban lookup for a deleted post")
}

func TestDecider_RefusesAPostItCannotFind(t *testing.T) {
	t.Parallel()

	h := newDeciderHarness()
	h.posts.post = nil // GetByURI answers ErrNotFound

	decision, err := h.decide(t)

	assert.Falsef(t, decision.Admitted(),
		"an admission row whose post was never indexed has no content to judge and must not be admitted: %+v", decision)
	if err == nil {
		assert.NotEmpty(t, decision.Code, "a refusal must carry a code")
	}
	assert.Zero(t, h.communities.resolveCalls, "a subject with no post must not reach the policy")
}

func TestDecider_ClassifiesTheAuthorFromTheTrustedSet(t *testing.T) {
	t.Parallel()

	// A trusted aggregator skips visibility, ban and authorization (admit.go's
	// check order). Proving the class was applied means proving those lookups
	// did NOT happen — the verdict alone cannot distinguish "trusted, so skipped"
	// from "an ordinary user who happened to pass every check".
	h := newDeciderHarness()
	h.posts.post.AuthorDID = deciderTrustedDID
	h.trusted[deciderTrustedDID] = true
	h.bans.membership = banned()

	decision, err := h.decide(t)

	require.NoError(t, err)
	assert.Truef(t, decision.Admitted(),
		"a trusted aggregator skips the ban check entirely; a refusal here means the author was classified as an ordinary user: %+v", decision)
	// The class can only have been applied if the AUTHOR was read, and the
	// author only comes from the post. Without this, a decider that returned the
	// zero value would satisfy every assertion below by never looking at
	// anything.
	assert.Positivef(t, h.posts.calls,
		"the author's class was decided without reading the post that names them (%d lookups)", h.posts.calls)
	assert.Zerof(t, h.bans.calls,
		"the ban lookup ran for a TRUSTED aggregator (%d calls): the class was not applied", h.bans.calls)
	assert.Zerof(t, h.aggregators.calls,
		"a DID in the trusted set must not cost an IsAggregator lookup — the set is checked first, and the lookup is the expensive path")
}

func TestDecider_ClassifiesARegisteredAggregator(t *testing.T) {
	t.Parallel()

	h := newDeciderHarness()
	h.aggregators.registered[deciderAuthorDID] = true

	decision, err := h.decide(t)

	require.NoError(t, err)
	assert.True(t, decision.Admitted(), "an authorized aggregator is admitted: %+v", decision)
	assert.Positivef(t, h.aggregators2().calls,
		"a registered aggregator must be held to the community's authorization record; %d authorization checks ran", h.aggregators2().calls)
	assert.Zero(t, h.bans.calls, "an aggregator is not held to member bans")
}

func TestDecider_EngineDefersWhenTheClassificationLookupFails(t *testing.T) {
	t.Parallel()

	// THE ENGINE AND THE WRITE PATH ANSWER THIS DIFFERENTLY, ON PURPOSE, and the
	// reason is what each does with the answer afterwards.
	//
	// CreatePost is talking to a live client. A failed IsAggregator lookup there
	// downgrades the caller to ActorUser (service.go step 3): the strict checks
	// apply, an aggregator loses a few posts until the table comes back, and the
	// client is told something it can retry. Nothing is written down.
	//
	// The engine writes the verdict INTO THE ADMISSION ROW, and a policy refusal
	// is stamped redrivable = false — terminal, never revisited by the redrive
	// pass. So the same downgrade here does not cost an aggregator a retry; it
	// permanently marks their post refused for a reason that was never true,
	// because a table was briefly unreachable. Nothing in the system would ever
	// look at it again.
	//
	// That asymmetry is the whole rule: a decision that PERSISTS may only be made
	// from an answer that was actually obtained.
	h := newDeciderHarness()
	h.aggregators.err = errors.New("aggregators table unreachable")
	h.bans.membership = banned()

	decision, err := h.decide(t)

	require.Error(t, err,
		"a classification that could not be made must be reported as undecided; the engine has no safe way to guess when its guess is written down permanently")
	assert.False(t, decision.Admitted(), "an undecided answer is not an admission")
	assert.Emptyf(t, decision.Code,
		"the decider minted %q from a failed lookup. The engine persists codes and sets redrivable = false on policy refusals, "+
			"so this would leave a permanent, unretryable refusal on a post whose author may not be banned at all", decision.Code)
}

func TestDecider_TheWritePathStillFallsToTheStricterClass(t *testing.T) {
	t.Parallel()

	// THE OTHER HALF, and it has to stay pinned or the fix above has an obvious
	// wrong generalisation available: making a failed lookup undecided
	// EVERYWHERE. On the write path that would turn a brief aggregators-table
	// blip into a 500 for every caller, where today they are simply held to the
	// ordinary user's rules — which is the strict direction and costs nothing.
	//
	// This asserts the composition rather than re-deriving the classification:
	// ActorUser is what service.go step 3 downgrades to, and what must follow
	// from it is that the checks a user is held to actually run and actually
	// answer. A refusal reaching a live client is retryable; that is precisely
	// what makes the guess safe there and unsafe in the engine.
	h := newAdmitHarness()
	h.bans.membership = banned()

	decision, err := evaluateAdmissionPolicy(context.Background(), h.deps(), AdmissionRequest{
		Actor:       ActorUser,
		AuthorDID:   admitAuthorDID,
		Community:   admitCommunityHandle,
		Fingerprint: "downgraded-classification",
	})

	require.NoError(t, err)
	assert.False(t, decision.Admitted(),
		"the stricter class must actually apply the checks it implies, or the downgrade is a downgrade in name only")
	assert.Equal(t, DecisionAuthorBanned, decision.Code,
		"falling to ActorUser means the ban check runs and answers — that is what makes it the SAFE guess for a caller who can retry")
	assert.Positive(t, h.bans.calls, "the ban lookup must have been consulted")
}

func TestDecider_IsUndecidedWhenThePolicyCannotBeEvaluated(t *testing.T) {
	t.Parallel()

	// An infrastructure failure inside the policy — the ban lookup is down — must
	// come back as UNDECIDED, never as a code. The engine writes a decision code
	// onto the admission row and sets redrivable=false for policy refusals, so a
	// Postgres blip minted as a code becomes a permanent verdict about somebody's
	// post that nothing will ever retry.
	h := newDeciderHarness()
	h.bans.err = errors.New("memberships unreachable")

	decision, err := h.decide(t)

	require.Error(t, err, "a policy that could not be evaluated must report an error, not a verdict")
	assert.False(t, decision.Admitted(), "an undecided answer is not an admission")
	assert.Emptyf(t, decision.Code,
		"an infrastructure failure minted the decision code %q: the engine persists codes and marks policy refusals non-redrivable, so an outage would become a permanent refusal", decision.Code)
}

func TestDecider_IsUndecidedWhenThePostLookupFails(t *testing.T) {
	t.Parallel()

	// The mirror of the tombstone case, and it must NOT collapse into it. "The
	// post is gone" and "I could not read the post" look the same from the call
	// site and mean opposite things: the first is terminal, the second clears.
	h := newDeciderHarness()
	h.posts.err = errors.New("posts table unreachable")

	decision, err := h.decide(t)

	require.Error(t, err, "a post lookup that FAILED is not a post that is absent")
	assert.False(t, decision.Admitted())
	assert.Emptyf(t, decision.Code,
		"a failed post lookup minted the code %q; an unreadable post must be retried, not permanently refused", decision.Code)
}
