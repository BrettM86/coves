package integration

import (
	"Coves/internal/atproto/oauth"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// CreateTestUserOnPDS creates a user on the local PDS for OAuth testing
// Returns the DID, access token, and refresh token
func CreateTestUserOnPDS(t *testing.T, handle, email, password string) (did, accessToken, refreshToken string) {
	t.Helper()

	pdsURL := getTestPDSURL()

	// Use the existing createPDSAccount helper which returns accessToken and DID
	accessToken, did, err := createPDSAccount(pdsURL, handle, email, password)
	require.NoError(t, err, "Failed to create PDS account for OAuth test")
	require.NotEmpty(t, accessToken, "Access token should not be empty")
	require.NotEmpty(t, did, "DID should not be empty")

	// Note: The PDS createAccount endpoint may not return refresh token directly
	// For OAuth flow testing, we'll get the refresh token from the OAuth callback
	// For now, return empty refresh token
	refreshToken = ""

	return did, accessToken, refreshToken
}

// getTestPLCURL returns the PLC directory URL for testing from env var or default
func getTestPLCURL() string {
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	if plcURL == "" {
		plcURL = "http://localhost:3002" // Local PLC directory for testing
	}
	return plcURL
}

// SetupOAuthTestClient creates an OAuth client configured for testing with a PDS
// When PDS_URL starts with https://, production mode is used (DevMode=false)
// Otherwise, dev mode is used for localhost testing
func SetupOAuthTestClient(t *testing.T, store oauthlib.ClientAuthStore) *oauth.OAuthClient {
	t.Helper()

	// Generate a seal secret for testing (32 bytes)
	sealSecret := make([]byte, 32)
	_, err := rand.Read(sealSecret)
	require.NoError(t, err, "Failed to generate seal secret")

	sealSecretB64 := base64.StdEncoding.EncodeToString(sealSecret)

	// Detect if we're testing against a production (HTTPS) PDS
	pdsURL := getTestPDSURL()
	isProductionPDS := strings.HasPrefix(pdsURL, "https://")

	// Configure based on PDS type
	var config *oauth.OAuthConfig
	if isProductionPDS {
		// Production mode: HTTPS PDS, use real PLC directory
		config = &oauth.OAuthConfig{
			PublicURL:       "http://localhost:3000", // Test server callback URL
			SealSecret:      sealSecretB64,           // For sealing mobile tokens
			Scopes:          []string{"atproto", "transition:generic"},
			DevMode:         false, // Production mode for HTTPS PDS
			AllowPrivateIPs: false, // No private IPs in production mode
			PLCURL:          "",    // Use default PLC directory (plc.directory)
		}
		t.Logf("🌐 OAuth client configured for production PDS: %s", pdsURL)
	} else {
		// Dev mode: localhost PDS with HTTP
		config = &oauth.OAuthConfig{
			PublicURL:       "http://localhost:3000", // Match the callback URL expected by PDS
			SealSecret:      sealSecretB64,           // For sealing mobile tokens
			Scopes:          []string{"atproto", "transition:generic"},
			DevMode:         true,            // Enable dev mode for localhost testing
			AllowPrivateIPs: true,            // Allow private IPs for local testing
			PLCURL:          getTestPLCURL(), // Use local PLC directory for DID resolution
		}
		t.Logf("🔧 OAuth client configured for local PDS: %s", pdsURL)
	}

	client, err := oauth.NewOAuthClient(config, store)
	require.NoError(t, err, "Failed to create OAuth client")
	require.NotNil(t, client, "OAuth client should not be nil")

	return client
}

// SetupOAuthTestStore creates a test OAuth store backed by the test database.
// The store is wrapped with MobileAwareStoreWrapper to support mobile OAuth flows.
func SetupOAuthTestStore(t *testing.T, db *sql.DB) oauthlib.ClientAuthStore {
	t.Helper()

	baseStore := oauth.NewPostgresOAuthStore(db, 0) // Use default TTL
	require.NotNil(t, baseStore, "OAuth base store should not be nil")

	// Wrap with MobileAwareStoreWrapper to support mobile OAuth
	// Without this, mobile OAuth silently fails (no server-side CSRF data is stored)
	wrappedStore := oauth.NewMobileAwareStoreWrapper(baseStore)
	require.NotNil(t, wrappedStore, "OAuth wrapped store should not be nil")

	return wrappedStore
}

// CleanupOAuthTestData removes OAuth test data from the database
func CleanupOAuthTestData(t *testing.T, db *sql.DB, did string) {
	t.Helper()

	ctx := context.Background()

	// Delete sessions for this DID
	_, err := db.ExecContext(ctx, "DELETE FROM oauth_sessions WHERE did = $1", did)
	if err != nil {
		t.Logf("Warning: Failed to cleanup OAuth sessions: %v", err)
	}

	// Delete auth requests (cleanup all expired ones)
	_, err = db.ExecContext(ctx, "DELETE FROM oauth_requests WHERE created_at < NOW() - INTERVAL '1 hour'")
	if err != nil {
		t.Logf("Warning: Failed to cleanup OAuth auth requests: %v", err)
	}
}

// VerifySessionData verifies that session data is properly stored and retrievable
func VerifySessionData(t *testing.T, store oauthlib.ClientAuthStore, did syntax.DID, sessionID string) {
	t.Helper()

	ctx := context.Background()

	sessData, err := store.GetSession(ctx, did, sessionID)
	require.NoError(t, err, "Should be able to retrieve saved session")
	require.NotNil(t, sessData, "Session data should not be nil")
	require.Equal(t, did, sessData.AccountDID, "Session DID should match")
	require.Equal(t, sessionID, sessData.SessionID, "Session ID should match")
	require.NotEmpty(t, sessData.AccessToken, "Access token should be present")
}

// NOTE: Full OAuth redirect flow testing requires both HTTPS PDS and HTTPS Coves.
// The following functions would be used for end-to-end OAuth flow testing with a real PDS:
//
// SimulatePDSOAuthApproval would simulate the PDS OAuth authorization flow:
//   - User logs into PDS
//   - User approves OAuth request
//   - PDS redirects back to Coves with authorization code
//
// WaitForOAuthCallback would wait for async OAuth callback processing:
//   - Poll database for auth request deletion
//   - Wait for session creation
//   - Timeout if callback doesn't complete
//
// These helpers are NOT implemented because:
// 1. OAuth spec requires HTTPS for authorization servers (no localhost testing)
// 2. The indigo library enforces this requirement strictly
// 3. Component tests (using mocked sessions) provide sufficient coverage
// 4. Full OAuth flow requires production-like HTTPS setup
//
// For full OAuth flow testing, use a production PDS with HTTPS and update
// the integration tests to handle the redirect flow.

// GenerateTestSealSecret generates a test seal secret for OAuth token sealing
func GenerateTestSealSecret() string {
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	if err != nil {
		panic(fmt.Sprintf("Failed to generate seal secret: %v", err))
	}
	return base64.StdEncoding.EncodeToString(secret)
}
