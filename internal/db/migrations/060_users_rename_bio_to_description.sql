-- +goose Up
-- The profile record, the getProfile view and the updateProfile input all name
-- this field `description` (as app.bsky.actor.profile does); the column follows.
--
-- The CHECKs move from code points to the lexicon's byte caps (displayName
-- 640, description 2560). The lexicon's grapheme caps cannot be expressed in
-- Postgres, and a code-point cap rejected lexicon-valid text made of
-- multi-code-point graphemes (flags, ZWJ emoji).
--
-- Binaries before this migration query `bio`: stop the old AppView before Up,
-- and run Down before rolling the binary back.
ALTER TABLE users RENAME COLUMN bio TO description;
ALTER TABLE users DROP CONSTRAINT users_bio_check;
ALTER TABLE users ADD CONSTRAINT users_description_check
    CHECK (description IS NULL OR octet_length(description) <= 2560);
ALTER TABLE users DROP CONSTRAINT users_display_name_check;
ALTER TABLE users ADD CONSTRAINT users_display_name_check
    CHECK (display_name IS NULL OR octet_length(display_name) <= 640);

-- +goose Down
-- The old code-point CHECKs are narrower, so values only the byte caps allowed
-- are cut to fit before the CHECKs are restored.
ALTER TABLE users DROP CONSTRAINT users_display_name_check;
UPDATE users SET display_name = left(display_name, 64) WHERE length(display_name) > 64;
ALTER TABLE users ADD CONSTRAINT users_display_name_check
    CHECK (display_name IS NULL OR length(display_name) <= 64);
ALTER TABLE users DROP CONSTRAINT users_description_check;
UPDATE users SET description = left(description, 256) WHERE length(description) > 256;
ALTER TABLE users RENAME COLUMN description TO bio;
ALTER TABLE users ADD CONSTRAINT users_bio_check
    CHECK (bio IS NULL OR length(bio) <= 256);
