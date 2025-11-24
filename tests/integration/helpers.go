package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/auth"
	"Coves/internal/core/users"

	"github.com/golang-jwt/jwt/v5"
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

// createSimpleTestJWT creates a minimal JWT for testing (Phase 1 - no signature)
// In production, this would be a real OAuth token from PDS with proper signatures
func createSimpleTestJWT(userDID string) string {
	// Create minimal JWT claims using RegisteredClaims
	// Use userDID as issuer since we don't have a proper PDS DID for testing
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userDID,
			Issuer:    userDID, // Use DID as issuer for testing (valid per atProto)
			Audience:  jwt.ClaimStrings{getTestInstanceDID()},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
		Scope: "com.atproto.access",
	}

	// For Phase 1 testing, we create an unsigned JWT
	// The middleware is configured with skipVerify=true for testing
	header := map[string]interface{}{
		"alg": "none",
		"typ": "JWT",
	}

	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	// Base64url encode (without padding)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	// For "alg: none", signature is empty
	return headerB64 + "." + claimsB64 + "."
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
