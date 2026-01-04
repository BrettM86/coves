package pds

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// NewFromOAuthSession creates a PDS client from an OAuth session.
// This uses DPoP authentication - the correct method for OAuth tokens.
//
// The oauthClient is used to resume the session and get a properly configured
// APIClient that handles DPoP proof generation and nonce rotation automatically.
func NewFromOAuthSession(ctx context.Context, oauthClient *oauth.ClientApp, sessionData *oauth.ClientSessionData) (Client, error) {
	if oauthClient == nil {
		return nil, fmt.Errorf("oauthClient is required")
	}
	if sessionData == nil {
		return nil, fmt.Errorf("sessionData is required")
	}

	// ResumeSession reconstructs the OAuth session with DPoP key
	// and returns a ClientSession that can generate authenticated requests.
	// Common failure modes:
	// - Expired access/refresh tokens → User needs to re-authenticate
	// - Session revoked on PDS → User needs to re-authenticate
	// - DPoP nonce mismatch → Retry may help (transient)
	// - DPoP key mismatch → Session data corrupted, re-authenticate
	sess, err := oauthClient.ResumeSession(ctx, sessionData.AccountDID, sessionData.SessionID)
	if err != nil {
		// Include DID and session context for debugging
		return nil, fmt.Errorf("failed to resume OAuth session for DID=%s, sessionID=%s: %w",
			sessionData.AccountDID.String(), sessionData.SessionID, err)
	}

	// APIClient() returns an *atclient.APIClient configured with DPoP auth
	apiClient := sess.APIClient()

	return &client{
		apiClient: apiClient,
		did:       sessionData.AccountDID.String(),
		host:      sessionData.HostURL,
	}, nil
}

// NewFromPasswordAuth creates a PDS client using password authentication.
// This uses Bearer token authentication from com.atproto.server.createSession.
//
// Primarily used for:
// - E2E tests with local PDS
// - Development/debugging tools
// - Non-OAuth clients
//
// Note: This establishes a new session with the PDS. For repeated calls,
// consider using NewFromAccessToken if you already have a valid access token.
func NewFromPasswordAuth(ctx context.Context, host, handle, password string) (Client, error) {
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if handle == "" {
		return nil, fmt.Errorf("handle is required")
	}
	if password == "" {
		return nil, fmt.Errorf("password is required")
	}

	// LoginWithPasswordHost creates a session and returns an authenticated APIClient
	// This handles the createSession call and Bearer token setup
	apiClient, err := atclient.LoginWithPasswordHost(ctx, host, handle, password, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to login with password: %w", err)
	}

	// Get DID from the authenticated client
	did := ""
	if apiClient.AccountDID != nil {
		did = apiClient.AccountDID.String()
	}

	return &client{
		apiClient: apiClient,
		did:       did,
		host:      host,
	}, nil
}

// NewFromAccessToken creates a PDS client from an existing access token.
// This is useful when you already have a valid Bearer token (e.g., from createSession)
// and don't want to re-authenticate.
//
// WARNING: This creates a client with Bearer auth only. Do NOT use this with
// OAuth access tokens - those require DPoP proofs. Use NewFromOAuthSession instead.
func NewFromAccessToken(host, did, accessToken string) (Client, error) {
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if did == "" {
		return nil, fmt.Errorf("did is required")
	}
	if accessToken == "" {
		return nil, fmt.Errorf("accessToken is required")
	}

	// Create APIClient with Bearer auth
	apiClient := atclient.NewAPIClient(host)
	apiClient.Auth = &bearerAuth{token: accessToken}

	return &client{
		apiClient: apiClient,
		did:       did,
		host:      host,
	}, nil
}

// bearerAuth implements atclient.AuthMethod for simple Bearer token auth.
// This is used for password-based sessions where DPoP is not required.
type bearerAuth struct {
	token string
}

// Ensure bearerAuth implements atclient.AuthMethod.
var _ atclient.AuthMethod = (*bearerAuth)(nil)

// DoWithAuth adds the Bearer token to the request and executes it.
func (b *bearerAuth) DoWithAuth(c *http.Client, req *http.Request, _ syntax.NSID) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return c.Do(req)
}
