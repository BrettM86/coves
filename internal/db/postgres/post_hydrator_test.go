//go:build integration

package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B14: HydratePosts returns, in input order, exactly the posts the old hot
// feed of the same scope would show the viewer, as the same PostViews.
func TestPostHydrator_FiltersInvisiblePostsInInputOrder(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		viewer     = "did:plc:hydviewer"
		authorA    = "did:plc:hydauthora"
		authorB    = "did:plc:hydauthorb"
		restricted = "did:plc:hydrestricted"
		blocked    = "did:plc:hydblocked"

		subscribed       = "did:plc:hydsubscribed"
		unsubscribed     = "did:plc:hydunsubscribed"
		blockedCommunity = "did:plc:hydblockedcommunity"
	)
	for _, did := range []string{viewer, authorA, authorB, restricted, blocked} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	for _, did := range []string{subscribed, unsubscribed, blockedCommunity} {
		createTestCommunity(t, db, did, "c-"+did[len("did:plc:"):]+".coves.social", authorA)
	}
	subscribeAtLevel(t, db, viewer, subscribed, 3)
	subscribeAtLevel(t, db, viewer, blockedCommunity, 3)
	insertCommunityBlock(t, db, viewer, blockedCommunity)
	insertUserBlock(t, db, viewer, blocked)
	accountstatusfixture.Set(t, db, restricted, accountstatus.Takendown)

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }

	visible := seedAcceptedHotPost(t, db, subscribed, authorA, "visible", at(hours(2)), 5) // 0.92
	pendingOther := seedHotBoundPost(t, db, subscribed, authorB, "pendingother", at(hours(1)), 10)
	seedHotBoundPendingAdmission(t, db, subscribed, pendingOther)
	ownPending := seedHotBoundPost(t, db, subscribed, viewer, "ownpending", at(hours(3)), 3) // 0.66
	seedHotBoundPendingAdmission(t, db, subscribed, ownPending)
	removed := seedAcceptedHotPost(t, db, subscribed, authorA, "removed", at(hours(1)), 10)
	insertDiscoverHotRemoval(t, db, removed)
	deleted := seedAcceptedHotPost(t, db, subscribed, authorA, "deleted", at(hours(1)), 10)
	_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
	require.NoError(t, err)
	blockedAuthor := seedAcceptedHotPost(t, db, subscribed, blocked, "blockedauthor", at(hours(1)), 10)
	restrictedAuthor := seedAcceptedHotPost(t, db, subscribed, restricted, "restrictedauthor", at(hours(1)), 10)
	otherCommunity := seedAcceptedHotPost(t, db, unsubscribed, authorA, "othercommunity", at(hours(1)), 10)
	nonexistent := frequencyPostURI(subscribed, "nonexistent")
	inBlockedCommunity := seedAcceptedHotPost(t, db, blockedCommunity, authorA, "inblockedcommunity", at(hours(1)), 10)
	// Newest and highest ranked, listed last: input order is not rank order.
	visibleLast := seedAcceptedHotPost(t, db, subscribed, authorA, "visiblelast", at(hours(1)), 50) // 2.05

	everyKind := []string{
		visible, pendingOther, ownPending, removed, deleted, blockedAuthor,
		restrictedAuthor, otherCommunity, nonexistent, inBlockedCommunity, visibleLast,
	}

	restriction := WithAccountRestrictionAuthority(fixtures.InstanceDID())
	// The old feeds (the feed oracle: today's GetTimeline and GetCommunityFeed
	// SQL and row scan) are the reference HydratePosts is compared against.
	oracle := newFeedOracle(db, getFeedEquivalenceCursorSecret, restriction)

	// The old feeds' PostViews, by URI. Their served sets prove the fixture.
	homeFeed, _, err := oracleFeedPage(t.Context(), oracle, oracleTimeline, oracleFeedRequest{ViewerDID: viewer, Sort: "hot", Limit: 50})
	require.NoError(t, err)
	homeURIs := make([]string, 0, len(homeFeed))
	homeViews := make(map[string]*posts.PostView)
	for _, post := range homeFeed {
		homeURIs = append(homeURIs, post.URI)
		homeViews[post.URI] = post
	}
	require.Equal(t, []string{visibleLast, visible, ownPending}, homeURIs, "old home hot: fixture")
	communityViews := func(communityDID string) ([]string, map[string]*posts.PostView) {
		t.Helper()
		feed, _, err := oracleFeedPage(t.Context(), oracle, oracleCommunity, oracleFeedRequest{
			Community: communityDID, ViewerDID: viewer, Sort: "hot", Limit: 50,
		})
		require.NoError(t, err)
		uris := make([]string, 0, len(feed))
		views := make(map[string]*posts.PostView)
		for _, post := range feed {
			uris = append(uris, post.URI)
			views[post.URI] = post
		}
		return uris, views
	}
	subscribedURIs, subscribedViews := communityViews(subscribed)
	require.Equal(t, []string{visibleLast, visible, ownPending}, subscribedURIs, "old community hot: fixture")
	blockedCommunityURIs, blockedCommunityViews := communityViews(blockedCommunity)
	require.Equal(t, []string{inBlockedCommunity}, blockedCommunityURIs,
		"old community hot: the community's own feed ignores the viewer's community block")

	hydrator := NewPostHydrator(db, restriction)
	cases := []struct {
		name     string
		scope    feedgen.Scope
		uris     []string
		want     []string
		oldViews map[string]*posts.PostView
	}{
		{
			// The blocked community's post is excluded (aggregate surface).
			name:     "home",
			scope:    feedgen.Scope{Name: feedgen.ScopeHome},
			uris:     everyKind,
			want:     []string{visible, ownPending, visibleLast},
			oldViews: homeViews,
		},
		{
			name:     "community",
			scope:    feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: subscribed},
			uris:     everyKind,
			want:     []string{visible, ownPending, visibleLast},
			oldViews: subscribedViews,
		},
		{
			// Explicit surface: the viewer's block on this very community
			// does not hide its posts.
			name:     "community the viewer blocks",
			scope:    feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: blockedCommunity},
			uris:     []string{inBlockedCommunity, visible},
			want:     []string{inBlockedCommunity},
			oldViews: blockedCommunityViews,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hydrated, err := hydrator.HydratePosts(t.Context(), feedgen.HydrateRequest{
				ViewerDID: viewer,
				Scope:     tc.scope,
				URIs:      tc.uris,
			})
			require.NoError(t, err)

			uris := make([]string, 0, len(hydrated))
			for _, view := range hydrated {
				uris = append(uris, view.URI)
			}
			require.Equal(t, tc.want, uris)

			for _, view := range hydrated {
				old, ok := tc.oldViews[view.URI]
				require.Truef(t, ok, "%s is not in the old feed", view.URI)
				wantJSON, err := json.Marshal(old)
				require.NoError(t, err)
				gotJSON, err := json.Marshal(view)
				require.NoError(t, err)
				assert.JSONEqf(t, string(wantJSON), string(gotJSON), "%s: hydrated PostView equals the old feed's", view.URI)
			}
		})
	}
}

