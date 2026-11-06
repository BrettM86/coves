-- +goose Up
-- When the user answered the automatic Bluesky profile import offer, or ran
-- any import, including one from settings. NULL means the offer may still be
-- shown. Private UI state: no profile view exposes it.
ALTER TABLE users ADD COLUMN bluesky_profile_import_dismissed_at TIMESTAMPTZ NULL;

-- +goose Down
ALTER TABLE users DROP COLUMN bluesky_profile_import_dismissed_at;
