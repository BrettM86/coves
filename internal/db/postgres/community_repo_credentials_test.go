//go:build integration

package postgres

import (
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"testing"
	"time"
)

// Encryption at rest for a community's PDS credentials.
//
// A community in the V2 model owns a PDS account, and the repository is the only
// place its password and its access and refresh tokens are encrypted and
// decrypted: Create seals them with the app-side AES-256-GCM credential cipher
// before the INSERT, and the read paths open them after the SELECT. The key stays
// in the process, and each value is bound to its table, column, and community DID,
// so the storage boundary is why this suite lives beside community_repo.go rather
// than in internal/core/communities.
//
// The load-bearing assertion is the one that reads the ciphertext columns
// directly. A repository that stored the tokens in plaintext would pass every
// round-trip check in this file, so at least one test has to look at what is
// actually on disk.

// TestCommunityRepository_CredentialPersistence covers the plumbing: that the
// credential fields survive a Create/GetByDID round trip, and that a community
// carrying none of them (the shape the firehose consumer indexes for a remote
// community) is still a valid row.
func TestCommunityRepository_CredentialPersistence(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	t.Run("persists PDS credentials on create", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id

		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!cred-test-%s@coves.local", id),
			Name:         "cred-test",
			OwnerDID:     communityDID, // V2: self-owned
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			// V2: PDS credentials. The password is handed over in cleartext;
			// encrypting it is the repository's job, not the caller's.
			PDSEmail:        "community-test@communities.coves.local",
			PDSPassword:     "cleartext-password-encrypted-by-repo",
			PDSAccessToken:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.token",
			PDSRefreshToken: "refresh_token_xyz123",
			PDSURL:          testkit.Endpoints().PDS.BaseURL,
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
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id

		// A community indexed from the firehose has no credentials at all: the
		// encrypt expressions must write NULL rather than the ciphertext of an
		// empty string, and the read path must give back "" rather than fail.
		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!nocred-test-%s@coves.local", id),
			Name:         "nocred-test",
			OwnerDID:     communityDID,
			CreatedByDID: "did:plc:user123",
			HostedByDID:  "did:web:coves.local",
			Visibility:   "public",
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
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
		// The password is the column this file's siblings care most about, and it
		// was the one missing from this list. It goes through the same
		// app-side credential cipher as the tokens above but is read on a different
		// path — EnsureFreshToken falls back to it to open a new
		// session when the refresh token has expired — so a firehose-indexed
		// community giving back ciphertext, or an error, instead of "" would
		// surface there rather than here.
		if retrieved.PDSPassword != "" {
			t.Errorf("Expected empty PDSPassword, got %q", retrieved.PDSPassword)
		}

		// Verify community is still functional
		if created.ID == 0 {
			t.Error("Expected non-zero ID even without credentials")
		}
	})
}

// TestCommunityRepository_EncryptedCredentials proves the tokens are ciphertext
// on disk, and that the encoding survives the byte sequences a real JWT contains.
func TestCommunityRepository_EncryptedCredentials(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	t.Run("credentials are encrypted in database", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id

		accessToken := "sensitive_access_token_xyz123"
		refreshToken := "sensitive_refresh_token_abc456"

		community := &communities.Community{
			DID:             communityDID,
			Handle:          fmt.Sprintf("!encrypt-test-%s@coves.local", id),
			Name:            "encrypt-test",
			OwnerDID:        communityDID,
			CreatedByDID:    "did:plc:user123",
			HostedByDID:     "did:web:coves.local",
			Visibility:      "public",
			PDSEmail:        "encrypted@communities.coves.local",
			PDSPassword:     "cleartext-password-for-encryption",
			PDSAccessToken:  accessToken,
			PDSRefreshToken: refreshToken,
			PDSURL:          testkit.Endpoints().PDS.BaseURL,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		if _, err := repo.Create(ctx, community); err != nil {
			t.Fatalf("Failed to create community: %v", err)
		}

		// Read the raw columns, bypassing the repository's decryption. This is
		// the assertion the round trip cannot make: a repository that never
		// encrypted anything would still round-trip perfectly.
		var encryptedAccess, encryptedRefresh []byte
		query := `
			SELECT pds_access_token_encrypted, pds_refresh_token_encrypted
			FROM communities
			WHERE did = $1
		`
		if err := db.QueryRowContext(ctx, query, communityDID).Scan(&encryptedAccess, &encryptedRefresh); err != nil {
			t.Fatalf("Failed to query encrypted data: %v", err)
		}

		if string(encryptedAccess) == accessToken {
			t.Error("Access token should be encrypted, but found plaintext in database")
		}
		if string(encryptedRefresh) == refreshToken {
			t.Error("Refresh token should be encrypted, but found plaintext in database")
		}

		// Non-empty rules out the other failure: a CASE expression that decided
		// the credential was absent and stored NULL.
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
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id

		// A real JWT: dots between segments, and base64url padding that includes
		// '+', '/' and '='. Any of those would be mangled by an encoding that
		// assumed the plaintext was word characters only.
		specialToken := "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJodHRwczovL2NvdmVzLnNvY2lhbCIsInN1YiI6ImRpZDpwbGM6YWJjMTIzIiwiaWF0IjoxNzA5MjQwMDAwfQ.special/chars+here=="

		community := &communities.Community{
			DID:             communityDID,
			Handle:          fmt.Sprintf("!special-test-%s@coves.local", id),
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

// TestCommunityRepository_V2OwnershipModel pins the V2 ownership invariant at the
// storage layer: a community's owner_did is its own DID, because the community
// holds its own PDS account rather than living inside its creator's repository.
// created_by_did records the human who asked for it and is deliberately
// different.
func TestCommunityRepository_V2OwnershipModel(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	t.Run("V2 communities are self-owned", func(t *testing.T) {
		id := testkit.UniqueID(t)
		communityDID := "did:plc:test" + id

		community := &communities.Community{
			DID:          communityDID,
			Handle:       fmt.Sprintf("!v2-test-%s@coves.local", id),
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
