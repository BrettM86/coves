//go:build integration

package posts_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The firehose path's submission quota (§8).
//
// # WHY THE LEDGER CANNOT SERVE THIS
//
// admitPost's quota counts post_submissions (migration 035), a table CreatePost
// writes. That works for the write path and is structurally blind on the
// ingestion path: a post from an author on another server arrives over the
// firehose, has no ledger row, and never will. Counting the ledger here would
// hold LOCAL users to the limit and exempt precisely the remote ones the limit
// exists for — anyone can write unlimited postv2 records naming any community,
// and §8's answer is that the admission layer absorbs them.
//
// So the engine counts what it can actually see: the admission rows this
// community already holds for this author. Accepted and pending together, over
// the same rolling window, refusing with the same code.
//
// This runs against the real repository rather than a counting fake, because
// the query is where the two subtle parts live — matching the author out of the
// AT-URI's authority (the admission may have no posts row to join to yet, §5.4)
// and excluding rejected and removed rows, since §8 is explicit that a refusal
// consumes no quota and counting refusals would let an author extend their own
// lockout by continuing to post.

const quotaLimit = 3

// quotaCommunities resolves one community and nothing else.
type quotaCommunities struct{ community *communities.Community }

func (q *quotaCommunities) ResolveCommunityIdentifier(context.Context, string) (string, error) {
	return q.community.DID, nil
}

func (q *quotaCommunities) GetByDID(context.Context, string) (*communities.Community, error) {
	return q.community, nil
}

// quotaBans reports no membership, which is the ordinary case: posting in a
// public community has never required joining it.
type quotaBans struct{}

func (quotaBans) GetMembership(context.Context, string, string) (*communities.Membership, error) {
	return nil, communities.ErrMembershipNotFound
}

// quotaPosts serves whichever post the subject names.
type quotaPosts struct{ posts map[string]*posts.Post }

func (q *quotaPosts) GetRawIndexedRow(_ context.Context, uri string) (*posts.Post, error) {
	if p, ok := q.posts[uri]; ok {
		return p, nil
	}
	return nil, posts.ErrNotFound
}

// quotaFixture is the production decider over the real admissions table.
type quotaFixture struct {
	decider      *posts.AdmissionEngineDecider
	admissions   posts.AdmissionRepository
	postLookup   *quotaPosts
	communityDID string
	authorDID    string
	now          time.Time
	trusted      map[string]bool
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()

	db := testkit.DB(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "quota")
	communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
	require.NoError(t, err)

	f := &quotaFixture{
		admissions:   postgres.NewAdmissionRepository(db),
		postLookup:   &quotaPosts{posts: map[string]*posts.Post{}},
		communityDID: communityDID,
		authorDID:    fixtures.DID(testkit.UniqueID(t)),
		// TWO CLOCKS, AND THEY HAVE TO AGREE. The quota compares an injected
		// `now` against created_at on the admission rows — and those are stamped
		// by POSTGRES, with NOW(), by design: the repository owns that column
		// and a test does not get to hand it one. So the injected clock is only
		// free to move RELATIVE to the database's, never to be planted somewhere
		// else entirely.
		//
		// A fixed instant looks like the suite's injected-clock rule and inverts
		// it. Pinned at a hardcoded 2026-08-08T12:00:00Z with a one-hour window,
		// the count's floor is 11:00 that day while the rows carry whatever the
		// database's real clock said — so the two only overlap during one hour
		// of one day, and worse, they overlap in a way no implementation can
		// satisfy: rows stamped hours LATER than the injected now stay inside
		// every window the test then advances through, so the refusal pin and
		// TheWindowRolls become mutually unsatisfiable.
		//
		// Anchoring on the real clock keeps the injection where it belongs — on
		// the DELTAS the tests apply — which is the whole reason the clock is
		// injectable: crossing a window boundary must cost no wall time
		// (docs/TEST_ARCHITECTURE.md forbids sleeping for it).
		now:     time.Now().UTC(),
		trusted: map[string]bool{},
	}

	f.decider = posts.NewAdmissionEngineDecider(posts.DeciderDeps{
		Posts:       f.postLookup,
		Communities: &quotaCommunities{community: &communities.Community{DID: communityDID, Visibility: "public"}},
		Admissions:  f.admissions,
		Policy: posts.AdmissionPolicy{
			Ledger: posts.NewAllowAllAdmissionPolicyForTests().Ledger,
			Bans:   quotaBans{},
			Limits: posts.SubmissionLimits{
				MaxPerAuthorPerCommunity: quotaLimit,
				Window:                   time.Hour,
				DedupeWindow:             time.Hour,
			},
			Now: func() time.Time { return f.now },
		},
		TrustedAggregatorDIDs: f.trusted,
	})
	return f
}

