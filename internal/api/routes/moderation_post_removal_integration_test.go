//go:build integration

package routes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/internal/validation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func moderationPostAcceptanceNoContentKeys(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			require.NotContains(t, []string{"record", "title", "embed"}, key, "content key in moderated post: %s", key)
			moderationPostAcceptanceNoContentKeys(t, child)
		}
	case []any:
		for _, child := range value {
			moderationPostAcceptanceNoContentKeys(t, child)
		}
	}
}

func moderationPostAcceptanceFeedURIs(t *testing.T, body map[string]any) []string {
	t.Helper()
	var uris []string
	for _, entry := range moderationAcceptanceArray(t, body["feed"]) {
		post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["post"])
		uri, ok := post["uri"].(string)
		require.True(t, ok)
		uris = append(uris, uri)
	}
	return uris
}

func TestModerationPostRemovalAcceptance(t *testing.T) {
	db := testkit.DB(t)
	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	admissionRepo := postgres.NewAdmissionRepository(db)
	instanceDID := fixtures.InstanceDID()
	moderationService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepo, commentRepo),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	pdsURL := testkit.Endpoints().PDS.BaseURL
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, pdsURL, instanceDID, "", nil, nil, nil,
		communities.PrivateHostOptions(true)...,
	)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
		posts.WithSyncAcceptance(admissionRepo, nil),
	)
	commentService := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo, nil, nil, nil)
	feedService := communityFeeds.NewCommunityFeedService(
		postgres.NewCommunityFeedRepository(db, "moderation-acceptance-cursor-secret"), communityService,
	)

	authorName := testkit.UniqueIDWithPrefix(t, "postauthor")
	const authorDID = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "postcommunity")
	const communityDID = "did:plc:cccccccccccccccccccccccc"
	const ownerDID = "did:plc:dddddddddddddddddddddddd"
	fixtures.User(t, db, "owner"+communityName+".test", ownerDID)
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, $3, $4, $5, $6, NOW())
	`, communityDID, communityName, ownerDID, instanceDID, communityName+".coves.social", pdsURL)
	require.NoError(t, err)
	const postTitle = "original post removal acceptance title"
	const postBody = "original post removal acceptance body text"
	const postCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	insertAcceptedPost := func(title, content string) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + rkey
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`, uri, postCID, rkey, authorDID, communityDID, title, content)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO community_post_admissions
				(community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
			VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
		`, communityDID, uri, postCID)
		require.NoError(t, err)
		return uri
	}
	postURI := insertAcceptedPost(postTitle, postBody)
	controlURI := insertAcceptedPost("unremoved acceptance control", "control body")
	commentURI := moderationAcceptanceInsertComment(t, db, authorDID, postURI, postCID, postURI, postCID,
		"bafyreipostacceptancecomment", "comment under the accepted post")

	adminADID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "postadmina"))
	adminBDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "postadminb"))
	const adminAToken = "post-removal-admin-a-session"
	const adminBToken = "post-removal-admin-b-session"
	const authorToken = "post-removal-author-session"
	unsealer := fixtures.NewSessionUnsealer()
	oauthStore := fixtures.NewOAuthStore()
	for _, session := range []struct{ token, did, id string }{
		{adminAToken, adminADID, "post-admin-a"},
		{adminBToken, adminBDID, "post-admin-b"},
		{authorToken, authorDID, "post-author"},
	} {
		unsealer.AddSession(session.token, session.did, session.id)
		oauthStore.AddSession(session.did, session.id, "test-access-token")
	}
	adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, oauthStore, nil,
		moderation.NewAllowlistAuthority([]string{adminADID, adminBDID}))
	optionalAuth := middleware.NewOAuthAuthMiddleware(unsealer, oauthStore)
	mux := chi.NewRouter()
	routes.RegisterModerationRoutes(mux, moderationService, adminAuth)
	routes.RegisterPostRoutes(mux, postService, nil, nil, optionalAuth, optionalAuth)
	routes.RegisterCommunityFeedRoutes(mux, feedService, nil, nil, optionalAuth)
	mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.community.comment.getComments",
		commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(commentService), nil).HandleGetComments)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := server.Client()
	postURL := server.URL + "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {postURI}}.Encode()
	threadURL := server.URL + "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {postURI}, "sort": {"new"}}.Encode()
	feedURL := server.URL + "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {communityDID}, "sort": {"new"}}.Encode()

	initialPost := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, postURL, "", nil))
	initialPosts := moderationAcceptanceArray(t, initialPost["posts"])
	require.Len(t, initialPosts, 1)
	require.Equal(t, postTitle, moderationAcceptanceObject(t, moderationAcceptanceObject(t, initialPosts[0])["record"])["title"])
	initialThread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	initialComments := moderationAcceptanceArray(t, initialThread["comments"])
	require.Len(t, initialComments, 1)
	initialComment := moderationAcceptanceObject(t, moderationAcceptanceObject(t, initialComments[0])["comment"])
	require.Equal(t, commentURI, initialComment["uri"])
	initialFeed := moderationPostAcceptanceFeedURIs(t, moderationAcceptanceBody(t,
		moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil)))
	require.ElementsMatch(t, []string{postURI, controlURI}, initialFeed)

	state := moderationAcceptanceObject(t, moderationAcceptanceBody(t,
		requestSubjectState(t, client, server.URL, postURI, adminAToken))["state"])
	require.Equal(t, postCID, moderationAcceptanceObject(t, state["currentSubject"])["cid"])
	version, ok := state["version"].(string)
	require.True(t, ok)
	removeResponse := moderationAcceptanceRequest(t, client, http.MethodPost,
		server.URL+"/xrpc/social.coves.moderation.removeContent", adminAToken, map[string]any{
			"subject": map[string]string{"uri": postURI, "cid": postCID}, "expectedVersion": version,
			"idempotencyKey": "remove-post", "reason": "social.coves.moderation.defs#reasonSpam",
		})
	removed := moderationAcceptanceBody(t, removeResponse)
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"))
	mutationData, err := atdata.UnmarshalJSON(removeResponse.body)
	require.NoError(t, err)
	require.NoError(t, validation.ValidateData(catalog, mutationData, "social.coves.moderation.defs#mutationResult", 0))
	require.Equal(t, "applied", removed["outcome"])
	removalAction := moderationAcceptanceObject(t, moderationAcceptanceObject(t, removed["action"])["action"])
	removalID, ok := moderationAcceptanceObject(t, removalAction["ref"])["actionId"].(string)
	require.True(t, ok)
	require.NotEmpty(t, removalID)
	removedVersion, ok := moderationAcceptanceObject(t, removed["state"])["version"].(string)
	require.True(t, ok)

	for _, viewer := range []struct{ name, token string }{{"anonymous", ""}, {"author", authorToken}} {
		t.Run(viewer.name, func(t *testing.T) {
			response := moderationAcceptanceRequest(t, client, http.MethodGet, postURL, viewer.token, nil)
			body := moderationAcceptanceBody(t, response)
			items := moderationAcceptanceArray(t, body["posts"])
			require.Len(t, items, 1)
			item := moderationAcceptanceObject(t, items[0])
			require.Equal(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
			require.Equal(t, postURI, item["uri"])
			view := moderationAcceptanceObject(t, item["moderation"])
			require.Equal(t, "removed", view["state"])
			sources := moderationAcceptanceArray(t, view["sources"])
			require.Len(t, sources, 1)
			source := moderationAcceptanceObject(t, sources[0])
			require.Equal(t, instanceDID, source["authorityDid"])
			require.Equal(t, "instance", moderationAcceptanceObject(t, source["scope"])["kind"])
			moderationPostAcceptanceNoContentKeys(t, item)
			require.NotContains(t, string(response.body), postTitle)
			require.NotContains(t, string(response.body), postBody)
			itemJSON, err := json.Marshal(item)
			require.NoError(t, err)
			data, err := atdata.UnmarshalJSON(itemJSON)
			require.NoError(t, err)
			require.NoError(t, validation.ValidateData(catalog, data, "social.coves.community.post.defs#moderatedPost", 0))
		})
	}

	missingRootURI := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()
	missingThreadURL := server.URL + "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {missingRootURI}, "sort": {"new"}}.Encode()
	missingRoot := moderationAcceptanceRequest(t, client, http.MethodGet, missingThreadURL, "", nil)
	removedRoot := moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil)
	requireXRPCError(t, missingRoot, http.StatusNotFound, "RootNotFound")
	requireXRPCError(t, removedRoot, http.StatusNotFound, "RootNotFound")
	require.JSONEq(t, string(missingRoot.body), string(removedRoot.body), "a removed root must have the same error code and message as a never-indexed root")
	removedFeed := moderationPostAcceptanceFeedURIs(t, moderationAcceptanceBody(t,
		moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil)))
	require.NotContains(t, removedFeed, postURI)
	require.Contains(t, removedFeed, controlURI)

	restoreResponse := moderationAcceptanceRequest(t, client, http.MethodPost,
		server.URL+"/xrpc/social.coves.moderation.restoreContent", adminBToken, map[string]any{
			"actionId": removalID, "reviewedSubject": map[string]string{"uri": postURI, "cid": postCID},
			"expectedVersion": removedVersion, "idempotencyKey": "restore-post",
			"reason": "social.coves.moderation.defs#reasonModeratorDiscretion",
		})
	restored := moderationAcceptanceBody(t, restoreResponse)
	restoreData, err := atdata.UnmarshalJSON(restoreResponse.body)
	require.NoError(t, err)
	require.NoError(t, validation.ValidateData(catalog, restoreData, "social.coves.moderation.defs#mutationResult", 0))
	require.Equal(t, "applied", restored["outcome"])
	restoredPost := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, postURL, "", nil))
	restoredItems := moderationAcceptanceArray(t, restoredPost["posts"])
	require.Len(t, restoredItems, 1)
	restoredItem := moderationAcceptanceObject(t, restoredItems[0])
	require.Equal(t, postURI, restoredItem["uri"])
	require.Equal(t, postTitle, moderationAcceptanceObject(t, restoredItem["record"])["title"])
	require.NotContains(t, restoredItem, "moderation")
	restoredThread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	restoredComments := moderationAcceptanceArray(t, restoredThread["comments"])
	require.Len(t, restoredComments, 1)
	restoredComment := moderationAcceptanceObject(t, moderationAcceptanceObject(t, restoredComments[0])["comment"])
	require.Equal(t, commentURI, restoredComment["uri"])
	restoredFeed := moderationPostAcceptanceFeedURIs(t, moderationAcceptanceBody(t,
		moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil)))
	require.ElementsMatch(t, []string{postURI, controlURI}, restoredFeed)

}
