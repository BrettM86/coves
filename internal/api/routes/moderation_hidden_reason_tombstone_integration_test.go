//go:build integration

package routes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// hiddenReasonContentServer serves post.get and getComments over the modlog
// fixture's database, wired as the post and comment removal acceptance tests
// wire them.
func hiddenReasonContentServer(t *testing.T, f *modlogFixture) *httptest.Server {
	t.Helper()
	postRepo := postgres.NewPostRepository(f.db)
	communityRepo := postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed())
	pdsURL := testkit.Endpoints().PDS.BaseURL
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, pdsURL, fixtures.InstanceDID(), "", nil, nil, nil,
		communities.PrivateHostOptions(true)...,
	)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
		posts.WithSyncAcceptance(postgres.NewAdmissionRepository(f.db), nil),
	)
	commentService := comments.NewCommentService(postgres.NewCommentRepository(f.db),
		postgres.NewUserRepository(f.db), postRepo, communityRepo, nil, nil, nil)
	optionalAuth := middleware.NewOAuthAuthMiddleware(f.unsealer, f.oauthStore)
	mux := chi.NewRouter()
	routes.RegisterPostRoutes(mux, postService, nil, nil, optionalAuth, optionalAuth)
	mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.community.comment.getComments",
		commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(commentService), nil).HandleGetComments)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// hiddenReasonPostAuthor is a well-formed did:plc: post.get validates the
// authority of every URI it is asked for, which the modlog fixture's author
// DID is not.
const hiddenReasonPostAuthor = "did:plc:hhhhhhhhhhhhhhhhhhhhhhhh"

