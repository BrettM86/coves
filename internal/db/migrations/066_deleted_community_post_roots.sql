-- +goose Up
-- Roots must outlive the community's post cascade: a missing post alone is
-- also ordinary out-of-order indexing and cannot identify deleted content.
CREATE TABLE deleted_community_post_roots (
    root_uri TEXT PRIMARY KEY,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE deleted_community_post_roots;
