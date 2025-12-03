package integration

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// getTestPDSURL returns the PDS URL for testing from env var or default
func getTestPDSURL() string {
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}
	return pdsURL
}

// getTestInstanceDID returns the instance DID for testing from env var or default
func getTestInstanceDID() string {
	instanceDID := os.Getenv("INSTANCE_DID")
	if instanceDID == "" {
		instanceDID = "did:web:test.coves.social"
	}
	return instanceDID
}

// createTestUser creates a test user in the database for use in integration tests
// Returns the created user or fails the test
func createTestUser(t *testing.T, db *sql.DB, handle, did string) *users.User {
	t.Helper()

	ctx := context.Background()

	// Create user directly in DB for speed
	query := `
		INSERT INTO users (did, handle, pds_url, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		RETURNING did, handle, pds_url, created_at, updated_at
	`

	user := &users.User{}
	err := db.QueryRowContext(ctx, query, did, handle, getTestPDSURL()).Scan(
		&user.DID,
		&user.Handle,
		&user.PDSURL,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	return user
}

// contains checks if string s contains substring substr
// Helper for error message assertions
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// authenticateWithPDS authenticates with PDS to get access token and DID
// Used for setting up test environments that need PDS credentials
func authenticateWithPDS(pdsURL, handle, password string) (string, string, error) {
	// Call com.atproto.server.createSession
	sessionReq := map[string]string{
		"identifier": handle,
		"password":   password,
	}

	reqBody, marshalErr := json.Marshal(sessionReq)
	if marshalErr != nil {
		return "", "", fmt.Errorf("failed to marshal session request: %w", marshalErr)
	}
	resp, err := http.Post(
		pdsURL+"/xrpc/com.atproto.server.createSession",
		"application/json",
		bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", "", fmt.Errorf("PDS auth failed (status %d, failed to read body: %w)", resp.StatusCode, readErr)
		}
		return "", "", fmt.Errorf("PDS auth failed (status %d): %s", resp.StatusCode, string(body))
	}

	var sessionResp struct {
		AccessJwt string `json:"accessJwt"`
		DID       string `json:"did"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return "", "", fmt.Errorf("failed to decode session response: %w", err)
	}

	return sessionResp.AccessJwt, sessionResp.DID, nil
}

// generateTID generates a simple timestamp-based identifier for testing
// In production, PDS generates proper TIDs
func generateTID() string {
	return fmt.Sprintf("3k%d", time.Now().UnixNano()/1000)
}

// createPDSAccount creates a new account on PDS and returns access token + DID
// This is used for E2E tests that need real PDS accounts
func createPDSAccount(pdsURL, handle, email, password string) (accessToken, did string, err error) {
	// Call com.atproto.server.createAccount
	reqBody := map[string]string{
		"handle":   handle,
		"email":    email,
		"password": password,
	}

	reqJSON, marshalErr := json.Marshal(reqBody)
	if marshalErr != nil {
		return "", "", fmt.Errorf("failed to marshal account request: %w", marshalErr)
	}

	resp, httpErr := http.Post(
		pdsURL+"/xrpc/com.atproto.server.createAccount",
		"application/json",
		bytes.NewBuffer(reqJSON),
	)
	if httpErr != nil {
		return "", "", fmt.Errorf("failed to create account: %w", httpErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", "", fmt.Errorf("account creation failed (status %d, failed to read body: %w)", resp.StatusCode, readErr)
		}
		return "", "", fmt.Errorf("account creation failed (status %d): %s", resp.StatusCode, string(body))
	}

	var accountResp struct {
		AccessJwt string `json:"accessJwt"`
		DID       string `json:"did"`
	}

	if decodeErr := json.NewDecoder(resp.Body).Decode(&accountResp); decodeErr != nil {
		return "", "", fmt.Errorf("failed to decode account response: %w", decodeErr)
	}

	return accountResp.AccessJwt, accountResp.DID, nil
}

// writePDSRecord writes a record to PDS via com.atproto.repo.createRecord
// Returns the AT-URI and CID of the created record
func writePDSRecord(pdsURL, accessToken, repo, collection, rkey string, record interface{}) (uri, cid string, err error) {
	reqBody := map[string]interface{}{
		"repo":       repo,
		"collection": collection,
		"record":     record,
	}

	// If rkey is provided, include it
	if rkey != "" {
		reqBody["rkey"] = rkey
	}

	reqJSON, marshalErr := json.Marshal(reqBody)
	if marshalErr != nil {
		return "", "", fmt.Errorf("failed to marshal record request: %w", marshalErr)
	}

	req, reqErr := http.NewRequest("POST", pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(reqJSON))
	if reqErr != nil {
		return "", "", fmt.Errorf("failed to create request: %w", reqErr)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, httpErr := http.DefaultClient.Do(req)
	if httpErr != nil {
		return "", "", fmt.Errorf("failed to write record: %w", httpErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", "", fmt.Errorf("record creation failed (status %d, failed to read body: %w)", resp.StatusCode, readErr)
		}
		return "", "", fmt.Errorf("record creation failed (status %d): %s", resp.StatusCode, string(body))
	}

	var recordResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if decodeErr := json.NewDecoder(resp.Body).Decode(&recordResp); decodeErr != nil {
		return "", "", fmt.Errorf("failed to decode record response: %w", decodeErr)
	}

	return recordResp.URI, recordResp.CID, nil
}

// createFeedTestCommunity creates a test community for feed tests
// Returns the community DID or an error
func createFeedTestCommunity(db *sql.DB, ctx context.Context, name, ownerHandle string) (string, error) {
	// Get configuration from env vars
	pdsURL := getTestPDSURL()
	instanceDID := getTestInstanceDID()

	// Create owner user first (directly insert to avoid service dependencies)
	ownerDID := fmt.Sprintf("did:plc:%s", ownerHandle)
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO NOTHING
	`, ownerDID, ownerHandle, pdsURL)
	if err != nil {
		return "", err
	}

	// Create community
	communityDID := fmt.Sprintf("did:plc:community-%s", name)
	_, err = db.ExecContext(ctx, `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (did) DO NOTHING
	`, communityDID, name, ownerDID, ownerDID, instanceDID, fmt.Sprintf("%s.coves.social", name), pdsURL)

	return communityDID, err
}

