package oauth

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestDB creates a test database connection and runs migrations
func setupTestDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://test_user:test_password@localhost:5434/coves_test?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err, "Failed to connect to test database")

	// Run migrations
	require.NoError(t, goose.Up(db, "../../db/migrations"), "Failed to run migrations")

	return db
}

// cleanupOAuth removes all test OAuth data from the database
func cleanupOAuth(t *testing.T, db *sql.DB) {
	_, err := db.Exec("DELETE FROM oauth_sessions WHERE did LIKE 'did:plc:test%'")
	require.NoError(t, err, "Failed to cleanup oauth_sessions")

	_, err = db.Exec("DELETE FROM oauth_requests WHERE state LIKE 'test%'")
	require.NoError(t, err, "Failed to cleanup oauth_requests")
}

func TestPostgresOAuthStore_SaveAndGetSession(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:test123abc")
	require.NoError(t, err)

	session := oauth.ClientSessionData{
		AccountDID:                   did,
		SessionID:                    "session123",
		HostURL:                      "https://pds.example.com",
		AuthServerURL:                "https://auth.example.com",
		AuthServerTokenEndpoint:      "https://auth.example.com/oauth/token",
		AuthServerRevocationEndpoint: "https://auth.example.com/oauth/revoke",
		Scopes:                       []string{"atproto"},
		AccessToken:                  "at_test_token_abc123",
		RefreshToken:                 "rt_test_token_xyz789",
		DPoPAuthServerNonce:          "nonce_auth_123",
		DPoPHostNonce:                "nonce_host_456",
		DPoPPrivateKeyMultibase:      "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	// Save session
	err = store.SaveSession(ctx, session)
	assert.NoError(t, err)

	// Retrieve session
	retrieved, err := store.GetSession(ctx, did, "session123")
	assert.NoError(t, err)
	assert.NotNil(t, retrieved)
	assert.Equal(t, session.AccountDID.String(), retrieved.AccountDID.String())
	assert.Equal(t, session.SessionID, retrieved.SessionID)
	assert.Equal(t, session.HostURL, retrieved.HostURL)
	assert.Equal(t, session.AuthServerURL, retrieved.AuthServerURL)
	assert.Equal(t, session.AuthServerTokenEndpoint, retrieved.AuthServerTokenEndpoint)
	assert.Equal(t, session.AccessToken, retrieved.AccessToken)
	assert.Equal(t, session.RefreshToken, retrieved.RefreshToken)
	assert.Equal(t, session.DPoPAuthServerNonce, retrieved.DPoPAuthServerNonce)
	assert.Equal(t, session.DPoPHostNonce, retrieved.DPoPHostNonce)
	assert.Equal(t, session.DPoPPrivateKeyMultibase, retrieved.DPoPPrivateKeyMultibase)
	assert.Equal(t, session.Scopes, retrieved.Scopes)
}

func TestPostgresOAuthStore_SaveSession_Upsert(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:testupsert")
	require.NoError(t, err)

	// Initial session
	session1 := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "session_upsert",
		HostURL:                 "https://pds1.example.com",
		AuthServerURL:           "https://auth1.example.com",
		AuthServerTokenEndpoint: "https://auth1.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "old_access_token",
		RefreshToken:            "old_refresh_token",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	err = store.SaveSession(ctx, session1)
	require.NoError(t, err)

	// Updated session (same DID and session ID)
	session2 := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "session_upsert",
		HostURL:                 "https://pds2.example.com",
		AuthServerURL:           "https://auth2.example.com",
		AuthServerTokenEndpoint: "https://auth2.example.com/oauth/token",
		Scopes:                  []string{"atproto", "transition:generic"},
		AccessToken:             "new_access_token",
		RefreshToken:            "new_refresh_token",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktX",
	}

	// Save again - should update
	err = store.SaveSession(ctx, session2)
	assert.NoError(t, err)

	// Retrieve should get updated values
	retrieved, err := store.GetSession(ctx, did, "session_upsert")
	assert.NoError(t, err)
	assert.Equal(t, "new_access_token", retrieved.AccessToken)
	assert.Equal(t, "new_refresh_token", retrieved.RefreshToken)
	assert.Equal(t, "https://pds2.example.com", retrieved.HostURL)
	assert.Equal(t, []string{"atproto", "transition:generic"}, retrieved.Scopes)
}

