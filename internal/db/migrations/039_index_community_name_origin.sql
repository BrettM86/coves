-- +goose Up
-- name@origin is a first-class community identifier (/c/gaming resolves as
-- (gaming, <local instance>); /c/comicstrips@lemmy.world as
-- (comicstrips, lemmy.world)). Native rows still take the handle fast path;
-- this index serves the remote-origin lookup, which compares the name
-- case-insensitively (records may spell it ComicStrips; identifiers are
-- lower-cased). It is NOT unique: both name and origin are copied from the
-- record and neither is constrained by the unique handle, so two admitted
-- rows can carry the same pair, which the resolver reports as ambiguous
-- rather than picking one.
CREATE INDEX idx_communities_name_origin ON communities (lower(name), origin) WHERE origin IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_communities_name_origin;
