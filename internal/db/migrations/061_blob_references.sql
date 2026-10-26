-- +goose Up
CREATE TABLE blob_references (
    owner_did TEXT NOT NULL,
    blob_cid TEXT NOT NULL,
    subject_uri TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('post_image', 'comment_image', 'external_thumb', 'video_thumb', 'avatar', 'banner')),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (owner_did, blob_cid, subject_uri, kind)
);

CREATE INDEX idx_blob_references_blob_cid ON blob_references (blob_cid);
CREATE INDEX idx_blob_references_subject_uri ON blob_references (subject_uri);

-- +goose Down
DROP TABLE blob_references;
