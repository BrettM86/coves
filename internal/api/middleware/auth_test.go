package middleware

import (
	"Coves/internal/atproto/oauth"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// mockOAuthClient is a test double for OAuthClient
type mockOAuthClient struct {
	sealSecret     []byte
	shouldFailSeal bool
}

func newMockOAuthClient() *mockOAuthClient {
	// Create a 32-byte seal secret for testing
	secret := []byte("test-secret-key-32-bytes-long!!")
	return &mockOAuthClient{
		sealSecret: secret,
	}
}

func (m *mockOAuthClient) UnsealSession(token string) (*oauth.SealedSession, error) {
	if m.shouldFailSeal {
		return nil, fmt.Errorf("mock unseal failure")
	}

	// For testing, we'll decode a simple format: base64(did|sessionID|expiresAt)
	// In production this would be AES-GCM encrypted
	// Using pipe separator to avoid conflicts with colon in DIDs
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token encoding: %w", err)
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	var expiresAt int64
	_, _ = fmt.Sscanf(parts[2], "%d", &expiresAt)

	// Check expiration
	if expiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("token expired")
	}

	return &oauth.SealedSession{
		DID:       parts[0],
		SessionID: parts[1],
		ExpiresAt: expiresAt,
	}, nil
}

// Helper to create a test sealed token
func (m *mockOAuthClient) createTestToken(did, sessionID string, ttl time.Duration) string {
	expiresAt := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d", did, sessionID, expiresAt)
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// mockOAuthStore is a test double for ClientAuthStore
type mockOAuthStore struct {
	sessions map[string]*oauthlib.ClientSessionData
}

func newMockOAuthStore() *mockOAuthStore {
	return &mockOAuthStore{
		sessions: make(map[string]*oauthlib.ClientSessionData),
	}
}

func (m *mockOAuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauthlib.ClientSessionData, error) {
	key := did.String() + ":" + sessionID
	session, ok := m.sessions[key]
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	return session, nil
}

func (m *mockOAuthStore) SaveSession(ctx context.Context, session oauthlib.ClientSessionData) error {
	key := session.AccountDID.String() + ":" + session.SessionID
	m.sessions[key] = &session
	return nil
}

func (m *mockOAuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	key := did.String() + ":" + sessionID
	delete(m.sessions, key)
	return nil
}

func (m *mockOAuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauthlib.AuthRequestData, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockOAuthStore) SaveAuthRequestInfo(ctx context.Context, info oauthlib.AuthRequestData) error {
	return fmt.Errorf("not implemented")
}

func (m *mockOAuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	return fmt.Errorf("not implemented")
}

// TestRequireAuth_ValidToken tests that valid sealed tokens are accepted
func TestRequireAuth_ValidToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
		HostURL:     "https://pds.example.com",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted and injected into context
		extractedDID := GetUserDID(r)
		if extractedDID != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", extractedDID)
		}

		// Verify OAuth session was injected
		oauthSession := GetOAuthSession(r)
		if oauthSession == nil {
			t.Error("expected OAuth session to be non-nil")
			return
		}
		if oauthSession.SessionID != sessionID {
			t.Errorf("expected session ID '%s', got %s", sessionID, oauthSession.SessionID)
		}

		// Verify access token is available
		accessToken := GetUserAccessToken(r)
		if accessToken != "test_access_token" {
			t.Errorf("expected access token 'test_access_token', got %s", accessToken)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_MissingAuthHeader tests that missing Authorization header is rejected
func TestRequireAuth_MissingAuthHeader(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Authorization header
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_InvalidAuthHeaderFormat tests that non-Bearer tokens are rejected
func TestRequireAuth_InvalidAuthHeaderFormat(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	tests := []struct {
		name   string
		header string
	}{
		{"Basic auth", "Basic dGVzdDp0ZXN0"},
		{"DPoP scheme", "DPoP some-token"},
		{"Invalid format", "InvalidFormat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("handler should not be called")
			}))

			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", w.Code)
			}
		})
	}
}

