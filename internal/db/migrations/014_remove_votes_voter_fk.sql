-- +goose Up
-- Remove foreign key constraint on votes.voter_did to prevent race conditions
-- between user and vote Jetstream consumers.
--
-- Rationale:
-- - Vote events can arrive before user events in Jetstream
-- - Creating votes should not fail if user hasn't been indexed yet
-- - Users are validated at the PDS level (votes come from user repos)
-- - Orphaned votes (from deleted users) are harmless and can be ignored in queries

ALTER TABLE votes DROP CONSTRAINT IF EXISTS fk_voter;

-- Add check constraint to ensure voter_did is a valid DID format
ALTER TABLE votes ADD CONSTRAINT chk_voter_did_format
CHECK (voter_did ~ '^did:(plc|web|key):');

-- +goose Down
-- Restore foreign key constraint (note: this may fail if orphaned votes exist)
ALTER TABLE votes DROP CONSTRAINT IF EXISTS chk_voter_did_format;

ALTER TABLE votes ADD CONSTRAINT fk_voter
FOREIGN KEY (voter_did) REFERENCES users(did) ON DELETE CASCADE;