func TestPostgresOAuthStore_GetSession_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:nonexistent")
	require.NoError(t, err)

	_, err = store.GetSession(ctx, did, "nonexistent_session")
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestPostgresOAuthStore_DeleteSession(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:testdelete")
	require.NoError(t, err)

	session := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "session_delete",
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "test_token",
		RefreshToken:            "test_refresh",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	// Save session
	err = store.SaveSession(ctx, session)
	require.NoError(t, err)

	// Delete session
	err = store.DeleteSession(ctx, did, "session_delete")
	assert.NoError(t, err)

	// Verify session is gone
	_, err = store.GetSession(ctx, did, "session_delete")
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestPostgresOAuthStore_DeleteSession_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:nonexistent")
	require.NoError(t, err)

	err = store.DeleteSession(ctx, did, "nonexistent_session")
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestPostgresOAuthStore_SaveAndGetAuthRequestInfo(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:testrequest")
	require.NoError(t, err)

	info := oauth.AuthRequestData{
		State:                        "test_state_abc123",
		AuthServerURL:                "https://auth.example.com",
		AccountDID:                   &did,
		Scopes:                       []string{"atproto"},
		RequestURI:                   "urn:ietf:params:oauth:request_uri:abc123",
		AuthServerTokenEndpoint:      "https://auth.example.com/oauth/token",
		AuthServerRevocationEndpoint: "https://auth.example.com/oauth/revoke",
		PKCEVerifier:                 "verifier_xyz789",
		DPoPAuthServerNonce:          "nonce_abc",
		DPoPPrivateKeyMultibase:      "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	// Save auth request info
	err = store.SaveAuthRequestInfo(ctx, info)
	assert.NoError(t, err)

	// Retrieve auth request info
	retrieved, err := store.GetAuthRequestInfo(ctx, "test_state_abc123")
	assert.NoError(t, err)
	assert.NotNil(t, retrieved)
	assert.Equal(t, info.State, retrieved.State)
	assert.Equal(t, info.AuthServerURL, retrieved.AuthServerURL)
	assert.NotNil(t, retrieved.AccountDID)
	assert.Equal(t, info.AccountDID.String(), retrieved.AccountDID.String())
	assert.Equal(t, info.Scopes, retrieved.Scopes)
	assert.Equal(t, info.RequestURI, retrieved.RequestURI)
	assert.Equal(t, info.AuthServerTokenEndpoint, retrieved.AuthServerTokenEndpoint)
	assert.Equal(t, info.PKCEVerifier, retrieved.PKCEVerifier)
	assert.Equal(t, info.DPoPAuthServerNonce, retrieved.DPoPAuthServerNonce)
	assert.Equal(t, info.DPoPPrivateKeyMultibase, retrieved.DPoPPrivateKeyMultibase)
}

func TestPostgresOAuthStore_SaveAuthRequestInfo_NoDID(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	info := oauth.AuthRequestData{
		State:                   "test_state_nodid",
		AuthServerURL:           "https://auth.example.com",
		AccountDID:              nil, // No DID provided
		Scopes:                  []string{"atproto"},
		RequestURI:              "urn:ietf:params:oauth:request_uri:nodid",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		PKCEVerifier:            "verifier_nodid",
		DPoPAuthServerNonce:     "nonce_nodid",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	// Save auth request info without DID
	err := store.SaveAuthRequestInfo(ctx, info)
	assert.NoError(t, err)

	// Retrieve and verify DID is nil
	retrieved, err := store.GetAuthRequestInfo(ctx, "test_state_nodid")
	assert.NoError(t, err)
	assert.Nil(t, retrieved.AccountDID)
	assert.Equal(t, info.State, retrieved.State)
}

func TestPostgresOAuthStore_GetAuthRequestInfo_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	_, err := store.GetAuthRequestInfo(ctx, "nonexistent_state")
	assert.ErrorIs(t, err, ErrAuthRequestNotFound)
}

func TestPostgresOAuthStore_DeleteAuthRequestInfo(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	info := oauth.AuthRequestData{
		State:                   "test_state_delete",
		AuthServerURL:           "https://auth.example.com",
		Scopes:                  []string{"atproto"},
		RequestURI:              "urn:ietf:params:oauth:request_uri:delete",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		PKCEVerifier:            "verifier_delete",
		DPoPAuthServerNonce:     "nonce_delete",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	// Save auth request info
	err := store.SaveAuthRequestInfo(ctx, info)
	require.NoError(t, err)

	// Delete auth request info
	err = store.DeleteAuthRequestInfo(ctx, "test_state_delete")
	assert.NoError(t, err)

	// Verify it's gone
	_, err = store.GetAuthRequestInfo(ctx, "test_state_delete")
	assert.ErrorIs(t, err, ErrAuthRequestNotFound)
}

func TestPostgresOAuthStore_DeleteAuthRequestInfo_NotFound(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	err := store.DeleteAuthRequestInfo(ctx, "nonexistent_state")
	assert.ErrorIs(t, err, ErrAuthRequestNotFound)
}

func TestPostgresOAuthStore_CleanupExpiredSessions(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	storeInterface := NewPostgresOAuthStore(db, 0) // Use default TTL
	store, ok := storeInterface.(*PostgresOAuthStore)
	require.True(t, ok, "store should be *PostgresOAuthStore")
	ctx := context.Background()

	did1, err := syntax.ParseDID("did:plc:testexpired1")
	require.NoError(t, err)
	did2, err := syntax.ParseDID("did:plc:testexpired2")
	require.NoError(t, err)

	// Create an expired session (manually insert with past expiration)
	_, err = db.ExecContext(ctx, `
		INSERT INTO oauth_sessions (
			did, session_id, handle, pds_url, host_url,
			access_token, refresh_token,
			dpop_private_key_multibase, auth_server_iss,
			auth_server_token_endpoint, scopes,
			expires_at, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8, $9,
			$10, $11,
			NOW() - INTERVAL '1 day', NOW()
		)
	`, did1.String(), "expired_session", "test.handle", "https://pds.example.com", "https://pds.example.com",
		"expired_token", "expired_refresh",
		"z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH", "https://auth.example.com",
		"https://auth.example.com/oauth/token", `{"atproto"}`)
	require.NoError(t, err)

	// Create a valid session
	validSession := oauth.ClientSessionData{
		AccountDID:              did2,
		SessionID:               "valid_session",
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "valid_token",
		RefreshToken:            "valid_refresh",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}
	err = store.SaveSession(ctx, validSession)
	require.NoError(t, err)

	// Cleanup expired sessions
	count, err := store.CleanupExpiredSessions(ctx)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), count, "Should delete 1 expired session")

	// Verify expired session is gone
	_, err = store.GetSession(ctx, did1, "expired_session")
	assert.ErrorIs(t, err, ErrSessionNotFound)

	// Verify valid session still exists
	_, err = store.GetSession(ctx, did2, "valid_session")
	assert.NoError(t, err)
}

func TestPostgresOAuthStore_CleanupExpiredAuthRequests(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	storeInterface := NewPostgresOAuthStore(db, 0)
	pgStore, ok := storeInterface.(*PostgresOAuthStore)
	require.True(t, ok, "store should be *PostgresOAuthStore")
	store := oauth.ClientAuthStore(pgStore)
	ctx := context.Background()

	// Create an old auth request (manually insert with old timestamp)
	_, err := db.ExecContext(ctx, `
		INSERT INTO oauth_requests (
			state, did, handle, pds_url, pkce_verifier,
			dpop_private_key_multibase, dpop_authserver_nonce,
			auth_server_iss, request_uri,
			auth_server_token_endpoint, scopes,
			created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8, $9,
			$10, $11,
			NOW() - INTERVAL '1 hour'
		)
	`, "test_old_state", "did:plc:testold", "test.handle", "https://pds.example.com",
		"old_verifier", "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
		"nonce_old", "https://auth.example.com", "urn:ietf:params:oauth:request_uri:old",
		"https://auth.example.com/oauth/token", `{"atproto"}`)
	require.NoError(t, err)

	// Create a recent auth request
	recentInfo := oauth.AuthRequestData{
		State:                   "test_recent_state",
		AuthServerURL:           "https://auth.example.com",
		Scopes:                  []string{"atproto"},
		RequestURI:              "urn:ietf:params:oauth:request_uri:recent",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		PKCEVerifier:            "recent_verifier",
		DPoPAuthServerNonce:     "nonce_recent",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}
	err = store.SaveAuthRequestInfo(ctx, recentInfo)
	require.NoError(t, err)

	// Cleanup expired auth requests (older than 30 minutes)
	count, err := pgStore.CleanupExpiredAuthRequests(ctx)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), count, "Should delete 1 expired auth request")

	// Verify old request is gone
	_, err = store.GetAuthRequestInfo(ctx, "test_old_state")
	assert.ErrorIs(t, err, ErrAuthRequestNotFound)

	// Verify recent request still exists
	_, err = store.GetAuthRequestInfo(ctx, "test_recent_state")
	assert.NoError(t, err)
}

