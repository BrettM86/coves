-- +goose Up
-- The submission ledger: the rows that both deduplicate submissions and meter
-- the per-author quota of docs/PRD_AUTHOR_OWNED_POSTS.md §4.1 and §8.
--
-- WHY A NEW TABLE RATHER THAN COUNTING `posts`. Three independent reasons, any
-- one of which is disqualifying. `posts` is firehose-fed, so its rows appear
-- after ingestion lag — which is to say, after the burst being limited has
-- already landed. Its created_at is author-supplied, so once writes flip to
-- author repos (§4.2) the window a quota is measured over becomes
-- attacker-controlled. And its read indexes exclude soft-deleted rows, so
-- deleting a post would return its quota slot and make delete-to-evade the
-- cheapest way past the limit. This table is written synchronously on the
-- write path, stamped by the server, and never soft-deleted.
--
-- THE UNIQUE CONSTRAINT IS THE DEDUPE GATE, not an assertion about one. The
-- admission decision does not SELECT and then INSERT: it INSERTs and reads the
-- unique violation as the answer, because the database is the only participant
-- that two racing double-taps both talk to. A read-then-write would pass every
-- sequential test and admit both halves of a concurrent duplicate.
--
-- WHY THE BUCKET IS PART OF THE KEY. Without it the constraint would say "this
-- author may never post this content into this community again", which is a
-- vastly stronger policy than "do not accept the same thing twice right now".
-- The bucket is the index of the application's dedupe window, derived from the
-- injected clock, so the key expires on its own without a sweeper.
--
-- WHY NO FOREIGN KEYS. Migration 034 dropped posts.fk_author because a
-- federated author has no `users` row (§5.3) — the AppView only bootstraps
-- authors from trusted bridge PDSs today — and a community named by a
-- submission may be one this AppView has not indexed. An FK to either would
-- turn an ordinary submission into an insert failure, and the refusal would
-- surface as a dead letter rather than as a decision. community_blocks
-- (migration 009) and aggregator_posts' successor rationale set the same
-- precedent for the same reason.
CREATE TABLE post_submissions (
    -- The surrogate key exists so a reservation can be RELEASED by identity.
    -- Releasing by the natural key would work too, right up until the row it
    -- deleted is not the one this request inserted — which is exactly the
    -- concurrent case the reservation exists for.
    id BIGSERIAL PRIMARY KEY,

    author_did TEXT NOT NULL,
    community_did TEXT NOT NULL,

    -- The hash of the canonical record with createdAt removed (see
    -- posts.submissionFingerprint). TEXT rather than BYTEA so it is greppable
    -- during an incident and comparable in psql; the column is never
    -- interpreted, only equated.
    fingerprint TEXT NOT NULL,

    -- The index of the dedupe window this submission falls in, derived from
    -- the application's injected clock. An integer rather than a timestamp
    -- deliberately: a timestamp here invites comparison against created_at,
    -- and the two come from different clocks — one the application's, one the
    -- database's.
    dedupe_bucket BIGINT NOT NULL,

    -- Server-stamped. The rolling window is measured against this column, so a
    -- caller who could set it could set their own quota.
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_post_submissions_dedupe
        UNIQUE (author_did, community_did, fingerprint, dedupe_bucket)
);

-- The rolling-window quota query, run on the write path of every post.
-- Migration 012 built idx_aggregator_posts_rate_limit for the identical shape;
-- without the equivalent here the count degrades into a scan of every
-- submission the instance has ever accepted.
CREATE INDEX idx_post_submissions_rate_limit
    ON post_submissions (author_did, community_did, created_at DESC);

COMMENT ON TABLE post_submissions IS 'Synchronous ledger of admitted post submissions: the dedupe gate and the per-author/per-community quota counter (PRD_AUTHOR_OWNED_POSTS 4.1, 8)';
COMMENT ON COLUMN post_submissions.fingerprint IS 'Hash of the canonical post record with createdAt removed, so a retry of identical content matches the attempt it repeats';
COMMENT ON COLUMN post_submissions.dedupe_bucket IS 'Index of the application dedupe window; part of the unique key, which is what makes dedupe expire instead of banning a repost forever';
COMMENT ON COLUMN post_submissions.created_at IS 'Server-stamped submission time; the rolling quota window is measured against it';
COMMENT ON INDEX idx_post_submissions_rate_limit IS 'CRITICAL: the per-author, per-community rolling-window count taken on every post write';

-- +goose Down
DROP TABLE IF EXISTS post_submissions;
