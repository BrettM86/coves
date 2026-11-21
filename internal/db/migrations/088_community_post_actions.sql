-- +goose Up
-- The removal record a community holds for a post, indexed apart from the
-- post's admission: an acceptance never clears it, only the removal record's
-- delete does. A removal delete finds its post and code here whatever order
-- the commit's events arrive in, and a firehose copy of a removal the index
-- already holds (the same record CID) is recognized as no new change.
-- record_key and record_cid are NULL for a removal indexed before this table
-- existed (the back-fill below); a removal record's delete resolves such a row
-- by recomputing the subject key from post_uri. There is no foreign key:
-- the community delete and the author's erasure remove the rows.
CREATE TABLE community_post_removal_records (
    community_did TEXT NOT NULL,
    post_uri TEXT NOT NULL,
    record_key TEXT,
    record_cid TEXT,
    code TEXT NOT NULL,
    reason TEXT,
    rev TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, post_uri)
);

CREATE INDEX community_post_removal_records_key ON community_post_removal_records (community_did, record_key);

INSERT INTO community_post_removal_records (community_did, post_uri, code, reason)
SELECT community_did, post_uri, COALESCE(decision_code, ''), removal_reason
FROM community_post_admissions
WHERE status = 'removed';

-- Each batch a post action's writer was about to send, recorded in its own
-- transaction before the send: the guard head used as swapCommit, the stand
-- record's CID before the write (NULL for a create), the exact stand-record
-- body, and the CID and code of the record the batch deletes (NULL when it
-- deletes none: for a restore, the removal it withdraws, whose code the
-- restore's action reason is). A retry or the sweep reads the latest attempt
-- to tell this intent's lost commit from a decision already in place. Insert-only and with no
-- foreign key, so recording an attempt never waits on the intent row an
-- action transaction holds; the community delete removes the rows.
CREATE TABLE community_write_intent_attempts (
    id BIGSERIAL PRIMARY KEY,
    intent_id TEXT NOT NULL,
    guard_commit_cid TEXT NOT NULL,
    guard_rev TEXT NOT NULL,
    stand_cid TEXT,
    stand_record JSONB NOT NULL,
    clear_cid TEXT,
    clear_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX community_write_intent_attempts_intent ON community_write_intent_attempts (intent_id, id);

-- A removal record's delete indexed while a restore intent of this instance
-- is open, and deleting the removal that intent's latest batch would delete,
-- is logged as an inherited restore that awaits the intent (action_id); its
-- rev is kept so the intent's completion reuses that action only for its own
-- commit.
ALTER TABLE community_write_intents ADD COLUMN awaiting_delete_rev TEXT;

-- A post removal's intent deletes the acceptance as its paired write: a
-- paired target with no paired record is a paired delete.
ALTER TABLE community_write_intents DROP CONSTRAINT community_write_intent_paired_write;
ALTER TABLE community_write_intents ADD CONSTRAINT community_write_intent_paired_write CHECK (
    paired_record IS NULL OR paired_target_uri IS NOT NULL
);

-- A community post removal or restore made outside this instance (no
-- matching intent) is logged with no actor, inherited.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
    OR (scope_kind = 'community' AND origin = 'inherited' AND action IN ('remove', 'restore')
        AND subject_collection IS NOT NULL)
);

-- The community removal limits count a community's recent local removals.
CREATE INDEX moderation_actions_community_removals ON moderation_actions (scope_community_did, created_at)
    WHERE scope_kind = 'community' AND action = 'remove' AND origin = 'local' AND NOT admin_acting;

-- +goose Down
-- Actorless community post actions have no place in the 086 schema.
DELETE FROM moderation_actions
WHERE actor_did = '' AND scope_kind = 'community' AND action IN ('remove', 'restore')
  AND subject_collection IS NOT NULL;
DROP INDEX IF EXISTS moderation_actions_community_removals;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
);
-- Post removal intents (a paired delete) have no place in the 086 schema.
DELETE FROM community_write_intents WHERE paired_target_uri IS NOT NULL AND paired_record IS NULL;
ALTER TABLE community_write_intents DROP CONSTRAINT community_write_intent_paired_write;
ALTER TABLE community_write_intents ADD CONSTRAINT community_write_intent_paired_write CHECK (
    (paired_target_uri IS NULL) = (paired_record IS NULL)
);
ALTER TABLE community_write_intents DROP COLUMN IF EXISTS awaiting_delete_rev;
DROP TABLE IF EXISTS community_write_intent_attempts;
DROP TABLE IF EXISTS community_post_removal_records;
