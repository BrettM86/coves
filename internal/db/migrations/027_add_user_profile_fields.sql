-- +goose Up
ALTER TABLE users ADD COLUMN display_name TEXT CHECK (display_name IS NULL OR length(display_name) <= 64);
ALTER TABLE users ADD COLUMN bio TEXT CHECK (bio IS NULL OR length(bio) <= 256);
ALTER TABLE users ADD COLUMN avatar_cid TEXT;
ALTER TABLE users ADD COLUMN banner_cid TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS banner_cid;
ALTER TABLE users DROP COLUMN IF EXISTS avatar_cid;
ALTER TABLE users DROP COLUMN IF EXISTS bio;
ALTER TABLE users DROP COLUMN IF EXISTS display_name;