// TestRequireAuth_CaseInsensitiveScheme verifies that Bearer scheme matching is case-insensitive
func TestRequireAuth_CaseInsensitiveScheme(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)
	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)

	testCases := []struct {
		name   string
		scheme string
	}{
		{"lowercase", "bearer"},
		{"uppercase", "BEARER"},
		{"mixed_case", "BeArEr"},
		{"standard", "Bearer"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handlerCalled := false
			handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("Authorization", tc.scheme+" "+token)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if !handlerCalled {
				t.Errorf("scheme %q should be accepted (case-insensitive per RFC 7235), got status %d: %s",
					tc.scheme, w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireAuth_InvalidToken tests that malformed sealed tokens are rejected
func TestRequireAuth_InvalidToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-sealed-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_ExpiredToken tests that expired sealed tokens are rejected
func TestRequireAuth_ExpiredToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for expired token")
	}))

	// Create expired token (expired 1 hour ago)
	token := client.createTestToken("did:plc:test123", sessionID, -time.Hour)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_SessionNotFound tests that tokens with non-existent sessions are rejected
func TestRequireAuth_SessionNotFound(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	// Create token for session that doesn't exist in store
	token := client.createTestToken("did:plc:nonexistent", "session999", time.Hour)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_DIDMismatch tests that session DID must match token DID
func TestRequireAuth_DIDMismatch(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a session with different DID than token
	did := syntax.DID("did:plc:different")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
	}
	// Store with key that matches token DID
	key := "did:plc:test123:" + sessionID
	store.sessions[key] = session

	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when DID mismatches")
	}))

	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestOptionalAuth_WithToken tests that OptionalAuth accepts valid Bearer tokens
func TestOptionalAuth_WithToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted
		extractedDID := GetUserDID(r)
		if extractedDID != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", extractedDID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestOptionalAuth_WithoutToken tests that OptionalAuth allows requests without tokens
func TestOptionalAuth_WithoutToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify no DID is set
		did := GetUserDID(r)
		if did != "" {
			t.Errorf("expected empty DID, got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Authorization header
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestOptionalAuth_InvalidToken tests that OptionalAuth continues without auth on invalid token
func TestOptionalAuth_InvalidToken(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify no DID is set (invalid token ignored)
		did := GetUserDID(r)
		if did != "" {
			t.Errorf("expected empty DID for invalid token, got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-sealed-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestGetUserDID_NotAuthenticated tests that GetUserDID returns empty string when not authenticated
func TestGetUserDID_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	did := GetUserDID(req)

	if did != "" {
		t.Errorf("expected empty string, got %s", did)
	}
}

// TestGetOAuthSession_NotAuthenticated tests that GetOAuthSession returns nil when not authenticated
func TestGetOAuthSession_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	session := GetOAuthSession(req)

	if session != nil {
		t.Errorf("expected nil session, got %+v", session)
	}
}

// TestGetUserAccessToken_NotAuthenticated tests that GetUserAccessToken returns empty when not authenticated
func TestGetUserAccessToken_NotAuthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	token := GetUserAccessToken(req)

	if token != "" {
		t.Errorf("expected empty token, got %s", token)
	}
}

// TestSetTestUserDID tests the testing helper function
func TestSetTestUserDID(t *testing.T) {
	ctx := context.Background()
	ctx = SetTestUserDID(ctx, "did:plc:testuser")

	did, ok := ctx.Value(UserDIDKey).(string)
	if !ok {
		t.Error("DID not found in context")
	}
	if did != "did:plc:testuser" {
		t.Errorf("expected 'did:plc:testuser', got %s", did)
	}
}

// TestExtractBearerToken tests the Bearer token extraction logic
func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name        string
		authHeader  string
		expectToken string
		expectOK    bool
	}{
		{"valid bearer", "Bearer token123", "token123", true},
		{"lowercase bearer", "bearer token123", "token123", true},
		{"uppercase bearer", "BEARER token123", "token123", true},
		{"mixed case", "BeArEr token123", "token123", true},
		{"empty header", "", "", false},
		{"wrong scheme", "DPoP token123", "", false},
		{"no token", "Bearer", "", false},
		{"no space", "Bearertoken123", "", false},
		{"extra spaces", "Bearer  token123  ", "token123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, ok := extractBearerToken(tt.authHeader)
			if ok != tt.expectOK {
				t.Errorf("expected ok=%v, got %v", tt.expectOK, ok)
			}
			if token != tt.expectToken {
				t.Errorf("expected token '%s', got '%s'", tt.expectToken, token)
			}
		})
	}
}

