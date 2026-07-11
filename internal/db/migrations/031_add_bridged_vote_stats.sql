-- +goose Up
-- Add bridge-asserted vote aggregates to posts and comments.
--
-- Federated/bridged content (e.g. Lemmy posts materialized by the tidepool bridge)
-- carries an optional `bridgedStats` field on its record asserting the origin
-- platform's vote counts. We store those counts separately from native atproto
-- votes so the two can never clobber each other, and fold them into the displayed
-- stats and the denormalized `score` at read/write time.
--
-- Invariant after this migration:
--   score = (upvote_count + bridged_upvote_count) - (downvote_count + bridged_downvote_count)
--
-- bridged_stats_as_of records when the origin counts were sampled; incoming record
-- updates only overwrite the bridged counts when their asOf is newer-or-equal to the
-- stored one, which discards strictly-older/out-of-order updates while remaining
-- idempotent on replays (equal asOf carries identical counts) and tolerant of asOf
-- string-equality collisions from timestamp truncation.
--
-- SECURITY: only records whose repo is hosted on a trusted bridge PDS may assert
-- bridgedStats (enforced in the Jetstream consumers via a provenance gate); a negative
-- or absurdly large count is rejected at the parse boundary and by the CHECK below, so
-- no native repo can self-inflate its own score.

ALTER TABLE posts
    ADD COLUMN bridged_upvote_count INT NOT NULL DEFAULT 0,
    ADD COLUMN bridged_downvote_count INT NOT NULL DEFAULT 0,
    ADD COLUMN bridged_stats_as_of TIMESTAMPTZ,
    -- Bridged counts are unsigned aggregates; a negative value can only come from a
    -- malformed/hostile record. The consumer already clamps at the parse boundary, but
    -- this constraint is the last line of defence and keeps the score invariant sane.
    ADD CONSTRAINT posts_bridged_counts_non_negative
        CHECK (bridged_upvote_count >= 0 AND bridged_downvote_count >= 0);

ALTER TABLE comments
    ADD COLUMN bridged_upvote_count INT NOT NULL DEFAULT 0,
    ADD COLUMN bridged_downvote_count INT NOT NULL DEFAULT 0,
    ADD COLUMN bridged_stats_as_of TIMESTAMPTZ,
    ADD CONSTRAINT comments_bridged_counts_non_negative
        CHECK (bridged_upvote_count >= 0 AND bridged_downvote_count >= 0);

COMMENT ON COLUMN posts.bridged_upvote_count IS 'Origin-platform upvotes asserted by the bridge (folded into displayed upvotes and score)';
COMMENT ON COLUMN posts.bridged_downvote_count IS 'Origin-platform downvotes asserted by the bridge (folded into displayed downvotes and score)';
COMMENT ON COLUMN posts.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply only when strictly newer';
COMMENT ON COLUMN comments.bridged_upvote_count IS 'Origin-platform upvotes asserted by the bridge (folded into displayed upvotes and score)';
COMMENT ON COLUMN comments.bridged_downvote_count IS 'Origin-platform downvotes asserted by the bridge (folded into displayed downvotes and score)';
COMMENT ON COLUMN comments.bridged_stats_as_of IS 'When the bridged counts were sampled; updates apply only when strictly newer';

-- +goose Down
ALTER TABLE posts
    DROP COLUMN IF EXISTS bridged_upvote_count,
    DROP COLUMN IF EXISTS bridged_downvote_count,
    DROP COLUMN IF EXISTS bridged_stats_as_of;

ALTER TABLE comments
    DROP COLUMN IF EXISTS bridged_upvote_count,
    DROP COLUMN IF EXISTS bridged_downvote_count,
    DROP COLUMN IF EXISTS bridged_stats_as_of;
