//go:build integration

package oauth_test

import (
	"Coves/internal/atproto/oauth"
	"Coves/tests/testkit"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/require"
)

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
	pdsURL := testkit.Endpoints().PDS.BaseURL
	isProductionPDS := strings.HasPrefix(pdsURL, "https://")

	// Configure based on PDS type
	var config *oauth.OAuthConfig
	if isProductionPDS {
		// Production mode: HTTPS PDS, use real PLC directory
		config = &oauth.OAuthConfig{
			PublicURL:       "http://localhost:3000", // Test server callback URL
			SealSecret:      sealSecretB64,           // For sealing mobile tokens
			Scopes:          []string{"atproto"},
			DevMode:         false,                   // Production mode for HTTPS PDS
			AllowPrivateIPs: false,                   // No private IPs in production mode
			PLCURL:          "https://plc.directory", // READ-ONLY: resolving DIDs that already exist on the production directory
		}
		t.Logf("🌐 OAuth client configured for production PDS: %s", pdsURL)
	} else {
		// Dev mode: localhost PDS with HTTP
		config = &oauth.OAuthConfig{
			PublicURL:       "http://localhost:3000", // Match the callback URL expected by PDS
			SealSecret:      sealSecretB64,           // For sealing mobile tokens
			Scopes:          []string{"atproto"},
			DevMode:         true,                            // Enable dev mode for localhost testing
			AllowPrivateIPs: true,                            // Allow private IPs for local testing
			PLCURL:          testkit.Endpoints().PLC.BaseURL, // Use local PLC directory for DID resolution
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
