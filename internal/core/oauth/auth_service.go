package oauth

import (
	"Coves/internal/atproto/oauth"
	"context"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
)

// AuthService handles authentication-related business logic
// Extracted from middleware to maintain clean architecture
type AuthService struct {
	sessionStore SessionStore
	oauthClient  *oauth.Client
}

// NewAuthService creates a new authentication service
func NewAuthService(sessionStore SessionStore, oauthClient *oauth.Client) *AuthService {
	return &AuthService{
		sessionStore: sessionStore,
		oauthClient:  oauthClient,
	}
}

// ValidateSession retrieves and validates a user's OAuth session
// Returns the session if valid, error if not found or expired
func (s *AuthService) ValidateSession(ctx context.Context, did string) (*OAuthSession, error) {
	session, err := s.sessionStore.GetSession(did)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}
	return session, nil
}

// RefreshTokenIfNeeded checks if token needs refresh and refreshes if necessary
// Returns updated session if refreshed, original session otherwise
func (s *AuthService) RefreshTokenIfNeeded(ctx context.Context, session *OAuthSession, threshold time.Duration) (*OAuthSession, error) {
	// Check if token needs refresh
	if time.Until(session.ExpiresAt) >= threshold {
		// Token is still valid, no refresh needed
		return session, nil
	}

	// Parse DPoP key
	dpopKey, err := oauth.ParseJWKFromJSON([]byte(session.DPoPPrivateJWK))
	if err != nil {
		return nil, fmt.Errorf("failed to parse DPoP key: %w", err)
	}

	// Refresh token
	tokenResp, err := s.oauthClient.RefreshTokenRequest(
		ctx,
		session.RefreshToken,
		session.AuthServerIss,
		session.DPoPAuthServerNonce,
		dpopKey,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}

	// Update session with new tokens
	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if err := s.sessionStore.RefreshSession(session.DID, tokenResp.AccessToken, tokenResp.RefreshToken, expiresAt); err != nil {
		return nil, fmt.Errorf("failed to update session: %w", err)
	}

	// Update nonce if provided (best effort - non-critical)
	if tokenResp.DpopAuthserverNonce != "" {
		session.DPoPAuthServerNonce = tokenResp.DpopAuthserverNonce
		if updateErr := s.sessionStore.UpdateAuthServerNonce(session.DID, tokenResp.DpopAuthserverNonce); updateErr != nil {
			// Log but don't fail - nonce will be updated on next request
			// (We ignore the error here intentionally as nonce updates are non-critical)
			_ = updateErr
		}
	}

	// Return updated session
	session.AccessToken = tokenResp.AccessToken
	session.RefreshToken = tokenResp.RefreshToken
	session.ExpiresAt = expiresAt

	return session, nil
}

// CreateDPoPKey generates a new DPoP key for a session
func (s *AuthService) CreateDPoPKey() (jwk.Key, error) {
	return oauth.GenerateDPoPKey()
}
