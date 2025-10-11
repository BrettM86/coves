-- +goose Up
-- Enable pg_trgm extension for fuzzy text search
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Communities table: stores community metadata indexed from firehose
CREATE TABLE communities (
    id SERIAL PRIMARY KEY,
    did TEXT UNIQUE NOT NULL,                                       -- Community DID (did:coves:xxx or did:plc:xxx)
    handle TEXT UNIQUE NOT NULL,                                    -- Scoped handle (!gaming@coves.social)
    name TEXT NOT NULL,                                             -- Short community name (local part of handle)
    display_name TEXT,                                              -- Display name for UI
    description TEXT,                                               -- Community description
    description_facets JSONB,                                       -- Rich text annotations
    avatar_cid TEXT,                                                -- CID of avatar image blob
    banner_cid TEXT,                                                -- CID of banner image blob

    -- Ownership & hosting (V2: community owns its own repo)
    owner_did TEXT NOT NULL,                                        -- V1: instance DID, V2: same as did (self-owned)
    created_by_did TEXT NOT NULL,                                   -- DID of user who created community
    hosted_by_did TEXT NOT NULL,                                    -- DID of hosting instance

    -- V2: PDS Account Credentials (community has its own PDS account)
    pds_email TEXT,                                                 -- System email for community PDS account
    pds_password_hash TEXT,                                         -- bcrypt hash for re-authentication
    pds_access_token TEXT,                                          -- JWT for API calls (expires)
    pds_refresh_token TEXT,                                         -- For refreshing sessions
    pds_url TEXT DEFAULT 'http://localhost:2583',                   -- PDS hosting this community's repo

    -- Visibility & federation
    visibility TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'unlisted', 'private')),
    allow_external_discovery BOOLEAN NOT NULL DEFAULT true,

    -- Moderation
    moderation_type TEXT CHECK (moderation_type IN ('moderator', 'sortition')),
    content_warnings TEXT[],                                        -- Array of content warning types

    -- Statistics (cached counts)
    member_count INTEGER DEFAULT 0,
    subscriber_count INTEGER DEFAULT 0,
    post_count INTEGER DEFAULT 0,

    -- Federation metadata (for future cross-platform support)
    federated_from TEXT,                                            -- 'lemmy', 'coves', etc.
    federated_id TEXT,                                              -- Original ID on federated platform

    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,

    -- AT-Proto metadata
    record_uri TEXT,                                                -- AT-URI of the community profile record
    record_cid TEXT                                                 -- CID of the community profile record
);

-- Indexes for efficient queries
CREATE INDEX idx_communities_handle ON communities(handle);
CREATE INDEX idx_communities_visibility ON communities(visibility);
CREATE INDEX idx_communities_hosted_by ON communities(hosted_by_did);
CREATE INDEX idx_communities_created_by ON communities(created_by_did);
CREATE INDEX idx_communities_created_at ON communities(created_at);
CREATE INDEX idx_communities_name_trgm ON communities USING gin(name gin_trgm_ops);  -- For fuzzy search
CREATE INDEX idx_communities_description_trgm ON communities USING gin(description gin_trgm_ops);
CREATE INDEX idx_communities_pds_email ON communities(pds_email);  -- V2: For credential lookups

-- Security comments for V2 credentials
COMMENT ON COLUMN communities.pds_password_hash IS 'V2: bcrypt hash - NEVER return in API responses';
COMMENT ON COLUMN communities.pds_access_token IS 'V2: JWT - rotate frequently, NEVER log';
COMMENT ON COLUMN communities.pds_refresh_token IS 'V2: Refresh token - NEVER log or expose in APIs';

