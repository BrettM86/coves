//go:build integration

package posts_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What CreatePost does differently when the author is an aggregator rather than
// a person.
//
// An aggregator is a service that writes into communities it does not belong
// to, so the membership and visibility rules a human is held to say nothing
// useful about it. CreatePost swaps them for two others (service.go steps 3, 4
// and 9): the community must have published an authorization record naming
// this aggregator, and the aggregator must be inside its hourly quota. Both are
// checked BEFORE anything reaches the community's repository, and a successful
// post is then recorded against the aggregator — which is what makes the next
// quota check meaningful.
//
// This is where the branch is worth testing rather than in the aggregator
// domain itself: aggregators.ValidateAggregatorPost has its own tests
// (internal/core/aggregators/service_validate_post_test.go) and knows nothing
// about posts. What is unproven without these is the WIRING — that CreatePost
// consults it at all, that it does so before writing, and that it records the
// result afterwards. A service that skipped every one of those calls would pass
// every test in both packages otherwise.
//
// These assertions used to live in tests/integration/aggregator_e2e_test.go,
// where the aggregator's post went through the HTTP handler and the "Jetstream"
// step that indexed it was a hand-built event the test fed to a consumer it had
// constructed itself. Ingestion is now tests/e2e/'s contract; what is left is
// this, the part that decides whether a record gets written at all.

// aggregatorFixture is the post service wired with a real aggregator service,
// over a community that has authorized one aggregator.
type aggregatorFixture struct {
	base          *postFixture
	service       posts.Service
	index         aggregators.Repository
	aggregator    *testkit.Account
	aggregatorDID string

	// The authorization's AT-URI, reused on every re-index so that flipping
	// `enabled` updates the community's existing record instead of adding a
	// second one beside it.
	authorizationURI string
}

// newAggregatorFixture declares an aggregator, has the fixture's community
// authorize it, and points a post service at both.
//
// THE AGGREGATOR NOW NEEDS A REPOSITORY OF ITS OWN. It used to need only an
// identity the AppView had indexed, because a post lived in the COMMUNITY's repo
// and named its author in a field. Under §4.2 step 3 an aggregator writes into
// its own repo like any other author — through its stored OAuth tokens
// (migration 025) in production, through a registered PDS account here — so the
// fixture provisions one and registers it with the author-repo factory.
func newAggregatorFixture(t *testing.T) *aggregatorFixture {
	t.Helper()

	base := newPostFixture(t)
	ctx := context.Background()

	index := postgres.NewAggregatorRepository(base.db)
	aggregatorAccount := base.authorRepos.register(
		base.pds.CreateAccount(t, testkit.WithHandlePrefix("ag")))
	aggregatorDID := aggregatorAccount.DID
	require.NoError(t, index.CreateAggregator(ctx, &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "RSS Feed Aggregator",
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   "at://" + aggregatorDID + "/social.coves.aggregator.service/self",
		RecordCID:   "bafyservice",
	}))

	f := &aggregatorFixture{
		base: base,
		service: posts.NewPostService(
			postgres.NewPostRepository(base.db), base.communityService,
			aggregators.NewAggregatorService(index, base.communityService),
			nil, nil, nil, base.pds.URL(),
			append(base.writePathOptions(),
				// The aggregator's OWN hourly quota is the subject here; the §8
				// per-author policy is opted out of explicitly.
				posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))...),
		index:         index,
		aggregator:    aggregatorAccount,
		aggregatorDID: aggregatorDID,
		authorizationURI: "at://" + base.community.DID +
			"/social.coves.aggregator.authorization/" + testkit.UniqueID(t),
	}
	f.setAuthorization(t, true)
	return f
}

// setAuthorization indexes the authorization record the community publishes to
// grant or revoke posting rights. A revocation keeps the record and clears its
// enabled flag, which is how one arrives over the firehose — deleting it would
// throw away the audit trail of who granted access in the first place.
func (f *aggregatorFixture) setAuthorization(t *testing.T, enabled bool) {
	t.Helper()

	auth := &aggregators.Authorization{
		AggregatorDID: f.aggregatorDID,
		CommunityDID:  f.base.community.DID,
		Enabled:       enabled,
		CreatedBy:     f.base.community.DID,
		CreatedAt:     time.Now(),
		IndexedAt:     time.Now(),
		RecordURI:     f.authorizationURI,
		RecordCID:     "bafyauthorization",
	}
	if !enabled {
		disabledAt := time.Now()
		auth.DisabledAt = &disabledAt
		auth.DisabledBy = f.base.community.DID
	}
	require.NoError(t, f.index.CreateAuthorization(context.Background(), auth))
}

// createPost posts as the aggregator. The DID goes into the context as well as
// the request because CreatePost re-checks the two against each other.
func (f *aggregatorFixture) createPost(t *testing.T, title string) (*posts.CreatePostResponse, error) {
	t.Helper()

	content := "syndicated from a feed"
	return f.service.CreatePost(
		middleware.SetTestUserDID(context.Background(), f.aggregatorDID),
		nil, // an aggregator authenticates by API key: there is no browser session, and
		//    the service resolves its stored tokens instead (§4.2 step 3).
		posts.CreatePostRequest{
			Community: f.base.community.DID,
			Title:     &title,
			Content:   &content,
			AuthorDID: f.aggregatorDID,
		})
}

