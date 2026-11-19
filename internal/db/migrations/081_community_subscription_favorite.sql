-- +goose Up
-- Whether the subscriber has pinned this community as a favorite, indexed from
-- the favorite field of the social.coves.community.subscription record.
ALTER TABLE community_subscriptions
ADD COLUMN favorite BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN community_subscriptions.favorite IS 'Subscriber pinned this community as a favorite (see social.coves.community.subscription lexicon)';

-- +goose Down
ALTER TABLE community_subscriptions DROP COLUMN favorite;
