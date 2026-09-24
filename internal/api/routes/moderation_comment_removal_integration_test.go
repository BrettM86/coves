//go:build integration

package routes_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	actorAPI "Coves/internal/api/handlers/actor"
	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/internal/validation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func moderationAcceptanceRequest(t *testing.T, client *http.Client, method, target, token string, body any) subjectStateHTTPResponse {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, target, input)
	require.NoError(t, err)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return subjectStateHTTPResponse{status: response.StatusCode, body: data}
}

func moderationAcceptanceObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.Truef(t, ok, "expected an object, got %#v", value)
	return object
}

func moderationAcceptanceArray(t *testing.T, value any) []any {
	t.Helper()
	array, ok := value.([]any)
	require.Truef(t, ok, "expected an array, got %#v", value)
	return array
}

func moderationAcceptanceBody(t *testing.T, response subjectStateHTTPResponse) map[string]any {
	t.Helper()
	require.Equalf(t, http.StatusOK, response.status, "response body: %s", response.body)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.body, &body))
	return body
}

func moderationAcceptanceThreadComment(t *testing.T, body map[string]any) (map[string]any, []any) {
	t.Helper()
	comments := moderationAcceptanceArray(t, body["comments"])
	require.Len(t, comments, 1)
	node := moderationAcceptanceObject(t, comments[0])
	return moderationAcceptanceObject(t, node["comment"]), moderationAcceptanceArray(t, node["replies"])
}

func moderationAcceptanceActorURIs(t *testing.T, body map[string]any) []string {
	t.Helper()
	var uris []string
	for _, entry := range moderationAcceptanceArray(t, body["comments"]) {
		uri, ok := moderationAcceptanceObject(t, entry)["uri"].(string)
		require.True(t, ok)
		uris = append(uris, uri)
	}
	return uris
}

