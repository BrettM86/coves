-- +goose Up
-- Moderation state is durable operator history, independent of indexed content.
CREATE TABLE moderation_actions (
    id TEXT PRIMARY KEY,
    actor_did TEXT NOT NULL,
    authority_did TEXT NOT NULL,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('instance', 'community')),
    scope_community_did TEXT,
    subject_uri TEXT NOT NULL,
    subject_collection TEXT NOT NULL,
    subject_community_did TEXT,
    observed_cid TEXT,
    action TEXT NOT NULL CHECK (action IN ('remove', 'restore', 'apply-removal', 'retract-removal', 'label', 'retract-label')),
    label_value TEXT,
    reason TEXT,
    private_classification TEXT,
    private_note TEXT,
    reverses_action_id TEXT REFERENCES moderation_actions(id),
    origin TEXT NOT NULL CHECK (origin IN ('local', 'inherited')),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT moderation_action_scope CHECK ((scope_kind = 'instance') = (scope_community_did IS NULL))
);

CREATE INDEX moderation_actions_subject ON moderation_actions (subject_uri, created_at DESC, id DESC);

CREATE TABLE moderation_subjects (
    subject_uri TEXT PRIMARY KEY,
    version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE moderation_decisions (
    authority_did TEXT NOT NULL,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('instance', 'community')),
    scope_community_did TEXT,
    subject_uri TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('removal', 'label')),
    value TEXT,
    active_action_id TEXT NOT NULL REFERENCES moderation_actions(id),
    active BOOLEAN NOT NULL,
    CONSTRAINT moderation_decision_scope CHECK ((scope_kind = 'instance') = (scope_community_did IS NULL)),
    CONSTRAINT moderation_decision_value CHECK ((kind = 'removal') = (value IS NULL)),
    CONSTRAINT moderation_decisions_key UNIQUE NULLS NOT DISTINCT
        (authority_did, scope_kind, scope_community_did, subject_uri, kind, value)
);

CREATE INDEX moderation_decisions_active_subject ON moderation_decisions (subject_uri, authority_did)
    WHERE active;

CREATE TABLE moderation_idempotency_keys (
    actor_did TEXT NOT NULL,
    authority_did TEXT NOT NULL,
    key TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    stored_result JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_did, authority_did, key)
);

CREATE INDEX moderation_idempotency_actor_live ON moderation_idempotency_keys (actor_did, expires_at);
CREATE INDEX moderation_idempotency_expiry ON moderation_idempotency_keys (expires_at);

CREATE TABLE moderation_media_blocks (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_did TEXT,
    blob_cid TEXT NOT NULL,
    action_id TEXT NOT NULL REFERENCES moderation_actions(id),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    CONSTRAINT moderation_media_block_key UNIQUE NULLS NOT DISTINCT (owner_did, blob_cid, action_id)
);

CREATE INDEX moderation_media_blocks_owner_active ON moderation_media_blocks (owner_did, blob_cid) WHERE active;
CREATE INDEX moderation_media_blocks_cid_active ON moderation_media_blocks (blob_cid) WHERE active AND owner_did IS NULL;
CREATE INDEX moderation_media_blocks_action_active ON moderation_media_blocks (action_id) WHERE active;

-- +goose Down
-- NEVER run this Down migration in production: moderation history is not disposable indexing state.
DROP TABLE IF EXISTS moderation_media_blocks;
DROP TABLE IF EXISTS moderation_idempotency_keys;
DROP TABLE IF EXISTS moderation_decisions;
DROP TABLE IF EXISTS moderation_subjects;
DROP TABLE IF EXISTS moderation_actions;
