//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The timeline contract: social.coves.feed.getTimeline, the personalised home
// feed, served to a signed-in viewer.
//
// # WHY THIS IS ITS OWN FILE
//
// Subscription fan-out is what a subscription DOES, but it is observed on the
// timeline's endpoint, answered by the timeline's repository join
// (internal/db/postgres/timeline_repo.go), and every other thing worth pinning
// about the home feed — what a block hides from it, which pending posts it
// withholds — is a claim about that same endpoint for a given viewer. So the
// contract lives with the endpoint rather than inside the subscription
// collection's ingestion proof, and subscription_contract_test.go points here.
//
// # WHY THERE IS A SECOND VIEWER
//
// A post appearing in the subscriber's timeline proves the post and the
// subscription were both indexed and that the join finds them. It does not
// prove the join is SCOPED to the viewer: a timeline that forgot
// `cs.user_did = $1` would serve every subscribed community's posts to anyone
// signed in, and the subscriber would still see the post. The second viewer
// subscribes to nothing, so the only way the post can reach them is that
// failure. Their read is made only after the subscriber's timeline has shown
// the post, because before that the absence would prove nothing — the post might
// simply not have been indexed yet.

// timelineURIs reads a viewer's first page of social.coves.feed.getTimeline and
// returns the post URIs it serves, in order.
func timelineURIs(ctx context.Context, viewer *testkit.AppView) ([]string, error) {
	var timeline struct {
		Feed []struct {
			Post struct {
				URI string `json:"uri"`
			} `json:"post"`
		} `json:"feed"`
	}
	if err := viewer.Query(ctx, "social.coves.feed.getTimeline", nil, &timeline); err != nil {
		return nil, err
	}
	uris := make([]string, 0, len(timeline.Feed))
	for _, item := range timeline.Feed {
		uris = append(uris, item.Post.URI)
	}
	return uris, nil
}

// TestTimelineContract_SubscribedCommunityPostsReachTheSubscribersTimeline is
// subscription fan-out end to end: a subscription made through the AppView as
// the subscriber, a post accepted into the community by its community, and the
// post served in the subscriber's personalised timeline — and not in the
// timeline of a signed-in viewer who never subscribed.
//
// The subscriber count is awaited between the subscription and the post so that
// a timeline which never shows the post fails on a subscription already known to
// be indexed, and names the fan-out rather than the subscription pipeline.
func TestTimelineContract_SubscribedCommunityPostsReachTheSubscribersTimeline(t *testing.T) {
	p := newPipeline(t)

	// Given a community, an author, a subscriber and a bystander, the latter two
	// signed in once each (every sign-in spends two of the login limiter's ten
	// per minute, or three if the AppView's dev-mode host-canonicalization
	// redirect runs, so none happens inside a wait).
	creator := p.IndexedAccount(t, "tlc")
	author := p.IndexedAccount(t, "tla")
	subscriberAccount := p.IndexedAccount(t, "tls")
	bystanderAccount := p.IndexedAccount(t, "tlv")
	community := indexedCommunity(t, p, "tl", creator.DID)
	subscriber := p.AppView.As(p.AppView.SignIn(t, subscriberAccount))
	bystander := p.AppView.As(p.AppView.SignIn(t, bystanderAccount))

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// When the subscriber subscribes to the community through the AppView.
	var subscribed struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, subscriber.Procedure(ctx, "social.coves.community.subscribe",
		map[string]string{"community": community.DID}, &subscribed),
		"social.coves.community.subscribe must accept %s's sealed session", subscriberAccount.Handle)
	require.True(t, strings.HasPrefix(subscribed.URI, "at://"+subscriberAccount.DID+"/"+subscriptionCollection+"/"),
		"subscribe's uri %q must name a %s record in %s's own repo", subscribed.URI, subscriptionCollection, subscriberAccount.Handle)
	require.NotEmpty(t, subscribed.CID, "subscribe answered without a cid")

	p.Await(t, "the subscription made through the AppView to be indexed against the community", func() (bool, error) {
		view, err := p.Community(context.Background(), community.DID)
		if err != nil {
			return false, err
		}
		return view.SubscriberCount == 1, nil
	})

	// And the author posts into the community, and the community accepts it.
	post := indexedPost(t, p, community, author, "a post for the subscriber's timeline")

	// Then the subscriber's timeline serves that exact post.
	p.Await(t, "the accepted post to reach the subscriber's personalised timeline", func() (bool, error) {
		uris, err := timelineURIs(context.Background(), subscriber)
		if err != nil {
			return false, err
		}
		for _, uri := range uris {
			if uri == post.URI {
				return true, nil
			}
		}
		return false, nil
	})

	// And, now that the post is known to be served, a signed-in viewer who never
	// subscribed does not get it. The read gets its own budget: ctx predates the
	// waits above, each of which has its own.
	readCtx, readCancel := context.WithTimeout(context.Background(), contractBudget)
	defer readCancel()
	uris, err := timelineURIs(readCtx, bystander)
	require.NoError(t, err, "social.coves.feed.getTimeline must serve %s, who subscribes to nothing", bystanderAccount.Handle)
	require.NotContains(t, uris, post.URI,
		"%s's timeline serves a post from a community they never subscribed to: the timeline is not scoped to its viewer",
		bystanderAccount.Handle)
}
