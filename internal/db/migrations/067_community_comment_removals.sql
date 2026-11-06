-- +goose Up
-- Index of social.coves.community.removal records whose subject is a comment.
-- A delete commit carries no record, so the row keyed by (repo DID, rkey) is
-- how a withdrawal finds its subject. A removal that arrives before its comment
-- or the thread's root post is held here until both are indexed. The decision
-- itself lives in moderation_decisions; this table is index state.
--
-- moved_action_id is the system retract-removal that returned an applied
-- removal to held when its comment was re-created under another community's
-- post. It is set only while held, so a re-application when the comment comes
-- back can name the action it reverses.
CREATE TABLE community_comment_removals (
    repository_did TEXT NOT NULL,
    record_key TEXT NOT NULL,
    record_cid TEXT NOT NULL,
    subject_uri TEXT NOT NULL,
    subject_cid TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('held', 'applied')),
    apply_action_id TEXT REFERENCES moderation_actions(id),
    moved_action_id TEXT REFERENCES moderation_actions(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repository_did, record_key),
    CONSTRAINT community_comment_removal_applied_action CHECK ((state = 'applied') = (apply_action_id IS NOT NULL)),
    CONSTRAINT community_comment_removal_moved_action CHECK (moved_action_id IS NULL OR state = 'held')
);

-- Reconciliation scans removals of a comment and its descendants in both states.
CREATE INDEX community_comment_removals_subject ON community_comment_removals (subject_uri);

-- actor_did is the DID of the account that acted. It is '' only for system
-- actions: the subject-moved retract-removal/apply-removal pair the AppView
-- logs when a comment moves between communities' posts. reason is stored NULL
-- when empty, so the comparison must be NULL-safe.
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present
    CHECK (actor_did <> '' OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved');

-- +goose Down
-- Refuse while any mapping exists. Dropping them would leave active
-- community-scope removal decisions and their rev gates (and held removals)
-- without the mapping a withdrawal finds them by, and a later up starts the
-- table empty, so those removals could never be retracted after down+up.
-- Lock first so no consumer inserts a mapping between the check and the drop.
LOCK TABLE community_comment_removals IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM community_comment_removals) THEN
        RAISE EXCEPTION 'community_comment_removals holds removal mappings; rolling back 067 would leave those removals impossible to retract';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE moderation_actions DROP CONSTRAINT IF EXISTS moderation_action_actor_present;
DROP TABLE IF EXISTS community_comment_removals;
