-- +goose Up
-- A community ban record written or deleted outside this instance (no
-- matching intent) is logged as a community-scope remove or restore of the
-- banned account with no actor, inherited, besides the 087/088 actorless
-- community post and comment actions.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
    OR (scope_kind = 'community' AND origin = 'inherited'
        AND action IN ('remove', 'restore', 'apply-removal', 'retract-removal')
        AND subject_collection IS NOT NULL)
    OR (scope_kind = 'community' AND origin = 'inherited'
        AND action IN ('remove', 'restore') AND subject_kind = 'account')
);

-- +goose Down
-- In the 088 schema a firehose community ban or unban is the community's act:
-- actorless ones are attributed to their community.
UPDATE moderation_actions SET actor_did = authority_did
WHERE actor_did = '' AND scope_kind = 'community' AND origin = 'inherited'
  AND action IN ('remove', 'restore') AND subject_kind = 'account';
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_actor_present;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_actor_present CHECK (
    actor_did <> ''
    OR reason IS NOT DISTINCT FROM 'social.coves.moderation.defs#reasonSubjectMoved'
    OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
    OR (scope_kind = 'community' AND origin = 'inherited'
        AND action IN ('remove', 'restore', 'apply-removal', 'retract-removal')
        AND subject_collection IS NOT NULL)
);
