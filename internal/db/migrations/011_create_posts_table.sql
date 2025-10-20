-- +goose Up
-- Create posts table for AppView indexing
-- Posts are indexed from the firehose after being written to community repositories
CREATE TABLE posts (
    id BIGSERIAL PRIMARY KEY,
    uri TEXT UNIQUE NOT NULL,              -- AT-URI (at://community_did/social.coves.post.record/rkey)
    cid TEXT NOT NULL,                      -- Content ID
    rkey TEXT NOT NULL,                     -- Record key (TID)
    author_did TEXT NOT NULL,               -- Author's DID (from record metadata)
    community_did TEXT NOT NULL,            -- Community DID (from AT-URI repo field)

    -- Content (all nullable per lexicon)
    title TEXT,                             -- Post title
    content TEXT,                           -- Post content/body
    content_facets JSONB,                   -- Rich text facets (app.bsky.richtext.facet)
    embed JSONB,                            -- Embedded content (images, video, external, record)
    content_labels TEXT[],                  -- Self-applied labels (nsfw, spoiler, violence)

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL,        -- Author's timestamp from record
    edited_at TIMESTAMPTZ,                  -- Last edit timestamp (future)
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed by AppView
    deleted_at TIMESTAMPTZ,                 -- Soft delete (for firehose delete events)

    -- Stats (denormalized for performance)
    upvote_count INT NOT NULL DEFAULT 0,
    downvote_count INT NOT NULL DEFAULT 0,
    score INT NOT NULL DEFAULT 0,           -- upvote_count - downvote_count (for sorting)
    comment_count INT NOT NULL DEFAULT 0,

    -- Foreign keys
    CONSTRAINT fk_author FOREIGN KEY (author_did) REFERENCES users(did) ON DELETE CASCADE,
    CONSTRAINT fk_community FOREIGN KEY (community_did) REFERENCES communities(did) ON DELETE CASCADE
);

-- Indexes for common query patterns
CREATE INDEX idx_posts_community_created ON posts(community_did, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_posts_community_score ON posts(community_did, score DESC, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_posts_author ON posts(author_did, created_at DESC);
CREATE INDEX idx_posts_uri ON posts(uri);

-- Index for full-text search on content (future)
-- CREATE INDEX idx_posts_content_search ON posts USING gin(to_tsvector('english', content)) WHERE deleted_at IS NULL;

-- Comment on table
COMMENT ON TABLE posts IS 'Posts indexed from community repositories via Jetstream firehose consumer';
COMMENT ON COLUMN posts.uri IS 'AT-URI in format: at://community_did/social.coves.post.record/rkey';
COMMENT ON COLUMN posts.score IS 'Computed as upvote_count - downvote_count for ranking algorithms';

-- +goose Down
DROP TABLE IF EXISTS posts CASCADE;
