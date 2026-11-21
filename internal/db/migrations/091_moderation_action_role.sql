-- +goose Up
-- A community roster action (grant-role, remove-role, transfer-ownership)
-- records the role it granted, removed or transferred, so the public modlog
-- can show it. Rows logged before this migration keep a NULL role.
ALTER TABLE moderation_actions ADD COLUMN role TEXT;
ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_role_roster_only CHECK (
    role IS NULL OR action IN ('grant-role', 'remove-role', 'transfer-ownership')
);

-- +goose Down
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_role_roster_only;
ALTER TABLE moderation_actions DROP COLUMN role;
