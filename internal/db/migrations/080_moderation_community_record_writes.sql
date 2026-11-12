-- +goose Up
-- One row per (community, subject): the community records an instance removal
-- or restore is owed in a community this AppView hosts. Every desired change
-- bumps generation and makes the row pending and due now; an attempt holds the
-- row by its claim number until lease_expires_at.
CREATE TABLE moderation_community_record_writes (
    community_did TEXT NOT NULL CHECK (community_did <> ''),
    subject_uri TEXT NOT NULL CHECK (subject_uri <> ''),
    subject_collection TEXT NOT NULL,
    subject_cid TEXT NOT NULL CHECK (subject_cid <> ''),
    removal_action_id TEXT NOT NULL REFERENCES moderation_actions (id),
    code TEXT NOT NULL CHECK (code <> ''),
    removal_created_at TIMESTAMPTZ NOT NULL,
    desired TEXT NOT NULL CHECK (desired IN ('removed', 'restored')),
    restore_acceptance_cid TEXT,
    owned_removal_cid TEXT,
    acceptance_seen BOOLEAN NOT NULL DEFAULT FALSE,
    withdrew_acceptance BOOLEAN NOT NULL DEFAULT FALSE,
    last_write_started_at TIMESTAMPTZ,
    -- The content of the removal record the last write sent, kept across a
    -- takeover so a record left by a write whose response was lost is still
    -- recognised as the instance's.
    last_write_subject_cid TEXT,
    last_write_code TEXT,
    last_write_record_created_at TEXT,
    -- An undo committed whose admission stamp is still owed (rev set), and
    -- what that stamp writes: an acceptance pinning restore_stamp_pinned_cid,
    -- or the removal's delete when it is NULL.
    restore_stamp_rev TEXT,
    restore_stamp_acceptance_uri TEXT,
    restore_stamp_acceptance_rkey TEXT,
    restore_stamp_pinned_cid TEXT,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_expires_at TIMESTAMPTZ,
    last_failure_code TEXT,
    generation BIGINT NOT NULL DEFAULT 1,
    claim BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, subject_uri)
);

CREATE INDEX moderation_community_record_writes_due ON moderation_community_record_writes (next_attempt_at)
    WHERE state = 'pending';

-- +goose Down
DROP TABLE IF EXISTS moderation_community_record_writes;
