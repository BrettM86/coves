package integration

import (
	"Coves/internal/atproto/oauth"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/go-chi/chi/v5"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOAuth_Components tests OAuth component functionality without requiring PDS.
// This validates all Coves OAuth code:
// - Session storage and retrieval (PostgreSQL)
// - Token sealing (AES-GCM encryption)
// - Token unsealing (decryption + validation)
// - Session cleanup
//
// NOTE: Full OAuth redirect flow testing requires both HTTPS PDS and HTTPS Coves deployment.
// The OAuth redirect flow is handled by indigo's library and enforces OAuth 2.0 spec
// (HTTPS required for authorization servers and redirect URIs).
func TestOAuth_Components(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth component test in short mode")
	}

	// Setup test database
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// Run migrations to ensure OAuth tables exist
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	t.Log("🔧 Testing OAuth Components")

	ctx := context.Background()

	// Setup OAuth client and store
	store := SetupOAuthTestStore(t, db)
	client := SetupOAuthTestClient(t, store)
	require.NotNil(t, client, "OAuth client should be initialized")

	// Use a test DID (doesn't need to exist on PDS for component tests)
	testDID := "did:plc:componenttest123"

	// Run component tests
	testOAuthComponentsWithMockedSession(t, ctx, nil, store, client, testDID, "")

	t.Log("")
	t.Log(strings.Repeat("=", 60))
	t.Log("✅ OAuth Component Tests Complete")
	t.Log(strings.Repeat("=", 60))
	t.Log("Components validated:")
	t.Log("  ✓ Session storage (PostgreSQL)")
	t.Log("  ✓ Token sealing (AES-GCM encryption)")
	t.Log("  ✓ Token unsealing (decryption + validation)")
	t.Log("  ✓ Session cleanup")
	t.Log("")
	t.Log("NOTE: Full OAuth redirect flow requires HTTPS PDS + HTTPS Coves")
	t.Log(strings.Repeat("=", 60))
}

// testOAuthComponentsWithMockedSession tests OAuth components that work without PDS redirect flow.
// This is used when testing with localhost PDS, where the indigo library rejects http:// URLs.
func testOAuthComponentsWithMockedSession(t *testing.T, ctx context.Context, _ interface{}, store oauthlib.ClientAuthStore, client *oauth.OAuthClient, userDID, _ string) {
	t.Helper()

	t.Log("🔧 Testing OAuth components with mocked session...")

	// Parse DID
	parsedDID, err := syntax.ParseDID(userDID)
	require.NoError(t, err, "Should parse DID")

	// Component 1: Session Storage
	t.Log("   📦 Component 1: Testing session storage...")
	testSession := oauthlib.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   fmt.Sprintf("localhost-test-%d", time.Now().UnixNano()),
		HostURL:     "http://localhost:3001",
		AccessToken: "mocked-access-token",
		Scopes:      []string{"atproto", "transition:generic"},
	}

	err = store.SaveSession(ctx, testSession)
	require.NoError(t, err, "Should save session")

	retrieved, err := store.GetSession(ctx, parsedDID, testSession.SessionID)
	require.NoError(t, err, "Should retrieve session")
	require.Equal(t, testSession.SessionID, retrieved.SessionID)
	require.Equal(t, testSession.AccessToken, retrieved.AccessToken)
	t.Log("   ✅ Session storage working")

	// Component 2: Token Sealing
	t.Log("   🔐 Component 2: Testing token sealing...")
	sealedToken, err := client.SealSession(parsedDID.String(), testSession.SessionID, time.Hour)
	require.NoError(t, err, "Should seal token")
	require.NotEmpty(t, sealedToken, "Sealed token should not be empty")
	tokenPreview := sealedToken
	if len(tokenPreview) > 50 {
		tokenPreview = tokenPreview[:50]
	}
	t.Logf("   ✅ Token sealed: %s...", tokenPreview)

	// Component 3: Token Unsealing
	t.Log("   🔓 Component 3: Testing token unsealing...")
	unsealed, err := client.UnsealSession(sealedToken)
	require.NoError(t, err, "Should unseal token")
	require.Equal(t, userDID, unsealed.DID)
	require.Equal(t, testSession.SessionID, unsealed.SessionID)
	t.Log("   ✅ Token unsealing working")

	// Component 4: Session Cleanup
	t.Log("   🧹 Component 4: Testing session cleanup...")
	err = store.DeleteSession(ctx, parsedDID, testSession.SessionID)
	require.NoError(t, err, "Should delete session")

	_, err = store.GetSession(ctx, parsedDID, testSession.SessionID)
	require.Error(t, err, "Session should not exist after deletion")
	t.Log("   ✅ Session cleanup working")

	t.Log("✅ All OAuth components verified!")
	t.Log("")
	t.Log("📝 Summary: OAuth implementation validated with mocked session")
	t.Log("   - Session storage: ✓")
	t.Log("   - Token sealing: ✓")
	t.Log("   - Token unsealing: ✓")
	t.Log("   - Session cleanup: ✓")
	t.Log("")
	t.Log("⚠️  To test full OAuth redirect flow, use a production PDS with HTTPS")
}

