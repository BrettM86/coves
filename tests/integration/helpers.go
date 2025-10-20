package integration

import (
	"Coves/internal/core/users"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

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
	err := db.QueryRowContext(ctx, query, did, handle, "http://localhost:3001").Scan(
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
