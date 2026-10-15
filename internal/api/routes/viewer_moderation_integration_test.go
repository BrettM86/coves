//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// viewerModerationRow is one read path and the item on it whose viewer
// moderation actions are checked for the admin, non-admin and signed-out
// viewers. want is the admin's literal action list; every other viewer must
// get no "moderation" key at all.
type viewerModerationRow struct {
	name   string
	target string
	item   viewerModerationRowItem
	// signedIn marks a read that requires authentication, so it has no
	// signed-out viewer.
	signedIn bool
	want     []any
}

// viewerModerationRowItem picks the item whose viewer a row checks out of a
// decoded response body.
type viewerModerationRowItem func(t *testing.T, body map[string]any) map[string]any

func viewerModerationFeedPost(uri string) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		for _, entry := range moderationAcceptanceArray(t, body["feed"]) {
			post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["post"])
			if post["uri"] == uri {
				return post
			}
		}
		require.Failf(t, "post missing from feed", "%s not in %v", uri, body["feed"])
		return nil
	}
}

func viewerModerationGetPost(uri string, tombstone bool) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		items := moderationAcceptanceArray(t, body["posts"])
		require.Len(t, items, 1)
		item := moderationAcceptanceObject(t, items[0])
		require.Equal(t, uri, item["uri"])
		if tombstone {
			require.Equal(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
		} else {
			require.NotEqual(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
		}
		return item
	}
}

func viewerModerationThreadNode(t *testing.T, body map[string]any, uri string) map[string]any {
	t.Helper()
	for _, entry := range moderationAcceptanceArray(t, body["comments"]) {
		node := moderationAcceptanceObject(t, entry)
		if moderationAcceptanceObject(t, node["comment"])["uri"] == uri {
			return node
		}
	}
	require.Failf(t, "comment missing from thread", "%s not in %v", uri, body["comments"])
	return nil
}

// viewerModerationThreadComment picks the top-level comment with uri out of a
// comment.getComments thread.
func viewerModerationThreadComment(uri string) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		return moderationAcceptanceObject(t, viewerModerationThreadNode(t, body, uri)["comment"])
	}
}

// viewerModerationThreadReply picks the reply with uri beneath the top-level
// comment parentURI out of a comment.getComments thread.
func viewerModerationThreadReply(parentURI, uri string) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		replies := viewerModerationThreadNode(t, body, parentURI)["replies"]
		for _, entry := range moderationAcceptanceArray(t, replies) {
			comment := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["comment"])
			if comment["uri"] == uri {
				return comment
			}
		}
		require.Failf(t, "reply missing from thread", "%s not beneath %s in %v", uri, parentURI, replies)
		return nil
	}
}

// viewerModerationThreadPost picks the post view a comment.getComments thread
// is about.
func viewerModerationThreadPost(uri string) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		post := moderationAcceptanceObject(t, body["post"])
		require.Equal(t, uri, post["uri"])
		return post
	}
}

func viewerModerationActorComment(uri string) viewerModerationRowItem {
	return func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		for _, entry := range moderationAcceptanceArray(t, body["comments"]) {
			comment := moderationAcceptanceObject(t, entry)
			if comment["uri"] == uri {
				return comment
			}
		}
		require.Failf(t, "comment missing from actor comments", "%s not in %v", uri, body["comments"])
		return nil
	}
}

// runViewerModerationRows requests every row as each viewer and checks the
// row's item: the admin gets the literal action list, everyone else gets no
// viewer.moderation key.
func runViewerModerationRows(t *testing.T, f contentLabelRouteFixture, rows []viewerModerationRow) {
	t.Helper()
	viewers := []struct {
		name  string
		token string
		admin bool
	}{
		{name: "admin", token: f.adminToken, admin: true},
		{name: "non-admin", token: f.nonAdminToken},
		{name: "signed-out", token: ""},
	}
	for _, row := range rows {
		for _, viewer := range viewers {
			if viewer.token == "" && row.signedIn {
				continue
			}
			t.Run(row.name+"/"+viewer.name, func(t *testing.T) {
				body := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, f.client, http.MethodGet,
					f.server.URL+row.target, viewer.token, nil))
				item := row.item(t, body)
				var actions any
				present := false
				if rawViewer, ok := item["viewer"]; ok {
					actions, present = moderationAcceptanceObject(t, rawViewer)["moderation"]
				}
				if viewer.admin {
					require.Truef(t, present, "admin viewer must get viewer.moderation; item viewer was %#v", item["viewer"])
					require.Equal(t, row.want, actions)
					return
				}
				require.Falsef(t, present, "viewer.moderation must be omitted for a %s viewer; got %#v", viewer.name, actions)
			})
		}
	}
}