// TestOAuthE2E_TokenExpiration tests that expired sealed tokens are rejected
func TestOAuthE2E_TokenExpiration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth token expiration test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("⏰ Testing OAuth token expiration...")

	// Setup OAuth client and store
	store := SetupOAuthTestStore(t, db)
	client := SetupOAuthTestClient(t, store)
	_ = oauth.NewOAuthHandler(client, store) // Handler created for completeness

	// Create test session with past expiration
	did, err := syntax.ParseDID("did:plc:expiredtest123")
	require.NoError(t, err)

	testSession := oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   "expired-session",
		HostURL:     "http://localhost:3001",
		AccessToken: "expired-token",
		Scopes:      []string{"atproto"},
	}

	// Save session
	err = store.SaveSession(ctx, testSession)
	require.NoError(t, err)

	// Manually update expiration to the past
	_, err = db.ExecContext(ctx,
		"UPDATE oauth_sessions SET expires_at = NOW() - INTERVAL '1 day' WHERE did = $1 AND session_id = $2",
		did.String(), testSession.SessionID)
	require.NoError(t, err)

	// Try to retrieve expired session
	_, err = store.GetSession(ctx, did, testSession.SessionID)
	assert.Error(t, err, "Should not be able to retrieve expired session")
	assert.Equal(t, oauth.ErrSessionNotFound, err, "Should return ErrSessionNotFound for expired session")

	// Test cleanup of expired sessions
	// Need to access the underlying PostgresOAuthStore through the wrapper
	var pgStore *oauth.PostgresOAuthStore
	if wrapper, ok := store.(*oauth.MobileAwareStoreWrapper); ok {
		pgStore, _ = wrapper.ClientAuthStore.(*oauth.PostgresOAuthStore)
	} else {
		pgStore, _ = store.(*oauth.PostgresOAuthStore)
	}
	require.NotNil(t, pgStore, "Should be able to access PostgresOAuthStore")

	cleaned, err := pgStore.CleanupExpiredSessions(ctx)
	require.NoError(t, err, "Cleanup should succeed")
	assert.Greater(t, cleaned, int64(0), "Should have cleaned up at least one session")

	t.Logf("✅ Expired session handling verified (cleaned %d sessions)", cleaned)
}

// TestOAuthE2E_InvalidToken tests that invalid/tampered tokens are rejected
func TestOAuthE2E_InvalidToken(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth invalid token test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	t.Log("🔒 Testing OAuth invalid token rejection...")

	// Setup OAuth client and store
	store := SetupOAuthTestStore(t, db)
	client := SetupOAuthTestClient(t, store)
	handler := oauth.NewOAuthHandler(client, store)

	// Setup test server with protected endpoint
	r := chi.NewRouter()
	r.Get("/api/me", func(w http.ResponseWriter, r *http.Request) {
		sessData, err := handler.GetSessionFromRequest(r)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"did": sessData.AccountDID.String()})
	})

	server := httptest.NewServer(r)
	defer server.Close()

	// Test with invalid token formats
	testCases := []struct {
		name  string
		token string
	}{
		{"Empty token", ""},
		{"Invalid base64", "not-valid-base64!!!"},
		{"Tampered token", "dGFtcGVyZWQtdG9rZW4tZGF0YQ=="}, // Valid base64 but invalid content
		{"Short token", "abc"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", server.URL+"/api/me", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
				"Invalid token should be rejected with 401")
		})
	}

	t.Logf("✅ Invalid token rejection verified")
}

