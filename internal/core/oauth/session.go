package oauth

import (
	"time"
)

// OAuthRequest represents a temporary OAuth authorization flow state
// Stored during the redirect to auth server, deleted after callback
type OAuthRequest struct {
	CreatedAt           time.Time `db:"created_at"`
	State               string    `db:"state"`
	DID                 string    `db:"did"`
	Handle              string    `db:"handle"`
	PDSURL              string    `db:"pds_url"`
	PKCEVerifier        string    `db:"pkce_verifier"`
	DPoPPrivateJWK      string    `db:"dpop_private_jwk"`
	DPoPAuthServerNonce string    `db:"dpop_authserver_nonce"`
	AuthServerIss       string    `db:"auth_server_iss"`
	ReturnURL           string    `db:"return_url"`
}

// OAuthSession represents a long-lived authenticated user session
// Stored after successful OAuth login, used for all authenticated requests
type OAuthSession struct {
	ExpiresAt           time.Time `db:"expires_at"`
	CreatedAt           time.Time `db:"created_at"`
	UpdatedAt           time.Time `db:"updated_at"`
	DID                 string    `db:"did"`
	Handle              string    `db:"handle"`
	PDSURL              string    `db:"pds_url"`
	AccessToken         string    `db:"access_token"`
	RefreshToken        string    `db:"refresh_token"`
	DPoPPrivateJWK      string    `db:"dpop_private_jwk"`
	DPoPAuthServerNonce string    `db:"dpop_authserver_nonce"`
	DPoPPDSNonce        string    `db:"dpop_pds_nonce"`
	AuthServerIss       string    `db:"auth_server_iss"`
}

// SessionStore defines the interface for OAuth session storage
type SessionStore interface {
	// OAuth flow state management
	SaveRequest(req *OAuthRequest) error
	GetRequestByState(state string) (*OAuthRequest, error)
	GetAndDeleteRequest(state string) (*OAuthRequest, error) // Atomic get-and-delete for CSRF protection
	DeleteRequest(state string) error

	// User session management
	SaveSession(session *OAuthSession) error
	GetSession(did string) (*OAuthSession, error)
	UpdateSession(session *OAuthSession) error
	DeleteSession(did string) error

	// Token refresh
	RefreshSession(did, newAccessToken, newRefreshToken string, expiresAt time.Time) error

	// Nonce updates (for DPoP)
	UpdateAuthServerNonce(did, nonce string) error
	UpdatePDSNonce(did, nonce string) error
}