// TestRequireAuth_ValidCookie tests that valid session cookies are accepted
func TestRequireAuth_ValidCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
		HostURL:     "https://pds.example.com",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted and injected into context
		extractedDID := GetUserDID(r)
		if extractedDID != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", extractedDID)
		}

		// Verify OAuth session was injected
		oauthSession := GetOAuthSession(r)
		if oauthSession == nil {
			t.Error("expected OAuth session to be non-nil")
			return
		}
		if oauthSession.SessionID != sessionID {
			t.Errorf("expected session ID '%s', got %s", sessionID, oauthSession.SessionID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)
	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  "coves_session",
		Value: token,
	})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_HeaderPrecedenceOverCookie tests that Authorization header takes precedence over cookie
func TestRequireAuth_HeaderPrecedenceOverCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create two test sessions
	did1 := syntax.DID("did:plc:header")
	sessionID1 := "session_header"
	session1 := &oauthlib.ClientSessionData{
		AccountDID:  did1,
		SessionID:   sessionID1,
		AccessToken: "header_token",
		HostURL:     "https://pds.example.com",
	}
	_ = store.SaveSession(context.Background(), *session1)

	did2 := syntax.DID("did:plc:cookie")
	sessionID2 := "session_cookie"
	session2 := &oauthlib.ClientSessionData{
		AccountDID:  did2,
		SessionID:   sessionID2,
		AccessToken: "cookie_token",
		HostURL:     "https://pds.example.com",
	}
	_ = store.SaveSession(context.Background(), *session2)

	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Should get header DID, not cookie DID
		extractedDID := GetUserDID(r)
		if extractedDID != "did:plc:header" {
			t.Errorf("expected header DID 'did:plc:header', got %s", extractedDID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	headerToken := client.createTestToken("did:plc:header", sessionID1, time.Hour)
	cookieToken := client.createTestToken("did:plc:cookie", sessionID2, time.Hour)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+headerToken)
	req.AddCookie(&http.Cookie{
		Name:  "coves_session",
		Value: cookieToken,
	})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestRequireAuth_MissingBothHeaderAndCookie tests that missing both auth methods is rejected
func TestRequireAuth_MissingBothHeaderAndCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Authorization header and no cookie
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestRequireAuth_InvalidCookie tests that malformed cookie tokens are rejected
func TestRequireAuth_InvalidCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handler := middleware.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  "coves_session",
		Value: "not-a-valid-sealed-token",
	})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestOptionalAuth_WithCookie tests that OptionalAuth accepts valid session cookies
func TestOptionalAuth_WithCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()

	// Create a test session
	did := syntax.DID("did:plc:test123")
	sessionID := "session123"
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		SessionID:   sessionID,
		AccessToken: "test_access_token",
	}
	_ = store.SaveSession(context.Background(), *session)

	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify DID was extracted
		extractedDID := GetUserDID(r)
		if extractedDID != "did:plc:test123" {
			t.Errorf("expected DID 'did:plc:test123', got %s", extractedDID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	token := client.createTestToken("did:plc:test123", sessionID, time.Hour)
	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  "coves_session",
		Value: token,
	})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestOptionalAuth_InvalidCookie tests that OptionalAuth continues without auth on invalid cookie
func TestOptionalAuth_InvalidCookie(t *testing.T) {
	client := newMockOAuthClient()
	store := newMockOAuthStore()
	middleware := NewOAuthAuthMiddleware(client, store)

	handlerCalled := false
	handler := middleware.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true

		// Verify no DID is set (invalid cookie ignored)
		did := GetUserDID(r)
		if did != "" {
			t.Errorf("expected empty DID for invalid cookie, got %s", did)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  "coves_session",
		Value: "not-a-valid-sealed-token",
	})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

// TestWriteAuthError_JSONEscaping tests that writeAuthError properly escapes messages
func TestWriteAuthError_JSONEscaping(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{"simple message", "Missing authentication"},
		{"message with quotes", `Invalid "token" format`},
		{"message with newlines", "Invalid\ntoken\nformat"},
		{"message with backslashes", `Invalid \ token`},
		{"message with special chars", `Invalid <script>alert("xss")</script> token`},
		{"message with unicode", "Invalid token: \u2028\u2029"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeAuthError(w, tt.message)

			// Verify status code
			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", w.Code)
			}

			// Verify content type
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected Content-Type 'application/json', got %s", ct)
			}

			// Verify response is valid JSON
			var response map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("response is not valid JSON: %v\nBody: %s", err, w.Body.String())
			}

			// Verify fields
			if response["error"] != "AuthenticationRequired" {
				t.Errorf("expected error 'AuthenticationRequired', got %s", response["error"])
			}
			if response["message"] != tt.message {
				t.Errorf("expected message %q, got %q", tt.message, response["message"])
			}
		})
	}
}
