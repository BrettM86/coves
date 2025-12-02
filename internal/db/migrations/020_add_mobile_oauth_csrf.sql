-- +goose Up
-- Add columns for mobile OAuth CSRF protection with server-side state
-- This ties the CSRF token to the OAuth state, allowing validation against
-- a value that comes back through the OAuth response (the state parameter)
-- rather than only validating cookies against each other.

ALTER TABLE oauth_requests
    ADD COLUMN mobile_csrf_token TEXT,
    ADD COLUMN mobile_redirect_uri TEXT;

-- Index for quick lookup of mobile data when callback is received
CREATE INDEX idx_oauth_requests_mobile_csrf ON oauth_requests(state)
    WHERE mobile_csrf_token IS NOT NULL;

COMMENT ON COLUMN oauth_requests.mobile_csrf_token IS 'CSRF token for mobile OAuth flows, validated against cookie on callback';
COMMENT ON COLUMN oauth_requests.mobile_redirect_uri IS 'Mobile redirect URI (Universal Link) for this OAuth flow';

-- +goose Down
DROP INDEX IF EXISTS idx_oauth_requests_mobile_csrf;

ALTER TABLE oauth_requests
    DROP COLUMN IF EXISTS mobile_redirect_uri,
    DROP COLUMN IF EXISTS mobile_csrf_token;