// createTestPost creates a test post and returns its URI
func createTestPost(t *testing.T, db *sql.DB, communityDID, authorDID, title string, score int, createdAt time.Time) string {
	t.Helper()

	ctx := context.Background()

	// Create author user if not exists (directly insert to avoid service dependencies)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO NOTHING
	`, authorDID, fmt.Sprintf("%s.bsky.social", authorDID), getTestPDSURL())

	// Generate URI
	rkey := fmt.Sprintf("post-%d", time.Now().UnixNano())
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", communityDID, rkey)

	// Insert post
	_, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, uri, "bafytest", rkey, authorDID, communityDID, title, createdAt, score, score)
	if err != nil {
		t.Fatalf("Failed to create test post: %v", err)
	}

	return uri
}

// MockSessionUnsealer is a mock implementation of SessionUnsealer for testing
// It returns predefined sessions based on token value
type MockSessionUnsealer struct {
	sessions map[string]*oauth.SealedSession
}

// NewMockSessionUnsealer creates a new mock unsealer
func NewMockSessionUnsealer() *MockSessionUnsealer {
	return &MockSessionUnsealer{
		sessions: make(map[string]*oauth.SealedSession),
	}
}

// AddSession adds a token -> session mapping
func (m *MockSessionUnsealer) AddSession(token, did, sessionID string) {
	m.sessions[token] = &oauth.SealedSession{
		DID:       did,
		SessionID: sessionID,
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	}
}

// UnsealSession returns the predefined session for a token
func (m *MockSessionUnsealer) UnsealSession(token string) (*oauth.SealedSession, error) {
	if sess, ok := m.sessions[token]; ok {
		return sess, nil
	}
	return nil, fmt.Errorf("unknown token")
}

// MockOAuthStore is a mock implementation of ClientAuthStore for testing
type MockOAuthStore struct {
	sessions map[string]*oauthlib.ClientSessionData
}

// NewMockOAuthStore creates a new mock OAuth store
func NewMockOAuthStore() *MockOAuthStore {
	return &MockOAuthStore{
		sessions: make(map[string]*oauthlib.ClientSessionData),
	}
}

// AddSession adds a session to the store
func (m *MockOAuthStore) AddSession(did, sessionID, accessToken string) {
	m.AddSessionWithPDS(did, sessionID, accessToken, getTestPDSURL())
}

// AddSessionWithPDS adds a session to the store with a specific PDS URL
func (m *MockOAuthStore) AddSessionWithPDS(did, sessionID, accessToken, pdsURL string) {
	key := did + ":" + sessionID
	parsedDID, _ := syntax.ParseDID(did)
	m.sessions[key] = &oauthlib.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   sessionID,
		AccessToken: accessToken,
		HostURL:     pdsURL,
	}
}

// GetSession implements ClientAuthStore
func (m *MockOAuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauthlib.ClientSessionData, error) {
	key := did.String() + ":" + sessionID
	if sess, ok := m.sessions[key]; ok {
		return sess, nil
	}
	return nil, fmt.Errorf("session not found")
}

// SaveSession implements ClientAuthStore
func (m *MockOAuthStore) SaveSession(ctx context.Context, sess oauthlib.ClientSessionData) error {
	key := sess.AccountDID.String() + ":" + sess.SessionID
	m.sessions[key] = &sess
	return nil
}

// DeleteSession implements ClientAuthStore
func (m *MockOAuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	key := did.String() + ":" + sessionID
	delete(m.sessions, key)
	return nil
}

// GetAuthRequestInfo implements ClientAuthStore
func (m *MockOAuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauthlib.AuthRequestData, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

// SaveAuthRequestInfo implements ClientAuthStore
func (m *MockOAuthStore) SaveAuthRequestInfo(ctx context.Context, info oauthlib.AuthRequestData) error {
	return nil
}

// DeleteAuthRequestInfo implements ClientAuthStore
func (m *MockOAuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	return nil
}

// CreateTestOAuthMiddleware creates an OAuth middleware with mock implementations for testing
// The returned middleware accepts a test token that maps to the specified userDID
func CreateTestOAuthMiddleware(userDID string) (*middleware.OAuthAuthMiddleware, string) {
	unsealer := NewMockSessionUnsealer()
	store := NewMockOAuthStore()

	testToken := "test-token-" + userDID
	sessionID := "test-session-123"

	// Add the test session
	unsealer.AddSession(testToken, userDID, sessionID)
	store.AddSession(userDID, sessionID, "test-access-token")

	authMiddleware := middleware.NewOAuthAuthMiddleware(unsealer, store)
	return authMiddleware, testToken
}

// E2EOAuthMiddleware wraps OAuth middleware for E2E testing with multiple users
type E2EOAuthMiddleware struct {
	*middleware.OAuthAuthMiddleware
	unsealer *MockSessionUnsealer
	store    *MockOAuthStore
}

// NewE2EOAuthMiddleware creates an OAuth middleware for E2E testing
func NewE2EOAuthMiddleware() *E2EOAuthMiddleware {
	unsealer := NewMockSessionUnsealer()
	store := NewMockOAuthStore()
	m := middleware.NewOAuthAuthMiddleware(unsealer, store)
	return &E2EOAuthMiddleware{m, unsealer, store}
}

// AddUser registers a user DID and returns the token to use in Authorization header
func (e *E2EOAuthMiddleware) AddUser(did string) string {
	token := "test-token-" + did
	sessionID := "session-" + did
	e.unsealer.AddSession(token, did, sessionID)
	e.store.AddSession(did, sessionID, "access-token-"+did)
	return token
}

// AddUserWithPDSToken registers a user with their real PDS access token
// Use this for E2E tests that need to write to the real PDS
func (e *E2EOAuthMiddleware) AddUserWithPDSToken(did, pdsAccessToken, pdsURL string) string {
	token := "test-token-" + did
	sessionID := "session-" + did
	e.unsealer.AddSession(token, did, sessionID)
	e.store.AddSessionWithPDS(did, sessionID, pdsAccessToken, pdsURL)
	return token
}
