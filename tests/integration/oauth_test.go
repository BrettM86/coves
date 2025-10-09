package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"Coves/internal/api/handlers/oauth"
	"Coves/internal/atproto/identity"
	oauthCore "Coves/internal/core/oauth"

	"github.com/lestrrat-go/jwx/v2/jwk"
)

// TestOAuthClientMetadata tests the /oauth/client-metadata.json endpoint
func TestOAuthClientMetadata(t *testing.T) {
	tests := []struct {
		name              string
		appviewURL        string
		expectedClientID  string
		expectedJWKSURI   string
		expectedRedirect  string
	}{
		{
			name:              "localhost development",
			appviewURL:        "http://localhost:8081",
			expectedClientID:  "http://localhost?redirect_uri=http://localhost:8081/oauth/callback&scope=atproto%20transition:generic",
			expectedJWKSURI:   "", // No JWKS URI for localhost
			expectedRedirect:  "http://localhost:8081/oauth/callback",
		},
		{
			name:              "production HTTPS",
			appviewURL:        "https://coves.social",
			expectedClientID:  "https://coves.social/oauth/client-metadata.json",
			expectedJWKSURI:   "https://coves.social/oauth/jwks.json",
			expectedRedirect:  "https://coves.social/oauth/callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment
			os.Setenv("APPVIEW_PUBLIC_URL", tt.appviewURL)
			defer os.Unsetenv("APPVIEW_PUBLIC_URL")

			// Create request
			req := httptest.NewRequest("GET", "/oauth/client-metadata.json", nil)
			w := httptest.NewRecorder()

			// Call handler
			oauth.HandleClientMetadata(w, req)

			// Check status code
			if w.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", w.Code)
			}

			// Parse response
			var metadata oauth.ClientMetadata
			if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			// Verify client ID
			if metadata.ClientID != tt.expectedClientID {
				t.Errorf("expected client_id %q, got %q", tt.expectedClientID, metadata.ClientID)
			}

			// Verify JWKS URI
			if metadata.JwksURI != tt.expectedJWKSURI {
				t.Errorf("expected jwks_uri %q, got %q", tt.expectedJWKSURI, metadata.JwksURI)
			}

			// Verify redirect URI
			if len(metadata.RedirectURIs) != 1 || metadata.RedirectURIs[0] != tt.expectedRedirect {
				t.Errorf("expected redirect_uris [%q], got %v", tt.expectedRedirect, metadata.RedirectURIs)
			}

			// Verify OAuth spec compliance
			if metadata.ClientName != "Coves" {
				t.Errorf("expected client_name 'Coves', got %q", metadata.ClientName)
			}
			if metadata.TokenEndpointAuthMethod != "private_key_jwt" {
				t.Errorf("expected token_endpoint_auth_method 'private_key_jwt', got %q", metadata.TokenEndpointAuthMethod)
			}
			if metadata.TokenEndpointAuthSigningAlg != "ES256" {
				t.Errorf("expected token_endpoint_auth_signing_alg 'ES256', got %q", metadata.TokenEndpointAuthSigningAlg)
			}
			if !metadata.DpopBoundAccessTokens {
				t.Error("expected dpop_bound_access_tokens to be true")
			}
		})
	}
}

// TestOAuthJWKS tests the /oauth/jwks.json endpoint
func TestOAuthJWKS(t *testing.T) {
	// Use the test JWK from .env.dev
	testJWK := `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`

	tests := []struct {
		name          string
		envValue      string
		expectSuccess bool
	}{
		{
			name:          "valid plain JWK",
			envValue:      testJWK,
			expectSuccess: true,
		},
		{
			name:          "missing JWK",
			envValue:      "",
			expectSuccess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment
			if tt.envValue != "" {
				os.Setenv("OAUTH_PRIVATE_JWK", tt.envValue)
				defer os.Unsetenv("OAUTH_PRIVATE_JWK")
			}

			// Create request
			req := httptest.NewRequest("GET", "/oauth/jwks.json", nil)
			w := httptest.NewRecorder()

			// Call handler
			oauth.HandleJWKS(w, req)

			// Check status code
			if tt.expectSuccess {
				if w.Code != http.StatusOK {
					t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
				}

				// Parse response
				var jwksResp struct {
					Keys []map[string]interface{} `json:"keys"`
				}
				if err := json.NewDecoder(w.Body).Decode(&jwksResp); err != nil {
					t.Fatalf("failed to decode JWKS: %v", err)
				}

				// Verify we got a public key
				if len(jwksResp.Keys) != 1 {
					t.Fatalf("expected 1 key, got %d", len(jwksResp.Keys))
				}

				key := jwksResp.Keys[0]
				if key["kty"] != "EC" {
					t.Errorf("expected kty 'EC', got %v", key["kty"])
				}
				if key["alg"] != "ES256" {
					t.Errorf("expected alg 'ES256', got %v", key["alg"])
				}
				if key["kid"] != "oauth-client-key" {
					t.Errorf("expected kid 'oauth-client-key', got %v", key["kid"])
				}

				// Verify private key is NOT exposed
				if _, hasPrivate := key["d"]; hasPrivate {
					t.Error("SECURITY: private key 'd' should not be in JWKS!")
				}

			} else {
				if w.Code == http.StatusOK {
					t.Fatalf("expected error status, got 200")
				}
			}
		})
	}
}

