-- +goose Up
CREATE TABLE community_blocks (
    id SERIAL PRIMARY KEY,
    user_did TEXT NOT NULL CHECK (user_did ~ '^did:(plc|web):[a-zA-Z0-9._:%-]+$'),
    community_did TEXT NOT NULL CHECK (community_did ~ '^did:(plc|web):[a-zA-Z0-9._:%-]+$'),
    blocked_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- AT-Proto metadata (block record lives in user's repo)
    -- These are required for atProto record verification and federation
    record_uri TEXT NOT NULL,  -- atProto record identifier (at://user_did/social.coves.community.block/rkey)
    record_cid TEXT NOT NULL,  -- Content address (critical for verification)

    UNIQUE(user_did, community_did)
);

-- Indexes for efficient queries
-- Note: UNIQUE constraint on (user_did, community_did) already creates an index for those columns
CREATE INDEX idx_blocks_user ON community_blocks(user_did);
CREATE INDEX idx_blocks_community ON community_blocks(community_did);
CREATE INDEX idx_blocks_record_uri ON community_blocks(record_uri);  -- For GetBlockByURI (Jetstream DELETE operations)
CREATE INDEX idx_blocks_blocked_at ON community_blocks(blocked_at);

-- +goose Down
DROP INDEX IF EXISTS idx_blocks_blocked_at;
DROP INDEX IF EXISTS idx_blocks_record_uri;
DROP INDEX IF EXISTS idx_blocks_community;
DROP INDEX IF EXISTS idx_blocks_user;
DROP TABLE IF EXISTS community_blocks;
