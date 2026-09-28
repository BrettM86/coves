-- +goose Up
-- +goose NO TRANSACTION
-- Read-path indexes, and removal of indexes that only cost writes.
--
-- CONCURRENTLY throughout: posts, comments and votes are the largest, most
-- written tables, and a plain CREATE/DROP INDEX would block the firehose
-- consumers' writes for the whole build.

-- Discover's new and top sorts, and the timeline's, order posts across
-- communities. Every existing posts index leads with community_did, so both
-- read and sorted the entire table per request. Walking these instead stops at
-- the page's LIMIT. The trailing uri matches the feeds' final tiebreaker.
CREATE INDEX CONCURRENTLY idx_posts_created
    ON posts (created_at DESC, uri DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX CONCURRENTLY idx_posts_score
    ON posts (score DESC, created_at DESC, uri DESC)
    WHERE deleted_at IS NULL;

-- Comment permalinks and "continue this thread" (getComments?parentRkey=) find
-- the parent by (root_uri, rkey). idx_comments_root has no rkey, so the lookup
-- read every comment in the thread, on every page.
CREATE INDEX CONCURRENTLY idx_comments_root_rkey
    ON comments (root_uri, rkey);

-- The subscription consumer resolves an unsubscribe by its record URI, which
-- had no index: every unsubscribe event, and every replay of one, scanned the
-- whole table.
CREATE INDEX CONCURRENTLY idx_subscriptions_record_uri
    ON community_subscriptions (record_uri);

-- Each of these duplicates the index behind a UNIQUE constraint exactly (or is
-- a prefix of it), so the planner never needs it, and every insert and every
-- non-HOT update of the row paid to maintain it. Votes update posts.score, so
-- that is every vote.
DROP INDEX CONCURRENTLY IF EXISTS idx_posts_uri;                    -- posts_uri_key
DROP INDEX CONCURRENTLY IF EXISTS idx_votes_uri;                    -- votes_uri_key
DROP INDEX CONCURRENTLY IF EXISTS idx_votes_voter_subject;          -- unique_voter_subject_active
DROP INDEX CONCURRENTLY IF EXISTS idx_comments_uri;                 -- comments_uri_key
DROP INDEX CONCURRENTLY IF EXISTS idx_comments_uri_lookup;          -- comments_uri_key
DROP INDEX CONCURRENTLY IF EXISTS idx_subscriptions_user_community; -- community_subscriptions_user_did_community_did_key
DROP INDEX CONCURRENTLY IF EXISTS idx_subscriptions_user;           -- prefix of the same key

-- +goose Down
-- +goose NO TRANSACTION
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscriptions_user ON community_subscriptions(user_did);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscriptions_user_community ON community_subscriptions(user_did, community_did);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_comments_uri_lookup ON comments(uri);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_comments_uri ON comments(uri);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_votes_voter_subject ON votes(voter_did, subject_uri) WHERE deleted_at IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_votes_uri ON votes(uri);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_posts_uri ON posts(uri);

DROP INDEX CONCURRENTLY IF EXISTS idx_subscriptions_record_uri;
DROP INDEX CONCURRENTLY IF EXISTS idx_comments_root_rkey;
DROP INDEX CONCURRENTLY IF EXISTS idx_posts_score;
DROP INDEX CONCURRENTLY IF EXISTS idx_posts_created;
