-- +goose Up
-- Index of social.coves.moderation.ban records. A community bans an account by
-- writing the record in its own repo at a record key derived from the banned
-- DID, so there is at most one row per (community, subject). A delete commit
-- carries no record, so the unban is resolved by (community DID, record key).
CREATE TABLE community_bans (
    community_did TEXT NOT NULL,
    subject_did TEXT NOT NULL,
    record_key TEXT NOT NULL,
    record_cid TEXT NOT NULL,
    reason TEXT,
    expires_at TIMESTAMPTZ,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, subject_did),
    CONSTRAINT community_bans_record_key UNIQUE (community_did, record_key)
);

-- Bans are read from community_bans only. No code ever set this column true.
ALTER TABLE community_memberships DROP COLUMN is_banned;

-- +goose Down
ALTER TABLE community_memberships ADD COLUMN is_banned BOOLEAN DEFAULT false;
DROP TABLE IF EXISTS community_bans;
