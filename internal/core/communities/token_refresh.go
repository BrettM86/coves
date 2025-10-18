package communities

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/xrpc"
)

// refreshPDSToken exchanges a refresh token for new access and refresh tokens
// Uses com.atproto.server.refreshSession endpoint via Indigo SDK
// CRITICAL: Refresh tokens are single-use - old refresh token is revoked on success
func refreshPDSToken(ctx context.Context, pdsURL, currentAccessToken, refreshToken string) (newAccessToken, newRefreshToken string, err error) {
	if pdsURL == "" {
		return "", "", fmt.Errorf("PDS URL is required")
	}
	if refreshToken == "" {
		return "", "", fmt.Errorf("refresh token is required")
	}

	// Create XRPC client with auth credentials
	// The refresh endpoint requires authentication with the refresh token
	client := &xrpc.Client{
		Host: pdsURL,
		Auth: &xrpc.AuthInfo{
			AccessJwt:  currentAccessToken, // Can be expired (not used for refresh auth)
			RefreshJwt: refreshToken,       // This is what authenticates the refresh request
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
func reauthenticateWithPassword(ctx context.Context, pdsURL, email, password string) (accessToken, refreshToken string, err error) {
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
		Host: pdsURL,
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
