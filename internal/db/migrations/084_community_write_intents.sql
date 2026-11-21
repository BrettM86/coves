-- +goose Up
-- A community write intent is the durable record of one write this instance
-- makes to a hosted community's repo on an account's behalf (a moderator's
-- resignation, an owner's or admin's removal of a moderator). It is stored,
-- open, before the PDS write and settled afterwards: completed when the write
-- landed (by the response, a same-key retry, the expiry sweep or the
-- firehose copy), failed when the repo moved without it, expired when it
-- never happened. Completion writes the one moderation action naming the
-- actor. There is no foreign key: the community delete removes its rows.
CREATE TABLE community_write_intents (
    id TEXT PRIMARY KEY,
    action TEXT NOT NULL,
    actor_did TEXT NOT NULL CHECK (actor_did <> ''),
    admin_acting BOOLEAN NOT NULL,
    community_did TEXT NOT NULL,
    subject_did TEXT NOT NULL,
    target_uri TEXT NOT NULL,
    op TEXT NOT NULL CHECK (op IN ('create', 'update', 'delete')),
    record JSONB,
    replaced_cid TEXT,
    guard_commit_cid TEXT NOT NULL,
    guard_rev TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    -- How long the idempotency record saved at completion is kept, so a
    -- firehose-first completion keeps it as long as the service would.
    idempotency_retention_microseconds BIGINT NOT NULL CHECK (idempotency_retention_microseconds > 0),
    state TEXT NOT NULL CHECK (state IN ('open', 'completed', 'failed', 'expired')),
    commit_rev TEXT,
    record_cid TEXT,
    action_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    CONSTRAINT community_write_intent_completed CHECK (
        (state = 'completed') = (commit_rev IS NOT NULL AND action_id IS NOT NULL)
    ),
    CONSTRAINT community_write_intent_finished CHECK ((state = 'open') = (finished_at IS NULL))
);

-- At most one write in flight per record.
CREATE UNIQUE INDEX community_write_intents_open_target ON community_write_intents (target_uri)
    WHERE state = 'open';
-- A same-key retry finds its intent.
CREATE INDEX community_write_intents_key ON community_write_intents (actor_did, community_did, idempotency_key);
-- The expiry sweep.
CREATE INDEX community_write_intents_open_expiry ON community_write_intents (expires_at)
    WHERE state = 'open';

-- Role actions: grant-role (a role record indexed), remove-role (a role
-- record deleted), transfer-ownership. admin_acting marks an instance admin
-- acting for a community in which it holds no role.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_actions_action_check;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_actions_action_check CHECK (action IN (
    'remove', 'restore', 'apply-removal', 'retract-removal', 'label', 'retract-label',
    'grant-role', 'remove-role', 'transfer-ownership'
));
ALTER TABLE moderation_actions ADD COLUMN admin_acting BOOLEAN NOT NULL DEFAULT false;
-- A role action names the role record it logged, by record key and CID, so a
-- completion that finds the record already removed links to the action that
-- logged that record's removal, and provisioning attributes the grant the
-- firehose logged for the owner record it wrote.
ALTER TABLE moderation_actions ADD COLUMN role_record_key TEXT;
ALTER TABLE moderation_actions ADD COLUMN role_record_cid TEXT;

-- actor_did is '' for a role change made outside this instance (no matching
-- intent), besides the 067 subject-moved system actions.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
);

-- +goose Down
-- Role actions and intents have no place in the 082 schema; they are dropped.
DELETE FROM moderation_actions WHERE action IN ('grant-role', 'remove-role', 'transfer-ownership');
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present
    CHECK (actor_did <> '' OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved');
ALTER TABLE moderation_actions DROP COLUMN role_record_cid;
ALTER TABLE moderation_actions DROP COLUMN role_record_key;
ALTER TABLE moderation_actions DROP COLUMN admin_acting;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_actions_action_check;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_actions_action_check CHECK (action IN (
    'remove', 'restore', 'apply-removal', 'retract-removal', 'label', 'retract-label'
));
DROP TABLE IF EXISTS community_write_intents;
