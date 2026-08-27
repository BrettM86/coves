-- +goose Up
-- One-time repair of the vote drift left behind by the pre-gate vote consumer.
--
-- Going forward the consumer holds two invariants: a vote row only lands when
-- its subject row exists, and every live vote row is included in its subject's
-- counts whatever that subject's deleted_at says. Neither held for data written
-- before the fix, and neither is self-healing — the counts are denormalized
-- columns nothing recomputes, so a subject that drifted stays drifted.
--
-- The two halves run in this order on purpose: the sweep retires the rows the
-- recount must not count.

-- 1. SWEEP the legacy orphans: live votes naming a subject that has no row.
--
-- These are the votes the old consumer indexed before their subject arrived. It
-- inserted the row, found nothing to increment, and logged the zero-row update.
-- Left live, each is a decrement waiting to fire, because the withdrawal path
-- subtracts from whatever the stored row says and cannot tell an increment that
-- was applied from one that was skipped.
--
-- Restricted to the three collections whose votes are SUPPOSED to be counted. A
-- vote on any other collection — app.bsky.feed.post above all — is not an orphan
-- at all: it is deliberately-uncounted viewer state for bridged content, and no
-- local row for its subject is ever expected. Sweeping on subject-absence alone
-- would erase every bridged like in the database.
--
-- Soft delete, never DELETE: the row stays auditable, and the withdrawal path
-- can still find it. The cost is cosmetic and bounded — a swept vote whose
-- subject is indexed later reads as un-voted to the voter until they tap again,
-- which is the same thing they see today for a vote that was never counted.
--
-- split_part(subject_uri, '/', 4) is the collection segment: an AT-URI splits as
-- 'at:', '', <repo did>, <collection>, <rkey>.
UPDATE votes AS v
SET deleted_at = NOW()
WHERE v.deleted_at IS NULL
  AND split_part(v.subject_uri, '/', 4) IN ('social.coves.community.post', 'social.coves.community.postv2')
  AND NOT EXISTS (SELECT 1 FROM posts p WHERE p.uri = v.subject_uri);

UPDATE votes AS v
SET deleted_at = NOW()
WHERE v.deleted_at IS NULL
  AND split_part(v.subject_uri, '/', 4) = 'social.coves.community.comment'
  AND NOT EXISTS (SELECT 1 FROM comments c WHERE c.uri = v.subject_uri);

-- 2. RECOUNT every post and comment from the live vote rows.
--
-- Soft-deleted subjects are recounted too: a deleted post is a row that can be
-- restored and whose counts moderation surfaces still read, so skipping it
-- leaves the rows the bug hit hardest wrong and revives corrupt counts on
-- resurrection. Subjects with no live votes are reset rather than skipped, which
-- is what repairs the erasure case — migration 036 hard-deletes an erased
-- account's vote rows, so the only evidence of the inflation they left behind is
-- the absence of rows to justify it.
--
-- The bridged terms are carried through untouched. They are asserted by the
-- origin platform and derivable from no vote row here, so a score rebuilt from
-- native votes alone would discard every bridged aggregate in the database.
UPDATE posts AS p
SET upvote_count = tally.upvotes,
    downvote_count = tally.downvotes,
    score = tally.upvotes - tally.downvotes + p.bridged_upvote_count - p.bridged_downvote_count
FROM (
    SELECT s.uri,
           COUNT(*) FILTER (WHERE v.direction = 'up')   AS upvotes,
           COUNT(*) FILTER (WHERE v.direction = 'down') AS downvotes
    FROM posts s
    LEFT JOIN votes v ON v.subject_uri = s.uri AND v.deleted_at IS NULL
    GROUP BY s.uri
) AS tally
WHERE p.uri = tally.uri;

UPDATE comments AS c
SET upvote_count = tally.upvotes,
    downvote_count = tally.downvotes,
    score = tally.upvotes - tally.downvotes + c.bridged_upvote_count - c.bridged_downvote_count
FROM (
    SELECT s.uri,
           COUNT(*) FILTER (WHERE v.direction = 'up')   AS upvotes,
           COUNT(*) FILTER (WHERE v.direction = 'down') AS downvotes
    FROM comments s
    LEFT JOIN votes v ON v.subject_uri = s.uri AND v.deleted_at IS NULL
    GROUP BY s.uri
) AS tally
WHERE c.uri = tally.uri;

-- +goose Down
-- No-op by design. This migration repairs drifted data, it does not change
-- schema, and a repair cannot be un-repaired: the pre-migration counts and the
-- orphaned live vote rows are not recoverable once corrected. Rolling back
-- leaves the corrected data in place, which is the safe outcome.
SELECT 1;
