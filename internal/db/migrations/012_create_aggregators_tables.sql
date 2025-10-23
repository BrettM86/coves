-- +goose Up
-- Create aggregators tables for indexing aggregator service declarations and authorizations
-- These records are indexed from Jetstream firehose consumer

-- ============================================================================
-- Table: aggregators
-- Purpose: Index aggregator service declarations from social.coves.aggregator.service records
-- Source: Aggregator's own repository (at://aggregator_did/social.coves.aggregator.service/self)
-- ============================================================================
CREATE TABLE aggregators (
    -- Primary identity
    did TEXT PRIMARY KEY,                       -- Aggregator's DID (must match repo DID)

    -- Service metadata (from lexicon)
    display_name TEXT NOT NULL,                 -- Human-readable name
    description TEXT,                           -- What this aggregator does
    config_schema JSONB,                        -- JSON Schema for community config validation
    avatar_url TEXT,                            -- Avatar image URL (extracted from blob)
    source_url TEXT,                            -- URL to source code (transparency)
    maintainer_did TEXT,                        -- DID of maintainer

    -- atProto record metadata
    record_uri TEXT NOT NULL UNIQUE,            -- AT-URI of service declaration record
    record_cid TEXT NOT NULL,                   -- CID of current record version
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When the aggregator service was created (from lexicon createdAt field)
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed/updated by AppView

    -- Cached stats (updated by aggregator_posts table triggers/queries)
    communities_using INTEGER NOT NULL DEFAULT 0,  -- Count of communities with enabled authorizations
    posts_created BIGINT NOT NULL DEFAULT 0        -- Total posts created by this aggregator
);

-- Indexes for discovery and lookups
CREATE INDEX idx_aggregators_created_at ON aggregators(created_at DESC);
CREATE INDEX idx_aggregators_indexed_at ON aggregators(indexed_at DESC);
CREATE INDEX idx_aggregators_maintainer ON aggregators(maintainer_did);

-- Comments
COMMENT ON TABLE aggregators IS 'Aggregator service declarations indexed from social.coves.aggregator.service records';
COMMENT ON COLUMN aggregators.did IS 'DID of the aggregator service (matches repo DID)';
COMMENT ON COLUMN aggregators.config_schema IS 'JSON Schema defining what config options communities can set';
COMMENT ON COLUMN aggregators.created_at IS 'When the aggregator service was created (from lexicon record createdAt field)';
COMMENT ON COLUMN aggregators.communities_using IS 'Cached count of communities with enabled=true authorizations';


