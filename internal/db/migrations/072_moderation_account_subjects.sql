-- +goose Up
-- An account is a moderation subject: at://<did>, with no collection. No
-- existing row is rewritten: every stored action has a record URI and a
-- collection, which the shape constraint accepts.
ALTER TABLE moderation_actions ALTER COLUMN subject_collection DROP NOT NULL;

ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_subject_shape CHECK (
    (subject_collection IS NULL) = (subject_uri ~ '^at://did:[a-z]+:[a-zA-Z0-9._:%-]*[a-zA-Z0-9._-]$')
);

-- expires_at is a temporary account ban's end. The ban stops applying then
-- with nothing written; only an account removal carries one.
ALTER TABLE moderation_actions ADD COLUMN expires_at TIMESTAMPTZ;

ALTER TABLE moderation_actions ADD CONSTRAINT moderation_action_expiry CHECK (
    expires_at IS NULL OR (subject_collection IS NULL AND action = 'remove')
);

-- +goose Down
-- Fails while any account action is stored: the 071 schema cannot hold one.
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_expiry;
ALTER TABLE moderation_actions DROP COLUMN expires_at;
ALTER TABLE moderation_actions DROP CONSTRAINT moderation_action_subject_shape;
ALTER TABLE moderation_actions ALTER COLUMN subject_collection SET NOT NULL;