-- Subscriptions table: lightweight feed following
CREATE TABLE community_subscriptions (
    id SERIAL PRIMARY KEY,
    user_did TEXT NOT NULL,
    community_did TEXT NOT NULL REFERENCES communities(did) ON DELETE CASCADE,
    subscribed_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,

    -- AT-Proto metadata (subscription is a record in user's repo)
    record_uri TEXT,                                                -- AT-URI of the subscription record
    record_cid TEXT,                                                -- CID of the subscription record

    UNIQUE(user_did, community_did)
);

-- Indexes for subscription queries
CREATE INDEX idx_subscriptions_user ON community_subscriptions(user_did);
CREATE INDEX idx_subscriptions_community ON community_subscriptions(community_did);
CREATE INDEX idx_subscriptions_user_community ON community_subscriptions(user_did, community_did); -- Composite index for GetSubscription
CREATE INDEX idx_subscriptions_subscribed_at ON community_subscriptions(subscribed_at);

-- Memberships table: active participation & reputation tracking
CREATE TABLE community_memberships (
    id SERIAL PRIMARY KEY,
    user_did TEXT NOT NULL,
    community_did TEXT NOT NULL REFERENCES communities(did) ON DELETE CASCADE,

    -- Reputation system
    reputation_score INTEGER DEFAULT 0,                            -- Gained through participation
    contribution_count INTEGER DEFAULT 0,                          -- Total posts + comments + actions

    -- Activity tracking
    joined_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    last_active_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,

    -- Moderation status
    is_banned BOOLEAN DEFAULT false,
    is_moderator BOOLEAN DEFAULT false,

    UNIQUE(user_did, community_did)
);

-- Indexes for membership queries
CREATE INDEX idx_memberships_user ON community_memberships(user_did);
CREATE INDEX idx_memberships_community ON community_memberships(community_did);
CREATE INDEX idx_memberships_reputation ON community_memberships(community_did, reputation_score DESC);
CREATE INDEX idx_memberships_last_active ON community_memberships(last_active_at);

-- Community moderation actions table (V2 feature, schema prepared)
CREATE TABLE community_moderation (
    id SERIAL PRIMARY KEY,
    community_did TEXT NOT NULL REFERENCES communities(did) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('delist', 'quarantine', 'remove')),
    reason TEXT,
    instance_did TEXT NOT NULL,                                    -- Which instance took this action
    broadcast BOOLEAN DEFAULT false,                               -- Share moderation signal with network?
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP WITH TIME ZONE                            -- Optional: temporary moderation
);

-- Indexes for moderation queries
CREATE INDEX idx_moderation_community ON community_moderation(community_did);
CREATE INDEX idx_moderation_instance ON community_moderation(instance_did);
CREATE INDEX idx_moderation_action ON community_moderation(action);
CREATE INDEX idx_moderation_created_at ON community_moderation(created_at);

-- +goose Down
-- Drop security comments
COMMENT ON COLUMN communities.pds_refresh_token IS NULL;
COMMENT ON COLUMN communities.pds_access_token IS NULL;
COMMENT ON COLUMN communities.pds_password_hash IS NULL;

DROP INDEX IF EXISTS idx_communities_pds_email;
DROP INDEX IF EXISTS idx_moderation_created_at;
DROP INDEX IF EXISTS idx_moderation_action;
DROP INDEX IF EXISTS idx_moderation_instance;
DROP INDEX IF EXISTS idx_moderation_community;
DROP TABLE IF EXISTS community_moderation;

DROP INDEX IF EXISTS idx_memberships_last_active;
DROP INDEX IF EXISTS idx_memberships_reputation;
DROP INDEX IF EXISTS idx_memberships_community;
DROP INDEX IF EXISTS idx_memberships_user;
DROP TABLE IF EXISTS community_memberships;

DROP INDEX IF EXISTS idx_subscriptions_subscribed_at;
DROP INDEX IF EXISTS idx_subscriptions_user_community;
DROP INDEX IF EXISTS idx_subscriptions_community;
DROP INDEX IF EXISTS idx_subscriptions_user;
DROP TABLE IF EXISTS community_subscriptions;

DROP INDEX IF EXISTS idx_communities_description_trgm;
DROP INDEX IF EXISTS idx_communities_name_trgm;
DROP INDEX IF EXISTS idx_communities_created_at;
DROP INDEX IF EXISTS idx_communities_created_by;
DROP INDEX IF EXISTS idx_communities_hosted_by;
DROP INDEX IF EXISTS idx_communities_visibility;
DROP INDEX IF EXISTS idx_communities_handle;
DROP TABLE IF EXISTS communities;