// B13: HydratePosts at scope all revalidates explicitly supplied URIs the way
// Discover does: it drops a post by an author the viewer blocks, a post in a
// community the viewer blocks (aggregate surface), a moderator-removed post,
// another author's pending post and a post in an instance-removed community,
// and returns the rest in input order as Discover's own PostViews.
func TestPostHydrator_ScopeAllRevalidatesSuppliedPosts(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		viewer  = "did:plc:hydallviewer"
		author  = "did:plc:hydallauthor"
		other   = "did:plc:hydallother"
		blocked = "did:plc:hydallblocked"

		plain            = "did:plc:hydallplain"
		blockedCommunity = "did:plc:hydallblockedc"
		removedCommunity = "did:plc:hydallremovedc"
	)
	for _, did := range []string{viewer, author, other, blocked} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	for _, did := range []string{plain, blockedCommunity, removedCommunity} {
		createTestCommunity(t, db, did, "c-"+did[len("did:plc:"):]+".coves.social", author)
	}
	insertUserBlock(t, db, viewer, blocked)
	insertCommunityBlock(t, db, viewer, blockedCommunity)

	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(age time.Duration) time.Time { return base.Add(-age) }

	visible := seedAcceptedHotPost(t, db, plain, author, "visible", at(hours(3)), 5)
	blockedAuthor := seedAcceptedHotPost(t, db, plain, blocked, "blockedauthor", at(hours(2)), 10)
	inBlockedCommunity := seedAcceptedHotPost(t, db, blockedCommunity, author, "inblockedc", at(hours(2)), 10)
	removed := seedAcceptedHotPost(t, db, plain, author, "removed", at(hours(2)), 10)
	insertDiscoverHotRemoval(t, db, removed)
	pending := seedHotBoundPost(t, db, plain, other, "pending", at(hours(2)), 10)
	seedHotBoundPendingAdmission(t, db, plain, pending)
	inRemovedCommunity := seedAcceptedHotPost(t, db, removedCommunity, author, "inremovedc", at(hours(2)), 10)
	removeCommunityAtInstance(t, db, removedCommunity)
	// Newest and highest scored, listed last: input order is not rank order.
	visibleLast := seedAcceptedHotPost(t, db, plain, author, "visiblelast", at(hours(1)), 50)

	everyKind := []string{visible, blockedAuthor, inBlockedCommunity, removed, pending, inRemovedCommunity, visibleLast}

	restriction := WithAccountRestrictionAuthority(fixtures.InstanceDID())
	// Discover new's PostViews (the feed oracle: today's GetDiscover New SQL
	// and row scan), by URI. Its served sets prove the fixture.
	oracle := newFeedOracle(db, getFeedEquivalenceCursorSecret, restriction)
	discoverViews := func(viewerDID string) ([]string, map[string]*posts.PostView) {
		t.Helper()
		feed, _, err := oracleFeedPage(t.Context(), oracle, oracleDiscover, oracleFeedRequest{ViewerDID: viewerDID, Sort: "new", Limit: 50})
		require.NoError(t, err)
		uris := make([]string, 0, len(feed))
		views := make(map[string]*posts.PostView)
		for _, post := range feed {
			uris = append(uris, post.URI)
			views[post.URI] = post
		}
		return uris, views
	}
	signedInURIs, signedInViews := discoverViews(viewer)
	require.Equal(t, []string{visibleLast, visible}, signedInURIs, "Discover new signed in: fixture")
	anonymousURIs, anonymousViews := discoverViews("")
	require.ElementsMatch(t, []string{visibleLast, visible, blockedAuthor, inBlockedCommunity}, anonymousURIs,
		"Discover new anonymous: fixture")

	hydrator := NewPostHydrator(db, restriction)
	cases := []struct {
		name     string
		viewer   string
		want     []string
		oldViews map[string]*posts.PostView
	}{
		{name: "signed in", viewer: viewer, want: []string{visible, visibleLast}, oldViews: signedInViews},
		{
			// The viewer's blocks do not apply to an anonymous reader.
			name: "anonymous", viewer: "",
			want:     []string{visible, blockedAuthor, inBlockedCommunity, visibleLast},
			oldViews: anonymousViews,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hydrated, err := hydrator.HydratePosts(t.Context(), feedgen.HydrateRequest{
				ViewerDID: tc.viewer,
				Scope:     feedgen.Scope{Name: feedgen.ScopeAll},
				URIs:      everyKind,
			})
			require.NoError(t, err)

			uris := make([]string, 0, len(hydrated))
			for _, view := range hydrated {
				uris = append(uris, view.URI)
			}
			require.Equal(t, tc.want, uris)

			for _, view := range hydrated {
				old, ok := tc.oldViews[view.URI]
				require.Truef(t, ok, "%s is not in Discover new", view.URI)
				wantJSON, err := json.Marshal(old)
				require.NoError(t, err)
				gotJSON, err := json.Marshal(view)
				require.NoError(t, err)
				assert.JSONEqf(t, string(wantJSON), string(gotJSON), "%s: hydrated PostView equals Discover's", view.URI)
			}
		})
	}
}
