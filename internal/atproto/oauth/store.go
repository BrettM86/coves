package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/lib/pq"
)

var (
	ErrSessionNotFound     = errors.New("oauth session not found")
	ErrAuthRequestNotFound = errors.New("oauth auth request not found")
)

// PostgresOAuthStore implements oauth.ClientAuthStore interface using PostgreSQL
type PostgresOAuthStore struct {
	db         *sql.DB
	sessionTTL time.Duration
}

// NewPostgresOAuthStore creates a new PostgreSQL-backed OAuth store
func NewPostgresOAuthStore(db *sql.DB, sessionTTL time.Duration) oauth.ClientAuthStore {
	if sessionTTL == 0 {
		sessionTTL = 7 * 24 * time.Hour // Default to 7 days
	}
	return &PostgresOAuthStore{
		db:         db,
		sessionTTL: sessionTTL,
	}
}

// GetSession retrieves a session by DID and session ID
func (s *PostgresOAuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	query := `
		SELECT
			did, session_id, host_url, auth_server_iss,
			auth_server_token_endpoint, auth_server_revocation_endpoint,
			scopes, access_token, refresh_token,
			dpop_authserver_nonce, dpop_pds_nonce, dpop_private_key_multibase
		FROM oauth_sessions
		WHERE did = $1 AND session_id = $2 AND expires_at > NOW()
	`

	var session oauth.ClientSessionData
	var authServerIss, authServerTokenEndpoint, authServerRevocationEndpoint sql.NullString
	var hostURL, dpopPrivateKeyMultibase sql.NullString
	var scopes pq.StringArray
	var dpopAuthServerNonce, dpopHostNonce sql.NullString

	err := s.db.QueryRowContext(ctx, query, did.String(), sessionID).Scan(
		&session.AccountDID,
		&session.SessionID,
		&hostURL,
		&authServerIss,
		&authServerTokenEndpoint,
		&authServerRevocationEndpoint,
		&scopes,
		&session.AccessToken,
		&session.RefreshToken,
		&dpopAuthServerNonce,
		&dpopHostNonce,
		&dpopPrivateKeyMultibase,
	)

	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	// Convert nullable fields
	if hostURL.Valid {
		session.HostURL = hostURL.String
	}
	if authServerIss.Valid {
		session.AuthServerURL = authServerIss.String
	}
	if authServerTokenEndpoint.Valid {
		session.AuthServerTokenEndpoint = authServerTokenEndpoint.String
	}
	if authServerRevocationEndpoint.Valid {
		session.AuthServerRevocationEndpoint = authServerRevocationEndpoint.String
	}
	if dpopAuthServerNonce.Valid {
		session.DPoPAuthServerNonce = dpopAuthServerNonce.String
	}
	if dpopHostNonce.Valid {
		session.DPoPHostNonce = dpopHostNonce.String
	}
	if dpopPrivateKeyMultibase.Valid {
		session.DPoPPrivateKeyMultibase = dpopPrivateKeyMultibase.String
	}
	session.Scopes = scopes

	return &session, nil
}

