//go:build integration

package routes_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderationRouteRemovePath = "/xrpc/social.coves.moderation.removeContent"
	moderationRouteReason     = "social.coves.moderation.defs#reasonSpam"
)

// moderationRouteComments indexes count comments under one post and returns
// their URIs and CIDs.
func moderationRouteComments(t *testing.T, db *sql.DB, count int) ([]string, []string) {
	t.Helper()
	authorName := testkit.UniqueIDWithPrefix(t, "routeauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "routecommunity")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, authorDID, "moderation route post", 0, time.Now())
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	var uris, cids []string
	for index := range count {
		cid := "bafyreiroutecomment" + string(rune('a'+index))
		uris = append(uris, moderationAcceptanceInsertComment(t, db, authorDID, postURI, post.CID, postURI, post.CID, cid, "route comment"))
		cids = append(cids, cid)
	}
	return uris, cids
}

func moderationRouteServer(t *testing.T, service moderation.Service, token, adminDID string, allowlist []string) *httptest.Server {
	t.Helper()
	unsealer := fixtures.NewSessionUnsealer()
	oauthStore := fixtures.NewOAuthStore()
	unsealer.AddSession(token, adminDID, "moderation-route-session")
	oauthStore.AddSession(adminDID, "moderation-route-session", "moderation-route-access-token")
	adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, oauthStore, nil, moderation.NewAllowlistAuthority(allowlist))
	router := chi.NewRouter()
	routes.RegisterModerationRoutes(router, service, adminAuth)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func moderationRouteRemoveRequest(uri, cid, key string) map[string]any {
	return map[string]any{
		"subject":         map[string]string{"uri": uri, "cid": cid},
		"expectedVersion": "v0", "idempotencyKey": key, "reason": moderationRouteReason,
	}
}

func TestModerationRemoveReplayRechecksInstanceAdminAllowlist(t *testing.T) {
	db := testkit.DB(t)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	uris, cids := moderationRouteComments(t, db, 1)
	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "formeradmin"))
	const token = "moderation-replay-admin-session"
	removeRequest := moderationRouteRemoveRequest(uris[0], cids[0], "k-remove")

	listed := moderationRouteServer(t, service, token, adminDID, []string{adminDID})
	removed := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, listed.Client(), http.MethodPost, listed.URL+moderationRouteRemovePath, token, removeRequest))
	require.Equal(t, "applied", removed["outcome"])
	removalID, ok := moderationAcceptanceObject(t, moderationAcceptanceObject(t, moderationAcceptanceObject(t, removed["action"])["action"])["ref"])["actionId"].(string)
	require.True(t, ok)
	require.NotEmpty(t, removalID)
	removedVersion, ok := moderationAcceptanceObject(t, removed["state"])["version"].(string)
	require.True(t, ok)
	require.Equal(t, "v1", removedVersion)

	// The operator removes the DID from the allowlist. The same session replays
	// the same key and body, which the service would answer from storage.
	delisted := moderationRouteServer(t, service, token, adminDID, nil)
	replay := moderationAcceptanceRequest(t, delisted.Client(), http.MethodPost, delisted.URL+moderationRouteRemovePath, token, removeRequest)
	requireXRPCError(t, replay, http.StatusForbidden, "Forbidden")
	var body map[string]any
	require.NoError(t, json.Unmarshal(replay.body, &body))
	assert.ElementsMatch(t, []string{"error", "message"}, keysOf(body), "a revoked admin receives only the error envelope")
	assert.NotContains(t, string(replay.body), removalID)
	assert.NotContains(t, string(replay.body), `"`+removedVersion+`"`)
	var removeActions int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1`, uris[0]).Scan(&removeActions))
	assert.Equal(t, 1, removeActions)
}

func TestModerationRemoveLiveKeyCapNamesTheLimit(t *testing.T) {
	db := testkit.DB(t)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1},
	)
	uris, cids := moderationRouteComments(t, db, 2)
	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "capadmin"))
	const token = "moderation-cap-admin-session"
	server := moderationRouteServer(t, service, token, adminDID, []string{adminDID})
	target := server.URL + moderationRouteRemovePath

	first := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, server.Client(), http.MethodPost, target, token, moderationRouteRemoveRequest(uris[0], cids[0], "cap-first")))
	require.Equal(t, "applied", first["outcome"])
	capped := moderationAcceptanceRequest(t, server.Client(), http.MethodPost, target, token, moderationRouteRemoveRequest(uris[1], cids[1], "cap-second"))
	requireXRPCError(t, capped, http.StatusBadRequest, "InvalidRequest")
	var body map[string]any
	require.NoError(t, json.Unmarshal(capped.body, &body))
	assert.Equal(t, "invalid request: live idempotency key limit 1 reached", body["message"])
}

func keysOf(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
