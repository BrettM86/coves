-- +goose Up
-- Add content_visibility column to community_subscriptions table
-- This implements the feed slider (1-5 scale) from DOMAIN_KNOWLEDGE.md
-- 1 = Only show the best/most popular content from this community
-- 5 = Show all content from this community
-- Default = 3 (balanced)
ALTER TABLE community_subscriptions
ADD COLUMN content_visibility INTEGER NOT NULL DEFAULT 3
CHECK (content_visibility >= 1 AND content_visibility <= 5);

-- Index for feed generation queries (filter by visibility level)
CREATE INDEX idx_subscriptions_visibility ON community_subscriptions(content_visibility);

-- Composite index for user feed queries (user_did + visibility level)
CREATE INDEX idx_subscriptions_user_visibility ON community_subscriptions(user_did, content_visibility);

COMMENT ON COLUMN community_subscriptions.content_visibility IS 'Feed slider: 1=only best content, 5=all content (see social.coves.community.subscription lexicon)';

-- +goose Down
-- Remove content_visibility column and indexes
DROP INDEX IF EXISTS idx_subscriptions_user_visibility;
DROP INDEX IF EXISTS idx_subscriptions_visibility;
ALTER TABLE community_subscriptions DROP COLUMN content_visibility;