// SaveSession saves or updates a session (upsert operation)
func (s *PostgresOAuthStore) SaveSession(ctx context.Context, sess oauth.ClientSessionData) error {
	// Input validation per atProto OAuth security requirements

	// Validate DID format
	if _, err := syntax.ParseDID(sess.AccountDID.String()); err != nil {
		return fmt.Errorf("invalid DID format: %w", err)
	}

	// Validate token lengths (max 10000 chars to prevent memory issues)
	const maxTokenLength = 10000
	if len(sess.AccessToken) > maxTokenLength {
		return fmt.Errorf("access_token exceeds maximum length of %d characters", maxTokenLength)
	}
	if len(sess.RefreshToken) > maxTokenLength {
		return fmt.Errorf("refresh_token exceeds maximum length of %d characters", maxTokenLength)
	}

	// Validate session ID is not empty
	if sess.SessionID == "" {
		return fmt.Errorf("session_id cannot be empty")
	}

	// Validate URLs if provided
	if sess.HostURL != "" {
		if _, err := url.Parse(sess.HostURL); err != nil {
			return fmt.Errorf("invalid host_url: %w", err)
		}
	}
	if sess.AuthServerURL != "" {
		if _, err := url.Parse(sess.AuthServerURL); err != nil {
			return fmt.Errorf("invalid auth_server URL: %w", err)
		}
	}
	if sess.AuthServerTokenEndpoint != "" {
		if _, err := url.Parse(sess.AuthServerTokenEndpoint); err != nil {
			return fmt.Errorf("invalid auth_server_token_endpoint: %w", err)
		}
	}
	if sess.AuthServerRevocationEndpoint != "" {
		if _, err := url.Parse(sess.AuthServerRevocationEndpoint); err != nil {
			return fmt.Errorf("invalid auth_server_revocation_endpoint: %w", err)
		}
	}

	query := `
		INSERT INTO oauth_sessions (
			did, session_id, handle, pds_url, host_url,
			access_token, refresh_token,
			dpop_private_jwk, dpop_private_key_multibase,
			dpop_authserver_nonce, dpop_pds_nonce,
			auth_server_iss, auth_server_token_endpoint, auth_server_revocation_endpoint,
			scopes, expires_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			NULL, $8,
			$9, $10,
			$11, $12, $13,
			$14, $15, NOW(), NOW()
		)
		ON CONFLICT (did, session_id) DO UPDATE SET
			handle = EXCLUDED.handle,
			pds_url = EXCLUDED.pds_url,
			host_url = EXCLUDED.host_url,
			access_token = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			dpop_private_key_multibase = EXCLUDED.dpop_private_key_multibase,
			dpop_authserver_nonce = EXCLUDED.dpop_authserver_nonce,
			dpop_pds_nonce = EXCLUDED.dpop_pds_nonce,
			auth_server_iss = EXCLUDED.auth_server_iss,
			auth_server_token_endpoint = EXCLUDED.auth_server_token_endpoint,
			auth_server_revocation_endpoint = EXCLUDED.auth_server_revocation_endpoint,
			scopes = EXCLUDED.scopes,
			expires_at = EXCLUDED.expires_at,
			updated_at = NOW()
	`

	// Calculate token expiration using configured TTL
	expiresAt := time.Now().Add(s.sessionTTL)

	// Convert empty strings to NULL for optional fields
	var authServerRevocationEndpoint sql.NullString
	if sess.AuthServerRevocationEndpoint != "" {
		authServerRevocationEndpoint.String = sess.AuthServerRevocationEndpoint
		authServerRevocationEndpoint.Valid = true
	}

	// Extract handle from DID (placeholder - in real implementation, resolve from identity)
	// For now, use DID as handle since we don't have the handle in ClientSessionData
	handle := sess.AccountDID.String()

	// Use HostURL as PDS URL
	pdsURL := sess.HostURL
	if pdsURL == "" {
		pdsURL = sess.AuthServerURL // Fallback to auth server URL
	}

	_, err := s.db.ExecContext(
		ctx, query,
		sess.AccountDID.String(),
		sess.SessionID,
		handle,
		pdsURL,
		sess.HostURL,
		sess.AccessToken,
		sess.RefreshToken,
		sess.DPoPPrivateKeyMultibase,
		sess.DPoPAuthServerNonce,
		sess.DPoPHostNonce,
		sess.AuthServerURL,
		sess.AuthServerTokenEndpoint,
		authServerRevocationEndpoint,
		pq.Array(sess.Scopes),
		expiresAt,
	)
	if err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	return nil
}

// DeleteSession deletes a session by DID and session ID
func (s *PostgresOAuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	query := `DELETE FROM oauth_sessions WHERE did = $1 AND session_id = $2`

	result, err := s.db.ExecContext(ctx, query, did.String(), sessionID)
	if err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return ErrSessionNotFound
	}

	return nil
}

