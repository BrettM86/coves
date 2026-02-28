-- +goose Up
CREATE TABLE community_suggestions (
    id BIGSERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    submitter_did TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    vote_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_suggestion_status CHECK (status IN ('open', 'under_review', 'approved', 'declined')),
    CONSTRAINT title_not_empty CHECK (LENGTH(TRIM(title)) > 0),
    CONSTRAINT title_max_length CHECK (LENGTH(title) <= 200),
    CONSTRAINT description_max_length CHECK (LENGTH(description) <= 5000),
    CONSTRAINT description_not_empty CHECK (LENGTH(TRIM(description)) > 0)
);

CREATE TABLE suggestion_votes (
    id BIGSERIAL PRIMARY KEY,
    suggestion_id BIGINT NOT NULL REFERENCES community_suggestions(id) ON DELETE CASCADE,
    voter_did TEXT NOT NULL,
    value SMALLINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_vote_value CHECK (value IN (1, -1)),
    CONSTRAINT unique_suggestion_voter UNIQUE (suggestion_id, voter_did)
);

-- Indexes
CREATE INDEX idx_suggestions_status ON community_suggestions(status);
CREATE INDEX idx_suggestions_created_at ON community_suggestions(created_at DESC);
CREATE INDEX idx_suggestions_vote_count ON community_suggestions(vote_count DESC);
CREATE INDEX idx_suggestions_submitter ON community_suggestions(submitter_did);
CREATE INDEX idx_suggestion_votes_suggestion ON suggestion_votes(suggestion_id);
CREATE INDEX idx_suggestion_votes_voter ON suggestion_votes(voter_did);

-- +goose Down
DROP TABLE IF EXISTS suggestion_votes;
DROP TABLE IF EXISTS community_suggestions;
