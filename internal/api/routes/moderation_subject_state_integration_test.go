//go:build integration

package routes_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/moderation"
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

const getSubjectStatePath = "/xrpc/social.coves.moderation.getSubjectState"

type subjectStateHTTPResponse struct {
	status int
	body   []byte
}

func requestSubjectState(t *testing.T, client *http.Client, serverURL, subject, token string) subjectStateHTTPResponse {
	t.Helper()

	target := serverURL + getSubjectStatePath + "?" + url.Values{"subject": {subject}}.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return subjectStateHTTPResponse{status: response.StatusCode, body: body}
}

func requireXRPCError(t *testing.T, response subjectStateHTTPResponse, status int, code string) {
	t.Helper()

	require.Equalf(t, status, response.status, "status = %d, want %d (body: %s)", response.status, status, response.body)
	var body map[string]any
	require.NoErrorf(t, json.Unmarshal(response.body, &body), "decoding XRPC error body %q", response.body)
	assert.Equal(t, code, body["error"])
}

func TestGetSubjectState(t *testing.T) {
	db := testkit.DB(t)
	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	reader := moderation.NewRepositorySubjectReader(postRepo, commentRepo)
	service := moderation.NewService(reader, postgres.NewModerationRepository(db), moderation.Config{InstanceDID: fixtures.InstanceDID()})

	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "admin"))
	nonAdminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "nonadmin"))
	authorName := testkit.UniqueIDWithPrefix(t, "subjectauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "subjectcommunity")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)

	const postTitle = "distinctive title"
	uri := fixtures.Post(t, db, communityDID, authorDID, postTitle, 0, time.Now())
	indexedPost, err := postRepo.GetRawIndexedRow(t.Context(), uri)
	require.NoError(t, err)

	const (
		adminToken        = "admin-sealed-session"
		adminSessionID    = "admin-session"
		nonAdminToken     = "non-admin-sealed-session"
		nonAdminSessionID = "non-admin-session"
	)
	unsealer := fixtures.NewSessionUnsealer()
	store := fixtures.NewOAuthStore()
	unsealer.AddSession(adminToken, adminDID, adminSessionID)
	store.AddSession(adminDID, adminSessionID, "admin-access-token")
	unsealer.AddSession(nonAdminToken, nonAdminDID, nonAdminSessionID)
	store.AddSession(nonAdminDID, nonAdminSessionID, "non-admin-access-token")

	authority := moderation.NewAllowlistAuthority([]string{adminDID})
	adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, store, nil, authority)
	mux := chi.NewRouter()
	routes.RegisterModerationRoutes(mux, service, adminAuth)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Run("given an indexed post when an admin requests its state then the response is content-free", func(t *testing.T) {
		response := requestSubjectState(t, server.Client(), server.URL, uri, adminToken)
		require.Equalf(t, http.StatusOK, response.status, "status = %d, want 200 (body: %s)", response.status, response.body)
		assert.NotContains(t, string(response.body), postTitle, "the moderation state must not retain indexed post content")

		data, err := atdata.UnmarshalJSON(response.body)
		require.NoError(t, err, "the response must decode as atProto JSON")
		catalog := lexicon.NewBaseCatalog()
		require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"))
		require.NoError(t, validation.ValidateData(
			catalog,
			data,
			"social.coves.moderation.getSubjectState#output",
			0,
		), "the response must satisfy the getSubjectState output lexicon")

		var body map[string]any
		require.NoError(t, json.Unmarshal(response.body, &body))
		state, ok := body["state"].(map[string]any)
		require.Truef(t, ok, "state must be an object; got %#v", body["state"])
		assert.Equal(t, uri, state["subject"])
		assert.Equal(t, "present", state["recordState"])
		assert.Equal(t, "v0", state["version"])
		assert.NotContains(t, state, "localRemoval")
		assert.NotContains(t, state, "localLabels")

		moderationView, ok := state["moderation"].(map[string]any)
		require.Truef(t, ok, "moderation must be an object; got %#v", state["moderation"])
		assert.Equal(t, "clear", moderationView["state"])

		currentSubject, ok := state["currentSubject"].(map[string]any)
		require.Truef(t, ok, "currentSubject must be an object; got %#v", state["currentSubject"])
		assert.Equal(t, uri, currentSubject["uri"])
		assert.Equal(t, indexedPost.CID, currentSubject["cid"])
	})

	t.Run("given a non-admin session when state is requested then access is forbidden", func(t *testing.T) {
		response := requestSubjectState(t, server.Client(), server.URL, uri, nonAdminToken)
		requireXRPCError(t, response, http.StatusForbidden, "Forbidden")
	})

	t.Run("given no credential when state is requested then authentication is required", func(t *testing.T) {
		response := requestSubjectState(t, server.Client(), server.URL, uri, "")
		requireXRPCError(t, response, http.StatusUnauthorized, "AuthRequired")
	})

	t.Run("given no credential and a malformed subject when requested then authentication is checked first", func(t *testing.T) {
		response := requestSubjectState(t, server.Client(), server.URL, "not a uri", "")
		requireXRPCError(t, response, http.StatusUnauthorized, "AuthRequired")
	})

	t.Run("given a non-admin and a malformed subject when requested then authorization is checked first", func(t *testing.T) {
		response := requestSubjectState(t, server.Client(), server.URL, "not a uri", nonAdminToken)
		requireXRPCError(t, response, http.StatusForbidden, "Forbidden")
	})

	t.Run("given a never-indexed supported subject when an admin requests it then unavailable state is returned", func(t *testing.T) {
		const neverIndexedURI = "at://did:plc:neverindexed/social.coves.community.postv2/3kabc"
		response := requestSubjectState(t, server.Client(), server.URL, neverIndexedURI, adminToken)
		require.Equalf(t, http.StatusOK, response.status, "status = %d, want 200 (body: %s)", response.status, response.body)

		var body map[string]any
		require.NoError(t, json.Unmarshal(response.body, &body))
		state, ok := body["state"].(map[string]any)
		require.Truef(t, ok, "state must be an object; got %#v", body["state"])
		assert.Equal(t, neverIndexedURI, state["subject"])
		assert.Equal(t, "unavailable", state["recordState"])
		assert.Equal(t, "v0", state["version"])
		assert.NotContains(t, state, "currentSubject")
		moderationView, ok := state["moderation"].(map[string]any)
		require.Truef(t, ok, "moderation must be an object; got %#v", state["moderation"])
		assert.Equal(t, "clear", moderationView["state"])
	})

	t.Run("given an unsupported subject collection when an admin requests it then the subject is invalid", func(t *testing.T) {
		const unsupportedURI = "at://did:plc:x/social.coves.actor.profile/self"
		response := requestSubjectState(t, server.Client(), server.URL, unsupportedURI, adminToken)
		requireXRPCError(t, response, http.StatusBadRequest, "InvalidSubject")
	})
}