// GetAuthRequestInfo retrieves auth request information by state
func (s *PostgresOAuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauth.AuthRequestData, error) {
	query := `
		SELECT
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_key_multibase, dpop_authserver_nonce,
			auth_server_iss, request_uri,
			auth_server_token_endpoint, auth_server_revocation_endpoint,
			scopes, created_at
		FROM oauth_requests
		WHERE state = $1
	`

	var info oauth.AuthRequestData
	var did, handle, pdsURL sql.NullString
	var dpopPrivateKeyMultibase, dpopAuthServerNonce sql.NullString
	var requestURI, authServerTokenEndpoint, authServerRevocationEndpoint sql.NullString
	var scopes pq.StringArray
	var createdAt time.Time

	err := s.db.QueryRowContext(ctx, query, state).Scan(
		&info.State,
		&did,
		&handle,
		&pdsURL,
		&info.PKCEVerifier,
		&dpopPrivateKeyMultibase,
		&dpopAuthServerNonce,
		&info.AuthServerURL,
		&requestURI,
		&authServerTokenEndpoint,
		&authServerRevocationEndpoint,
		&scopes,
		&createdAt,
	)

	if err == sql.ErrNoRows {
		return nil, ErrAuthRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get auth request info: %w", err)
	}

	// Parse DID if present
	if did.Valid && did.String != "" {
		parsedDID, err := syntax.ParseDID(did.String)
		if err != nil {
			return nil, fmt.Errorf("failed to parse DID: %w", err)
		}
		info.AccountDID = &parsedDID
	}

	// Convert nullable fields
	if dpopPrivateKeyMultibase.Valid {
		info.DPoPPrivateKeyMultibase = dpopPrivateKeyMultibase.String
	}
	if dpopAuthServerNonce.Valid {
		info.DPoPAuthServerNonce = dpopAuthServerNonce.String
	}
	if requestURI.Valid {
		info.RequestURI = requestURI.String
	}
	if authServerTokenEndpoint.Valid {
		info.AuthServerTokenEndpoint = authServerTokenEndpoint.String
	}
	if authServerRevocationEndpoint.Valid {
		info.AuthServerRevocationEndpoint = authServerRevocationEndpoint.String
	}
	info.Scopes = scopes

	return &info, nil
}

// SaveAuthRequestInfo saves auth request information (create only, not upsert)
func (s *PostgresOAuthStore) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	query := `
		INSERT INTO oauth_requests (
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_key_multibase, dpop_authserver_nonce,
			auth_server_iss, request_uri,
			auth_server_token_endpoint, auth_server_revocation_endpoint,
			scopes, return_url, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8, $9,
			$10, $11,
			$12, NULL, NOW()
		)
	`

	// Extract DID string if present
	var didStr sql.NullString
	if info.AccountDID != nil {
		didStr.String = info.AccountDID.String()
		didStr.Valid = true
	}

	// Convert empty strings to NULL for optional fields
	var authServerRevocationEndpoint sql.NullString
	if info.AuthServerRevocationEndpoint != "" {
		authServerRevocationEndpoint.String = info.AuthServerRevocationEndpoint
		authServerRevocationEndpoint.Valid = true
	}

	// Placeholder values for handle and pds_url (not in AuthRequestData)
	// In production, these would be resolved during the auth flow
	handle := ""
	pdsURL := ""
	if info.AccountDID != nil {
		handle = info.AccountDID.String() // Temporary placeholder
		pdsURL = info.AuthServerURL       // Temporary placeholder
	}

	_, err := s.db.ExecContext(
		ctx, query,
		info.State,
		didStr,
		handle,
		pdsURL,
		info.PKCEVerifier,
		info.DPoPPrivateKeyMultibase,
		info.DPoPAuthServerNonce,
		info.AuthServerURL,
		info.RequestURI,
		info.AuthServerTokenEndpoint,
		authServerRevocationEndpoint,
		pq.Array(info.Scopes),
	)
	if err != nil {
		// Check for duplicate state
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "oauth_requests_state_key") {
			return fmt.Errorf("auth request with state already exists: %s", info.State)
		}
		return fmt.Errorf("failed to save auth request info: %w", err)
	}

	return nil
}

