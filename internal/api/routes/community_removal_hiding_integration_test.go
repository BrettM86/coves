//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/fixtures"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityRemovalHidingSearchTerm   = "ocelotquandary"
	communityRemovalHidingCommentText  = "a comment on a post in the community the instance removes"
	communityRemovalHidingLiveComment  = "a comment on a post in a live community"
	communityRemovalHidingPostGetPath  = "/xrpc/social.coves.community.post.get"
	communityRemovalHidingThreadPath   = "/xrpc/social.coves.community.comment.getComments"
	communityRemovalHidingViewerOther  = "other viewer"
	communityRemovalHidingViewerPublic = "signed-out"
	communityRemovalHidingViewerAuthor = "author"
)

// communityRemovalHidingScene is the route fixture's community C
// (f.communityDID) with post P by author A (f.authorDID) and comment K by
// commenter X on P, post P2 in C under an active instance removal and post P3
// in C withdrawn by C itself, plus a live community D with A's post L and X's
// comment K2 on L. A and V (f.nonAdminDID) are subscribed to C and D.
type communityRemovalHidingScene struct {
	f                    contentLabelRouteFixture
	moderator            *communityremovalfixture.Moderator
	liveCommunity        string
	commenter            string
	post                 string
	comment              string
	removedByInstance    string
	withdrawnByCommunity string
	livePost             string
	liveComment          string
	viewerTokenFor       map[string]string
}

