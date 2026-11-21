-- +goose Up
-- A comment removal record's code and optional reason, kept so the removal's
-- actions carry the code's mapped reason, a restore's included (the delete
-- commit carries no record). Rows indexed before this migration have neither;
-- their actions map the missing code as moderator discretion.
ALTER TABLE community_comment_removals ADD COLUMN code TEXT;
ALTER TABLE community_comment_removals ADD COLUMN reason TEXT;

-- The record a community comment action's removal is about. A comment
-- intent's target is a removal record, as a post intent's is; its subject
-- tells the two apart, and names the comment a restore (a delete, with no
-- body) withdraws the removal of. NULL for every other intent.
ALTER TABLE community_write_intents ADD COLUMN subject_uri TEXT;

-- A comment restore's batch deletes the removal record and stands no record:
-- its attempt has no stand record, only the removal it deletes.
ALTER TABLE community_write_intent_attempts ALTER COLUMN stand_record DROP NOT NULL;

-- A community comment removal or its retraction indexed from the community's
-- repo with no matching intent of this instance is logged with no actor,
-- inherited, besides the 087 actorless community post actions.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
    OR (scope_kind = 'community' AND origin = 'inherited'
        AND action IN ('remove', 'restore', 'apply-removal', 'retract-removal')
        AND subject_collection IS NOT NULL)
);

-- The community removal limits count local comment removals alongside post
-- removals.
CREATE INDEX moderation_actions_community_comment_removals ON moderation_actions (scope_community_did, created_at)
    WHERE scope_kind = 'community' AND action = 'apply-removal' AND origin = 'local' AND NOT admin_acting;

-- +goose Down
DROP INDEX IF EXISTS moderation_actions_community_comment_removals;
-- In the 087 schema a firehose comment removal or retraction is the
-- community's act: actorless ones are attributed to their community.
UPDATE moderation_actions SET actor_did = authority_did
WHERE actor_did = '' AND scope_kind = 'community' AND origin = 'inherited'
  AND action IN ('apply-removal', 'retract-removal')
  AND reason IS DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved';
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
    OR (scope_kind = 'community' AND origin = 'inherited' AND action IN ('remove', 'restore')
        AND subject_collection IS NOT NULL)
);
-- Comment intents would read as post intents in the 087 schema.
DELETE FROM community_write_intent_attempts
WHERE intent_id IN (SELECT id FROM community_write_intents WHERE subject_uri IS NOT NULL);
DELETE FROM community_write_intents WHERE subject_uri IS NOT NULL;
DELETE FROM community_write_intent_attempts WHERE stand_record IS NULL;
ALTER TABLE community_write_intent_attempts ALTER COLUMN stand_record SET NOT NULL;
ALTER TABLE community_write_intents DROP COLUMN IF EXISTS subject_uri;
ALTER TABLE community_comment_removals DROP COLUMN IF EXISTS reason;
ALTER TABLE community_comment_removals DROP COLUMN IF EXISTS code;
