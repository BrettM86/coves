-- +goose Up
-- The erasure marker: proof that a DID was deleted ON PURPOSE
-- (docs/PRD_AUTHOR_OWNED_POSTS.md §5.3, rev 2.7).
--
-- WHY THIS EXISTS. Account deletion used to leave no trace. userRepo.Delete
-- removes the users row, the posts, and (since migration 034) the admission
-- rows — and then the firehose redelivers a post event for that same author,
-- or a dead letter for one is redriven, and every swept row comes straight
-- back. Nothing in the schema could tell the consumer not to re-index it.
--
-- The absence of a users row cannot carry that meaning, because under
-- author-owned posts it already means something else and something normal: a
-- post record now lives in the AUTHOR's repo, so its author may be someone
-- this AppView has never indexed, and §5.3 REQUIRES that event to index
-- anyway. "No users row" is therefore the ordinary state of a federated
-- author, and reading it as "erased" would refuse the open federated posting
-- the whole design exists to enable.
--
-- A row here means "this DID was erased on purpose"; no row means "never
-- seen". That is the entire distinction, and it is why the table holds a DID
-- and almost nothing else.
--
-- WHY NO FOREIGN KEY. The marker outlives the users row by construction — it
-- is written in the same transaction that deletes it — so a reference to
-- users(did) could never be satisfied. It is deliberately not scoped to
-- accounts this AppView hosts either: an erasure request may name a DID whose
-- repo lives elsewhere.
--
-- HOW IT IS CLEARED. An authenticated login, and nothing else. The OAuth
-- callback is the only place that knows the account itself is present — its
-- PDS attested the DID and the handle was verified in both directions — so it
-- calls users.IndexAuthenticatedUser, which calls ReinstateAccount, which is
-- the single statement that deletes a row from this table. An account that
-- comes back must be able to index: a marker left standing has the AppView
-- accept the profile and then silently drop every post, forever, with nothing
-- anywhere explaining why.
--
-- The users INSERT deliberately does NOT clear the marker. It used to, and
-- that put the decision within reach of every caller able to cause a row to be
-- written — including an unauthenticated endpoint asking only for a domain.
-- Un-erasing an account is a decision somebody makes, not a side effect of a
-- statement they happened to run, so the exit has a name and its call sites
-- can be read.
CREATE TABLE deleted_accounts (
    -- The DID is the whole key: one marker per account, so a re-delete
    -- updates in place rather than accumulating rows the ingestion gate would
    -- have to deduplicate on every event it reads.
    did TEXT PRIMARY KEY,

    -- NOT NULL because the marker's only job is to be READ by a consumer
    -- deciding whether to index an event, and a marker with no time cannot
    -- participate in any retention or audit answer later.
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Nullable, and expected to stay NULL for AppView-initiated deletions:
    -- nothing knows the account's repo revision at deletion time, because the
    -- deletion is a local administrative act rather than a commit. A column
    -- that had to be filled would be filled with a fabricated watermark, at
    -- the one place a real comparison happens. It exists for the future case
    -- where an erasure IS observed as a repo event carrying a rev.
    deleted_rev TEXT
);

COMMENT ON TABLE deleted_accounts IS 'Erasure markers: DIDs deleted on purpose, so ingestion can tell an erased account from a federated author it has never indexed (PRD_AUTHOR_OWNED_POSTS 5.3)';
COMMENT ON COLUMN deleted_accounts.deleted_at IS 'When the deletion happened; read by retention and audit, never by the ingestion gate itself';
COMMENT ON COLUMN deleted_accounts.deleted_rev IS 'Repo revision the erasure was observed at, when one is known; NULL for AppView-initiated deletions';

-- The acceptance engine's backlog scan (PRD_AUTHOR_OWNED_POSTS.md §5.6).
--
-- It rides along in this migration rather than getting one of its own because
-- both halves are the same task's enablers, and an index is not worth a
-- version of its own on a table one migration away.
--
-- WHY THE EXISTING INDEX CANNOT SERVE IT. Migration 034's
-- (community_did, status, created_at) index leads with the community, which is
-- exactly right for a moderator paging ONE community's queue and useless for
-- the driver's question — "what is undecided ANYWHERE" — which has no community
-- in hand and would scan every community's rows through it.
--
-- WHY PARTIAL. The undecided rows are a small and roughly constant slice of a
-- table that grows with every submission the instance has ever seen: a settled
-- admission stays forever, and pending ones drain. Restricting the index to the
-- two undecided statuses keeps it proportional to the BACKLOG rather than to
-- history, so a pass that runs on a timer does not get more expensive every day
-- the instance stays up.
--
-- Nothing FAILS without this index, which is precisely the danger: the query
-- keeps returning correct answers and quietly costs more every week, and the
-- symptom arrives as general database pressure with nothing pointing here.
CREATE INDEX idx_admissions_pending_queue
    ON community_post_admissions (created_at)
    WHERE status IN ('pending', 'pending_reacceptance');

COMMENT ON INDEX idx_admissions_pending_queue IS 'CRITICAL: the acceptance engine cross-community backlog scan, oldest first (PRD_AUTHOR_OWNED_POSTS 5.6)';

-- +goose Down
DROP INDEX IF EXISTS idx_admissions_pending_queue;

DROP TABLE IF EXISTS deleted_accounts;