func hiddenReasonAcceptedPost(t *testing.T, f *modlogFixture) (string, string) {
	t.Helper()
	rkey := testkit.TID()
	uri := "at://" + hiddenReasonPostAuthor + "/" + moderation.PostV2Collection + "/" + rkey
	cid := "bafyreilog" + testkit.UniqueIDWithPrefix(t, "postcid")
	_, err := f.db.ExecContext(t.Context(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at)
		VALUES ($1, $2, $3, $4, $5, 'hidden reason tombstone', 'hidden reason tombstone body', NOW())
	`, uri, cid, rkey, hiddenReasonPostAuthor, f.communityDID)
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `
		INSERT INTO community_post_admissions
			(community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
	`, f.communityDID, uri, cid)
	require.NoError(t, err)
	return uri, cid
}

// hiddenReasonNormalized replaces the identifiers that must differ between two
// subjects, so the rest of their tombstones can be compared byte for byte.
func hiddenReasonNormalized(t *testing.T, value any, uri, cid string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	normalized := strings.ReplaceAll(string(encoded), uri, "subject-uri")
	return strings.ReplaceAll(normalized, cid, "subject-cid")
}

// hiddenReasonRequireNoActionTrace fails on any reason or action reference in a
// served tombstone.
func hiddenReasonRequireNoActionTrace(t *testing.T, value any, actionIDs []string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			require.NotContains(t, []string{"reason", "reverses", "ref", "actionId", "privateNote"}, key,
				"a tombstone must not carry a moderation reason or action reference")
			hiddenReasonRequireNoActionTrace(t, child, actionIDs)
		}
	case []any:
		for _, child := range typed {
			hiddenReasonRequireNoActionTrace(t, child, actionIDs)
		}
	case string:
		lower := strings.ToLower(typed)
		for _, forbidden := range []string{"#reason", "doxing", "spam", "illegal"} {
			require.NotContains(t, lower, forbidden, "a tombstone must not name its removal reason")
		}
		for _, id := range actionIDs {
			require.NotContains(t, typed, id, "a tombstone must not reference its removal action")
		}
	}
}

// TestModerationHiddenReasonTombstonesMatchPublicOnes pins the accepted
// limitation of hidden-reason removals (Q-ORACLE option b): a doxing removal
// keeps the same public tombstone as a spam removal on post.get and
// getComments, and subject omission only keeps it out of the browsable and
// filterable public log. An observer who already holds the URI can still see
// that it is removed; the log must never be what hands the URI out.
func TestModerationHiddenReasonTombstonesMatchPublicOnes(t *testing.T) {
	f := newModlogFixture(t)
	content := hiddenReasonContentServer(t, f)
	client := content.Client()
	fixtures.User(t, f.db, testkit.UniqueIDWithPrefix(t, "hiddenauthor")+".test", hiddenReasonPostAuthor)
	type subject struct{ uri, cid, actionID string }
	var doxPost, spamPost, doxComment, spamComment subject
	doxPost.uri, doxPost.cid = hiddenReasonAcceptedPost(t, f)
	spamPost.uri, spamPost.cid = hiddenReasonAcceptedPost(t, f)
	doxComment.uri, doxComment.cid = f.comment(t)
	spamComment.uri, spamComment.cid = f.comment(t)
	// The comment view renders second-precision timestamps; pin them so the two
	// tombstones cannot differ by the clock ticking between the inserts.
	_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET created_at = $1, indexed_at = $1 WHERE uri IN ($2, $3)`,
		time.Now().UTC().Truncate(time.Second), doxComment.uri, spamComment.uri)
	require.NoError(t, err)

	doxPost.actionID, _ = f.remove(t, doxPost.uri, doxPost.cid, f.tokenA, modlogDoxing, "doxing post note")
	spamPost.actionID, _ = f.remove(t, spamPost.uri, spamPost.cid, f.tokenB, modlogSpam, "spam post note")
	doxComment.actionID, _ = f.remove(t, doxComment.uri, doxComment.cid, f.tokenB, modlogDoxing, "doxing comment note")
	spamComment.actionID, _ = f.remove(t, spamComment.uri, spamComment.cid, f.tokenA, modlogSpam, "spam comment note")
	actionIDs := []string{doxPost.actionID, spamPost.actionID, doxComment.actionID, spamComment.actionID}

	readPost := func(t *testing.T, uri string) map[string]any {
		t.Helper()
		target := content.URL + "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
		body := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, target, "", nil))
		items := moderationAcceptanceArray(t, body["posts"])
		require.Len(t, items, 1)
		item := moderationAcceptanceObject(t, items[0])
		require.Equal(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
		require.Equal(t, uri, item["uri"])
		require.Equal(t, "removed", moderationAcceptanceObject(t, item["moderation"])["state"])
		return body
	}
	doxPostBody, spamPostBody := readPost(t, doxPost.uri), readPost(t, spamPost.uri)
	hiddenReasonRequireNoActionTrace(t, doxPostBody, actionIDs)
	hiddenReasonRequireNoActionTrace(t, spamPostBody, actionIDs)
	require.JSONEq(t, hiddenReasonNormalized(t, spamPostBody, spamPost.uri, spamPost.cid),
		hiddenReasonNormalized(t, doxPostBody, doxPost.uri, doxPost.cid),
		"post.get must serve a doxing removal and a spam removal with the same tombstone")

	threadURL := content.URL + "/xrpc/social.coves.community.comment.getComments?" +
		url.Values{"post": {f.postURI}, "sort": {"new"}}.Encode()
	thread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	nodes := map[string]map[string]any{}
	for _, entry := range moderationAcceptanceArray(t, thread["comments"]) {
		node := moderationAcceptanceObject(t, entry)
		uri, ok := moderationAcceptanceObject(t, node["comment"])["uri"].(string)
		require.True(t, ok)
		nodes[uri] = node
	}
	for _, removed := range []subject{doxComment, spamComment} {
		node, found := nodes[removed.uri]
		require.True(t, found, "getComments must keep the removed comment's placeholder")
		comment := moderationAcceptanceObject(t, node["comment"])
		require.Equal(t, true, comment["isDeleted"])
		require.Equal(t, "moderator", comment["deletionReason"])
		require.Equal(t, "removed", moderationAcceptanceObject(t, comment["moderation"])["state"])
		hiddenReasonRequireNoActionTrace(t, node, actionIDs)
	}
	require.JSONEq(t, hiddenReasonNormalized(t, nodes[spamComment.uri], spamComment.uri, spamComment.cid),
		hiddenReasonNormalized(t, nodes[doxComment.uri], doxComment.uri, doxComment.cid),
		"getComments must serve a doxing removal and a spam removal with the same placeholder")

	for _, visible := range []subject{spamPost, spamComment} {
		_, items := modlogPage(t, f.list(t, false, "", url.Values{"subject": {visible.uri}}), false)
		require.Len(t, items, 1)
		action := modlogAction(t, items[0], false)
		require.Equal(t, visible.actionID, modlogID(t, action))
		require.Equal(t, visible.uri, moderationAcceptanceObject(t, action["subject"])["uri"])
		require.Equal(t, modlogSpam, action["reason"])
	}
	// Positive control: the hidden removals are in the log, visible to admins only.
	for _, hidden := range []subject{doxPost, doxComment} {
		_, items := modlogPage(t, f.list(t, true, f.tokenA, url.Values{"subject": {hidden.uri}}), true)
		require.Len(t, items, 1)
		require.Equal(t, hidden.uri, moderationAcceptanceObject(t, moderationAcceptanceObject(t, items[0])["privateSubject"])["uri"])
	}

	var hiddenTraces []string
	for _, hidden := range []subject{doxPost, doxComment} {
		hiddenTraces = append(hiddenTraces, hidden.uri, hidden.cid, hidden.uri[strings.LastIndex(hidden.uri, "/")+1:])
	}
	const since, until = "2020-01-01T00:00:00Z", "2100-01-01T00:00:00Z"
	filters := []url.Values{
		{},
		{"subject": {doxPost.uri}},
		{"subject": {doxComment.uri}},
		{"subject": {spamPost.uri}},
		{"subject": {spamComment.uri}},
		{"collection": {moderation.PostV2Collection}},
		{"collection": {moderation.CommentCollection}},
		{"community": {f.communityDID}},
		{"community": {f.communityHandle}},
		{"community": {f.communityName + "@coves.social"}},
		{"actor": {f.adminA}},
		{"actor": {f.adminB}},
		{"actor": {"admin-a.test"}},
		{"authority": {fixtures.InstanceDID()}},
		{"authority": {"authority.test"}},
		{"action": {"remove"}},
		{"origin": {"local"}},
		{"since": {since}},
		{"until": {until}},
		{"since": {since}, "until": {until}},
		{"actor": {f.adminA}, "action": {"remove"}, "origin": {"local"}, "authority": {fixtures.InstanceDID()}},
		{
			"community": {f.communityDID}, "collection": {moderation.PostV2Collection}, "action": {"remove"},
			"origin": {"local"}, "authority": {fixtures.InstanceDID()}, "actor": {f.adminA}, "since": {since}, "until": {until},
		},
		{"community": {f.communityHandle}, "collection": {moderation.CommentCollection}, "actor": {f.adminB}},
		{
			"subject": {doxPost.uri}, "community": {f.communityDID}, "collection": {moderation.PostV2Collection},
			"action": {"remove"}, "origin": {"local"}, "authority": {"authority.test"}, "actor": {"admin-a.test"},
			"since": {since}, "until": {until},
		},
		{"subject": {doxComment.uri}, "collection": {moderation.CommentCollection}, "actor": {f.adminB}},
	}
	for _, filter := range filters {
		t.Run(filter.Encode(), func(t *testing.T) {
			response := f.list(t, false, "", filter)
			_, items := modlogPage(t, response, false)
			for _, trace := range hiddenTraces {
				require.NotContains(t, string(response.body), trace)
			}
			var unpaged []string
			for _, item := range items {
				unpaged = append(unpaged, modlogID(t, modlogAction(t, item, false)))
			}
			walk := url.Values{"limit": {"1"}}
			for key, values := range filter {
				walk[key] = values
			}
			var walked []string
			for _, page := range modlogWalk(t, f, walk) {
				for _, trace := range hiddenTraces {
					require.NotContains(t, string(page), trace)
				}
				var body struct {
					Actions []struct {
						Ref struct {
							ActionID string `json:"actionId"`
						} `json:"ref"`
					} `json:"actions"`
				}
				require.NoError(t, json.Unmarshal(page, &body))
				for _, action := range body.Actions {
					walked = append(walked, action.Ref.ActionID)
				}
			}
			require.Equal(t, unpaged, walked, "a limit=1 walk must return the unpaged list")
		})
	}
}
