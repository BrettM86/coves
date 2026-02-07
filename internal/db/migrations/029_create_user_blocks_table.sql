-- +goose Up
CREATE TABLE user_blocks (
    id SERIAL PRIMARY KEY,
    blocker_did TEXT NOT NULL CHECK (blocker_did ~ '^did:(plc|web):[a-zA-Z0-9._:%-]+$'),
    blocked_did TEXT NOT NULL CHECK (blocked_did ~ '^did:(plc|web):[a-zA-Z0-9._:%-]+$'),
    blocked_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- AT-Proto metadata (block record lives in blocker's repo)
    -- These are required for atProto record verification and federation
    record_uri TEXT NOT NULL,  -- atProto record identifier (at://blocker_did/social.coves.actor.block/rkey)
    record_cid TEXT NOT NULL,  -- Content address (critical for verification)

    UNIQUE(blocker_did, blocked_did)
);

-- Indexes for efficient queries
-- Note: UNIQUE constraint on (blocker_did, blocked_did) already covers blocker_did as leading column
CREATE INDEX idx_user_blocks_blocked ON user_blocks(blocked_did);
CREATE UNIQUE INDEX idx_user_blocks_record_uri ON user_blocks(record_uri);  -- For GetBlockByURI (Jetstream DELETE operations)

-- +goose Down
DROP INDEX IF EXISTS idx_user_blocks_record_uri;
DROP INDEX IF EXISTS idx_user_blocks_blocked;
DROP TABLE IF EXISTS user_blocks;