// TestOAuthLoginHandler tests the OAuth login initiation
func TestOAuthLoginHandler(t *testing.T) {
	// Skip if running in CI without database
	if os.Getenv("SKIP_INTEGRATION") == "true" {
		t.Skip("Skipping integration test")
	}

	// Setup test database
	db := setupTestDB(t)
	defer db.Close()

	// Create session store
	sessionStore := oauthCore.NewPostgresSessionStore(db)

	// Create identity resolver (mock for now - we'll test with real PDS separately)
	// For now, just test the handler structure and validation

	tests := []struct {
		name           string
		requestBody    map[string]interface{}
		envJWK         string
		expectedStatus int
	}{
		{
			name: "missing handle",
			requestBody: map[string]interface{}{
				"handle": "",
			},
			envJWK:         `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "invalid handle format",
			requestBody: map[string]interface{}{
				"handle": "no-dots-invalid",
			},
			envJWK:         `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "missing OAuth JWK",
			requestBody: map[string]interface{}{
				"handle": "alice.bsky.social",
			},
			envJWK:         "",
			expectedStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment
			if tt.envJWK != "" {
				os.Setenv("OAUTH_PRIVATE_JWK", tt.envJWK)
				defer os.Unsetenv("OAUTH_PRIVATE_JWK")
			} else {
				os.Unsetenv("OAUTH_PRIVATE_JWK")
			}

			// Create mock identity resolver for validation tests
			mockResolver := &mockIdentityResolver{}

			// Create handler
			handler := oauth.NewLoginHandler(mockResolver, sessionStore)

			// Create request
			bodyBytes, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest("POST", "/oauth/login", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			// Call handler
			handler.HandleLogin(w, req)

			// Check status code
			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
			}
		})
	}
}

// TestOAuthCallbackHandler tests the OAuth callback handling
func TestOAuthCallbackHandler(t *testing.T) {
	// Skip if running in CI without database
	if os.Getenv("SKIP_INTEGRATION") == "true" {
		t.Skip("Skipping integration test")
	}

	// Setup test database
	db := setupTestDB(t)
	defer db.Close()

	// Create session store
	sessionStore := oauthCore.NewPostgresSessionStore(db)

	testJWK := `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`

	tests := []struct {
		name           string
		queryParams    map[string]string
		expectedStatus int
	}{
		{
			name: "missing code",
			queryParams: map[string]string{
				"state": "test-state",
				"iss":   "https://bsky.social",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "missing state",
			queryParams: map[string]string{
				"code": "test-code",
				"iss":  "https://bsky.social",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "missing issuer",
			queryParams: map[string]string{
				"code":  "test-code",
				"state": "test-state",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "OAuth error parameter",
			queryParams: map[string]string{
				"error":             "access_denied",
				"error_description": "User denied access",
			},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment
			os.Setenv("OAUTH_PRIVATE_JWK", testJWK)
			defer os.Unsetenv("OAUTH_PRIVATE_JWK")

			// Create handler
			handler := oauth.NewCallbackHandler(sessionStore)

			// Build query string
			req := httptest.NewRequest("GET", "/oauth/callback", nil)
			q := req.URL.Query()
			for k, v := range tt.queryParams {
				q.Add(k, v)
			}
			req.URL.RawQuery = q.Encode()

			w := httptest.NewRecorder()

			// Call handler
			handler.HandleCallback(w, req)

			// Check status code
			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
			}
		})
	}
}

// mockIdentityResolver is a mock for testing
type mockIdentityResolver struct{}

func (m *mockIdentityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	// Return a mock resolved identity
	return &identity.Identity{
		DID:    "did:plc:test123",
		Handle: identifier,
		PDSURL: "https://test.pds.example",
	}, nil
}

func (m *mockIdentityResolver) ResolveHandle(ctx context.Context, handle string) (string, string, error) {
	return "did:plc:test123", "https://test.pds.example", nil
}

func (m *mockIdentityResolver) ResolveDID(ctx context.Context, did string) (*identity.DIDDocument, error) {
	return &identity.DIDDocument{
		DID: did,
		Service: []identity.Service{
			{
				ID:              "#atproto_pds",
				Type:            "AtprotoPersonalDataServer",
				ServiceEndpoint: "https://test.pds.example",
			},
		},
	}, nil
}

func (m *mockIdentityResolver) Purge(ctx context.Context, identifier string) error {
	return nil
}

// TestJWKParsing tests that we can parse JWKs correctly
func TestJWKParsing(t *testing.T) {
	testJWK := `{"alg":"ES256","crv":"P-256","d":"9tCMceYSgyZfO5KYOCm3rWEhXLqq2l4LjP7-PJtJKyk","kid":"oauth-client-key","kty":"EC","use":"sig","x":"EOYWEgZ2d-smTO6jh0f-9B7YSFYdlrvlryjuXTCrOjE","y":"_FR2jBcWNxoJl5cd1eq9sYtAs33No9AVtd42UyyWYi4"}`

	// Parse the JWK
	key, err := jwk.ParseKey([]byte(testJWK))
	if err != nil {
		t.Fatalf("failed to parse JWK: %v", err)
	}

	// Verify it's an EC key
	if key.KeyType() != "EC" {
		t.Errorf("expected key type 'EC', got %v", key.KeyType())
	}

	// Verify we can get the public key
	pubKey, err := key.PublicKey()
	if err != nil {
		t.Fatalf("failed to get public key: %v", err)
	}

	// Verify public key doesn't have private component
	pubKeyJSON, _ := json.Marshal(pubKey)
	var pubKeyMap map[string]interface{}
	json.Unmarshal(pubKeyJSON, &pubKeyMap)

	if _, hasPrivate := pubKeyMap["d"]; hasPrivate {
		t.Error("SECURITY: public key should not contain private 'd' component!")
	}
}
