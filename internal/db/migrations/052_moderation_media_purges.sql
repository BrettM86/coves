-- +goose Up
CREATE TABLE moderation_media_purges (
    owner_did TEXT NOT NULL,
    blob_cid TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    earliest_completion_at TIMESTAMPTZ,
    last_failure_code TEXT,
    generation BIGINT NOT NULL DEFAULT 0,
    claim BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_did, blob_cid)
);

CREATE INDEX moderation_media_purges_due ON moderation_media_purges (next_attempt_at)
    WHERE state = 'pending';

-- +goose Down
DROP TABLE IF EXISTS moderation_media_purges;