func TestViewerModerationPostReadsAcceptance(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	const searchTerm = "zanzibarquokka"
	livePost := f.insertPost("viewer moderation live post " + searchTerm)
	removedPost := f.insertPost("viewer moderation removed post")
	labelledPost := f.insertPost("viewer moderation labelled post")
	for _, did := range []string{f.adminDID, f.nonAdminDID} {
		_, err := f.db.ExecContext(t.Context(),
			`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, did, f.communityDID)
		require.NoError(t, err)
	}
	removed := moderationAcceptanceBody(t, f.post(t, moderationRouteRemovePath, f.adminToken, map[string]any{
		"subject": map[string]string{"uri": removedPost, "cid": f.postCID}, "expectedVersion": "v0",
		"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
	}))
	require.Equal(t, "applied", removed["outcome"])
	f.label(t, labelledPost, "v0", "")

	communityFeed := "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {f.communityDID}, "sort": {"new"}}.Encode()
	getPost := func(uri string) string {
		return "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
	}
	removeOrLabel := []any{"remove", "label"}
	runViewerModerationRows(t, f, []viewerModerationRow{
		{name: "community feed", target: communityFeed, item: viewerModerationFeedPost(livePost), want: removeOrLabel},
		{
			name:   "search",
			target: "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {searchTerm}}.Encode(),
			item:   viewerModerationFeedPost(livePost), want: removeOrLabel,
		},
		{
			name: "timeline", target: "/xrpc/social.coves.feed.getTimeline?sort=new",
			item: viewerModerationFeedPost(livePost), signedIn: true, want: removeOrLabel,
		},
		{name: "discover new", target: "/xrpc/social.coves.feed.getDiscover?sort=new", item: viewerModerationFeedPost(livePost), want: removeOrLabel},
		{name: "discover hot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot", item: viewerModerationFeedPost(livePost), want: removeOrLabel},
		{
			name:   "actor posts",
			target: "/xrpc/social.coves.actor.getPosts?" + url.Values{"actor": {f.authorDID}}.Encode(),
			item:   viewerModerationFeedPost(livePost), want: removeOrLabel,
		},
		{name: "post.get live", target: getPost(livePost), item: viewerModerationGetPost(livePost, false), want: removeOrLabel},
		{name: "post.get removed tombstone", target: getPost(removedPost), item: viewerModerationGetPost(removedPost, true), want: []any{"restore"}},
		{
			name: "post.get labelled by this instance", target: getPost(labelledPost),
			item: viewerModerationGetPost(labelledPost, false), want: []any{"remove", "retract-label"},
		},
		{
			name: "community feed labelled by this instance", target: communityFeed,
			item: viewerModerationFeedPost(labelledPost), want: []any{"remove", "retract-label"},
		},
	})
}

func TestViewerModerationCommentReadsAcceptance(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	postURI := f.insertPost("viewer moderation thread post")
	const liveCID = "bafyreiviewermodlivecomment"
	const removedCID = "bafyreiviewermodremovedcomment"
	const replyCID = "bafyreiviewermodreplycomment"
	liveComment := moderationAcceptanceInsertComment(t, f.db, f.authorDID, postURI, f.postCID, postURI, f.postCID,
		liveCID, "live top-level comment")
	removedComment := moderationAcceptanceInsertComment(t, f.db, f.authorDID, postURI, f.postCID, postURI, f.postCID,
		removedCID, "top-level comment the admin removes")
	reply := moderationAcceptanceInsertComment(t, f.db, f.authorDID, postURI, f.postCID, removedComment, removedCID,
		replyCID, "live reply beneath the removed comment")
	_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET reply_count = 1 WHERE uri = $1`, removedComment)
	require.NoError(t, err)
	removed := moderationAcceptanceBody(t, f.post(t, moderationRouteRemovePath, f.adminToken, map[string]any{
		"subject": map[string]string{"uri": removedComment, "cid": removedCID}, "expectedVersion": "v0",
		"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
	}))
	require.Equal(t, "applied", removed["outcome"])

	thread := "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {postURI}, "sort": {"new"}}.Encode()
	runViewerModerationRows(t, f, []viewerModerationRow{
		{name: "thread live comment", target: thread, item: viewerModerationThreadComment(liveComment), want: []any{"remove"}},
		{
			name: "thread removed placeholder", target: thread, want: []any{"restore"},
			item: func(t *testing.T, body map[string]any) map[string]any {
				t.Helper()
				placeholder := viewerModerationThreadComment(removedComment)(t, body)
				require.Equal(t, true, placeholder["isDeleted"])
				require.Equal(t, "moderator", placeholder["deletionReason"])
				return placeholder
			},
		},
		{
			name: "thread reply beneath removed placeholder", target: thread, want: []any{"remove"},
			item: func(t *testing.T, body map[string]any) map[string]any {
				t.Helper()
				replies := moderationAcceptanceArray(t, viewerModerationThreadNode(t, body, removedComment)["replies"])
				require.Len(t, replies, 1)
				comment := moderationAcceptanceObject(t, moderationAcceptanceObject(t, replies[0])["comment"])
				require.Equal(t, reply, comment["uri"])
				return comment
			},
		},
		{
			name: "thread header post", target: thread, want: []any{"remove", "label"},
			item: viewerModerationThreadPost(postURI),
		},
		{
			name:   "actor comments",
			target: "/xrpc/social.coves.actor.getComments?" + url.Values{"actor": {f.authorDID}}.Encode(),
			want:   []any{"remove"},
			item:   viewerModerationActorComment(liveComment),
		},
	})
}
