package integration

import (
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostHandler_SecurityValidation tests HTTP handler-level security checks
func TestPostHandler_SecurityValidation(t *testing.T) {
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
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)

	postRepo := postgres.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, "http://localhost:3001") // nil optional services

	// Create handler
	handler := post.NewCreateHandler(postService)

	t.Run("Reject client-provided authorDid", func(t *testing.T) {
		// Client tries to impersonate another user
		payload := map[string]interface{}{
			"community": "did:plc:test123",
			"authorDid": "did:plc:attacker", // ❌ Client trying to set author
			"content":   "Malicious post",
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		// Mock authenticated user context
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 400 Bad Request
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errResp["error"])
		assert.Contains(t, errResp["message"], "authorDid must not be provided")
	})

	t.Run("Reject missing authentication", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": "did:plc:test123",
			"content":   "Test post",
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		// No auth context set
		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 401 Unauthorized
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "AuthRequired", errResp["error"])
	})

	t.Run("Reject request body > 1MB", func(t *testing.T) {
		// Create a payload larger than 1MB
		largeContent := strings.Repeat("A", 1*1024*1024+1000) // 1MB + 1KB

		payload := map[string]interface{}{
			"community": "did:plc:test123",
			"content":   largeContent,
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		// Mock authenticated user context
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 413 Request Entity Too Large
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "RequestTooLarge", errResp["error"])
	})

	t.Run("Reject malformed JSON", func(t *testing.T) {
		// Invalid JSON
		invalidJSON := []byte(`{"community": "did:plc:test123", "content": `)

		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(invalidJSON))

		// Mock authenticated user context
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 400 Bad Request
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errResp["error"])
	})

	t.Run("Reject empty community field", func(t *testing.T) {
		payload := map[string]interface{}{
			"community": "", // Empty community
			"content":   "Test post",
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		// Mock authenticated user context
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should return 400 Bad Request
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]interface{}
		err := json.Unmarshal(rec.Body.Bytes(), &errResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errResp["error"])
		assert.Contains(t, errResp["message"], "community is required")
	})

	t.Run("Reject invalid at-identifier format", func(t *testing.T) {
		invalidIdentifiers := []string{
			"not-a-did-or-handle",
			"just-plain-text",
			"http://example.com",
		}

		for _, invalidID := range invalidIdentifiers {
			t.Run(invalidID, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": invalidID,
					"content":   "Test post",
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				// Mock authenticated user context
				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// Should reject (either 400 InvalidRequest or 404 NotFound depending on how service resolves it)
				// Both are valid - the important thing is that it rejects invalid identifiers
				assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusNotFound,
					"Should reject invalid identifier with 400 or 404, got %d", rec.Code)

				var errResp map[string]interface{}
				err := json.Unmarshal(rec.Body.Bytes(), &errResp)
				require.NoError(t, err)

				// Should have an error type and message
				assert.NotEmpty(t, errResp["error"], "should have error type")
				assert.NotEmpty(t, errResp["message"], "should have error message")
			})
		}
	})

	t.Run("Accept valid DID format", func(t *testing.T) {
		validDIDs := []string{
			"did:plc:test123",
			"did:web:example.com",
		}

		for _, validDID := range validDIDs {
			t.Run(validDID, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": validDID,
					"content":   "Test post",
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				// Mock authenticated user context
				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// May fail at service layer (community not found), but should NOT fail at validation
				// Looking for anything OTHER than "community must be a DID" error
				if rec.Code == http.StatusBadRequest {
					var errResp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &errResp)
					require.NoError(t, err)

					// Should NOT be the format validation error
					assert.NotContains(t, errResp["message"], "community must be a DID")
				}
			})
		}
	})

	t.Run("Accept valid scoped handle format", func(t *testing.T) {
		// Scoped format: !name@instance (gets converted to c-name.instance internally)
		validScopedHandles := []string{
			"!mycommunity@bsky.social",  // Scoped format
			"!gaming@test.coves.social", // Scoped format
		}

		for _, validHandle := range validScopedHandles {
			t.Run(validHandle, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": validHandle,
					"content":   "Test post",
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				// Mock authenticated user context
				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// May fail at service layer (community not found), but should NOT fail at format validation
				if rec.Code == http.StatusBadRequest {
					var errResp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &errResp)
					require.NoError(t, err)

					// Should NOT be the format validation error
					assert.NotContains(t, errResp["message"], "community must be a DID")
					assert.NotContains(t, errResp["message"], "scoped handle must include")
				}
			})
		}
	})

	t.Run("Accept valid canonical handle format", func(t *testing.T) {
		// Canonical format: c-name.instance (DNS-resolvable atProto handle)
		validCanonicalHandles := []string{
			"c-gaming.test.coves.social",
			"c-books.bsky.social",
		}

		for _, validHandle := range validCanonicalHandles {
			t.Run(validHandle, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": validHandle,
					"content":   "Test post",
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				// Mock authenticated user context
				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// May fail at service layer (community not found), but should NOT fail at format validation
				// Canonical handles don't have strict validation at handler level - they're validated by the service
				if rec.Code == http.StatusBadRequest {
					var errResp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &errResp)
					require.NoError(t, err)

					// Should NOT be the format validation error (canonical handles pass basic validation)
					assert.NotContains(t, errResp["message"], "community must be a DID")
				}
			})
		}
	})

	t.Run("Accept valid @-prefixed handle format", func(t *testing.T) {
		// @-prefixed format: @c-name.instance (atProto standard, @ gets stripped)
		validAtHandles := []string{
			"@c-gaming.test.coves.social",
			"@c-books.bsky.social",
		}

		for _, validHandle := range validAtHandles {
			t.Run(validHandle, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": validHandle,
					"content":   "Test post",
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				// Mock authenticated user context
				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// May fail at service layer (community not found), but should NOT fail at format validation
				// @ prefix is valid and gets stripped by the resolver
				if rec.Code == http.StatusBadRequest {
					var errResp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &errResp)
					require.NoError(t, err)

					// Should NOT be the format validation error
					assert.NotContains(t, errResp["message"], "community must be a DID")
				}
			})
		}
	})

	t.Run("Reject non-POST methods", func(t *testing.T) {
		methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}

		for _, method := range methods {
			t.Run(method, func(t *testing.T) {
				req := httptest.NewRequest(method, "/xrpc/social.coves.community.post.create", nil)
				rec := httptest.NewRecorder()

				handler.HandleCreate(rec, req)

				// Should return 405 Method Not Allowed
				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			})
		}
	})
}

