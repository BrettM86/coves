-- +goose Up
-- +goose NO TRANSACTION
-- EarlierUpvoteExists must see soft-deleted upvotes; partial indexes on
-- deleted_at IS NULL cannot serve it, and idx_votes_voter otherwise scans every
-- vote the voter ever cast. CONCURRENTLY avoids blocking writes to the large,
-- heavily written votes table. No IF NOT EXISTS: an interrupted concurrent
-- build can leave an INVALID index that must be dropped by an operator with
-- DROP INDEX CONCURRENTLY before retrying, not silently retained.
CREATE INDEX CONCURRENTLY idx_votes_voter_subject_upvotes ON votes (voter_did, subject_uri) WHERE direction = 'up';

-- +goose Down
-- +goose NO TRANSACTION
DROP INDEX CONCURRENTLY IF EXISTS idx_votes_voter_subject_upvotes;
