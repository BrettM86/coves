-- +goose Up
-- name@origin is a first-class community identifier (/c/gaming resolves as
-- (gaming, <local instance>); /c/comicstrips@lemmy.world as
-- (comicstrips, lemmy.world)). Native rows still take the handle fast path;
-- this index serves the remote-origin lookup. It is NOT unique: origin is
-- self-asserted and a bridge can index two rows with the same pair, which the
-- resolver reports as ambiguous rather than picking one.
CREATE INDEX idx_communities_name_origin ON communities (name, origin) WHERE origin IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_communities_name_origin;
