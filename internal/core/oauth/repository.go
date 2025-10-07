package oauth

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PostgresSessionStore implements SessionStore using PostgreSQL
type PostgresSessionStore struct {
	db *sql.DB
}

// NewPostgresSessionStore creates a new PostgreSQL-backed session store
func NewPostgresSessionStore(db *sql.DB) SessionStore {
	return &PostgresSessionStore{db: db}
}

// SaveRequest stores a temporary OAuth request state
func (s *PostgresSessionStore) SaveRequest(req *OAuthRequest) error {
	query := `
		INSERT INTO oauth_requests (
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_jwk, dpop_authserver_nonce, auth_server_iss, return_url
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := s.db.Exec(
		query,
		req.State,
		req.DID,
		req.Handle,
		req.PDSURL,
		req.PKCEVerifier,
		req.DPoPPrivateJWK,
		req.DPoPAuthServerNonce,
		req.AuthServerIss,
		req.ReturnURL,
	)

	if err != nil {
		return fmt.Errorf("failed to save OAuth request: %w", err)
	}

	return nil
}

// GetRequestByState retrieves an OAuth request by state parameter
func (s *PostgresSessionStore) GetRequestByState(state string) (*OAuthRequest, error) {
	query := `
		SELECT
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_jwk, dpop_authserver_nonce, auth_server_iss,
			COALESCE(return_url, ''), created_at
		FROM oauth_requests
		WHERE state = $1
	`

	var req OAuthRequest
	err := s.db.QueryRow(query, state).Scan(
		&req.State,
		&req.DID,
		&req.Handle,
		&req.PDSURL,
		&req.PKCEVerifier,
		&req.DPoPPrivateJWK,
		&req.DPoPAuthServerNonce,
		&req.AuthServerIss,
		&req.ReturnURL,
		&req.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("OAuth request not found for state: %s", state)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get OAuth request: %w", err)
	}

	return &req, nil
}

// GetAndDeleteRequest atomically retrieves and deletes an OAuth request to prevent replay attacks
// This ensures the state parameter can only be used once
func (s *PostgresSessionStore) GetAndDeleteRequest(state string) (*OAuthRequest, error) {
	query := `
		DELETE FROM oauth_requests
		WHERE state = $1
		RETURNING
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_jwk, dpop_authserver_nonce, auth_server_iss,
			COALESCE(return_url, ''), created_at
	`

	var req OAuthRequest
	err := s.db.QueryRow(query, state).Scan(
		&req.State,
		&req.DID,
		&req.Handle,
		&req.PDSURL,
		&req.PKCEVerifier,
		&req.DPoPPrivateJWK,
		&req.DPoPAuthServerNonce,
		&req.AuthServerIss,
		&req.ReturnURL,
		&req.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("OAuth request not found or already used: %s", state)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get and delete OAuth request: %w", err)
	}

	return &req, nil
}

// DeleteRequest removes an OAuth request (cleanup after callback)
func (s *PostgresSessionStore) DeleteRequest(state string) error {
	query := `DELETE FROM oauth_requests WHERE state = $1`

	_, err := s.db.Exec(query, state)
	if err != nil {
		return fmt.Errorf("failed to delete OAuth request: %w", err)
	}

	return nil
}

// SaveSession stores a new OAuth session (upsert on DID)
func (s *PostgresSessionStore) SaveSession(session *OAuthSession) error {
	query := `
		INSERT INTO oauth_sessions (
			did, handle, pds_url, access_token, refresh_token,
			dpop_private_jwk, dpop_authserver_nonce, dpop_pds_nonce,
			auth_server_iss, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (did) DO UPDATE SET
			handle = EXCLUDED.handle,
			pds_url = EXCLUDED.pds_url,
			access_token = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			dpop_private_jwk = EXCLUDED.dpop_private_jwk,
			dpop_authserver_nonce = EXCLUDED.dpop_authserver_nonce,
			dpop_pds_nonce = EXCLUDED.dpop_pds_nonce,
			auth_server_iss = EXCLUDED.auth_server_iss,
			expires_at = EXCLUDED.expires_at,
			updated_at = CURRENT_TIMESTAMP
	`

	_, err := s.db.Exec(
		query,
		session.DID,
		session.Handle,
		session.PDSURL,
		session.AccessToken,
		session.RefreshToken,
		session.DPoPPrivateJWK,
		session.DPoPAuthServerNonce,
		session.DPoPPDSNonce,
		session.AuthServerIss,
		session.ExpiresAt,
	)

	if err != nil {
		return fmt.Errorf("failed to save OAuth session: %w", err)
	}

	return nil
}

// GetSession retrieves an OAuth session by DID
func (s *PostgresSessionStore) GetSession(did string) (*OAuthSession, error) {
	query := `
		SELECT
			did, handle, pds_url, access_token, refresh_token,
			dpop_private_jwk,
			COALESCE(dpop_authserver_nonce, ''),
			COALESCE(dpop_pds_nonce, ''),
			auth_server_iss, expires_at, created_at, updated_at
		FROM oauth_sessions
		WHERE did = $1
	`

	var session OAuthSession
	err := s.db.QueryRow(query, did).Scan(
		&session.DID,
		&session.Handle,
		&session.PDSURL,
		&session.AccessToken,
		&session.RefreshToken,
		&session.DPoPPrivateJWK,
		&session.DPoPAuthServerNonce,
		&session.DPoPPDSNonce,
		&session.AuthServerIss,
		&session.ExpiresAt,
		&session.CreatedAt,
		&session.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session not found for DID: %s", did)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get OAuth session: %w", err)
	}

	return &session, nil
}

// UpdateSession updates an existing OAuth session
func (s *PostgresSessionStore) UpdateSession(session *OAuthSession) error {
	query := `
		UPDATE oauth_sessions SET
			handle = $2,
			pds_url = $3,
			access_token = $4,
			refresh_token = $5,
			dpop_private_jwk = $6,
			dpop_authserver_nonce = $7,
			dpop_pds_nonce = $8,
			auth_server_iss = $9,
			expires_at = $10,
			updated_at = CURRENT_TIMESTAMP
		WHERE did = $1
	`

	result, err := s.db.Exec(
		query,
		session.DID,
		session.Handle,
		session.PDSURL,
		session.AccessToken,
		session.RefreshToken,
		session.DPoPPrivateJWK,
		session.DPoPAuthServerNonce,
		session.DPoPPDSNonce,
		session.AuthServerIss,
		session.ExpiresAt,
	)

	if err != nil {
		return fmt.Errorf("failed to update OAuth session: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("session not found for DID: %s", session.DID)
	}

	return nil
}

// DeleteSession removes an OAuth session (logout)
func (s *PostgresSessionStore) DeleteSession(did string) error {
	query := `DELETE FROM oauth_sessions WHERE did = $1`

	_, err := s.db.Exec(query, did)
	if err != nil {
		return fmt.Errorf("failed to delete OAuth session: %w", err)
	}

	return nil
}

// RefreshSession updates access and refresh tokens after a token refresh
func (s *PostgresSessionStore) RefreshSession(did, newAccessToken, newRefreshToken string, expiresAt time.Time) error {
	query := `
		UPDATE oauth_sessions SET
			access_token = $2,
			refresh_token = $3,
			expires_at = $4,
			updated_at = CURRENT_TIMESTAMP
		WHERE did = $1
	`

	result, err := s.db.Exec(query, did, newAccessToken, newRefreshToken, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to refresh OAuth session: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("session not found for DID: %s", did)
	}

	return nil
}

// UpdateAuthServerNonce updates the DPoP nonce for the auth server token endpoint
func (s *PostgresSessionStore) UpdateAuthServerNonce(did, nonce string) error {
	query := `
		UPDATE oauth_sessions SET
			dpop_authserver_nonce = $2,
			updated_at = CURRENT_TIMESTAMP
		WHERE did = $1
	`

	_, err := s.db.Exec(query, did, nonce)
	if err != nil {
		return fmt.Errorf("failed to update auth server nonce: %w", err)
	}

	return nil
}

// UpdatePDSNonce updates the DPoP nonce for PDS requests
func (s *PostgresSessionStore) UpdatePDSNonce(did, nonce string) error {
	query := `
		UPDATE oauth_sessions SET
			dpop_pds_nonce = $2,
			updated_at = CURRENT_TIMESTAMP
		WHERE did = $1
	`

	_, err := s.db.Exec(query, did, nonce)
	if err != nil {
		return fmt.Errorf("failed to update PDS nonce: %w", err)
	}

	return nil
}

// CleanupExpiredRequests removes OAuth requests older than 30 minutes
// Should be called periodically (e.g., via cron job or background goroutine)
func (s *PostgresSessionStore) CleanupExpiredRequests(ctx context.Context) error {
	query := `DELETE FROM oauth_requests WHERE created_at < NOW() - INTERVAL '30 minutes'`

	_, err := s.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to cleanup expired requests: %w", err)
	}

	return nil
}

// CleanupExpiredSessions removes OAuth sessions that have been expired for > 7 days
// Gives users time to refresh their tokens before permanent deletion
func (s *PostgresSessionStore) CleanupExpiredSessions(ctx context.Context) error {
	query := `DELETE FROM oauth_sessions WHERE expires_at < NOW() - INTERVAL '7 days'`

	_, err := s.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to cleanup expired sessions: %w", err)
	}

	return nil
}
