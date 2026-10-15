-- +goose Up
-- A bridged user is addressable by the fediverse address it was bridged from
-- (Alice_B@lemmy.world), resolved against the stored (origin_username, origin)
-- pair. The lookup matches the username exactly first and falls back to a
-- case-insensitive match, so it needs both an exact and a lower() index. They
-- are NOT unique: the pair is copied from the bridge and not constrained by
-- the unique handle, so two rows can carry it, which the resolver reports as
-- ambiguous rather than picking one.
CREATE INDEX idx_users_origin_username_origin ON users (origin_username, origin) WHERE origin IS NOT NULL;
CREATE INDEX idx_users_lower_origin_username_origin ON users (lower(origin_username), origin) WHERE origin IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_users_lower_origin_username_origin;
DROP INDEX IF EXISTS idx_users_origin_username_origin;
