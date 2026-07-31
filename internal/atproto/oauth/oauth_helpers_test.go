//go:build integration

package oauth_test

import (
	"Coves/internal/atproto/oauth"
	"Coves/tests/testkit"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"testing"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/require"
)

// testPDSURL is the PDS this package's OAuth sessions belong to: the address a
// ClientSessionData records as its HostURL and the issuer a callback carries.
//
// It comes from testkit rather than a literal so a relocated stack moves the
// tests with it (docs/TEST_ARCHITECTURE.md §3.7, layer 1).
func testPDSURL() string {
	return testkit.Endpoints().PDS.BaseURL
}

// SetupOAuthTestClient creates an OAuth client configured for testing against
// the test stack's PDS.
//
// The client is ALWAYS in dev mode. There used to be a second branch here that
// switched to production mode and the public plc.directory whenever PDS_URL
// began with "https://", and no run could ever select it: the stack's PDS is a
// local HTTP service in both the dev and the hermetic CI compose files, and
// CI's network is egress-blocked (docs/TEST_ARCHITECTURE.md §3.7, layer 3), so
// a run that somehow did select it would fail at connect time rather than
// "resolve DIDs read-only" as its comment claimed. It is deleted, not exempted.
func SetupOAuthTestClient(t *testing.T, store oauthlib.ClientAuthStore) *oauth.OAuthClient {
	t.Helper()

	// Generate a seal secret for testing (32 bytes)
	sealSecret := make([]byte, 32)
	_, err := rand.Read(sealSecret)
	require.NoError(t, err, "Failed to generate seal secret")

	endpoints := testkit.Endpoints()

	config := &oauth.OAuthConfig{
		// PublicURL is the OAuth client's OWN public address — Coves, not the
		// PDS — which is what the callback URL and client_id are built from.
		PublicURL:       endpoints.AppView.BaseURL,
		SealSecret:      base64.StdEncoding.EncodeToString(sealSecret), // For sealing mobile tokens
		Scopes:          []string{"atproto"},
		DevMode:         true,                  // Loopback client: the stack's PDS speaks HTTP
		AllowPrivateIPs: true,                  // Allow private IPs for local testing
		PLCURL:          endpoints.PLC.BaseURL, // Local PLC directory for DID resolution
	}
	t.Logf("🔧 OAuth client configured for local PDS: %s", endpoints.PDS.BaseURL)

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