func newCommunityRemovalHidingScene(t *testing.T) communityRemovalHidingScene {
	t.Helper()
	f := newContentLabelRouteFixture(t)
	fixtures.User(t, f.db, testkit.UniqueIDWithPrefix(t, "removalviewer")+".test", f.nonAdminDID)
	commenterName := testkit.UniqueIDWithPrefix(t, "removalcommenter")
	commenter := fixtures.DID(commenterName)
	fixtures.User(t, f.db, commenterName+".test", commenter)
	liveName := testkit.UniqueIDWithPrefix(t, "livecommunity")
	liveCommunity, err := fixtures.Community(t.Context(), f.db, liveName, "owner"+liveName)
	require.NoError(t, err)
	for _, subscriber := range []string{f.authorDID, f.nonAdminDID} {
		for _, community := range []string{f.communityDID, liveCommunity} {
			_, err := f.db.ExecContext(t.Context(),
				`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, subscriber, community)
			require.NoError(t, err)
		}
	}

	post := f.insertPost("post in the removed community " + communityRemovalHidingSearchTerm)
	comment := moderationAcceptanceInsertComment(t, f.db, commenter, post, f.postCID, post, f.postCID,
		"bafyreicommunityremovalhidingcomment", communityRemovalHidingCommentText)
	livePost := f.insertPostIn(liveCommunity, f.authorDID, "post in the live community "+communityRemovalHidingSearchTerm)
	liveComment := moderationAcceptanceInsertComment(t, f.db, commenter, livePost, f.postCID, livePost, f.postCID,
		"bafyreicommunityremovalhidinglivecomment", communityRemovalHidingLiveComment)

	removedByInstance := f.insertPost("post the instance removed on its own")
	removed := moderationAcceptanceBody(t, f.post(t, moderationRouteRemovePath, f.adminToken, map[string]any{
		"subject": map[string]string{"uri": removedByInstance, "cid": f.postCID}, "expectedVersion": "v0",
		"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
	}))
	require.Equal(t, "applied", removed["outcome"], "fixture: the instance removes P2")

	withdrawnByCommunity := f.insertPost("post its community withdrew")
	_, err = f.db.ExecContext(t.Context(), `
		UPDATE community_post_admissions SET status = 'removed', accepted_cid = NULL,
			decision_code = 'rule-violation', decision_at = NOW(), updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2
	`, f.communityDID, withdrawnByCommunity)
	require.NoError(t, err)

	return communityRemovalHidingScene{
		f: f, moderator: communityremovalfixture.New(f.db), liveCommunity: liveCommunity, commenter: commenter,
		post: post, comment: comment, removedByInstance: removedByInstance, withdrawnByCommunity: withdrawnByCommunity,
		livePost: livePost, liveComment: liveComment,
		viewerTokenFor: map[string]string{
			communityRemovalHidingViewerOther:  f.nonAdminToken,
			communityRemovalHidingViewerPublic: "",
			communityRemovalHidingViewerAuthor: f.authorToken,
		},
	}
}

func (s communityRemovalHidingScene) request(t *testing.T, target, viewer string) subjectStateHTTPResponse {
	t.Helper()
	token, ok := s.viewerTokenFor[viewer]
	require.Truef(t, ok, "unknown viewer %q", viewer)
	return moderationAcceptanceRequest(t, s.f.client, http.MethodGet, s.f.server.URL+target, token, nil)
}

func (s communityRemovalHidingScene) get(t *testing.T, target, viewer string) map[string]any {
	t.Helper()
	return moderationAcceptanceBody(t, s.request(t, target, viewer))
}

func (s communityRemovalHidingScene) communityFeed(community string) string {
	return "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {community}, "sort": {"new"}}.Encode()
}

func (s communityRemovalHidingScene) thread(post string) string {
	return communityRemovalHidingThreadPath + "?" + url.Values{"post": {post}, "sort": {"new"}}.Encode()
}

func (s communityRemovalHidingScene) postGet(uris ...string) string {
	return communityRemovalHidingPostGetPath + "?" + url.Values{"uris": uris}.Encode()
}

// postGetItems returns post.get's items keyed by uri.
func (s communityRemovalHidingScene) postGetItems(t *testing.T, viewer string, uris ...string) map[string]map[string]any {
	t.Helper()
	items := moderationAcceptanceArray(t, s.get(t, s.postGet(uris...), viewer)["posts"])
	require.Len(t, items, len(uris))
	byURI := make(map[string]map[string]any, len(items))
	for _, entry := range items {
		item := moderationAcceptanceObject(t, entry)
		uri, ok := item["uri"].(string)
		require.Truef(t, ok, "post.get item without a uri: %v", item)
		byURI[uri] = item
	}
	return byURI
}

// communityRemovalHidingRequirePostView checks item is a full postView for uri: no tombstone
// discriminator and a record.
func communityRemovalHidingRequirePostView(t *testing.T, item map[string]any, uri string) {
	t.Helper()
	require.Equal(t, uri, item["uri"])
	assert.NotContains(t, item, "$type", "a full postView carries no tombstone type")
	for _, key := range []string{"notFound", "removed", "blocked"} {
		assert.NotContainsf(t, item, key, "a full postView carries no %s marker", key)
	}
	assert.NotNil(t, item["record"], "a full postView serves its record")
}

// communityRemovalHidingRequireNotPostView checks item is a tombstone union
// member (notFoundPost, removedPost or moderatedPost), not a postView.
func communityRemovalHidingRequireNotPostView(t *testing.T, item map[string]any, what string) {
	t.Helper()
	tombstone := item["notFound"] == true || item["removed"] == true ||
		item["$type"] == "social.coves.community.post.defs#moderatedPost"
	assert.Truef(t, tombstone, "%s must not be a postView: %v", what, item)
}

// communityRemovalHidingFeedPosts returns a feed's post views keyed by uri.
func communityRemovalHidingFeedPosts(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	posts := map[string]map[string]any{}
	for _, entry := range moderationAcceptanceArray(t, body["feed"]) {
		post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["post"])
		uri, ok := post["uri"].(string)
		require.True(t, ok)
		posts[uri] = post
	}
	return posts
}

// communityRemovalHidingRow is one read path checked for each listed viewer
// before and during the removal of community C.
type communityRemovalHidingRow struct {
	name    string
	target  string
	viewers []string
	check   func(t *testing.T, body map[string]any, viewer string, removed bool)
}

// communityRemovalHidingFeedRow checks that P is listed before the removal and
// absent while C is removed, and that the control L is listed when control is
// set.
func communityRemovalHidingFeedRow(s communityRemovalHidingScene, control bool) func(*testing.T, map[string]any, string, bool) {
	return func(t *testing.T, body map[string]any, _ string, removed bool) {
		t.Helper()
		listed := moderationPostAcceptanceFeedURIs(t, body)
		if control {
			assert.Contains(t, listed, s.livePost, "the live community's post stays listed")
		}
		if removed {
			assert.NotContains(t, listed, s.post, "a removed community's post must be absent")
			return
		}
		assert.Contains(t, listed, s.post)
	}
}

// TestCommunityRemovalHidesContentFromFeedsSearchAndProfiles reads the
// timeline, C's community feed, Discover (new, top and hot), search, A's
// profile posts and X's profile comments before and while C is removed. While
// removed, P and K are absent for another viewer and a signed-out viewer, and P
// is absent for A everywhere but A's own profile, where it carries status
// "removed". D's post L and comment K2 stay listed throughout.
func TestCommunityRemovalHidesContentFromFeedsSearchAndProfiles(t *testing.T) {
	s := newCommunityRemovalHidingScene(t)
	others := []string{communityRemovalHidingViewerOther, communityRemovalHidingViewerPublic}
	everyone := append(append([]string{}, others...), communityRemovalHidingViewerAuthor)
	rows := []communityRemovalHidingRow{
		{
			// The timeline requires sign-in.
			name: "timeline", target: "/xrpc/social.coves.feed.getTimeline?sort=new",
			viewers: []string{communityRemovalHidingViewerOther, communityRemovalHidingViewerAuthor},
			check:   communityRemovalHidingFeedRow(s, true),
		},
		{
			name: "removed community feed", target: s.communityFeed(s.f.communityDID),
			viewers: everyone, check: communityRemovalHidingFeedRow(s, false),
		},
		{
			name: "live community feed", target: s.communityFeed(s.liveCommunity), viewers: others,
			check: func(t *testing.T, body map[string]any, _ string, _ bool) {
				t.Helper()
				assert.Contains(t, moderationPostAcceptanceFeedURIs(t, body), s.livePost)
			},
		},
		{
			name: "discover new", target: "/xrpc/social.coves.feed.getDiscover?sort=new",
			viewers: everyone, check: communityRemovalHidingFeedRow(s, true),
		},
		{
			name: "discover top", target: "/xrpc/social.coves.feed.getDiscover?sort=top&timeframe=all",
			viewers: everyone, check: communityRemovalHidingFeedRow(s, true),
		},
		{
			// Each viewer's pre-removal Hot snapshot is reused during the
			// removal, so the page-time revalidation must drop P.
			name: "discover hot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot",
			viewers: everyone, check: communityRemovalHidingFeedRow(s, true),
		},
		{
			name: "search", target: "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {communityRemovalHidingSearchTerm}}.Encode(),
			viewers: everyone, check: communityRemovalHidingFeedRow(s, true),
		},
		{
			name: "author profile posts", target: "/xrpc/social.coves.actor.getPosts?" + url.Values{"actor": {s.f.authorDID}}.Encode(),
			viewers: everyone,
			check: func(t *testing.T, body map[string]any, viewer string, removed bool) {
				t.Helper()
				if !removed || viewer != communityRemovalHidingViewerAuthor {
					communityRemovalHidingFeedRow(s, true)(t, body, viewer, removed)
					return
				}
				listed := communityRemovalHidingFeedPosts(t, body)
				assert.Contains(t, listed, s.livePost, "the live community's post stays listed")
				require.Contains(t, listed, s.post, "the author's own profile keeps the removed community's post")
				assert.Equal(t, "removed", listed[s.post]["status"])
			},
		},
		{
			name: "commenter profile comments", target: "/xrpc/social.coves.actor.getComments?" + url.Values{"actor": {s.commenter}}.Encode(),
			viewers: others,
			check: func(t *testing.T, body map[string]any, _ string, removed bool) {
				t.Helper()
				listed := moderationAcceptanceActorURIs(t, body)
				assert.Contains(t, listed, s.liveComment, "the live community's comment stays listed")
				if removed {
					assert.NotContains(t, listed, s.comment, "a comment on a removed community's post must be absent")
					return
				}
				assert.Contains(t, listed, s.comment)
			},
		},
	}

	run := func(stage string, removed bool) {
		for _, row := range rows {
			for _, viewer := range row.viewers {
				t.Run(stage+"/"+row.name+"/"+viewer, func(t *testing.T) {
					row.check(t, s.get(t, row.target, viewer), viewer, removed)
				})
			}
		}
	}
	run("before removal", false)
	s.moderator.Remove(t, s.f.communityDID)
	run("during removal", true)
}

// TestCommunityRemovalThreadsAndPostGet reads P's thread and post.get while C
// is removed: another viewer and a signed-out viewer get RootNotFound for the
// thread and #notFoundPost for P, P2 and P3 beside L's full view; A gets the
// thread and the full view of P with status "removed".
func TestCommunityRemovalThreadsAndPostGet(t *testing.T) {
	s := newCommunityRemovalHidingScene(t)
	t.Run("before removal/thread/other viewer", func(t *testing.T) {
		viewerModerationThreadNode(t, s.get(t, s.thread(s.post), communityRemovalHidingViewerOther), s.comment)
	})
	s.moderator.Remove(t, s.f.communityDID)

	for _, viewer := range []string{communityRemovalHidingViewerOther, communityRemovalHidingViewerPublic} {
		t.Run("thread/"+viewer, func(t *testing.T) {
			requireXRPCError(t, s.request(t, s.thread(s.post), viewer), http.StatusNotFound, "RootNotFound")
		})
		t.Run("post.get/"+viewer, func(t *testing.T) {
			items := s.postGetItems(t, viewer, s.post, s.removedByInstance, s.withdrawnByCommunity, s.livePost)
			for _, uri := range []string{s.post, s.removedByInstance, s.withdrawnByCommunity} {
				assert.Equalf(t, map[string]any{"uri": uri, "notFound": true}, items[uri],
					"%s in a removed community must be a notFoundPost", uri)
			}
			communityRemovalHidingRequirePostView(t, items[s.livePost], s.livePost)
		})
	}
	t.Run("thread/author", func(t *testing.T) {
		body := s.get(t, s.thread(s.post), communityRemovalHidingViewerAuthor)
		post := moderationAcceptanceObject(t, body["post"])
		require.Equal(t, s.post, post["uri"])
		assert.Equal(t, "removed", post["status"])
	})
	t.Run("post.get/author", func(t *testing.T) {
		items := s.postGetItems(t, communityRemovalHidingViewerAuthor, s.post)
		communityRemovalHidingRequirePostView(t, items[s.post], s.post)
		assert.Equal(t, "removed", items[s.post]["status"])
	})
}

// TestCommunityRestoreRevealsOnlyCommunityRemovedContent removes and restores
// C: P comes back for another viewer, while P2's own instance removal keeps
// it hidden.
func TestCommunityRestoreRevealsOnlyCommunityRemovedContent(t *testing.T) {
	s := newCommunityRemovalHidingScene(t)
	viewer := communityRemovalHidingViewerOther
	s.moderator.Remove(t, s.f.communityDID)
	t.Run("during removal/community feed", func(t *testing.T) {
		assert.NotContains(t, moderationPostAcceptanceFeedURIs(t, s.get(t, s.communityFeed(s.f.communityDID), viewer)), s.post)
	})
	t.Run("during removal/post.get", func(t *testing.T) {
		communityRemovalHidingRequireNotPostView(t, s.postGetItems(t, viewer, s.post)[s.post], "P while C is removed")
	})

	s.moderator.Restore(t, s.f.communityDID)
	t.Run("after restore/community feed", func(t *testing.T) {
		listed := moderationPostAcceptanceFeedURIs(t, s.get(t, s.communityFeed(s.f.communityDID), viewer))
		assert.Contains(t, listed, s.post)
		assert.NotContains(t, listed, s.removedByInstance)
	})
	t.Run("after restore/post.get", func(t *testing.T) {
		items := s.postGetItems(t, viewer, s.post, s.removedByInstance)
		communityRemovalHidingRequirePostView(t, items[s.post], s.post)
		communityRemovalHidingRequireNotPostView(t, items[s.removedByInstance], "P2 under its own instance removal")
	})
	t.Run("after restore/thread", func(t *testing.T) {
		viewerModerationThreadNode(t, s.get(t, s.thread(s.post), viewer), s.comment)
	})
	t.Run("after restore/instance-removed thread", func(t *testing.T) {
		requireXRPCError(t, s.request(t, s.thread(s.removedByInstance), viewer), http.StatusNotFound, "RootNotFound")
	})
}
