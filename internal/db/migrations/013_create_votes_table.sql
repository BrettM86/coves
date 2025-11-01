-- +goose Up
-- Create votes table for AppView indexing
-- Votes are indexed from the firehose after being written to user repositories
CREATE TABLE votes (
    id BIGSERIAL PRIMARY KEY,
    uri TEXT UNIQUE NOT NULL,               -- AT-URI (at://voter_did/social.coves.interaction.vote/rkey)
    cid TEXT NOT NULL,                      -- Content ID
    rkey TEXT NOT NULL,                     -- Record key (TID)
    voter_did TEXT NOT NULL,                -- User who voted (from AT-URI repo field)

    -- Subject (strong reference to post/comment)
    subject_uri TEXT NOT NULL,              -- AT-URI of voted item
    subject_cid TEXT NOT NULL,              -- CID of voted item (strong reference)

    -- Vote data
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL,        -- Voter's timestamp from record
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed by AppView
    deleted_at TIMESTAMPTZ,                 -- Soft delete (for firehose delete events)

    -- Foreign keys
    CONSTRAINT fk_voter FOREIGN KEY (voter_did) REFERENCES users(did) ON DELETE CASCADE
);

-- Indexes for common query patterns
CREATE INDEX idx_votes_subject ON votes(subject_uri, direction) WHERE deleted_at IS NULL;
CREATE INDEX idx_votes_voter_subject ON votes(voter_did, subject_uri) WHERE deleted_at IS NULL;

-- Partial unique index: One active vote per user per subject (soft delete aware)
CREATE UNIQUE INDEX unique_voter_subject_active ON votes(voter_did, subject_uri) WHERE deleted_at IS NULL;
CREATE INDEX idx_votes_uri ON votes(uri);
CREATE INDEX idx_votes_voter ON votes(voter_did, created_at DESC);

-- Comment on table
COMMENT ON TABLE votes IS 'Votes indexed from user repositories via Jetstream firehose consumer';
COMMENT ON COLUMN votes.uri IS 'AT-URI in format: at://voter_did/social.coves.interaction.vote/rkey';
COMMENT ON COLUMN votes.subject_uri IS 'Strong reference to post/comment being voted on';
COMMENT ON INDEX unique_voter_subject_active IS 'Ensures one active vote per user per subject (soft delete aware)';

-- +goose Down
DROP TABLE IF EXISTS votes CASCADE;
