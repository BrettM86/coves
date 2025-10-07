-- +goose Up
-- Create OAuth tables for managing OAuth flow state and user sessions

-- Temporary state during OAuth authorization flow (30 min TTL)
-- This stores the intermediate state between redirect to auth server and callback
CREATE TABLE oauth_requests (
    id SERIAL PRIMARY KEY,
    state TEXT UNIQUE NOT NULL,                     -- OAuth state parameter (CSRF protection)
    did TEXT NOT NULL,                              -- User's DID (resolved from handle)
    handle TEXT NOT NULL,                           -- User's handle (e.g., alice.bsky.social)
    pds_url TEXT NOT NULL,                          -- User's PDS URL
    pkce_verifier TEXT NOT NULL,                    -- PKCE code verifier
    dpop_private_jwk JSONB NOT NULL,                -- DPoP private key (ES256) for this session
    dpop_authserver_nonce TEXT,                     -- DPoP nonce from authorization server
    auth_server_iss TEXT NOT NULL,                  -- Authorization server issuer
    return_url TEXT,                                -- Optional return URL after login
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Long-lived user sessions (7 day TTL, auto-refreshed)
-- This stores authenticated user sessions with OAuth tokens
CREATE TABLE oauth_sessions (
    id SERIAL PRIMARY KEY,
    did TEXT UNIQUE NOT NULL,                       -- User's DID (primary identifier)
    handle TEXT NOT NULL,                           -- User's handle (can change)
    pds_url TEXT NOT NULL,                          -- User's PDS URL
    access_token TEXT NOT NULL,                     -- OAuth access token (DPoP-bound)
    refresh_token TEXT NOT NULL,                    -- OAuth refresh token
    dpop_private_jwk JSONB NOT NULL,                -- DPoP private key for this session
    dpop_authserver_nonce TEXT,                     -- DPoP nonce for auth server token endpoint
    dpop_pds_nonce TEXT,                            -- DPoP nonce for PDS requests
    auth_server_iss TEXT NOT NULL,                  -- Authorization server issuer
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,   -- Token expiration time
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Indexes for efficient lookups
CREATE INDEX idx_oauth_requests_state ON oauth_requests(state);
CREATE INDEX idx_oauth_requests_created_at ON oauth_requests(created_at);
CREATE INDEX idx_oauth_sessions_did ON oauth_sessions(did);
CREATE INDEX idx_oauth_sessions_expires_at ON oauth_sessions(expires_at);

-- Function to update updated_at timestamp
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION update_oauth_session_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Trigger to automatically update updated_at
CREATE TRIGGER oauth_sessions_updated_at
    BEFORE UPDATE ON oauth_sessions
    FOR EACH ROW
    EXECUTE FUNCTION update_oauth_session_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS oauth_sessions_updated_at ON oauth_sessions;
DROP FUNCTION IF EXISTS update_oauth_session_updated_at();
DROP INDEX IF EXISTS idx_oauth_sessions_expires_at;
DROP INDEX IF EXISTS idx_oauth_sessions_did;
DROP INDEX IF EXISTS idx_oauth_requests_created_at;
DROP INDEX IF EXISTS idx_oauth_requests_state;
DROP TABLE IF EXISTS oauth_sessions;
DROP TABLE IF EXISTS oauth_requests;
