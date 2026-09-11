-- +goose Up
ALTER TABLE oauth_requests
 ADD COLUMN web_browser_nonce TEXT,
 ADD COLUMN web_expires_at TIMESTAMPTZ,
 ADD COLUMN web_claimed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE oauth_requests
 DROP COLUMN web_claimed_at,
 DROP COLUMN web_expires_at,
 DROP COLUMN web_browser_nonce;
