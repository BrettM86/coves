-- +goose Up
-- Index-time hide markers for what a community-banned account writes straight
-- to its own PDS. A comment or vote is hidden when a ban on its author in the
-- subject's community was in force at the record's Coves index order: indexed
-- before it, not lifted (deleted or overwritten) before it, and not expired on
-- the Coves clock when the record was indexed. The decision is recorded once;
-- existing rows were never classified and are left as they are.

-- One order shared by ban, comment and vote indexing. Jetstream time_us and
-- record createdAt are not used: only the order Coves indexed things in counts.
CREATE SEQUENCE coves_index_order;

-- hide_reason NULL means not hidden. 'pending_ban_check' marks a comment whose
-- thread root is not indexed yet (so its community is unknown), or a vote on
-- such a comment; it is classified when the root arrives, as of the record's
-- own index order and index time.
ALTER TABLE comments
    ADD COLUMN hide_reason TEXT CONSTRAINT comments_hide_reason CHECK (hide_reason IN ('community_ban', 'pending_ban_check')),
    ADD COLUMN coves_index_order BIGINT,
    ADD COLUMN coves_index_time TIMESTAMPTZ;

ALTER TABLE votes
    ADD COLUMN hide_reason TEXT CONSTRAINT votes_hide_reason CHECK (hide_reason IN ('community_ban', 'pending_ban_check')),
    ADD COLUMN coves_index_order BIGINT,
    ADD COLUMN coves_index_time TIMESTAMPTZ;

-- The partial indexes on the pending code are built CONCURRENTLY in 070.

-- Every ban Coves has indexed, kept after an unban or overwrite so a record
-- indexed while the ban was in force can still be classified later.
-- lifted_order is the index order of the delete or overwrite that ended it.
CREATE TABLE community_ban_history (
    id BIGSERIAL PRIMARY KEY,
    community_did TEXT NOT NULL,
    subject_did TEXT NOT NULL,
    record_key TEXT NOT NULL,
    record_cid TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    index_order BIGINT NOT NULL,
    lifted_order BIGINT,
    CONSTRAINT community_ban_history_lifted_after_indexed CHECK (lifted_order IS NULL OR lifted_order > index_order)
);

CREATE INDEX community_ban_history_subject ON community_ban_history (community_did, subject_did);
CREATE UNIQUE INDEX community_ban_history_one_unlifted ON community_ban_history (community_did, subject_did)
    WHERE lifted_order IS NULL;

INSERT INTO community_ban_history (community_did, subject_did, record_key, record_cid, expires_at, index_order)
SELECT community_did, subject_did, record_key, record_cid, expires_at, nextval('coves_index_order')
FROM community_bans
ORDER BY indexed_at;

-- +goose Down
-- Drops hide_reason without recounting tallies: a ban-hidden vote's row stays
-- and reads as unhidden, but the upvote, downvote and score counts it was kept
-- out of are not incremented, so they stay as they were.
DROP TABLE IF EXISTS community_ban_history;
ALTER TABLE votes DROP COLUMN IF EXISTS coves_index_time, DROP COLUMN IF EXISTS coves_index_order, DROP COLUMN IF EXISTS hide_reason;
ALTER TABLE comments DROP COLUMN IF EXISTS coves_index_time, DROP COLUMN IF EXISTS coves_index_order, DROP COLUMN IF EXISTS hide_reason;
DROP SEQUENCE IF EXISTS coves_index_order;