// postsThisHour is the count the quota is enforced against.
func (f *aggregatorFixture) postsThisHour(t *testing.T) int {
	t.Helper()

	count, err := f.index.CountRecentPosts(context.Background(),
		f.aggregatorDID, f.base.community.DID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	return count
}

func TestService_AuthorizedAggregatorPostsAndIsBilledForIt(t *testing.T) {
	t.Parallel()

	f := newAggregatorFixture(t)
	resp, err := f.createPost(t, "Breaking news from an RSS feed")
	require.NoError(t, err)

	// The record lands in the AGGREGATOR's own repo like any other author's post,
	// naming the community it was submitted to. Read back from the PDS rather
	// than from the response, because the response is the service quoting itself.
	assert.Equal(t, "at://"+f.aggregatorDID+"/"+posts.PostV2Collection+"/"+rkeyOf(t, resp.URI), resp.URI,
		"an aggregator's post belongs to the aggregator's repo, not to the community's")

	record := f.aggregator.GetRecord(t, posts.PostV2Collection, rkeyOf(t, resp.URI))
	assert.Equal(t, f.base.community.DID, record.Value["community"])
	assert.NotContains(t, record.Value, "author",
		"authorship is the repo the record lives in — an aggregator's post is no exception")

	// And the post is billed against the aggregator's quota. Without this the
	// rate limit would never engage, because CreatePost records the post itself
	// (step 12) and nothing else in the system does.
	assert.Equal(t, 1, f.postsThisHour(t))
}

// A declared aggregator with no authorization from this community. Being
// registered with the instance is not permission to write anywhere.
func TestService_UnauthorizedAggregatorIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()

	f := newAggregatorFixture(t)

	// The fixture's authorization is removed rather than disabled: this is the
	// aggregator the community has never heard of, which the next test
	// distinguishes from the one it has revoked.
	require.NoError(t, f.index.DeleteAuthorization(context.Background(),
		f.aggregatorDID, f.base.community.DID))

	_, err := f.createPost(t, "a post nobody asked for")
	require.Error(t, err)
	assert.True(t, aggregators.IsUnauthorized(err),
		"the handler maps this to a 403, so the wrapped sentinel is the contract; got: %v", err)

	// Refused means nothing was written and nothing was billed. The check runs
	// ahead of the PDS write, and an implementation that reordered them would
	// leave a record in a community that never authorized it.
	assert.Zero(t, f.postsThisHour(t))
}

// Revocation has to take effect on the next post, not on the next restart.
func TestService_RevokingAnAuthorizationStopsTheNextPost(t *testing.T) {
	t.Parallel()

	f := newAggregatorFixture(t)
	_, err := f.createPost(t, "posted while authorized")
	require.NoError(t, err)

	f.setAuthorization(t, false)

	_, err = f.createPost(t, "posted after the community said no")
	require.Error(t, err)
	assert.True(t, aggregators.IsUnauthorized(err),
		"a disabled authorization must refuse exactly like an absent one; got: %v", err)
}

// The quota is ten posts per community per rolling hour. Ten real posts are
// worth the cost here because the count they are checked against is the one
// CreatePost itself writes: this is the only test in the tree where the
// producer and the consumer of that counter are the same code path.
//
// THE TITLES ARE DISTINCT, AND THAT IS NOT COSMETIC. This loop used to submit
// ten copies of one string, which worked only while the record key came from
// the clock: ten identical submissions produced ten records because each got a
// fresh TID. Under the write-path flip the key is derived from the submission's
// own content (§4.2), so ten identical submissions converge on ONE record — the
// second through tenth find their own post already standing and are reported
// back the URI of the first, which is the designed idempotence and not a bug to
// route around. The quota, meanwhile, is metered per ACCEPTED submission, so the
// old loop would still reach ten and then assert the eleventh was refused while
// exactly one post existed. Distinct titles keep the premise honest: ten
// submissions, ten posts, ten metered, and the eleventh refused.
func TestService_AggregatorQuotaStopsTheEleventhPost(t *testing.T) {
	t.Parallel()

	f := newAggregatorFixture(t)

	for i := 0; i < aggregators.RateLimitMaxPosts; i++ {
		_, err := f.createPost(t, fmt.Sprintf("syndicated item %d", i))
		require.NoErrorf(t, err, "post %d of %d was refused inside the quota", i+1, aggregators.RateLimitMaxPosts)
	}
	require.Equal(t, aggregators.RateLimitMaxPosts, f.postsThisHour(t))

	_, err := f.createPost(t, "one item too many")
	require.Error(t, err)
	assert.True(t, aggregators.IsRateLimited(err),
		"the handler maps this to a 429; got: %v", err)

	// The refused post was not written, so an aggregator cannot spend past its
	// quota by ignoring the error.
	assert.Equal(t, aggregators.RateLimitMaxPosts, f.postsThisHour(t))

	// The same ten posts seen through the aggregator's own row, which is what
	// social.coves.aggregator.getServices?detailed=true serves as
	// stats.postsCreated.
	//
	// Worth the extra read because the two numbers are produced by different
	// machinery over the same rows: the quota is a COUNT(*) inside a one-hour
	// window, the stat is a column a trigger increments per insert
	// (migrations/012, update_aggregator_posts_count). They are only equal
	// while every insert fires the trigger — and the stat is the one a
	// third-party client sees, so a trigger dropped by a migration would show
	// up here and nowhere else. Every other assertion about this field in the
	// suite, including the pipeline contract's, is against zero.
	declared, err := f.index.GetAggregator(context.Background(), f.aggregatorDID)
	require.NoError(t, err)
	assert.Equal(t, aggregators.RateLimitMaxPosts, declared.PostsCreated,
		"the aggregator's stats.postsCreated must count the posts it actually made")
	assert.Equal(t, 1, declared.CommunitiesUsing,
		"and its communitiesUsing must still be the one community that authorized it — asserted "+
			"alongside postsCreated because the two are adjacent ints on the same view")
}
