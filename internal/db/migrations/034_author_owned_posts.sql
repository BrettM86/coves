-- +goose Up
-- Author-owned posts: relational admission state, and the author FK that has
-- to go with it (docs/PRD_AUTHOR_OWNED_POSTS.md §5.2, §5.3, §6.1).
--
-- WHY THIS EXISTS: a post record now lives in the AUTHOR's repo, and each
-- community publishes its own acceptance (and, for moderation, removal)
-- record in the COMMUNITY's repo. "Is this post visible?" therefore stops
-- being a property OF the post — the same post can carry independent
-- decisions from several communities at once — so admission state becomes a
-- row per (community, post) subject rather than a status column on `posts`.
-- It also gives `rejected` somewhere to live: rev 1 of the design promised
-- the status in getStatus and stored it nowhere.
--
-- THE ORDERING RULE (§5.2): acceptance and removal are DIFFERENT record URIs
-- describing the SAME subject, so migration 033's per-record rev gate cannot
-- order their combined effect — a redriven stale acceptance would resurrect a
-- removed post, a delayed acceptance-delete would flip `removed` back to
-- `pending`. This table carries a SUBJECT-scoped composite watermark instead:
-- (last_community_rev, last_community_op_rank), where rank(delete) = 0 <
-- rank(put) = 1. A community event applies only when its tuple is strictly
-- greater. One rule, applied per subject, makes every commit converge no
-- matter which half of it a consumer sees first: the removal commit
-- {acceptance-delete@(R,0), removal-create@(R,1)} settles on `removed`, the
-- restore commit {removal-delete@(R2,0), acceptance-create@(R2,1)} settles on
-- `accepted`, and an equal tuple is a replay (multi-feed overlap, dead-letter
-- redrive) that must not re-stamp a decision timestamp. Author-repo events
-- keep the 033 gate and NEVER touch this watermark: the two repos have
-- unrelated revision clocks, and mixing them would let an author's edit
-- outrank a moderator's removal.
--
-- WHY NO FOREIGN KEYS TO posts OR communities: acceptance-before-post has to
-- be REPRESENTABLE. Relay coverage gaps and cross-feed skew routinely deliver
-- a community's acceptance before the author's post event, and the admission
-- row is what records that the acceptance was seen at all — an FK would turn
-- a normal ordering artefact into an insert failure and a dead letter.
-- community_blocks (migration 009) set the same precedent for the same
-- reason: it holds DIDs for repos this AppView may not have indexed yet.
--
-- CONSUMER CONTRACT THIS TABLE DEPENDS ON: a post's `community` field is
-- IMMUTABLE across updates (§3.1) — an update event that changes it is
-- invalid and MUST be ignored. The subject key here is (community_did,
-- post_uri), so a post that could change communities would strand its
-- admission row behind it. If that contract is ever relaxed, this table needs
-- a migration, not a new branch in the consumer.
CREATE TABLE community_post_admissions (
    -- The subject: one decision per (community, post). Deliberately NOT a
    -- reference to posts.uri — see the no-FK note above.
    community_did TEXT NOT NULL,
    post_uri TEXT NOT NULL,

    status TEXT NOT NULL,

    -- The LIVE community acceptance record. Populated while an acceptance
    -- stands; a removal or an acceptance deletion clears all three.
    -- accepted_cid is the CID the acceptance PINS, which is what separates
    -- `accepted` from `pending_reacceptance` when compared with evaluated_cid.
    acceptance_uri TEXT,
    acceptance_rkey TEXT,
    accepted_cid TEXT,

    -- A rejection or removal, and when it was decided. Rejections are
    -- AppView-local and never correspond to a community-repo record.
    decision_code TEXT,
    decision_at TIMESTAMP WITH TIME ZONE,

    -- The exact content CID the AppView has indexed: what the next decision
    -- judges, and what an acceptance's pinned CID is compared against.
    evaluated_cid TEXT,

    -- Terminal policy rejections opt out of dead-letter redrive; a transient
    -- evaluation failure must stay retryable, so the default is true. The
    -- other default would make every dead letter terminal by omission.
    redrivable BOOLEAN NOT NULL DEFAULT true,

    -- The §5.2 subject-scoped composite watermark: the rev of the last
    -- APPLIED community event about this subject, plus its rank within that
    -- commit. NULL until the first community event applies.
    --
    -- COLLATE "C" pins the rev comparison to bytewise order regardless of the
    -- database's default collation. Revs are base32-sortable TIDs, so bytewise
    -- order IS commit order — under a locale-aware collation the gate at the
    -- heart of §5.2 degrades into something that merely mostly works.
    -- Migration 033 pins the same thing for the same reason.
    last_community_rev TEXT COLLATE "C",
    last_community_op_rank SMALLINT,

    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT pk_community_post_admissions PRIMARY KEY (community_did, post_uri),

    -- Read paths switch on status exhaustively, so the vocabulary is closed
    -- here rather than by convention.
    CONSTRAINT chk_admission_status CHECK (
        status IN ('pending', 'accepted', 'pending_reacceptance', 'rejected', 'removed')
    ),

    -- rejected and removed are the two states a reader RENDERS differently: a
    -- removal shows as #removedPost carrying its code, and a rejection has to
    -- explain itself to its author. A NULL code there is an unexplained
    -- decision, and the schema is the only place that can be made impossible.
    CONSTRAINT chk_admission_decision_code CHECK (
        status NOT IN ('rejected', 'removed') OR decision_code IS NOT NULL
    ),

    -- Both halves of the watermark move together or not at all. A half-set
    -- watermark makes the tuple comparison evaluate to NULL, which reads as
    -- "not strictly greater" — every subsequent community event for that
    -- subject would be skipped forever, silently.
    CONSTRAINT chk_admission_watermark_complete CHECK (
        (last_community_rev IS NULL) = (last_community_op_rank IS NULL)
    )
);