// TestPostHandler_SpecialCharacters tests content with special characters
func TestPostHandler_SpecialCharacters(t *testing.T) {
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
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)

	postRepo := postgres.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, "http://localhost:3001") // nil optional services

	handler := post.NewCreateHandler(postService)

	t.Run("Accept Unicode and emoji", func(t *testing.T) {
		content := "Hello 世界! 🌍 Testing unicode: café, naïve, Ω"

		payload := map[string]interface{}{
			"community": "did:plc:test123",
			"content":   content,
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.HandleCreate(rec, req)

		// Should NOT reject due to unicode/special characters
		// May fail at service layer for other reasons, but should pass handler validation
		assert.NotEqual(t, http.StatusBadRequest, rec.Code, "Handler should not reject valid unicode")
	})

	t.Run("SQL injection attempt is safely handled", func(t *testing.T) {
		// Common SQL injection patterns
		sqlInjections := []string{
			"'; DROP TABLE posts; --",
			"1' OR '1'='1",
			"<script>alert('xss')</script>",
			"../../../etc/passwd",
		}

		for _, injection := range sqlInjections {
			t.Run(injection, func(t *testing.T) {
				payload := map[string]interface{}{
					"community": "did:plc:test123",
					"content":   injection,
				}

				body, _ := json.Marshal(payload)
				req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.post.create", bytes.NewReader(body))

				ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice")
				req = req.WithContext(ctx)

				rec := httptest.NewRecorder()
				handler.HandleCreate(rec, req)

				// Handler should NOT crash or return 500
				// These are just strings, should be handled safely
				assert.NotEqual(t, http.StatusInternalServerError, rec.Code,
					"Handler should not crash on injection attempt")
			})
		}
	})
}

// TestPostService_DIDValidationSecurity tests service-layer DID validation (defense-in-depth)
func TestPostService_DIDValidationSecurity(t *testing.T) {
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
	communityService := communities.NewCommunityService(
		communityRepo,
		"http://localhost:3001",
		"did:web:test.coves.social",
		"test.coves.social",
		nil,
	)

	postRepo := postgres.NewPostRepository(db)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, "http://localhost:3001")

	t.Run("Reject posts when context DID is missing", func(t *testing.T) {
		// Simulate bypassing handler - no DID in context
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := middleware.SetTestUserDID(req.Context(), "") // Empty DID

		content := "Test post"
		postReq := posts.CreatePostRequest{
			Community: "did:plc:test123",
			AuthorDID: "did:plc:alice",
			Content:   &content,
		}

		_, err := postService.CreatePost(ctx, postReq)

		// Should fail with authentication error
		assert.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "authenticated")
	})

	t.Run("Reject posts when request DID doesn't match context DID", func(t *testing.T) {
		// SECURITY TEST: This prevents DID spoofing attacks
		// Simulates attack where handler is bypassed or compromised
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice") // Authenticated as Alice

		content := "Spoofed post"
		postReq := posts.CreatePostRequest{
			Community: "did:plc:test123",
			AuthorDID: "did:plc:bob", // ❌ Trying to post as Bob!
			Content:   &content,
		}

		_, err := postService.CreatePost(ctx, postReq)

		// Should fail with DID mismatch error
		assert.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "does not match")
	})

	t.Run("Accept posts when request DID matches context DID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := middleware.SetTestUserDID(req.Context(), "did:plc:alice") // Authenticated as Alice

		content := "Valid post"
		postReq := posts.CreatePostRequest{
			Community: "did:plc:test123",
			AuthorDID: "did:plc:alice", // ✓ Matching DID
			Content:   &content,
		}

		_, err := postService.CreatePost(ctx, postReq)
		// May fail for other reasons (community not found), but NOT due to DID mismatch
		if err != nil {
			assert.NotContains(t, strings.ToLower(err.Error()), "does not match",
				"Should not fail due to DID mismatch when DIDs match")
		}
	})
}
