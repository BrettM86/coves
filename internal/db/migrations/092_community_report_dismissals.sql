-- +goose Up
-- A community moderator's dismissal of one report queue entry: the
-- idempotency record and the audit record of the dismissal. One row per
-- (moderator, community, key); a replay of the key returns dismissed_count.
-- Account erasure deletes the rows naming the dismisser.
CREATE TABLE community_report_dismissals (
    id BIGSERIAL PRIMARY KEY,
    community_did TEXT NOT NULL,
    subject_uri TEXT NOT NULL,
    dismissed_by_did TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    dismissed_count INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT community_report_dismissals_key_length CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    CONSTRAINT community_report_dismissals_count_nonnegative CHECK (dismissed_count >= 0),
    CONSTRAINT community_report_dismissals_idempotency UNIQUE (dismissed_by_did, community_did, idempotency_key)
);

CREATE INDEX idx_community_report_dismissals_community ON community_report_dismissals (community_did);

-- +goose Down
DROP TABLE IF EXISTS community_report_dismissals;