-- ============================================================================
-- Table: aggregator_authorizations
-- Purpose: Index community authorization records for aggregators
-- Source: Community's repository (at://community_did/social.coves.aggregator.authorization/rkey)
-- ============================================================================
CREATE TABLE aggregator_authorizations (
    id BIGSERIAL PRIMARY KEY,

    -- Authorization identity
    aggregator_did TEXT NOT NULL,               -- DID of authorized aggregator
    community_did TEXT NOT NULL,                -- DID of community granting access

    -- Authorization state
    enabled BOOLEAN NOT NULL DEFAULT true,      -- Whether aggregator is currently active
    config JSONB,                               -- Community-specific config (validated against aggregator's schema)

    -- Audit trail (from lexicon)
    created_at TIMESTAMPTZ NOT NULL,            -- When authorization was created
    created_by TEXT NOT NULL,                   -- DID of moderator who authorized (set by API, not client)
    disabled_at TIMESTAMPTZ,                    -- When authorization was disabled (if enabled=false)
    disabled_by TEXT,                           -- DID of moderator who disabled

    -- atProto record metadata
    record_uri TEXT NOT NULL UNIQUE,            -- AT-URI of authorization record
    record_cid TEXT NOT NULL,                   -- CID of current record version
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When indexed/updated by AppView

    -- Constraints
    UNIQUE(aggregator_did, community_did),      -- One authorization per aggregator per community
    CONSTRAINT fk_aggregator FOREIGN KEY (aggregator_did) REFERENCES aggregators(did) ON DELETE CASCADE,
    CONSTRAINT fk_community FOREIGN KEY (community_did) REFERENCES communities(did) ON DELETE CASCADE
);

-- Indexes for authorization checks (CRITICAL PATH - used on every aggregator post)
CREATE INDEX idx_aggregator_auth_agg_enabled ON aggregator_authorizations(aggregator_did, enabled) WHERE enabled = true;
CREATE INDEX idx_aggregator_auth_comm_enabled ON aggregator_authorizations(community_did, enabled) WHERE enabled = true;
CREATE INDEX idx_aggregator_auth_lookup ON aggregator_authorizations(aggregator_did, community_did, enabled);

-- Indexes for listing/discovery
CREATE INDEX idx_aggregator_auth_agg_did ON aggregator_authorizations(aggregator_did, created_at DESC);
CREATE INDEX idx_aggregator_auth_comm_did ON aggregator_authorizations(community_did, created_at DESC);

-- Comments
COMMENT ON TABLE aggregator_authorizations IS 'Community authorizations for aggregators indexed from social.coves.aggregator.authorization records';
COMMENT ON COLUMN aggregator_authorizations.config IS 'Community-specific config, validated against aggregators.config_schema';
COMMENT ON INDEX idx_aggregator_auth_lookup IS 'CRITICAL: Fast lookup for post creation authorization checks';


-- ============================================================================
-- Table: aggregator_posts
-- Purpose: Track posts created by aggregators for rate limiting and stats
-- Note: This is AppView-only data, not from lexicon records
-- ============================================================================
CREATE TABLE aggregator_posts (
    id BIGSERIAL PRIMARY KEY,

    -- Post identity
    aggregator_did TEXT NOT NULL,               -- DID of aggregator that created the post
    community_did TEXT NOT NULL,                -- DID of community post was created in
    post_uri TEXT NOT NULL,                     -- AT-URI of the post record
    post_cid TEXT NOT NULL,                     -- CID of the post

    -- Timestamp
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- When post was created

    -- Constraints
    UNIQUE(post_uri),                           -- Each post tracked once
    CONSTRAINT fk_aggregator_posts_agg FOREIGN KEY (aggregator_did) REFERENCES aggregators(did) ON DELETE CASCADE,
    CONSTRAINT fk_aggregator_posts_comm FOREIGN KEY (community_did) REFERENCES communities(did) ON DELETE CASCADE
);

-- Indexes for rate limiting queries (CRITICAL PATH - used on every aggregator post)
CREATE INDEX idx_aggregator_posts_rate_limit ON aggregator_posts(aggregator_did, community_did, created_at DESC);

-- Indexes for stats
CREATE INDEX idx_aggregator_posts_agg_did ON aggregator_posts(aggregator_did, created_at DESC);
CREATE INDEX idx_aggregator_posts_comm_did ON aggregator_posts(community_did, created_at DESC);

-- Comments
COMMENT ON TABLE aggregator_posts IS 'AppView-only tracking of posts created by aggregators for rate limiting and stats';
COMMENT ON INDEX idx_aggregator_posts_rate_limit IS 'CRITICAL: Fast rate limit checks (posts in last hour per community)';


-- ============================================================================
-- Trigger: Update aggregator stats when authorizations change
-- Purpose: Keep aggregators.communities_using count accurate
-- ============================================================================
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION update_aggregator_communities_count()
RETURNS TRIGGER AS $$
BEGIN
    -- Recalculate communities_using count for affected aggregator
    IF TG_OP = 'DELETE' THEN
        UPDATE aggregators
        SET communities_using = (
            SELECT COUNT(*)
            FROM aggregator_authorizations
            WHERE aggregator_did = OLD.aggregator_did
              AND enabled = true
        )
        WHERE did = OLD.aggregator_did;
        RETURN OLD;
    ELSE
        UPDATE aggregators
        SET communities_using = (
            SELECT COUNT(*)
            FROM aggregator_authorizations
            WHERE aggregator_did = NEW.aggregator_did
              AND enabled = true
        )
        WHERE did = NEW.aggregator_did;
        RETURN NEW;
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trigger_update_aggregator_communities_count
    AFTER INSERT OR UPDATE OR DELETE ON aggregator_authorizations
    FOR EACH ROW
    EXECUTE FUNCTION update_aggregator_communities_count();

COMMENT ON FUNCTION update_aggregator_communities_count IS 'Maintains aggregators.communities_using count when authorizations change';


-- ============================================================================
-- Trigger: Update aggregator stats when posts are created
-- Purpose: Keep aggregators.posts_created count accurate
-- ============================================================================
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION update_aggregator_posts_count()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE aggregators
        SET posts_created = posts_created + 1
        WHERE did = NEW.aggregator_did;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE aggregators
        SET posts_created = posts_created - 1
        WHERE did = OLD.aggregator_did;
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trigger_update_aggregator_posts_count
    AFTER INSERT OR DELETE ON aggregator_posts
    FOR EACH ROW
    EXECUTE FUNCTION update_aggregator_posts_count();

COMMENT ON FUNCTION update_aggregator_posts_count IS 'Maintains aggregators.posts_created count when posts are tracked';


-- +goose Down
-- Drop triggers first
DROP TRIGGER IF EXISTS trigger_update_aggregator_posts_count ON aggregator_posts;
DROP TRIGGER IF EXISTS trigger_update_aggregator_communities_count ON aggregator_authorizations;

-- Drop functions
DROP FUNCTION IF EXISTS update_aggregator_posts_count();
DROP FUNCTION IF EXISTS update_aggregator_communities_count();

-- Drop tables in reverse order (respects foreign keys)
DROP TABLE IF EXISTS aggregator_posts CASCADE;
DROP TABLE IF EXISTS aggregator_authorizations CASCADE;
DROP TABLE IF EXISTS aggregators CASCADE;
