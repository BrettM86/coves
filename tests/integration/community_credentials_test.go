package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
)

// TestCommunityRepository_CredentialPersistence tests that PDS credentials are properly persisted
func TestCommunityRepository_CredentialPersistence(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("persists PDS credentials on create", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!cred-test-%s@coves.local", uniqueSuffix),
			Name:         "cred-test",
			OwnerDID:     communityDID, // V2: self-owned
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			// V2: PDS credentials
			PDSEmail:        "community-test@communities.coves.local",
			PDSPassword:     "cleartext-password-encrypted-by-repo", // V2: Cleartext (encrypted by repository)
			PDSAccessToken:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.token",
			PDSRefreshToken: "refresh_token_xyz123",
			PDSURL:          "http://localhost:2583",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create community with credentials: %v", err)
		}

		if created.ID == 0 {
			t.Error("Expected non-zero ID")
		}

		// Retrieve and verify credentials were persisted
		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve community: %v", err)
		}

		if retrieved.PDSEmail != community.PDSEmail {
			t.Errorf("Expected PDSEmail %s, got %s", community.PDSEmail, retrieved.PDSEmail)
		}
		if retrieved.PDSPassword != community.PDSPassword {
			t.Errorf("Expected PDSPassword to be persisted and encrypted/decrypted")
		}
		if retrieved.PDSAccessToken != community.PDSAccessToken {
			t.Errorf("Expected PDSAccessToken to be persisted and decrypted correctly")
		}
		if retrieved.PDSRefreshToken != community.PDSRefreshToken {
			t.Errorf("Expected PDSRefreshToken to be persisted and decrypted correctly")
		}
		if retrieved.PDSURL != community.PDSURL {
			t.Errorf("Expected PDSURL %s, got %s", community.PDSURL, retrieved.PDSURL)
		}
	})

	t.Run("handles empty credentials gracefully", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Community without PDS credentials (e.g., from Jetstream consumer)
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!nocred-test-%s@coves.local", uniqueSuffix),
			Name:         "nocred-test",
			OwnerDID:     communityDID,
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			// No PDS credentials
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create community without credentials: %v", err)
		}

		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve community: %v", err)
		}

		if retrieved.PDSEmail != "" {
			t.Errorf("Expected empty PDSEmail, got %s", retrieved.PDSEmail)
		}
		if retrieved.PDSAccessToken != "" {
			t.Errorf("Expected empty PDSAccessToken, got %s", retrieved.PDSAccessToken)
		}
		if retrieved.PDSRefreshToken != "" {
			t.Errorf("Expected empty PDSRefreshToken, got %s", retrieved.PDSRefreshToken)
		}

		// Verify community is still functional
		if created.ID == 0 {
			t.Error("Expected non-zero ID even without credentials")
		}
	})
}

// TestCommunityRepository_EncryptedCredentials tests encryption at rest
func TestCommunityRepository_EncryptedCredentials(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("credentials are encrypted in database", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		accessToken := "sensitive_access_token_xyz123"
		refreshToken := "sensitive_refresh_token_abc456"

		community := &communities.Community{
			DID:             communityDID,
			Handle:          fmt.Sprintf("!encrypt-test-%s@coves.local", uniqueSuffix),
			Name:            "encrypt-test",
			OwnerDID:        communityDID,
			CreatedByDID:    "did:plc:user123",
			HostedByDID:     "did:web:coves.local",
			Visibility:      "public",
			PDSEmail:        "encrypted@communities.coves.local",
			PDSPassword:     "cleartext-password-for-encryption", // V2: Cleartext (encrypted by repository)
			PDSAccessToken:  accessToken,
			PDSRefreshToken: refreshToken,
			PDSURL:          "http://localhost:2583",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Query database directly to verify encryption
		var encryptedAccess, encryptedRefresh []byte
		query := `
			SELECT pds_access_token_encrypted, pds_refresh_token_encrypted
			FROM communities
			WHERE did = $1
		`
		if err := db.QueryRowContext(ctx, query, communityDID).Scan(&encryptedAccess, &encryptedRefresh); err != nil {
			t.Fatalf("Failed to query encrypted data: %v", err)
		}

		// Verify encrypted data is NOT the same as plaintext
		if string(encryptedAccess) == accessToken {
			t.Error("Access token should be encrypted, but found plaintext in database")
		}
		if string(encryptedRefresh) == refreshToken {
			t.Error("Refresh token should be encrypted, but found plaintext in database")
		}

		// Verify encrypted data is not empty
		if len(encryptedAccess) == 0 {
			t.Error("Expected encrypted access token to have data")
		}
		if len(encryptedRefresh) == 0 {
			t.Error("Expected encrypted refresh token to have data")
		}

		// Verify repository decrypts correctly
		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve community: %v", err)
		}

		if retrieved.PDSAccessToken != accessToken {
			t.Errorf("Decrypted access token mismatch: expected %s, got %s", accessToken, retrieved.PDSAccessToken)
		}
		if retrieved.PDSRefreshToken != refreshToken {
			t.Errorf("Decrypted refresh token mismatch: expected %s, got %s", refreshToken, retrieved.PDSRefreshToken)
		}
	})

	t.Run("encryption handles special characters", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		// Token with special characters
		specialToken := "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJodHRwczovL2NvdmVzLnNvY2lhbCIsInN1YiI6ImRpZDpwbGM6YWJjMTIzIiwiaWF0IjoxNzA5MjQwMDAwfQ.special/chars+here=="

		community := &communities.Community{
			DID:             communityDID,
			Handle:          fmt.Sprintf("!special-test-%s@coves.local", uniqueSuffix),
			Name:            "special-test",
			OwnerDID:        communityDID,
			CreatedByDID:    "did:plc:user123",
			HostedByDID:     "did:web:coves.local",
			Visibility:      "public",
			PDSAccessToken:  specialToken,
			PDSRefreshToken: "refresh+with/special=chars",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community with special chars: %v", err)
		}

		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve community: %v", err)
		}

		if retrieved.PDSAccessToken != specialToken {
			t.Errorf("Special characters not preserved during encryption/decryption: expected %s, got %s", specialToken, retrieved.PDSAccessToken)
		}
	})
}

// TestCommunityRepository_V2OwnershipModel tests that communities are self-owned
func TestCommunityRepository_V2OwnershipModel(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database: %v", err)
		}
	}()

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("V2 communities are self-owned", func(t *testing.T) {
		uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		communityDID := generateTestDID(uniqueSuffix)

		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!v2-test-%s@coves.local", uniqueSuffix),
			Name:         "v2-test",
			OwnerDID:     communityDID, // V2: owner == community DID
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		created, err := repo.Create(ctx, community)
		if err != nil {
			t.Fatalf("Failed to create V2 community: %v", err)
		}

		// Verify self-ownership
		if created.OwnerDID != created.DID {
			t.Errorf("V2 community should be self-owned: expected OwnerDID=%s, got %s", created.DID, created.OwnerDID)
		}

		retrieved, err := repo.GetByDID(ctx, communityDID)
		if err != nil {
			t.Fatalf("Failed to retrieve community: %v", err)
		}

		if retrieved.OwnerDID != retrieved.DID {
			t.Errorf("V2 community should be self-owned after retrieval: expected OwnerDID=%s, got %s", retrieved.DID, retrieved.OwnerDID)
		}
	})
}
