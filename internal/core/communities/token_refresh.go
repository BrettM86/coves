package communities

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/xrpc"
)

// refreshPDSToken exchanges a refresh token for new access and refresh tokens
// Uses com.atproto.server.refreshSession endpoint via Indigo SDK
// CRITICAL: Refresh tokens are single-use - old refresh token is revoked on success
//
// httpClient is REQUIRED and is the SSRF guard. Leaving xrpc.Client.Client nil
// makes indigo substitute util.RobustHTTPClient() — unguarded — on a call that
// sends the community's refresh token as the Authorization header. See
// newPDSHTTPClient.
func refreshPDSToken(
	ctx context.Context, httpClient *http.Client, pdsURL, refreshToken string,
) (newAccessToken, newRefreshToken string, err error) {
	if httpClient == nil {
		return "", "", fmt.Errorf("HTTP client is required")
	}
	if pdsURL == "" {
		return "", "", fmt.Errorf("PDS URL is required")
	}
	if refreshToken == "" {
		return "", "", fmt.Errorf("refresh token is required")
	}

	// Create XRPC client with refresh token as the auth credential
	// IMPORTANT: The xrpc client always sends AccessJwt as the Authorization header,
	// but refreshSession requires the refresh token in that header.
	// So we put the refresh token in AccessJwt to make it work correctly.
	client := &xrpc.Client{
		Client: httpClient,
		Host:   pdsURL,
		Auth: &xrpc.AuthInfo{
			AccessJwt:  refreshToken, // Refresh token goes here (sent as Authorization header)
			RefreshJwt: refreshToken, // Also set here for completeness
		},
	}

	// Call com.atproto.server.refreshSession
	output, err := atproto.ServerRefreshSession(ctx, client)
	if err != nil {
		// Check for expired refresh token (401 Unauthorized)
		// Try typed error first (more reliable), fallback to string check
		var xrpcErr *xrpc.Error
		if errors.As(err, &xrpcErr) && xrpcErr.StatusCode == 401 {
			return "", "", fmt.Errorf("refresh token expired or invalid (needs password re-auth)")
		}

		// Fallback: string-based detection (in case error isn't wrapped as xrpc.Error)
		errStr := err.Error()
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "Unauthorized") {
			return "", "", fmt.Errorf("refresh token expired or invalid (needs password re-auth)")
		}

		return "", "", fmt.Errorf("failed to refresh session: %w", err)
	}

	// Validate response
	if output.AccessJwt == "" || output.RefreshJwt == "" {
		return "", "", fmt.Errorf("refresh response missing tokens")
	}

	return output.AccessJwt, output.RefreshJwt, nil
}

// reauthenticateWithPassword creates a new session using stored credentials
// This is the fallback when refresh tokens expire (after ~2 months)
// Uses com.atproto.server.createSession endpoint via Indigo SDK
//
// httpClient is REQUIRED and is the SSRF guard. This call POSTs the community's
// CLEARTEXT password and system email in the request body, which makes it the
// worst payload in this package to deliver to an address someone else chose. See
// newPDSHTTPClient.
func reauthenticateWithPassword(
	ctx context.Context, httpClient *http.Client, pdsURL, email, password string,
) (accessToken, refreshToken string, err error) {
	if httpClient == nil {
		return "", "", fmt.Errorf("HTTP client is required")
	}
	if pdsURL == "" {
		return "", "", fmt.Errorf("PDS URL is required")
	}
	if email == "" {
		return "", "", fmt.Errorf("email is required")
	}
	if password == "" {
		return "", "", fmt.Errorf("password is required")
	}

	// Create unauthenticated XRPC client
	client := &xrpc.Client{
		Client: httpClient,
		Host:   pdsURL,
	}

	// Prepare createSession input
	// The identifier can be either email or handle
	input := &atproto.ServerCreateSession_Input{
		Identifier: email,
		Password:   password,
	}

	// Call com.atproto.server.createSession
	output, err := atproto.ServerCreateSession(ctx, client, input)
	if err != nil {
		return "", "", fmt.Errorf("failed to create session: %w", err)
	}

	// Validate response
	if output.AccessJwt == "" || output.RefreshJwt == "" {
		return "", "", fmt.Errorf("createSession response missing tokens")
	}

	return output.AccessJwt, output.RefreshJwt, nil
}