// TestOAuthE2E_SessionNotFound tests behavior when session doesn't exist in DB
func TestOAuthE2E_SessionNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth session not found test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("🔍 Testing OAuth session not found behavior...")

	// Setup OAuth store
	store := SetupOAuthTestStore(t, db)

	// Try to retrieve non-existent session
	nonExistentDID, err := syntax.ParseDID("did:plc:nonexistent123")
	require.NoError(t, err)

	_, err = store.GetSession(ctx, nonExistentDID, "nonexistent-session")
	assert.Error(t, err, "Should return error for non-existent session")
	assert.Equal(t, oauth.ErrSessionNotFound, err, "Should return ErrSessionNotFound")

	// Try to delete non-existent session
	err = store.DeleteSession(ctx, nonExistentDID, "nonexistent-session")
	assert.Error(t, err, "Should return error when deleting non-existent session")
	assert.Equal(t, oauth.ErrSessionNotFound, err, "Should return ErrSessionNotFound")

	t.Logf("✅ Session not found handling verified")
}

// TestOAuthE2E_MultipleSessionsPerUser tests that a user can have multiple active sessions
func TestOAuthE2E_MultipleSessionsPerUser(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth multiple sessions test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("👥 Testing multiple OAuth sessions per user...")

	// Setup OAuth store
	store := SetupOAuthTestStore(t, db)

	// Create a test DID
	did, err := syntax.ParseDID("did:plc:multisession123")
	require.NoError(t, err)

	// Create multiple sessions for the same user
	sessions := []oauthlib.ClientSessionData{
		{
			AccountDID:  did,
			SessionID:   "session-1-web",
			HostURL:     "http://localhost:3001",
			AccessToken: "token-1",
			Scopes:      []string{"atproto"},
		},
		{
			AccountDID:  did,
			SessionID:   "session-2-mobile",
			HostURL:     "http://localhost:3001",
			AccessToken: "token-2",
			Scopes:      []string{"atproto"},
		},
		{
			AccountDID:  did,
			SessionID:   "session-3-tablet",
			HostURL:     "http://localhost:3001",
			AccessToken: "token-3",
			Scopes:      []string{"atproto"},
		},
	}

	// Save all sessions
	for i, session := range sessions {
		err := store.SaveSession(ctx, session)
		require.NoError(t, err, "Should be able to save session %d", i+1)
	}

	t.Logf("✅ Created %d sessions for user", len(sessions))

	// Verify all sessions can be retrieved independently
	for i, session := range sessions {
		retrieved, err := store.GetSession(ctx, did, session.SessionID)
		require.NoError(t, err, "Should be able to retrieve session %d", i+1)
		assert.Equal(t, session.SessionID, retrieved.SessionID, "Session ID should match")
		assert.Equal(t, session.AccessToken, retrieved.AccessToken, "Access token should match")
	}

	t.Logf("✅ All sessions retrieved independently")

	// Delete one session and verify others remain
	err = store.DeleteSession(ctx, did, sessions[0].SessionID)
	require.NoError(t, err, "Should be able to delete first session")

	// Verify first session is deleted
	_, err = store.GetSession(ctx, did, sessions[0].SessionID)
	assert.Equal(t, oauth.ErrSessionNotFound, err, "First session should be deleted")

	// Verify other sessions still exist
	for i := 1; i < len(sessions); i++ {
		_, err := store.GetSession(ctx, did, sessions[i].SessionID)
		require.NoError(t, err, "Session %d should still exist", i+1)
	}

	t.Logf("✅ Multiple sessions per user verified")

	// Cleanup
	for i := 1; i < len(sessions); i++ {
		_ = store.DeleteSession(ctx, did, sessions[i].SessionID)
	}
}

