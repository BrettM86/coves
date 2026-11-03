//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// setCommunityOriginSoftware writes communities.origin_software; nil stores NULL.
func setCommunityOriginSoftware(t *testing.T, f contentLabelRouteFixture, originSoftware any) {
	t.Helper()
	result, err := f.db.ExecContext(t.Context(),
		`UPDATE communities SET origin_software = $2 WHERE did = $1`, f.communityDID, originSoftware)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated, "community %s has no communities row", f.communityDID)
}

// runCommunityOriginSoftwareRows requests every row as the fixture's non-admin
// viewer and checks originSoftware on the row item's community ref: the literal
// want when non-empty, otherwise absent.
func runCommunityOriginSoftwareRows(t *testing.T, f contentLabelRouteFixture, rows []authorOriginRow, want string) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			body := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, f.client, http.MethodGet,
				f.server.URL+row.target, f.nonAdminToken, nil))
			community := moderationAcceptanceObject(t, row.item(t, body)["community"])
			value, present := community["originSoftware"]
			if want == "" {
				require.Falsef(t, present, "community must omit originSoftware; got %#v", value)
				return
			}
			require.Truef(t, present, "community must carry originSoftware; community was %#v", community)
			require.Equal(t, want, value)
		})
	}
}

func TestCommunityOriginSoftwareOnPostReads(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	setCommunityOriginSoftware(t, f, "lemmy")
	_, err := f.db.ExecContext(t.Context(),
		`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, f.nonAdminDID, f.communityDID)
	require.NoError(t, err)

	const searchTerm = "quetzalcommunitysoftware"
	postURI := f.insertPost("bridged community post " + searchTerm)

	runCommunityOriginSoftwareRows(t, f, []authorOriginRow{
		{name: "post.get", target: authorOriginPostGet(postURI), item: viewerModerationGetPost(postURI, false)},
		{
			name:   "community feed",
			target: "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {f.communityDID}, "sort": {"new"}}.Encode(),
			item:   viewerModerationFeedPost(postURI),
		},
		{name: "timeline", target: "/xrpc/social.coves.feed.getTimeline?sort=new", item: viewerModerationFeedPost(postURI)},
		{name: "discover hot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot", item: viewerModerationFeedPost(postURI)},
		{
			name:   "actor posts",
			target: "/xrpc/social.coves.actor.getPosts?" + url.Values{"actor": {f.authorDID}}.Encode(),
			item:   viewerModerationFeedPost(postURI),
		},
		{
			name:   "search",
			target: "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {searchTerm}}.Encode(),
			item:   viewerModerationFeedPost(postURI),
		},
		{name: "thread post", target: authorOriginThread(postURI), item: viewerModerationThreadPost(postURI)},
	}, "lemmy")
}

func TestCommunityOriginSoftwareOmittedWhenAbsent(t *testing.T) {
	for _, c := range []struct {
		name           string
		originSoftware any
	}{
		{name: "never set", originSoftware: nil},
		{name: "cleared", originSoftware: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newContentLabelRouteFixture(t)
			setCommunityOriginSoftware(t, f, c.originSoftware)
			postURI := f.insertPost("native community post " + c.name)
			runCommunityOriginSoftwareRows(t, f, []authorOriginRow{
				{name: "post.get", target: authorOriginPostGet(postURI), item: viewerModerationGetPost(postURI, false)},
				{name: "thread post", target: authorOriginThread(postURI), item: viewerModerationThreadPost(postURI)},
			}, "")
		})
	}
}