func moderationAcceptanceInsertComment(t *testing.T, db *sql.DB, authorDID, rootURI, rootCID, parentURI, parentCID, cid, content string) string {
	t.Helper()
	rkey := testkit.TID()
	uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
	`, uri, cid, rkey, authorDID, rootURI, rootCID, parentURI, parentCID, content)
	require.NoError(t, err)
	return uri
}

func TestModerationCommentRemovalAcceptance(t *testing.T) {
	db := testkit.DB(t)
	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	instanceDID := fixtures.InstanceDID()
	moderationService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepo, commentRepo),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	commentService := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo, nil, nil, nil)

	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "modadmin"))
	nonAdminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "modother"))
	authorName := testkit.UniqueIDWithPrefix(t, "modauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "modcommunity")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, authorDID, "moderation acceptance post", 0, time.Now())
	post, err := postRepo.GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	const commentContent = "unique original moderation acceptance comment text"
	const replyContent = "reply remains visible under moderated comment"
	const commentCID = "bafyreicommentacceptance"
	const replyCID = "bafyreireplyacceptance"
	commentURI := moderationAcceptanceInsertComment(t, db, authorDID, postURI, post.CID, postURI, post.CID, commentCID, commentContent)
	replyURI := moderationAcceptanceInsertComment(t, db, authorDID, postURI, post.CID, commentURI, commentCID, replyCID, replyContent)
	_, err = db.ExecContext(t.Context(), `UPDATE comments SET reply_count = 1 WHERE uri = $1`, commentURI)
	require.NoError(t, err)

	const adminToken = "moderation-acceptance-admin-session"
	const nonAdminToken = "moderation-acceptance-nonadmin-session"
	unsealer := fixtures.NewSessionUnsealer()
	oauthStore := fixtures.NewOAuthStore()
	unsealer.AddSession(adminToken, adminDID, "moderation-admin-session")
	oauthStore.AddSession(adminDID, "moderation-admin-session", "admin-access-token")
	unsealer.AddSession(nonAdminToken, nonAdminDID, "moderation-nonadmin-session")
	oauthStore.AddSession(nonAdminDID, "moderation-nonadmin-session", "non-admin-access-token")
	adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, oauthStore, nil, moderation.NewAllowlistAuthority([]string{adminDID}))
	optionalAuth := middleware.NewOAuthAuthMiddleware(unsealer, oauthStore)
	mux := chi.NewRouter()
	routes.RegisterModerationRoutes(mux, moderationService, adminAuth)
	mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.community.comment.getComments",
		commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(commentService), nil).HandleGetComments)
	mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.actor.getComments",
		actorAPI.NewGetCommentsHandler(commentService, nil, nil).HandleGetComments)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := server.Client()
	threadURL := server.URL + "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {postURI}, "sort": {"new"}}.Encode()
	actorURL := server.URL + "/xrpc/social.coves.actor.getComments?" + url.Values{"actor": {authorDID}}.Encode()
	removeURL := server.URL + "/xrpc/social.coves.moderation.removeContent"

	// Verify the fixture reaches both real read paths before exercising the mutation.
	initialThread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	initialComment, initialReplies := moderationAcceptanceThreadComment(t, initialThread)
	require.Equal(t, commentURI, initialComment["uri"])
	require.Equal(t, commentContent, moderationAcceptanceObject(t, initialComment["record"])["content"])
	require.Len(t, initialReplies, 1)
	require.Equal(t, replyURI, moderationAcceptanceObject(t, moderationAcceptanceObject(t, initialReplies[0])["comment"])["uri"])
	initialActivity := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, actorURL, "", nil))
	require.ElementsMatch(t, []string{commentURI, replyURI}, moderationAcceptanceActorURIs(t, initialActivity))

	stateResponse := requestSubjectState(t, client, server.URL, commentURI, adminToken)
	initialState := moderationAcceptanceObject(t, moderationAcceptanceBody(t, stateResponse)["state"])
	initialVersion, ok := initialState["version"].(string)
	require.True(t, ok)
	require.Equal(t, "v0", initialVersion)
	require.Equal(t, commentCID, moderationAcceptanceObject(t, initialState["currentSubject"])["cid"])
	require.Equal(t, "clear", moderationAcceptanceObject(t, initialState["moderation"])["state"])

	removeRequest := map[string]any{
		"subject":         map[string]string{"uri": commentURI, "cid": commentCID},
		"expectedVersion": initialVersion, "idempotencyKey": "k-remove",
		"reason": "social.coves.moderation.defs#reasonSpam",
	}
	unauthorized := moderationAcceptanceRequest(t, client, http.MethodPost, removeURL, nonAdminToken, removeRequest)
	requireXRPCError(t, unauthorized, http.StatusForbidden, "Forbidden")
	afterUnauthorized := moderationAcceptanceObject(t, moderationAcceptanceBody(t, requestSubjectState(t, client, server.URL, commentURI, adminToken))["state"])
	require.Equal(t, initialVersion, afterUnauthorized["version"])
	require.Equal(t, "clear", moderationAcceptanceObject(t, afterUnauthorized["moderation"])["state"])

	removedResponse := moderationAcceptanceRequest(t, client, http.MethodPost, removeURL, adminToken, removeRequest)
	removed := moderationAcceptanceBody(t, removedResponse)
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"))
	data, err := atdata.UnmarshalJSON(removedResponse.body)
	require.NoError(t, err)
	require.NoError(t, validation.ValidateData(catalog, data, "social.coves.moderation.defs#mutationResult", 0))
	require.Equal(t, "applied", removed["outcome"])
	removalAction := moderationAcceptanceObject(t, moderationAcceptanceObject(t, removed["action"])["action"])
	require.Equal(t, "remove", removalAction["action"])
	removalID, ok := moderationAcceptanceObject(t, removalAction["ref"])["actionId"].(string)
	require.True(t, ok)
	require.NotEmpty(t, removalID)
	removedState := moderationAcceptanceObject(t, removed["state"])
	removedVersion, ok := removedState["version"].(string)
	require.True(t, ok)
	require.NotEqual(t, initialVersion, removedVersion)
	require.Equal(t, "removed", moderationAcceptanceObject(t, removedState["moderation"])["state"])
	require.Equal(t, removalID, moderationAcceptanceObject(t, removedState["localRemoval"])["actionId"])

	removedThread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	placeholder, replies := moderationAcceptanceThreadComment(t, removedThread)
	require.Equal(t, commentURI, placeholder["uri"])
	require.Equal(t, true, placeholder["isDeleted"])
	require.Equal(t, "moderator", placeholder["deletionReason"])
	view := moderationAcceptanceObject(t, placeholder["moderation"])
	require.Equal(t, "removed", view["state"])
	sources := moderationAcceptanceArray(t, view["sources"])
	require.NotEmpty(t, sources)
	require.Equal(t, instanceDID, moderationAcceptanceObject(t, sources[0])["authorityDid"])
	require.Equal(t, "instance", moderationAcceptanceObject(t, moderationAcceptanceObject(t, sources[0])["scope"])["kind"])
	require.Nil(t, placeholder["record"])
	require.Equal(t, "handle.invalid", moderationAcceptanceObject(t, placeholder["author"])["handle"])
	require.NotContains(t, placeholder, "embed")
	placeholderJSON, err := json.Marshal(placeholder)
	require.NoError(t, err)
	require.NotContains(t, string(placeholderJSON), commentContent)
	require.Len(t, replies, 1)
	reply := moderationAcceptanceObject(t, moderationAcceptanceObject(t, replies[0])["comment"])
	require.Equal(t, replyURI, reply["uri"])
	require.Equal(t, replyContent, moderationAcceptanceObject(t, reply["record"])["content"])

	activity := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, actorURL, "", nil))
	activityURIs := moderationAcceptanceActorURIs(t, activity)
	assert.NotContains(t, activityURIs, commentURI)
	require.Contains(t, activityURIs, replyURI)

	restoreResponse := moderationAcceptanceRequest(t, client, http.MethodPost, server.URL+"/xrpc/social.coves.moderation.restoreContent", adminToken, map[string]any{
		"actionId":        removalID,
		"reviewedSubject": map[string]string{"uri": commentURI, "cid": commentCID},
		"expectedVersion": removedVersion, "idempotencyKey": "k-restore",
		"reason": "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	restored := moderationAcceptanceBody(t, restoreResponse)
	data, err = atdata.UnmarshalJSON(restoreResponse.body)
	require.NoError(t, err)
	require.NoError(t, validation.ValidateData(catalog, data, "social.coves.moderation.defs#mutationResult", 0))
	require.Equal(t, "applied", restored["outcome"])
	restoreAction := moderationAcceptanceObject(t, moderationAcceptanceObject(t, restored["action"])["action"])
	require.Equal(t, "restore", restoreAction["action"])
	require.Equal(t, removalID, moderationAcceptanceObject(t, restoreAction["reverses"])["actionId"])
	require.Equal(t, "clear", moderationAcceptanceObject(t, moderationAcceptanceObject(t, restored["state"])["moderation"])["state"])

	restoredThread := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, threadURL, "", nil))
	restoredComment, restoredReplies := moderationAcceptanceThreadComment(t, restoredThread)
	require.Equal(t, commentURI, restoredComment["uri"])
	require.NotContains(t, restoredComment, "isDeleted")
	require.Equal(t, commentContent, moderationAcceptanceObject(t, restoredComment["record"])["content"])
	require.Len(t, restoredReplies, 1)
	require.Equal(t, replyURI, moderationAcceptanceObject(t, moderationAcceptanceObject(t, restoredReplies[0])["comment"])["uri"])
}