-- Feed queries read accepted rows only. The primary key covers the same
-- columns for ALL statuses, so without the predicate a feed lookup drags
-- pending and removed rows through the index too.
CREATE INDEX idx_admissions_accepted ON community_post_admissions (community_did, post_uri)
    WHERE status = 'accepted';

-- "Every community's decision about this post" — the author's own view of
-- their post, and the consumer's lookup when an author-repo event arrives.
-- The primary key leads with community_did, so this would otherwise be a
-- sequential scan.
CREATE INDEX idx_admissions_post ON community_post_admissions (post_uri);

-- The moderation queue: one community's admissions by status, oldest first.
CREATE INDEX idx_admissions_queue ON community_post_admissions (community_did, status, created_at);

COMMENT ON TABLE community_post_admissions IS 'The admission decision one community has made about one author-owned post (PRD_AUTHOR_OWNED_POSTS 6.1)';
COMMENT ON COLUMN community_post_admissions.last_community_rev IS 'Rev of the last applied community-repo event about this subject; COLLATE "C" so comparison is bytewise, which for TIDs is commit order';
COMMENT ON COLUMN community_post_admissions.last_community_op_rank IS 'Rank of that event within its commit: 0 = delete, 1 = put, so a put outranks a delete carrying the same rev';

-- posts.fk_author has to go (§5.3). It is a hard FK to `users` with ON DELETE
-- CASCADE, and under open federated posting an author's users row may never
-- exist — the AppView only bootstraps authors from trusted bridge PDSs today,
-- which is precisely what makes a federated author's post unindexable. The
-- CASCADE is the second defect: a profile row going away must not silently
-- erase indexed posts. Author-requested deletion is an explicit tombstone
-- (the comments pattern, migration 021), not a side effect of a foreign key.
ALTER TABLE posts DROP CONSTRAINT IF EXISTS fk_author;

-- +goose Down
-- The rollback is DELIBERATELY not an identical restore: fk_author comes back
-- NOT VALID.
--
-- By the time this migration has been deployed, `posts` holds rows for authors
-- with no `users` row — that is the entire point of dropping the constraint —
-- so a Down that re-adds a VALIDATING foreign key aborts against any real
-- dataset, and the only way to complete it would be to delete real content. A
-- rollback that cannot run is not a rollback. NOT VALID restores the
-- constraint for future writes while leaving the rows already there alone,
-- which is what makes this migration reversible at all. Validating it later
-- (ALTER TABLE posts VALIDATE CONSTRAINT fk_author) is a deliberate,
-- separate act that first requires backfilling the missing users rows.
DROP TABLE IF EXISTS community_post_admissions;

ALTER TABLE posts ADD CONSTRAINT fk_author
    FOREIGN KEY (author_did) REFERENCES users(did) ON DELETE CASCADE NOT VALID;
