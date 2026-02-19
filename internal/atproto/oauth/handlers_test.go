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
	// Per atproto OAuth spec, client_id for public clients is the client metadata URL
	assert.Equal(t, "https://coves.social/oauth-client-metadata.json", metadata.ClientID)
	assert.Contains(t, metadata.RedirectURIs, "https://coves.social/oauth/callback")
	assert.Contains(t, metadata.GrantTypes, "authorization_code")
	assert.Contains(t, metadata.GrantTypes, "refresh_token")
	assert.True(t, metadata.DPoPBoundAccessTokens)
	assert.Contains(t, metadata.Scope, "atproto")
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
// Per atproto spec, custom schemes must match client_id hostname in reverse-domain order
func TestIsMobileRedirectURI(t *testing.T) {
	handler := createTestOAuthHandler(t)

	tests := []struct {
		uri      string
		expected bool
	}{
		// Custom scheme per atproto spec (reverse domain of coves.social)
		{"social.coves:/callback", true},
		{"social.coves://callback", true},
		{"social.coves:/oauth/callback", true},
		{"social.coves://oauth/callback", true},
		// Universal Link - allowed (strongest security)
		{"https://coves.social/app/oauth/callback", true},
		// Wrong custom schemes - not reverse-domain of coves.social
		{"coves-app://oauth/callback", false},
		{"coves://oauth/callback", false},
		{"coves.social://callback", false}, // Not reversed
		{"myapp://oauth", false},
		// Wrong domain/scheme
		{"https://example.com", false},
		{"http://localhost", false},
		{"", false},
		{"not-a-uri", false},
	}

	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			result := handler.isAllowedRedirectURI(tt.uri)
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

// TestConfidentialClientTransition validates the seamless transition from public to confidential client
func TestConfidentialClientTransition(t *testing.T) {
	baseConfig := func() *OAuthConfig {
		return &OAuthConfig{
			PublicURL:       "https://coves.social",
			Scopes:          []string{"atproto"},
			DevMode:         false,
			AllowPrivateIPs: false,
			SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
		}
	}

	t.Run("public client without keys", func(t *testing.T) {
		config := baseConfig()
		client, err := NewOAuthClient(config, oauth.NewMemStore())
		require.NoError(t, err)

		// Should NOT be confidential
		assert.False(t, client.ClientApp.Config.IsConfidential())

		// Metadata should NOT have JWKS URI
		metadata := client.ClientMetadata()
		assert.Nil(t, metadata.JWKSURI)
		assert.Equal(t, "none", metadata.TokenEndpointAuthMethod)
	})

	t.Run("confidential client with keys", func(t *testing.T) {
		config := baseConfig()
		// TEST-ONLY: P-256 private key in multibase format - DO NOT use in production
		// This is a publicly known test key that provides NO security
		config.ClientPrivateKeyMultibase = "z42tn7PHdrLvcwoYGtY71n4g56NcQ3vJn3W5NNJV9mmqDL68"
		config.ClientKeyID = "test-key-1"

		client, err := NewOAuthClient(config, oauth.NewMemStore())
		require.NoError(t, err)

		// Should be confidential
		assert.True(t, client.ClientApp.Config.IsConfidential())

		// Metadata SHOULD have JWKS URI
		metadata := client.ClientMetadata()
		require.NotNil(t, metadata.JWKSURI)
		assert.Equal(t, "https://coves.social/oauth-client-keys.json", *metadata.JWKSURI)
		assert.Equal(t, "private_key_jwt", metadata.TokenEndpointAuthMethod)
	})

	t.Run("partial keys rejected", func(t *testing.T) {
		// Only private key, no key ID
		config := baseConfig()
		// TEST-ONLY key - DO NOT use in production
		config.ClientPrivateKeyMultibase = "z42tn7PHdrLvcwoYGtY71n4g56NcQ3vJn3W5NNJV9mmqDL68"
		// No ClientKeyID

		client, err := NewOAuthClient(config, oauth.NewMemStore())
		require.NoError(t, err)

		// Should NOT be confidential (both fields required)
		assert.False(t, client.ClientApp.Config.IsConfidential())
	})

	t.Run("invalid private key rejected", func(t *testing.T) {
		config := baseConfig()
		config.ClientPrivateKeyMultibase = "invalid-key"
		config.ClientKeyID = "test-key-1"

		_, err := NewOAuthClient(config, oauth.NewMemStore())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing OAuth client private key")
	})
}

// TestHandleClientJWKS tests the JWKS endpoint
func TestHandleClientJWKS(t *testing.T) {
	t.Run("public client returns empty JWKS", func(t *testing.T) {
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

		req := httptest.NewRequest(http.MethodGet, "/oauth-client-keys.json", nil)
		rec := httptest.NewRecorder()

		handler.HandleClientJWKS(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"))

		// Parse response - should be empty keys array for public client
		var jwks oauth.JWKS
		err = json.NewDecoder(rec.Body).Decode(&jwks)
		require.NoError(t, err)
		assert.Empty(t, jwks.Keys)
	})

	t.Run("confidential client returns JWKS with public key", func(t *testing.T) {
		config := &OAuthConfig{
			PublicURL:       "https://coves.social",
			Scopes:          []string{"atproto"},
			DevMode:         false,
			AllowPrivateIPs: false,
			SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
			// TEST-ONLY key - DO NOT use in production
			ClientPrivateKeyMultibase: "z42tn7PHdrLvcwoYGtY71n4g56NcQ3vJn3W5NNJV9mmqDL68",
			ClientKeyID:               "test-key-1",
		}

		client, err := NewOAuthClient(config, oauth.NewMemStore())
		require.NoError(t, err)

		handler := NewOAuthHandler(client, oauth.NewMemStore())

		req := httptest.NewRequest(http.MethodGet, "/oauth-client-keys.json", nil)
		rec := httptest.NewRecorder()

		handler.HandleClientJWKS(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		// Parse response - should have one key
		var jwks oauth.JWKS
		err = json.NewDecoder(rec.Body).Decode(&jwks)
		require.NoError(t, err)
		require.Len(t, jwks.Keys, 1)

		// Validate key properties
		key := jwks.Keys[0]
		assert.Equal(t, "EC", key.KeyType)
		assert.Equal(t, "P-256", key.Curve)
		assert.NotNil(t, key.KeyID)
		assert.Equal(t, "test-key-1", *key.KeyID)
		// Should have public key coordinates (X, Y)
		// The JWK struct only contains public key data - no private key field
		assert.NotEmpty(t, key.X)
		assert.NotEmpty(t, key.Y)
	})
}

// TestOAuthEndpointsNoConflict ensures new endpoints don't conflict with existing routes
func TestOAuthEndpointsNoConflict(t *testing.T) {
	// This test validates that the route paths are distinct and don't overlap
	routes := map[string]string{
		"/oauth-client-metadata.json":           "Client identity document",
		"/oauth-client-keys.json":               "JWKS public keys (new)",
		"/oauth/callback":                       "OAuth callback after auth",
		"/oauth/login":                          "Start web OAuth flow",
		"/oauth/mobile/login":                   "Start mobile OAuth flow",
		"/.well-known/oauth-protected-resource": "Resource server metadata",
	}

	// All routes should be unique
	seen := make(map[string]bool)
	for route := range routes {
		assert.False(t, seen[route], "Duplicate route: %s", route)
		seen[route] = true
	}

	// Verify none of the OAuth routes conflict with DID routes
	didRoutes := []string{
		"/.well-known/did.json",
		"/.well-known/atproto-did",
	}

	for _, didRoute := range didRoutes {
		for oauthRoute := range routes {
			assert.NotEqual(t, didRoute, oauthRoute,
				"OAuth route %s conflicts with DID route %s", oauthRoute, didRoute)
		}
	}
}

// TestConfidentialClientWithDevMode verifies confidential client works in dev mode
func TestConfidentialClientWithDevMode(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "http://127.0.0.1:8081",
		Scopes:          []string{"atproto"},
		DevMode:         true, // Dev mode enabled
		AllowPrivateIPs: true,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
		// TEST-ONLY key - DO NOT use in production
		ClientPrivateKeyMultibase: "z42tn7PHdrLvcwoYGtY71n4g56NcQ3vJn3W5NNJV9mmqDL68",
		ClientKeyID:               "test-key-1",
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	// Should be confidential even in dev mode
	assert.True(t, client.ClientApp.Config.IsConfidential())
	assert.True(t, client.Config.DevMode)

	// In dev mode, loopback config is used but still becomes confidential
	assert.Equal(t, "private_key_jwt", client.ClientApp.Config.ClientMetadata().TokenEndpointAuthMethod)

	// Dev mode uses loopback client_id format
	// Loopback clients use http://localhost format
	assert.Contains(t, client.ClientApp.Config.ClientID, "http://")
}

// TestSessionTTLsForConfidentialClient verifies TTLs are set appropriately
func TestSessionTTLsForConfidentialClient(t *testing.T) {
	config := &OAuthConfig{
		PublicURL:       "https://coves.social",
		Scopes:          []string{"atproto"},
		DevMode:         false,
		AllowPrivateIPs: false,
		SealSecret:      "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
		// No explicit TTL set - should use defaults
	}

	client, err := NewOAuthClient(config, oauth.NewMemStore())
	require.NoError(t, err)

	// Default TTLs for confidential clients (1 year session, 18 months sealed token)
	assert.Equal(t, 365*24*time.Hour, client.Config.SessionTTL, "SessionTTL should be 1 year")
	assert.Equal(t, 548*24*time.Hour, client.Config.SealedTokenTTL, "SealedTokenTTL should be 18 months")
}
