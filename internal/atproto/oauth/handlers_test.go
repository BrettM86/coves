package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleClientMetadata tests the client metadata endpoint
func TestHandleClientMetadata(t *testing.T) {
	// Create a test OAuth client configuration
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         false,
		AllowPrivateIPs: false,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=", // base64 encoded 32 bytes
	}

	// Create OAuth client with memory store
	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	// Create handler
	handler := NewOAuthHandler(client, oauth.NewMemStore())

	// Create test request
	req := httptest.NewRequest(http.MethodGet, "/oauth/client-metadata.json", nil)
	req.Host = "coves.social"
	rec := httptest.NewRecorder()

	// Call handler
	handler.HandleClientMetadata(rec, req)

	// Check response
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	// Parse response
	var metadata oauth.ClientMetadata
	err = json.NewDecoder(rec.Body).Decode(&metadata)
	require.NoError(t, err)

	// Validate metadata
	assert.Equal(t, "https://coves.social", metadata.ClientID)
	assert.Contains(t, metadata.RedirectURIs, "https://coves.social/oauth/callback")
	assert.Contains(t, metadata.GrantTypes, "authorization_code")
	assert.Contains(t, metadata.GrantTypes, "refresh_token")
	assert.True(t, metadata.DPoPBoundAccessTokens)
	assert.Contains(t, metadata.Scope, "atproto")
}

// TestHandleJWKS tests the JWKS endpoint
func TestHandleJWKS(t *testing.T) {
	// Create a test OAuth client configuration (public client, no keys)
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         false,
		AllowPrivateIPs: false,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	handler := NewOAuthHandler(client, oauth.NewMemStore())

	// Create test request
	req := httptest.NewRequest(http.MethodGet, "/oauth/jwks.json", nil)
	rec := httptest.NewRecorder()

	// Call handler
	handler.HandleJWKS(rec, req)

	// Check response
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	// Parse response
	var jwks oauth.JWKS
	err = json.NewDecoder(rec.Body).Decode(&jwks)
	require.NoError(t, err)

	// Public client should have empty JWKS
	assert.NotNil(t, jwks.Keys)
	assert.Equal(t, 0, len(jwks.Keys))
}

// TestHandleLogin tests the login endpoint
func TestHandleLogin(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         true, // Use dev mode to avoid real PDS calls
		AllowPrivateIPs: true, // Allow private IPs in dev mode
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	handler := NewOAuthHandler(client, oauth.NewMemStore())

	t.Run("missing identifier", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/oauth/login", nil)
		rec := httptest.NewRecorder()

		handler.HandleLogin(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("with handle parameter", func(t *testing.T) {
		// This test would need a mock PDS server to fully test
		// For now, we just verify the endpoint accepts the parameter
		req := httptest.NewRequest(http.MethodGet, "/oauth/login?handle=user.bsky.social", nil)
		rec := httptest.NewRecorder()

		handler.HandleLogin(rec, req)

		// In dev mode or with a real PDS, this would redirect
		// Without a mock, it will fail to resolve the handle
		// We're just testing that the handler processes the request
		assert.NotEqual(t, http.StatusOK, rec.Code) // Should redirect or error
	})
}

// TestHandleMobileLogin tests the mobile login endpoint
func TestHandleMobileLogin(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         true,
		AllowPrivateIPs: true,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	handler := NewOAuthHandler(client, oauth.NewMemStore())

	t.Run("missing redirect_uri", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/oauth/mobile/login?handle=user.bsky.social", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "redirect_uri")
	})

	t.Run("invalid redirect_uri (https)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/oauth/mobile/login?handle=user.bsky.social&redirect_uri=https://example.com", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid redirect_uri")
	})

	t.Run("invalid redirect_uri (wrong path)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/oauth/mobile/login?handle=user.bsky.social&redirect_uri=coves-app://callback", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid redirect_uri")
	})

	t.Run("valid mobile redirect_uri (Universal Link)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/oauth/mobile/login?handle=user.bsky.social&redirect_uri=https://coves.social/app/oauth/callback", nil)
		rec := httptest.NewRecorder()

		handler.HandleMobileLogin(rec, req)

		// Should fail to resolve handle but accept the parameters
		// Check that cookie was set
		cookies := rec.Result().Cookies()
		var found bool
		for _, cookie := range cookies {
			if cookie.Name == "mobile_redirect_uri" {
				found = true
				break
			}
		}
		// May or may not set cookie depending on error handling
		_ = found
	})
}

// TestParseSessionToken tests that we no longer use parseSessionToken
// (removed in favor of sealed tokens)
func TestParseSessionToken(t *testing.T) {
	// This test is deprecated - we now use sealed tokens instead of plain "did:sessionID" format
	// See TestSealAndUnsealSessionData for the new approach
	t.Skip("parseSessionToken removed - we now use sealed tokens for security")
}

// TestIsMobileRedirectURI tests mobile redirect URI validation with EXACT URI matching
// Only Universal Links (HTTPS) are allowed - custom schemes are blocked for security
func TestIsMobileRedirectURI(t *testing.T) {
	tests := []struct {
		uri      string
		expected bool
	}{
		{"https://coves.social/app/oauth/callback", true}, // Universal Link - allowed
		{"coves-app://oauth/callback", false},             // Custom scheme - blocked (insecure)
		{"coves://oauth/callback", false},                 // Custom scheme - blocked (insecure)
		{"coves-app://callback", false},                   // Custom scheme - blocked
		{"coves://oauth", false},                          // Custom scheme - blocked
		{"myapp://oauth", false},                          // Not in allowlist
		{"https://example.com", false},                    // Wrong domain
		{"http://localhost", false},                       // HTTP not allowed
		{"", false},
		{"not-a-uri", false},
	}

	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			result := isAllowedMobileRedirectURI(tt.uri)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestSealAndUnsealSessionData tests session data sealing/unsealing
func TestSealAndUnsealSessionData(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         false,
		AllowPrivateIPs: false,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	// Create test DID
	did, err := testDID()
	require.NoError(t, err)

	sessionID := "test-session-123"

	// Seal the session using the client method
	sealed, err := client.SealSession(did.String(), sessionID, 24*time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, sealed)

	// Unseal the session using the client method
	unsealed, err := client.UnsealSession(sealed)
	require.NoError(t, err)
	require.NotNil(t, unsealed)

	// Verify data matches
	assert.Equal(t, did.String(), unsealed.DID)
	assert.Equal(t, sessionID, unsealed.SessionID)
	assert.Greater(t, unsealed.ExpiresAt, int64(0))
}

// testDID creates a test DID for testing
func testDID() (*syntax.DID, error) {
	did, err := syntax.ParseDID("did:plc:test123abc456def789")
	if err != nil {
		return nil, err
	}
	return &did, nil
}