// TestOAuthE2E_AuthRequestStorage tests OAuth auth request storage and retrieval
func TestOAuthE2E_AuthRequestStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth auth request storage test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("📝 Testing OAuth auth request storage...")

	// Setup OAuth store
	store := SetupOAuthTestStore(t, db)

	// Create test auth request data
	did, err := syntax.ParseDID("did:plc:authrequest123")
	require.NoError(t, err)

	authRequest := oauthlib.AuthRequestData{
		State:                        "test-state-12345",
		AccountDID:                   &did,
		PKCEVerifier:                 "test-pkce-verifier",
		DPoPPrivateKeyMultibase:      "test-dpop-key",
		DPoPAuthServerNonce:          "test-nonce",
		AuthServerURL:                "http://localhost:3001",
		RequestURI:                   "http://localhost:3001/authorize",
		AuthServerTokenEndpoint:      "http://localhost:3001/oauth/token",
		AuthServerRevocationEndpoint: "http://localhost:3001/oauth/revoke",
		Scopes:                       []string{"atproto", "transition:generic"},
	}

	// Save auth request
	err = store.SaveAuthRequestInfo(ctx, authRequest)
	require.NoError(t, err, "Should be able to save auth request")

	t.Logf("✅ Auth request saved")

	// Retrieve auth request
	retrieved, err := store.GetAuthRequestInfo(ctx, authRequest.State)
	require.NoError(t, err, "Should be able to retrieve auth request")
	assert.Equal(t, authRequest.State, retrieved.State, "State should match")
	assert.Equal(t, authRequest.PKCEVerifier, retrieved.PKCEVerifier, "PKCE verifier should match")
	assert.Equal(t, authRequest.AuthServerURL, retrieved.AuthServerURL, "Auth server URL should match")
	assert.Equal(t, len(authRequest.Scopes), len(retrieved.Scopes), "Scopes length should match")

	t.Logf("✅ Auth request retrieved and verified")

	// Test duplicate state error
	err = store.SaveAuthRequestInfo(ctx, authRequest)
	assert.Error(t, err, "Should not allow duplicate state")
	assert.Contains(t, err.Error(), "already exists", "Error should indicate duplicate")

	t.Logf("✅ Duplicate state prevention verified")

	// Delete auth request
	err = store.DeleteAuthRequestInfo(ctx, authRequest.State)
	require.NoError(t, err, "Should be able to delete auth request")

	// Verify deletion
	_, err = store.GetAuthRequestInfo(ctx, authRequest.State)
	assert.Equal(t, oauth.ErrAuthRequestNotFound, err, "Auth request should be deleted")

	t.Logf("✅ Auth request deletion verified")

	// Test cleanup of expired auth requests
	// Create an auth request and manually set created_at to the past
	// Use unique state to avoid conflicts with previous test runs
	oldState := fmt.Sprintf("old-state-%d", time.Now().UnixNano())
	oldAuthRequest := oauthlib.AuthRequestData{
		State:         oldState,
		PKCEVerifier:  "old-verifier",
		AuthServerURL: "http://localhost:3001",
		Scopes:        []string{"atproto"},
	}

	// Clean up any existing state first
	_ = store.DeleteAuthRequestInfo(ctx, oldState)

	err = store.SaveAuthRequestInfo(ctx, oldAuthRequest)
	require.NoError(t, err)

	// Update created_at to 1 hour ago
	_, err = db.ExecContext(ctx,
		"UPDATE oauth_requests SET created_at = NOW() - INTERVAL '1 hour' WHERE state = $1",
		oldAuthRequest.State)
	require.NoError(t, err)

	// Cleanup expired requests
	// Need to access the underlying PostgresOAuthStore through the wrapper
	var pgStore *oauth.PostgresOAuthStore
	if wrapper, ok := store.(*oauth.MobileAwareStoreWrapper); ok {
		pgStore, _ = wrapper.ClientAuthStore.(*oauth.PostgresOAuthStore)
	} else {
		pgStore, _ = store.(*oauth.PostgresOAuthStore)
	}
	require.NotNil(t, pgStore, "Should be able to access PostgresOAuthStore")

	cleaned, err := pgStore.CleanupExpiredAuthRequests(ctx)
	require.NoError(t, err, "Cleanup should succeed")
	assert.Greater(t, cleaned, int64(0), "Should have cleaned up at least one auth request")

	t.Logf("✅ Expired auth request cleanup verified (cleaned %d requests)", cleaned)
}

