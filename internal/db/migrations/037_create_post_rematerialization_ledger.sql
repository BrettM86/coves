-- +goose Up
-- The re-materialization ledger: one row per legacy social.coves.community.post
-- record, tracking how far the cutover tool moved it (docs/PRD_AUTHOR_OWNED_POSTS.md
-- §11 the rev-2.8 deploy runbook).
--
-- WHY THIS EXISTS. The tool moves every legacy community-repo post to an
-- author-owned postv2 plus a community acceptance, and then — and ONLY then —
-- deletes the old record. Getting that order wrong makes a live post vanish or
-- relaunders a removed one, so the tool has to be resumable and idempotent, and
-- this table is what makes resume possible: a crash reads the row back and picks
-- up exactly where it stopped rather than re-doing work or skipping the delete.
--
-- THE DISTINCTION THIS TABLE HAS TO MAKE REPRESENTABLE is migrated ≠ done.
-- `migrated` means "verified safe to delete, old record STILL PRESENT"; `done`
-- means "old record deleted". A crash between them resumes by retrying ONLY the
-- delete, and a delete of an already-gone record is success — which is coherent
-- only if the checkpoint BEFORE the delete is its own persisted state. Collapsing
-- the two would either re-write the postv2 on every resume or skip the delete a
-- crash owes.
--
-- THE TWO FALLBACK STATES are the credential census (§11 step 3): a record whose
-- author credentials cannot be restored is left as legacy — never re-authored
-- under a forged signature, which would reintroduce the §2 impersonation the flip
-- exists to remove — and the run refuses to report "complete" while any such row
-- survives, gating the operator's separate, irreversible legacy-removal step.
CREATE TABLE post_rematerialization_ledger (
    -- The OLD community.post AT-URI is the whole key: one row per legacy record,
    -- so a re-run UPDATES the row it resumes from rather than accumulating a
    -- second for the same post, and the deterministic postv2 rkey is derived from
    -- this exact string.
    old_uri TEXT PRIMARY KEY,

    -- The state machine's cursor. NOT NULL — a row with no state cannot be
    -- resumed from — and CHECK-closed so a typo'd transition is refused at the
    -- schema, where every writer meets it, rather than sitting in the table as an
    -- unresumable row nothing would ever advance. migrated and done are BOTH
    -- listed and DISTINCT on purpose (see the header).
    state TEXT NOT NULL CHECK (state IN (
        'discovered',
        'postv2_written',
        'verified',
        'migrated',
        'done',
        'fallback_left_legacy',
        'fallback_no_creds'
    )),

    -- Who the postv2 is re-authored under: the legacy record's `author` field.
    -- Nullable is acceptable — a fallback row may never resolve a repo to write
    -- into — but it is the audit trail for which repo the tool wrote to.
    author_did TEXT,

    -- The postv2 coordinates, populated at the postv2_written transition and read
    -- back (never recomputed) on resume, so a resumed run converges on the record
    -- its first attempt wrote rather than deriving a fresh CID. NULL until then.
    new_uri  TEXT,
    new_cid  TEXT,
    new_rkey TEXT,

    -- The human-readable note on a fallback row (why it was left as legacy).
    reason TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE post_rematerialization_ledger IS 'One row per legacy community.post: the cutover tool''s resumable, idempotent progress ledger (PRD_AUTHOR_OWNED_POSTS 11)';
COMMENT ON COLUMN post_rematerialization_ledger.old_uri IS 'The OLD community.post AT-URI; the primary key and the material the deterministic postv2 rkey is derived from';
COMMENT ON COLUMN post_rematerialization_ledger.state IS 'The state-machine cursor; CHECK-closed. migrated (safe to delete, old record present) is DISTINCT from done (old record deleted)';
COMMENT ON COLUMN post_rematerialization_ledger.new_uri IS 'The postv2 URI written at the postv2_written transition; read back on resume, never recomputed';

-- +goose Down
DROP TABLE IF EXISTS post_rematerialization_ledger;