// admit records one already-admitted post for this author, the way the consumer
// would: an indexed post plus its pending admission row.
func (f *quotaFixture) admit(t *testing.T, label string) string {
	t.Helper()

	uri := "at://" + f.authorDID + "/social.coves.community.postv2/" + testkit.TID()
	f.postLookup.posts[uri] = &posts.Post{
		URI: uri, CID: "bafyrei" + label, AuthorDID: f.authorDID, CommunityDID: f.communityDID,
	}
	_, err := f.admissions.UpsertPending(context.Background(), posts.UpsertPendingCommand{
		CommunityDID: f.communityDID,
		PostURI:      uri,
		EvaluatedCID: "bafyrei" + label,
	})
	require.NoError(t, err)
	return uri
}

func (f *quotaFixture) decide(t *testing.T, uri string) posts.AdmissionDecision {
	t.Helper()
	decision, err := f.decider.DecideAdmission(context.Background(), f.communityDID, uri)
	require.NoError(t, err)
	return decision
}

func TestDeciderQuota_RefusesThePostPastTheLimit(t *testing.T) {
	t.Parallel()

	f := newQuotaFixture(t)

	// The author fills their allowance. Each of these is a post that already
	// exists and is already counted — the engine is deciding, not accepting a
	// submission, so what bounds them is what the community already holds.
	for i := 0; i < quotaLimit; i++ {
		uri := f.admit(t, "within")
		decision := f.decide(t, uri)
		assert.Truef(t, decision.Admitted(), "post %d of %d must be admitted: %+v", i+1, quotaLimit, decision)
	}

	over := f.admit(t, "over")
	decision := f.decide(t, over)

	assert.Falsef(t, decision.Admitted(),
		"the %dth post inside the window was admitted with a limit of %d. Nothing else bounds a firehose author: they "+
			"can write postv2 records naming any community as fast as their PDS accepts them, and §8's answer is that "+
			"the admission layer absorbs it: %+v", quotaLimit+1, quotaLimit, decision)
	assert.Equal(t, posts.DecisionRateLimitExceeded, decision.Code,
		"the refusal must carry rate-limit-exceeded; it is what getStatus shows the author and what marks the row terminal")
}

func TestDeciderQuota_TheWindowRolls(t *testing.T) {
	t.Parallel()

	f := newQuotaFixture(t)
	for i := 0; i < quotaLimit; i++ {
		f.decide(t, f.admit(t, "old"))
	}

	// Past the window. A quota that never expires is a ban with a different
	// name, and the clock is injected precisely so crossing the boundary costs
	// no wall time (docs/TEST_ARCHITECTURE.md forbids sleeping for it).
	f.now = f.now.Add(2 * time.Hour)

	decision := f.decide(t, f.admit(t, "fresh"))
	assert.Truef(t, decision.Admitted(),
		"an author whose earlier posts have aged out of the window must be admitted again; a rolling window that never "+
			"releases is a permanent refusal nobody chose: %+v", decision)
}

func TestDeciderQuota_RefusalsDoNotConsumeQuota(t *testing.T) {
	t.Parallel()

	f := newQuotaFixture(t)
	for i := 0; i < quotaLimit; i++ {
		f.decide(t, f.admit(t, "filled"))
	}

	// A refused post, recorded as the engine would record it.
	refused := f.admit(t, "refused")
	_, err := f.admissions.RecordRejection(context.Background(), posts.RecordRejectionCommand{
		CommunityDID: f.communityDID,
		PostURI:      refused,
		DecisionCode: string(posts.DecisionRateLimitExceeded),
		JudgedCID:    "bafyreirefused",
		Redrivable:   false,
	})
	require.NoError(t, err)

	// §8: a refusal consumes no quota. Counting rejected rows would let an
	// author who keeps posting past their limit extend their own lockout
	// indefinitely — each refusal making the next one more certain.
	f.now = f.now.Add(2 * time.Hour)
	decision := f.decide(t, f.admit(t, "afterrefusal"))
	assert.Truef(t, decision.Admitted(),
		"a rejected admission was counted against the quota. §8 is explicit that a refusal consumes nothing, or an "+
			"author past their limit extends their own lockout every time they try: %+v", decision)
}

func TestDeciderQuota_TrustedAggregatorsAreExempt(t *testing.T) {
	t.Parallel()

	f := newQuotaFixture(t)
	f.trusted[f.authorDID] = true

	// A trusted aggregator has no submission limit today (admit.go's check
	// order), and inventing one here would be a silent production behaviour
	// change smuggled in under a new code path — the Kagi bridge posts far more
	// than any per-author quota would allow.
	for i := 0; i < quotaLimit+2; i++ {
		decision := f.decide(t, f.admit(t, "bridge"))
		assert.Truef(t, decision.Admitted(),
			"a trusted aggregator's post %d was refused; the trusted class skips the quota, and applying it would stop "+
				"the bridge dead at the limit: %+v", i+1, decision)
	}
}