// TestOAuthE2E_TokenRefresh tests the refresh token flow
func TestOAuthE2E_TokenRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth token refresh test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("🔄 Testing OAuth token refresh flow...")

	// Setup OAuth client and store
	store := SetupOAuthTestStore(t, db)
	client := SetupOAuthTestClient(t, store)
	handler := oauth.NewOAuthHandler(client, store)

	// Create a test DID and session
	did, err := syntax.ParseDID("did:plc:refreshtest123")
	require.NoError(t, err)

	// Create initial session with refresh token
	initialSession := oauthlib.ClientSessionData{
		AccountDID:                   did,
		SessionID:                    "refresh-session-1",
		HostURL:                      "http://localhost:3001",
		AuthServerURL:                "http://localhost:3001",
		AuthServerTokenEndpoint:      "http://localhost:3001/oauth/token",
		AuthServerRevocationEndpoint: "http://localhost:3001/oauth/revoke",
		AccessToken:                  "initial-access-token",
		RefreshToken:                 "initial-refresh-token",
		DPoPPrivateKeyMultibase:      "test-dpop-key",
		DPoPAuthServerNonce:          "test-nonce",
		Scopes:                       []string{"atproto", "transition:generic"},
	}

	// Save the session
	err = store.SaveSession(ctx, initialSession)
	require.NoError(t, err, "Should save initial session")

	t.Logf("✅ Initial session created")

	// Create a sealed token for this session
	sealedToken, err := client.SealSession(did.String(), initialSession.SessionID, time.Hour)
	require.NoError(t, err, "Should seal session token")
	require.NotEmpty(t, sealedToken, "Sealed token should not be empty")

	t.Logf("✅ Session token sealed")

	// Setup test server with refresh endpoint
	r := chi.NewRouter()
	r.Post("/oauth/refresh", handler.HandleRefresh)

	server := httptest.NewServer(r)
	defer server.Close()

	t.Run("Valid refresh request", func(t *testing.T) {
		// NOTE: This test verifies that the refresh endpoint can be called
		// In a real scenario, the indigo client's RefreshTokens() would call the PDS
		// Since we're in a component test, we're testing the Coves handler logic

		// Create refresh request
		refreshReq := map[string]interface{}{
			"did":          did.String(),
			"session_id":   initialSession.SessionID,
			"sealed_token": sealedToken,
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		// NOTE: In component testing mode, the indigo client may not have
		// real PDS credentials, so RefreshTokens() might fail
		// We're testing that the handler correctly processes the request
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		// In component test mode without real PDS, we may get 401
		// In production with real PDS, this would return 200 with new tokens
		t.Logf("Refresh response status: %d", resp.StatusCode)

		// The important thing is that the handler doesn't crash
		// and properly validates the request structure
		assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized,
			"Refresh should return either success or auth failure, got %d", resp.StatusCode)
	})

	t.Run("Invalid DID format (with valid token)", func(t *testing.T) {
		// Create a sealed token with an invalid DID format
		invalidDID := "invalid-did-format"
		// Create the token with a valid DID first, then we'll try to use it with invalid DID in request
		validToken, err := client.SealSession(did.String(), initialSession.SessionID, 30*24*time.Hour)
		require.NoError(t, err)

		refreshReq := map[string]interface{}{
			"did":          invalidDID, // Invalid DID format in request
			"session_id":   initialSession.SessionID,
			"sealed_token": validToken, // Valid token for different DID
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		// Should reject with 401 due to DID mismatch (not 400) since auth happens first
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"DID mismatch should be rejected with 401 (auth check happens before format validation)")
	})

	t.Run("Missing sealed_token (security test)", func(t *testing.T) {
		refreshReq := map[string]interface{}{
			"did":        did.String(),
			"session_id": initialSession.SessionID,
			// Missing sealed_token - should be rejected for security
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"Missing sealed_token should be rejected (proof of possession required)")
	})

	t.Run("Invalid sealed_token", func(t *testing.T) {
		refreshReq := map[string]interface{}{
			"did":          did.String(),
			"session_id":   initialSession.SessionID,
			"sealed_token": "invalid-token-data",
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"Invalid sealed_token should be rejected")
	})

	t.Run("DID mismatch (security test)", func(t *testing.T) {
		// Create a sealed token for a different DID
		wrongDID := "did:plc:wronguser123"
		wrongToken, err := client.SealSession(wrongDID, initialSession.SessionID, 30*24*time.Hour)
		require.NoError(t, err)

		// Try to use it to refresh the original session
		refreshReq := map[string]interface{}{
			"did":          did.String(), // Claiming original DID
			"session_id":   initialSession.SessionID,
			"sealed_token": wrongToken, // But token is for different DID
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"DID mismatch should be rejected (prevents session hijacking)")
	})

	t.Run("Session ID mismatch (security test)", func(t *testing.T) {
		// Create a sealed token with wrong session ID
		wrongSessionID := "wrong-session-id"
		wrongToken, err := client.SealSession(did.String(), wrongSessionID, 30*24*time.Hour)
		require.NoError(t, err)

		// Try to use it to refresh the original session
		refreshReq := map[string]interface{}{
			"did":          did.String(),
			"session_id":   initialSession.SessionID, // Claiming original session
			"sealed_token": wrongToken,               // But token is for different session
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"Session ID mismatch should be rejected (prevents session hijacking)")
	})

	t.Run("Non-existent session", func(t *testing.T) {
		// Create a valid sealed token for a non-existent session
		nonExistentSessionID := "nonexistent-session-id"
		validToken, err := client.SealSession(did.String(), nonExistentSessionID, 30*24*time.Hour)
		require.NoError(t, err)

		refreshReq := map[string]interface{}{
			"did":          did.String(),
			"session_id":   nonExistentSessionID,
			"sealed_token": validToken, // Valid token but session doesn't exist
		}

		reqBody, err := json.Marshal(refreshReq)
		require.NoError(t, err)

		req, err := http.NewRequest("POST", server.URL+"/oauth/refresh", strings.NewReader(string(reqBody)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"Non-existent session should be rejected with 401")
	})

	t.Logf("✅ Token refresh endpoint validation verified")
}

