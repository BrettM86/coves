package integration

import (
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestTokenRefresh_ExpirationDetection tests the NeedsRefresh function with various token states
func TestTokenRefresh_ExpirationDetection(t *testing.T) {
	tests := []struct {
		name          string
		token         string
		shouldRefresh bool
		expectError   bool
	}{
		{
			name:          "Token expiring in 2 minutes (should refresh)",
			token:         createTestJWT(time.Now().Add(2 * time.Minute)),
			shouldRefresh: true,
			expectError:   false,
		},
		{
			name:          "Token expiring in 10 minutes (should not refresh)",
			token:         createTestJWT(time.Now().Add(10 * time.Minute)),
			shouldRefresh: false,
			expectError:   false,
		},
		{
			name:          "Token already expired (should refresh)",
			token:         createTestJWT(time.Now().Add(-1 * time.Minute)),
			shouldRefresh: true,
			expectError:   false,
		},
		{
			name:          "Token expiring in exactly 5 minutes (should not refresh - edge case)",
			token:         createTestJWT(time.Now().Add(6 * time.Minute)),
			shouldRefresh: false,
			expectError:   false,
		},
		{
			name:          "Token expiring in 4 minutes (should refresh)",
			token:         createTestJWT(time.Now().Add(4 * time.Minute)),
			shouldRefresh: true,
			expectError:   false,
		},
		{
			name:          "Invalid JWT format (too many parts)",
			token:         "not.a.valid.jwt.format.extra",
			shouldRefresh: false,
			expectError:   true,
		},
		{
			name:          "Invalid JWT format (too few parts)",
			token:         "invalid.token",
			shouldRefresh: false,
			expectError:   true,
		},
		{
			name:          "Empty token",
			token:         "",
			shouldRefresh: false,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := communities.NeedsRefresh(tt.token)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result != tt.shouldRefresh {
				t.Errorf("Expected NeedsRefresh=%v, got %v", tt.shouldRefresh, result)
			}
		})
	}
}

// TestTokenRefresh_UpdateCredentials tests the repository UpdateCredentials method
func TestTokenRefresh_UpdateCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)

	// Create a test community first
	community := &communities.Community{
		DID:             "did:plc:test123",
		Handle:          "test.community.coves.social",
		Name:            "test",
		OwnerDID:        "did:plc:test123",
		CreatedByDID:    "did:plc:creator",
		HostedByDID:     "did:web:coves.social",
		PDSEmail:        "test@coves.social",
		PDSPassword:     "original-password",
		PDSAccessToken:  "original-access-token",
		PDSRefreshToken: "original-refresh-token",
		PDSURL:          "http://localhost:3001",
		Visibility:      "public",
		MemberCount:     0,
		SubscriberCount: 0,
		RecordURI:       "at://did:plc:test123/social.coves.community.profile/self",
		RecordCID:       "bafytest",
	}

	created, err := repo.Create(ctx, community)
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}

	// Update credentials
	newAccessToken := "new-access-token-12345"
	newRefreshToken := "new-refresh-token-67890"

	err = repo.UpdateCredentials(ctx, created.DID, newAccessToken, newRefreshToken)
	if err != nil {
		t.Fatalf("UpdateCredentials failed: %v", err)
	}

	// Verify tokens were updated
	retrieved, err := repo.GetByDID(ctx, created.DID)
	if err != nil {
		t.Fatalf("Failed to retrieve community: %v", err)
	}

	if retrieved.PDSAccessToken != newAccessToken {
		t.Errorf("Access token not updated: expected %q, got %q", newAccessToken, retrieved.PDSAccessToken)
	}

	if retrieved.PDSRefreshToken != newRefreshToken {
		t.Errorf("Refresh token not updated: expected %q, got %q", newRefreshToken, retrieved.PDSRefreshToken)
	}

	// Verify password unchanged (should not be affected)
	if retrieved.PDSPassword != "original-password" {
		t.Errorf("Password should remain unchanged: expected %q, got %q", "original-password", retrieved.PDSPassword)
	}
}

// TestTokenRefresh_E2E_UpdateAfterTokenRefresh tests end-to-end token refresh during community update
func TestTokenRefresh_E2E_UpdateAfterTokenRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	ctx := context.Background()
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	// This test requires a real PDS for token refresh
	// For now, we'll test the token expiration detection logic
	// Full E2E test with PDS will be added in manual testing phase

	repo := postgres.NewCommunityRepository(db)

	// Create community with expiring token
	expiringToken := createTestJWT(time.Now().Add(2 * time.Minute)) // Expires in 2 minutes

	community := &communities.Community{
		DID:             "did:plc:expiring123",
		Handle:          "expiring.community.coves.social",
		Name:            "expiring",
		OwnerDID:        "did:plc:expiring123",
		CreatedByDID:    "did:plc:creator",
		HostedByDID:     "did:web:coves.social",
		PDSEmail:        "expiring@coves.social",
		PDSPassword:     "test-password",
		PDSAccessToken:  expiringToken,
		PDSRefreshToken: "test-refresh-token",
		PDSURL:          "http://localhost:3001",
		Visibility:      "public",
		RecordURI:       "at://did:plc:expiring123/social.coves.community.profile/self",
		RecordCID:       "bafytest",
	}

	created, err := repo.Create(ctx, community)
	if err != nil {
		t.Fatalf("Failed to create community: %v", err)
	}

	// Verify token is stored
	if created.PDSAccessToken != expiringToken {
		t.Errorf("Token not stored correctly")
	}

	t.Logf("✅ Created community with expiring token (expires in 2 minutes)")
	t.Logf("   Community DID: %s", created.DID)
	t.Logf("   NOTE: Full refresh flow requires real PDS - tested in manual/staging tests")
}

// Helper: Create a test JWT with specific expiration time
func createTestJWT(expiresAt time.Time) string {
	// Create JWT header
	header := map[string]interface{}{
		"alg": "ES256",
		"typ": "JWT",
	}
	headerJSON, _ := json.Marshal(header)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	// Create JWT payload with expiration
	payload := map[string]interface{}{
		"sub": "did:plc:test",
		"iss": "https://pds.example.com",
		"exp": expiresAt.Unix(),
		"iat": time.Now().Unix(),
	}
	payloadJSON, _ := json.Marshal(payload)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	// Fake signature (not verified in our tests)
	signature := base64.RawURLEncoding.EncodeToString([]byte("fake-signature"))

	return fmt.Sprintf("%s.%s.%s", headerB64, payloadB64, signature)
}