func TestPostgresOAuthStore_MultipleSessions(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupOAuth(t, db)

	store := NewPostgresOAuthStore(db, 0) // Use default TTL
	ctx := context.Background()

	did, err := syntax.ParseDID("did:plc:testmulti")
	require.NoError(t, err)

	// Create multiple sessions for the same DID
	session1 := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "browser1",
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "token_browser1",
		RefreshToken:            "refresh_browser1",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH",
	}

	session2 := oauth.ClientSessionData{
		AccountDID:              did,
		SessionID:               "mobile_app",
		HostURL:                 "https://pds.example.com",
		AuthServerURL:           "https://auth.example.com",
		AuthServerTokenEndpoint: "https://auth.example.com/oauth/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             "token_mobile",
		RefreshToken:            "refresh_mobile",
		DPoPPrivateKeyMultibase: "z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktX",
	}

	// Save both sessions
	err = store.SaveSession(ctx, session1)
	require.NoError(t, err)
	err = store.SaveSession(ctx, session2)
	require.NoError(t, err)

	// Retrieve both sessions
	retrieved1, err := store.GetSession(ctx, did, "browser1")
	assert.NoError(t, err)
	assert.Equal(t, "token_browser1", retrieved1.AccessToken)

	retrieved2, err := store.GetSession(ctx, did, "mobile_app")
	assert.NoError(t, err)
	assert.Equal(t, "token_mobile", retrieved2.AccessToken)

	// Delete one session
	err = store.DeleteSession(ctx, did, "browser1")
	assert.NoError(t, err)

	// Verify only browser1 is deleted
	_, err = store.GetSession(ctx, did, "browser1")
	assert.ErrorIs(t, err, ErrSessionNotFound)

	// mobile_app should still exist
	_, err = store.GetSession(ctx, did, "mobile_app")
	assert.NoError(t, err)
}