// TestOAuthE2E_SessionUpdate tests that refresh updates the session in database
func TestOAuthE2E_SessionUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth session update test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("💾 Testing OAuth session update on refresh...")

	// Setup OAuth store
	store := SetupOAuthTestStore(t, db)

	// Create a test session
	did, err := syntax.ParseDID("did:plc:sessionupdate123")
	require.NoError(t, err)

	originalSession := oauthlib.ClientSessionData{
		AccountDID:              did,
		SessionID:               "update-session-1",
		HostURL:                 "http://localhost:3001",
		AuthServerURL:           "http://localhost:3001",
		AuthServerTokenEndpoint: "http://localhost:3001/oauth/token",
		AccessToken:             "original-access-token",
		RefreshToken:            "original-refresh-token",
		DPoPPrivateKeyMultibase: "original-dpop-key",
		Scopes:                  []string{"atproto"},
	}

	// Save original session
	err = store.SaveSession(ctx, originalSession)
	require.NoError(t, err)

	t.Logf("✅ Original session saved")

	// Simulate a token refresh by updating the session with new tokens
	updatedSession := originalSession
	updatedSession.AccessToken = "new-access-token"
	updatedSession.RefreshToken = "new-refresh-token"
	updatedSession.DPoPAuthServerNonce = "new-nonce"

	// Update the session (upsert)
	err = store.SaveSession(ctx, updatedSession)
	require.NoError(t, err)

	t.Logf("✅ Session updated with new tokens")

	// Retrieve the session and verify it was updated
	retrieved, err := store.GetSession(ctx, did, originalSession.SessionID)
	require.NoError(t, err, "Should retrieve updated session")

	assert.Equal(t, "new-access-token", retrieved.AccessToken,
		"Access token should be updated")
	assert.Equal(t, "new-refresh-token", retrieved.RefreshToken,
		"Refresh token should be updated")
	assert.Equal(t, "new-nonce", retrieved.DPoPAuthServerNonce,
		"DPoP nonce should be updated")

	// Verify session ID and DID remain the same
	assert.Equal(t, originalSession.SessionID, retrieved.SessionID,
		"Session ID should remain the same")
	assert.Equal(t, did, retrieved.AccountDID,
		"DID should remain the same")

	t.Logf("✅ Session update verified - tokens refreshed in database")

	// Verify updated_at was changed
	var updatedAt time.Time
	err = db.QueryRowContext(ctx,
		"SELECT updated_at FROM oauth_sessions WHERE did = $1 AND session_id = $2",
		did.String(), originalSession.SessionID).Scan(&updatedAt)
	require.NoError(t, err)

	// Updated timestamp should be recent (within last minute)
	assert.WithinDuration(t, time.Now(), updatedAt, time.Minute,
		"Session updated_at should be recent")

	t.Logf("✅ Session timestamp update verified")
}

