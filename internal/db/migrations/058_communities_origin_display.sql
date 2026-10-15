-- +goose Up
ALTER TABLE communities
    ADD COLUMN origin_url TEXT,
    ADD COLUMN origin_software TEXT;

-- +goose Down
ALTER TABLE communities
    DROP COLUMN origin_url,
    DROP COLUMN origin_software;
