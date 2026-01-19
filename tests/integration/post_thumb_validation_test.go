package integration

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestCommunityWithCredentials creates a test community with valid PDS credentials
func createTestCommunityWithCredentials(t *testing.T, repo communities.Repository, suffix string) *communities.Community {
	t.Helper()

	community := &communities.Community{
		DID:             "did:plc:testcommunity" + suffix,
		Name:            "test-community-" + suffix,
		Handle:          "test-community-" + suffix + ".communities.coves.local",
		Description:     "Test community for thumb validation",
		Visibility:      "public",
		PDSEmail:        "test@communities.coves.local",
		PDSPassword:     "test-password",
		PDSAccessToken:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkaWQ6cGxjOnRlc3Rjb21tdW5pdHkxMjMiLCJleHAiOjk5OTk5OTk5OTl9.test",
		PDSRefreshToken: "refresh_token_test123",
		PDSURL:          "http://localhost:3001",
	}

	created, err := repo.Create(context.Background(), community)
	require.NoError(t, err)

	return created
}

// TestPostHandler_ThumbValidation tests strict validation of thumb field in external embeds
func TestPostHandler_ThumbValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Setup services
	communityRepo := postgres.NewCommunityRepository(db)
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
		nil,
		nil,
	)

	postRepo := postgres.NewPostRepository(db)
	// No blobService or unfurlService for these validation tests
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, "http://localhost:3001")

	handler := post.NewCreateHandler(postService)

	// Create test user and community with PDS credentials (use unique IDs)
	testUser := createTestUser(t, db, "thumbtest.bsky.social", "did:plc:thumbtest"+t.Name())
	testCommunity := createTestCommunityWithCredentials(t, communityRepo, t.Name())

	t.Run("Reject thumb as URL string", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"content":   "Test content",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":   "https://streamable.com/test",
					"thumb": "https://example.com/thumb.jpg", // ❌ URL string (invalid)
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		// Mock authenticated user context
		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 400 Bad Request
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Contains(t, errResp["message"], "thumb must be a blob reference")
		assert.Contains(t, errResp["message"], "not URL string")
	})

	t.Run("Reject thumb missing $type", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://streamable.com/test",
					"thumb": map[string]interface{}{ // ❌ Missing $type
						"ref":      map[string]interface{}{"$link": "bafyrei123"},
						"mimeType": "image/jpeg",
						"size":     12345,
					},
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Contains(t, errResp["message"], "thumb must have $type: blob")
	})

	t.Run("Reject thumb missing ref field", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://streamable.com/test",
					"thumb": map[string]interface{}{
						"$type": "blob",
						// ❌ Missing ref field
						"mimeType": "image/jpeg",
						"size":     12345,
					},
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Contains(t, errResp["message"], "thumb blob missing required 'ref' field")
	})

	t.Run("Reject thumb missing mimeType field", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://streamable.com/test",
					"thumb": map[string]interface{}{
						"$type": "blob",
						"ref":   map[string]interface{}{"$link": "bafyrei123"},
						// ❌ Missing mimeType field
						"size": 12345,
					},
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Contains(t, errResp["message"], "thumb blob missing required 'mimeType' field")
	})

	t.Run("Accept valid blob reference", func(t *testing.T) {
		// Note: This test will fail at PDS write because the blob doesn't actually exist
		// But it validates that our thumb validation accepts properly formatted blobs
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://streamable.com/test",
					"thumb": map[string]interface{}{ // ✅ Valid blob
						"$type":    "blob",
						"ref":      map[string]interface{}{"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},
						"mimeType": "image/jpeg",
						"size":     52813,
					},
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should not fail with thumb validation error
		// (May fail later at PDS write, but that's expected for test data)
		if rec.Code == http.StatusBadRequest {
			var errResp map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
			// If it's a bad request, it should NOT be about thumb validation
			assert.NotContains(t, errResp["message"], "thumb must be")
			assert.NotContains(t, errResp["message"], "thumb blob missing")
		}
	})

	t.Run("Accept missing thumb (unfurl will handle)", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": testCommunity.DID,
			"title":     "Test Post",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri": "https://streamable.com/test",
					// ✅ No thumb field - unfurl service will handle
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), testUser.DID)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should not fail with thumb validation error
		if rec.Code == http.StatusBadRequest {
			var errResp map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
			// Should not be a thumb validation error
			assert.NotContains(t, errResp["message"], "thumb must be")
		}
	})
}
