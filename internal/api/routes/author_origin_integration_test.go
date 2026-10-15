//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// authorOriginKeys are the author-view keys that carry where a bridged author
// came from.
var authorOriginKeys = []string{"origin", "originUsername", "originSoftware"}

// authorOriginRow is one read path and the item on it whose author is
// checked. want holds the origin keys the author must carry with their
// literal values; every other origin key must be absent.
type authorOriginRow struct {
	name   string
	target string
	item   viewerModerationRowItem
	want   map[string]any
}

// runAuthorOriginRows requests every row as the fixture's non-admin viewer
// (subscribed to the community where a test needs the timeline) and checks the
// origin keys on the row item's author.
func runAuthorOriginRows(t *testing.T, f contentLabelRouteFixture, rows []authorOriginRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			body := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, f.client, http.MethodGet,
				f.server.URL+row.target, f.nonAdminToken, nil))
			author := moderationAcceptanceObject(t, row.item(t, body)["author"])
			for _, key := range authorOriginKeys {
				value, present := author[key]
				if want, ok := row.want[key]; ok {
					require.Truef(t, present, "author must carry %s; author was %#v", key, author)
					require.Equal(t, want, value, key)
					continue
				}
				require.Falsef(t, present, "author must omit %s; got %#v", key, value)
			}
		})
	}
}

// setAuthorOrigin writes the users origin columns; a nil argument stores NULL.
func setAuthorOrigin(t *testing.T, f contentLabelRouteFixture, did string, origin, originUsername, originSoftware any) {
	t.Helper()
	result, err := f.db.ExecContext(t.Context(),
		`UPDATE users SET origin = $2, origin_username = $3, origin_software = $4 WHERE did = $1`,
		did, origin, originUsername, originSoftware)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated, "author %s has no users row", did)
}

func authorOriginPostGet(uri string) string {
	return "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
}

func authorOriginThread(postURI string) string {
	return "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {postURI}, "sort": {"new"}}.Encode()
}

func TestAuthorOriginOnPostAndCommentReads(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	setAuthorOrigin(t, f, f.authorDID, "lemmy.world", "Alice_B", "lemmy")
	_, err := f.db.ExecContext(t.Context(),
		`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, f.nonAdminDID, f.communityDID)
	require.NoError(t, err)

	const searchTerm = "quetzalorigin"
	postURI := f.insertPost("bridged author post " + searchTerm)
	const commentCID = "bafyreiauthororigincomment"
	comment := moderationAcceptanceInsertComment(t, f.db, f.authorDID, postURI, f.postCID, postURI, f.postCID,
		commentCID, "bridged author top-level comment")
	reply := moderationAcceptanceInsertComment(t, f.db, f.authorDID, postURI, f.postCID, comment, commentCID,
		"bafyreiauthororiginreply", "bridged author nested reply")
	_, err = f.db.ExecContext(t.Context(), `UPDATE comments SET reply_count = 1 WHERE uri = $1`, comment)
	require.NoError(t, err)

	bridged := map[string]any{"origin": "lemmy.world", "originUsername": "Alice_B", "originSoftware": "lemmy"}
	thread := authorOriginThread(postURI)
	runAuthorOriginRows(t, f, []authorOriginRow{
		{name: "post.get", target: authorOriginPostGet(postURI), item: viewerModerationGetPost(postURI, false), want: bridged},
		{
			name:   "community feed",
			target: "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {f.communityDID}, "sort": {"new"}}.Encode(),
			item:   viewerModerationFeedPost(postURI), want: bridged,
		},
		{name: "timeline", target: "/xrpc/social.coves.feed.getTimeline?sort=new", item: viewerModerationFeedPost(postURI), want: bridged},
		{name: "discover new", target: "/xrpc/social.coves.feed.getDiscover?sort=new", item: viewerModerationFeedPost(postURI), want: bridged},
		{name: "discover hot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot", item: viewerModerationFeedPost(postURI), want: bridged},
		{
			name:   "actor posts",
			target: "/xrpc/social.coves.actor.getPosts?" + url.Values{"actor": {f.authorDID}}.Encode(),
			item:   viewerModerationFeedPost(postURI), want: bridged,
		},
		{
			name:   "search",
			target: "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {searchTerm}}.Encode(),
			item:   viewerModerationFeedPost(postURI), want: bridged,
		},
		{name: "thread top-level comment", target: thread, item: viewerModerationThreadComment(comment), want: bridged},
		{name: "thread nested reply", target: thread, item: viewerModerationThreadReply(comment, reply), want: bridged},
		{name: "thread post", target: thread, item: viewerModerationThreadPost(postURI), want: bridged},
		{
			name:   "actor comments",
			target: "/xrpc/social.coves.actor.getComments?" + url.Values{"actor": {f.authorDID}}.Encode(),
			item:   viewerModerationActorComment(comment), want: bridged,
		},
	})
}

func TestAuthorOriginOmittedWhenAbsent(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	cases := []struct {
		name           string
		authorDID      string
		origin         any
		originUsername any
		originSoftware any
		want           map[string]any
	}{
		{name: "native author", authorDID: "did:plc:eeeeeeeeeeeeeeeeeeeeeeee", want: map[string]any{}},
		{
			name: "software never set", authorDID: "did:plc:ffffffffffffffffffffffff", origin: "lemmy.world", originUsername: "Alice_B",
			want: map[string]any{"origin": "lemmy.world", "originUsername": "Alice_B"},
		},
		{
			name: "software cleared", authorDID: "did:plc:gggggggggggggggggggggggg", origin: "lemmy.world", originUsername: "Alice_B", originSoftware: "",
			want: map[string]any{"origin": "lemmy.world", "originUsername": "Alice_B"},
		},
	}
	var rows []authorOriginRow
	for i, c := range cases {
		authorDID := c.authorDID
		fixtures.User(t, f.db, testkit.UniqueIDWithPrefix(t, "originauthor")+".test", authorDID)
		setAuthorOrigin(t, f, authorDID, c.origin, c.originUsername, c.originSoftware)
		postURI := f.insertPostBy(authorDID, "origin omission post "+c.name)
		comment := moderationAcceptanceInsertComment(t, f.db, authorDID, postURI, f.postCID, postURI, f.postCID,
			"bafyreiauthororiginomitted"+string(rune('a'+i)), "origin omission comment "+c.name)
		rows = append(rows,
			authorOriginRow{name: c.name + "/post.get", target: authorOriginPostGet(postURI), item: viewerModerationGetPost(postURI, false), want: c.want},
			authorOriginRow{name: c.name + "/thread comment", target: authorOriginThread(postURI), item: viewerModerationThreadComment(comment), want: c.want},
		)
	}
	runAuthorOriginRows(t, f, rows)
}
