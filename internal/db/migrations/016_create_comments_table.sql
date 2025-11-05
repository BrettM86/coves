-- +goose Up
-- Create comments table for AppView indexing
-- Comments are indexed from the firehose after being written to user repositories
CREATE TABLE comments (
    id BIGSERIAL PRIMARY KEY,
    uri TEXT UNIQUE NOT NULL,               -- AT-URI (at://commenter_did/social.coves.feed.comment/rkey)
    cid TEXT NOT NULL,                      -- Content ID
    rkey TEXT NOT NULL,                     -- Record key (TID)
    commenter_did TEXT NOT NULL,            -- User who commented (from AT-URI repo field)

    -- Threading structure (reply references)
    root_uri TEXT NOT NULL,                 -- Strong reference to original post (at://...)
    root_cid TEXT NOT NULL,                 -- CID of root post (version pinning)
    parent_uri TEXT NOT NULL,               -- Strong reference to immediate parent (post or comment)
    parent_cid TEXT NOT NULL,               -- CID of parent (version pinning)

    -- Content (content is required per lexicon, others optional)
    content TEXT NOT NULL,                  -- Comment text (max 3000 graphemes, 30000 bytes)
    content_facets JSONB,                   -- Rich text facets (social.coves.richtext.facet)
    embed JSONB,                            -- Embedded content (images, quoted posts)
    content_labels JSONB,                   -- Self-applied labels (com.atproto.label.defs#selfLabels)
    langs TEXT[],                           -- Languages (ISO 639-1, max 3)

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL,        -- Commenter's timestamp from record
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed by AppView
    deleted_at TIMESTAMPTZ,                 -- Soft delete (for firehose delete events)

    -- Stats (denormalized for performance)
    upvote_count INT NOT NULL DEFAULT 0,    -- Comments can be voted on (per vote lexicon)
    downvote_count INT NOT NULL DEFAULT 0,
    score INT NOT NULL DEFAULT 0,           -- upvote_count - downvote_count (for sorting)
    reply_count INT NOT NULL DEFAULT 0      -- Number of direct replies to this comment

    -- NO foreign key constraint on commenter_did to allow out-of-order indexing from Jetstream
    -- Comment events may arrive before user events, which is acceptable since:
    -- 1. Comments are authenticated by the user's PDS (security maintained)
    -- 2. Orphaned comments from never-indexed users are harmless
    -- 3. This prevents race conditions in the firehose consumer
);

-- Indexes for threading queries (most important for comment UX)
CREATE INDEX idx_comments_root ON comments(root_uri, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent ON comments(parent_uri, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_parent_score ON comments(parent_uri, score DESC, created_at DESC) WHERE deleted_at IS NULL;

-- Indexes for user queries
CREATE INDEX idx_comments_commenter ON comments(commenter_did, created_at DESC);
CREATE INDEX idx_comments_uri ON comments(uri);

-- Index for vote targeting (when votes target comments)
CREATE INDEX idx_comments_uri_active ON comments(uri) WHERE deleted_at IS NULL;

-- Comment on table
COMMENT ON TABLE comments IS 'Comments indexed from user repositories via Jetstream firehose consumer';
COMMENT ON COLUMN comments.uri IS 'AT-URI in format: at://commenter_did/social.coves.feed.comment/rkey';
COMMENT ON COLUMN comments.root_uri IS 'Strong reference to the original post that started the thread';
COMMENT ON COLUMN comments.parent_uri IS 'Strong reference to immediate parent (post or comment)';
COMMENT ON COLUMN comments.score IS 'Computed as upvote_count - downvote_count for ranking replies';
COMMENT ON COLUMN comments.content_labels IS 'Self-applied labels per com.atproto.label.defs#selfLabels (JSONB: {"values":[{"val":"nsfw","neg":false}]})';

-- +goose Down
DROP TABLE IF EXISTS comments CASCADE;
