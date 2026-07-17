-- +goose Up
-- Per-record rev gate for Jetstream consumers.
--
-- WHY THIS EXISTS: the AppView is about to consume MULTIPLE Jetstream feeds
-- carrying the SAME repos (the public bsky.network feed plus our self-hosted
-- relay feed). Each feed is internally ordered, but the copies are skewed by
-- hours, so a later event for a repo can be processed before an earlier copy
-- of the same repo's history arriving on the other feed. Without a gate that
-- stale copy resurrects deleted records (create replayed after delete) or
-- regresses edited content (pre-edit update replayed after the edit).
--
-- THE MECHANISM: every commit event carries `rev`, the repo's monotonic TID —
-- a fixed-length base32-sortable string, so plain lexicographic comparison
-- IS commit order within one repo. Consumers record the rev of the last
-- APPLIED event per record URI here and apply an incoming create/update/
-- delete only when its rev is strictly greater than the stored one. Equal
-- rev means the same event replayed (reconnect rewind, redrive, duplicate
-- feed) and is a no-op; smaller means a stale cross-feed copy and is
-- skipped. One rule restores per-repo commit ordering regardless of how
-- feeds interleave — no heuristics, no CID comparisons.
--
-- WHY A SEPARATE TABLE instead of a rev column on each indexed table: the
-- row must SURVIVE the record it describes. Hard-deleted record types
-- (subscriptions, blocks, communities, aggregator rows) leave no row to
-- carry the delete's rev, so a per-table column cannot reject the stale
-- create that follows — this table doubles as the tombstone. Rows are tiny
-- (uri + 13-char rev) and bounded by the number of records ever indexed.
--
-- Events with no rev (synthetic test events, pre-existing dead letters)
-- bypass the gate entirely, preserving previous behavior.
CREATE TABLE jetstream_record_revs (
    record_uri TEXT PRIMARY KEY,
    -- COLLATE "C" pins the gate's rev comparison to bytewise order (for TIDs,
    -- bytewise IS commit order) regardless of the database's default collation.
    rev TEXT COLLATE "C" NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS jetstream_record_revs;