// DeleteAuthRequestInfo deletes auth request information by state
func (s *PostgresOAuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	query := `DELETE FROM oauth_requests WHERE state = $1`

	result, err := s.db.ExecContext(ctx, query, state)
	if err != nil {
		return fmt.Errorf("failed to delete auth request info: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return ErrAuthRequestNotFound
	}

	return nil
}

// CleanupExpiredSessions removes sessions that have expired
// Should be called periodically (e.g., via cron job)
func (s *PostgresOAuthStore) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	query := `DELETE FROM oauth_sessions WHERE expires_at < NOW()`

	result, err := s.db.ExecContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup expired sessions: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rows, nil
}

// CleanupExpiredAuthRequests removes auth requests older than 30 minutes
// Should be called periodically (e.g., via cron job)
func (s *PostgresOAuthStore) CleanupExpiredAuthRequests(ctx context.Context) (int64, error) {
	query := `DELETE FROM oauth_requests WHERE created_at < NOW() - INTERVAL '30 minutes'`

	result, err := s.db.ExecContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup expired auth requests: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rows, nil
}

// UpdateHandleByDID updates the handle for all OAuth sessions belonging to a DID.
// This is called when identity events indicate a handle change, keeping active
// sessions in sync with the user's current handle.
// Returns the number of sessions updated.
func (s *PostgresOAuthStore) UpdateHandleByDID(ctx context.Context, did, newHandle string) (int64, error) {
	query := `
		UPDATE oauth_sessions
		SET handle = $2, updated_at = NOW()
		WHERE did = $1 AND expires_at > NOW()
	`

	result, err := s.db.ExecContext(ctx, query, did, newHandle)
	if err != nil {
		return 0, fmt.Errorf("failed to update session handle: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows > 0 {
		slog.Info("updated OAuth session handles for identity change",
			"did", did, "new_handle", newHandle, "sessions_updated", rows)
	}

	return rows, nil
}

// MobileOAuthData holds mobile-specific OAuth flow data
type MobileOAuthData struct {
	CSRFToken   string
	RedirectURI string
}

// mobileFlowContextKey is the context key for mobile flow data
type mobileFlowContextKey struct{}

// ContextWithMobileFlowData adds mobile flow data to a context.
// This is used by HandleMobileLogin to pass mobile data to the store wrapper,
// which will save it when SaveAuthRequestInfo is called by indigo.
func ContextWithMobileFlowData(ctx context.Context, data MobileOAuthData) context.Context {
	return context.WithValue(ctx, mobileFlowContextKey{}, data)
}

// getMobileFlowDataFromContext retrieves mobile flow data from context, if present
func getMobileFlowDataFromContext(ctx context.Context) (MobileOAuthData, bool) {
	data, ok := ctx.Value(mobileFlowContextKey{}).(MobileOAuthData)
	return data, ok
}

// MobileAwareClientStore is a marker interface that indicates a store is properly
// configured for mobile OAuth flows. Only stores that intercept SaveAuthRequestInfo
// to save mobile CSRF data should implement this interface.
// This prevents silent mobile OAuth breakage when a plain PostgresOAuthStore is used.
type MobileAwareClientStore interface {
	IsMobileAware() bool
}

// MobileAwareStoreWrapper wraps a ClientAuthStore to automatically save mobile
// CSRF data when SaveAuthRequestInfo is called during a mobile OAuth flow.
// This is necessary because indigo's StartAuthFlow doesn't expose the OAuth state,
// so we intercept the SaveAuthRequestInfo call to capture it.
type MobileAwareStoreWrapper struct {
	oauth.ClientAuthStore
	mobileStore MobileOAuthStore
}

// IsMobileAware implements MobileAwareClientStore, indicating this store
// properly saves mobile CSRF data during OAuth flow initiation.
func (w *MobileAwareStoreWrapper) IsMobileAware() bool {
	return true
}

// NewMobileAwareStoreWrapper creates a wrapper that intercepts SaveAuthRequestInfo
// to also save mobile CSRF data when present in context.
func NewMobileAwareStoreWrapper(store oauth.ClientAuthStore) *MobileAwareStoreWrapper {
	wrapper := &MobileAwareStoreWrapper{
		ClientAuthStore: store,
	}
	// Check if the underlying store implements MobileOAuthStore
	if ms, ok := store.(MobileOAuthStore); ok {
		wrapper.mobileStore = ms
	}
	return wrapper
}

// SaveAuthRequestInfo saves the auth request and also saves mobile CSRF data
// if mobile flow data is present in the context.
func (w *MobileAwareStoreWrapper) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	// First, save the auth request to the underlying store
	if err := w.ClientAuthStore.SaveAuthRequestInfo(ctx, info); err != nil {
		return err
	}

	// Check if this is a mobile flow (mobile data in context)
	if mobileData, ok := getMobileFlowDataFromContext(ctx); ok && w.mobileStore != nil {
		// Save mobile CSRF data tied to this OAuth state
		// IMPORTANT: If this fails, we MUST propagate the error. Otherwise:
		// 1. No server-side CSRF record is stored
		// 2. Every mobile callback will "fail closed" to web flow
		// 3. Mobile sign-in silently breaks with no indication
		// Failing loudly here lets the user retry rather than being confused
		// about why they're getting a web flow instead of mobile.
		if err := w.mobileStore.SaveMobileOAuthData(ctx, info.State, mobileData); err != nil {
			slog.Error("failed to save mobile CSRF data - mobile login will fail",
				"state", info.State, "error", err)
			return fmt.Errorf("failed to save mobile OAuth data: %w", err)
		}
	}

	return nil
}