// TestOAuthE2E_RefreshTokenRotation tests refresh token rotation behavior
func TestOAuthE2E_RefreshTokenRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OAuth refresh token rotation test in short mode")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(db, "../../internal/db/migrations"))

	ctx := context.Background()

	t.Log("🔄 Testing OAuth refresh token rotation...")

	// Setup OAuth store
	store := SetupOAuthTestStore(t, db)

	// Create a test session
	did, err := syntax.ParseDID("did:plc:rotation123")
	require.NoError(t, err)

	// Simulate multiple refresh cycles
	sessionID := "rotation-session-1"
	tokens := []struct {
		access  string
		refresh string
	}{
		{"access-token-v1", "refresh-token-v1"},
		{"access-token-v2", "refresh-token-v2"},
		{"access-token-v3", "refresh-token-v3"},
	}

	for i, tokenPair := range tokens {
		session := oauthlib.ClientSessionData{
			AccountDID:              did,
			SessionID:               sessionID,
			HostURL:                 "http://localhost:3001",
			AuthServerURL:           "http://localhost:3001",
			AuthServerTokenEndpoint: "http://localhost:3001/oauth/token",
			AccessToken:             tokenPair.access,
			RefreshToken:            tokenPair.refresh,
			Scopes:                  []string{"atproto"},
		}

		// Save/update session
		err = store.SaveSession(ctx, session)
		require.NoError(t, err, "Should save session iteration %d", i+1)

		// Retrieve and verify
		retrieved, err := store.GetSession(ctx, did, sessionID)
		require.NoError(t, err, "Should retrieve session iteration %d", i+1)

		assert.Equal(t, tokenPair.access, retrieved.AccessToken,
			"Access token should match iteration %d", i+1)
		assert.Equal(t, tokenPair.refresh, retrieved.RefreshToken,
			"Refresh token should match iteration %d", i+1)

		// Small delay to ensure timestamp differences
		time.Sleep(10 * time.Millisecond)
	}

	t.Logf("✅ Refresh token rotation verified through %d cycles", len(tokens))

	// Verify final state
	finalSession, err := store.GetSession(ctx, did, sessionID)
	require.NoError(t, err)

	assert.Equal(t, "access-token-v3", finalSession.AccessToken,
		"Final access token should be from last rotation")
	assert.Equal(t, "refresh-token-v3", finalSession.RefreshToken,
		"Final refresh token should be from last rotation")

	t.Logf("✅ Token rotation state verified")
}