// GetMobileOAuthData implements MobileOAuthStore interface by delegating to underlying store
func (w *MobileAwareStoreWrapper) GetMobileOAuthData(ctx context.Context, state string) (*MobileOAuthData, error) {
	if w.mobileStore != nil {
		return w.mobileStore.GetMobileOAuthData(ctx, state)
	}
	return nil, nil
}

// SaveMobileOAuthData implements MobileOAuthStore interface by delegating to underlying store
func (w *MobileAwareStoreWrapper) SaveMobileOAuthData(ctx context.Context, state string, data MobileOAuthData) error {
	if w.mobileStore != nil {
		return w.mobileStore.SaveMobileOAuthData(ctx, state, data)
	}
	return nil
}

// UnwrapPostgresStore returns the underlying PostgresOAuthStore if present.
// This is useful for accessing cleanup methods that aren't part of the interface.
func (w *MobileAwareStoreWrapper) UnwrapPostgresStore() *PostgresOAuthStore {
	if ps, ok := w.ClientAuthStore.(*PostgresOAuthStore); ok {
		return ps
	}
	return nil
}

// SaveMobileOAuthData stores mobile CSRF data tied to an OAuth state
// This ties the CSRF token to the OAuth flow via the state parameter,
// which comes back through the OAuth response for server-side validation.
func (s *PostgresOAuthStore) SaveMobileOAuthData(ctx context.Context, state string, data MobileOAuthData) error {
	query := `
		UPDATE oauth_requests
		SET mobile_csrf_token = $2, mobile_redirect_uri = $3
		WHERE state = $1
	`

	result, err := s.db.ExecContext(ctx, query, state, data.CSRFToken, data.RedirectURI)
	if err != nil {
		return fmt.Errorf("failed to save mobile OAuth data: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return ErrAuthRequestNotFound
	}

	return nil
}

// GetMobileOAuthData retrieves mobile CSRF data by OAuth state
// This is called during callback to compare the server-side CSRF token
// (retrieved by state from the OAuth response) against the cookie CSRF.
func (s *PostgresOAuthStore) GetMobileOAuthData(ctx context.Context, state string) (*MobileOAuthData, error) {
	query := `
		SELECT mobile_csrf_token, mobile_redirect_uri
		FROM oauth_requests
		WHERE state = $1
	`

	var csrfToken, redirectURI sql.NullString
	err := s.db.QueryRowContext(ctx, query, state).Scan(&csrfToken, &redirectURI)

	if err == sql.ErrNoRows {
		return nil, ErrAuthRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get mobile OAuth data: %w", err)
	}

	// Return nil if no mobile data was stored (this was a web flow)
	if !csrfToken.Valid {
		return nil, nil
	}

	return &MobileOAuthData{
		CSRFToken:   csrfToken.String,
		RedirectURI: redirectURI.String,
	}, nil
}
