//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 040, the one-time repair of vote drift left behind by the consumer
// this branch fixes.
//
// Going forward the vote consumer holds two invariants: no vote row lands
// unless its subject row exists, and every live vote row on a post or comment
// subject is included in that subject's counts — unconditionally on the
// subject's deleted_at. Neither invariant holds for data written BEFORE the
// fix, and neither is self-healing: the counts are denormalized columns nothing
// recomputes, so a subject that drifted stays drifted forever. This migration
// is what establishes the invariant retroactively.
//
// # THE TWO HALVES, AND WHY THE SWEEP IS NARROW
//
// (a) The sweep soft-deletes LEGACY ORPHANS: live vote rows naming a subject
// that has no row in this database. Those are the votes the old consumer
// indexed before their subject arrived — it inserted the row, failed to find a
// subject to increment, and logged the zero-row UPDATE. Left live, each one is
// a decrement waiting to happen, because deleteVote subtracts from whatever the
// stored row says and cannot tell an increment that was applied from one that
// was skipped.
//
// The sweep must therefore be restricted to the three collections whose votes
// are supposed to be counted — social.coves.community.post,
// social.coves.community.postv2, social.coves.community.comment. A vote on any
// OTHER collection, app.bsky.feed.post above all, is not an orphan at all: it
// is deliberately-uncounted viewer state for bridged content, indexed so the
// voter's own like renders back to them, and there is BY DESIGN no local row
// for its subject. A sweep written as "no subject row exists" rather than "no
// subject row exists AND this collection was supposed to have one" would erase
// every bridged like in the database.
//
// (b) The recount rebuilds upvote_count, downvote_count and score for ALL posts
// and comments from the live vote rows. Two properties of that recount are easy
// to get wrong and are pinned below:
//
//   - Soft-deleted subjects are recounted TOO. A deleted post is not gone; it
//     is a row that can be restored, and one whose counts are still read by
//     moderation surfaces. Skipping deleted subjects leaves exactly the rows the
//     old bug hit hardest still wrong, and resurrection then revives corrupt
//     counts.
//   - The bridged terms SURVIVE. score is
//     upvote_count - downvote_count + bridged_upvote_count - bridged_downvote_count,
//     and the bridged halves are asserted by the origin platform, not derivable
//     from any vote row here. A recount that rebuilt score from native votes
//     alone would silently discard every bridged aggregate in the database.

// recountCounts is the denormalized vote state of one subject row.
type recountCounts struct {
	upvotes   int
	downvotes int
	score     int
}

func TestMigration040_RecountsVoteDriftAndSweepsLegacyOrphans(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	// Roll the newer migrations and 040 off, seed the pre-repair state, then roll them back on: the
	// migration has to find drifted rows already present, which is the whole
	// point of a repair migration and cannot be observed by seeding after it has
	// run. Asserting the version that came off is the tripwire that keeps this
	// pointed at 040 when later migrations land.
	require.EqualValues(t, 46, testkit.MigrateDownOne(t, db, 46),
		"046 (drop encryption_keys) sits on top of 045 and must be rolled back first")
	require.EqualValues(t, 45, testkit.MigrateDownOne(t, db, 45),
		"045 (the community subscriber recount) sits on top of 044 and must be rolled back first")
	require.EqualValues(t, 44, testkit.MigrateDownOne(t, db, 44),
		"044 (the posts search vector column and index) sits on top of 043 and must be rolled back next")
	require.EqualValues(t, 43, testkit.MigrateDownOne(t, db, 43),
		"043 (the bridged-vote poll watermark) sits on top of 042 and must be rolled back next")
	require.EqualValues(t, 42, testkit.MigrateDownOne(t, db, 42),
		"042 (the dead-letter retention index) sits on top of 041 and must be rolled back next")
	require.EqualValues(t, 41, testkit.MigrateDownOne(t, db, 41),
		"041 (the future comment created_at repair) sits on top of 040 and must be rolled back next")
	require.EqualValues(t, 40, testkit.MigrateDownOne(t, db, 40),
		"this test seeds the state migration 040 repairs; rolling back a different migration would seed against the wrong schema")

	communityName := testkit.UniqueIDWithPrefix(t, "drift")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)

	// A live post whose stored counts drifted upward: two live up-votes, one
	// live DOWN-vote and one withdrawn (soft-deleted) up-vote, recorded as five
	// up and one down. The bridged 3/0 is consistent with the stored score
	// (5-1+3-0 = 7), so nothing about this row looks wrong until it is
	// recounted. Both directions are seeded because the recount rebuilds two
	// columns from one scan of the vote rows, and a fixture with no down-votes
	// leaves the downvote half of that statement unexercised — it would pass
	// just as well if it counted every vote as an upvote.
	driftedPost := seedRecountPost(t, db, communityDID, "drifted", recountCounts{upvotes: 5, downvotes: 1, score: 7}, 3, 0, false)
	seedRecountVote(t, db, "drift-live-a", driftedPost, "up", true)
	seedRecountVote(t, db, "drift-live-b", driftedPost, "up", true)
	seedRecountVote(t, db, "drift-live-down", driftedPost, "down", true)
	seedRecountVote(t, db, "drift-withdrawn", driftedPost, "up", false)

	// A SOFT-DELETED comment. One live up-vote, stored as three. Its bridged
	// 2/1 is the other half of the score claim: the comments statement is a
	// separate UPDATE from the posts one, and preserving the bridged terms in
	// one is no evidence at all about the other.
	deletedComment := seedRecountComment(t, db, "deleted", recountCounts{upvotes: 3, downvotes: 0, score: 4}, 2, 1, true)
	seedRecountVote(t, db, "deleted-comment-live", deletedComment, "up", true)

	// The erasure case: a post crediting two up-votes whose vote rows are gone
	// outright. Account erasure — the migration-036 marker flow, whose deletes
	// live in user_repo.DeleteUser — HARD-deletes the erased user's vote rows
	// rather than tombstoning them, so nothing decremented the subject
	// on the way out and the only remaining evidence of the inflation is the
	// absence of rows to justify it.
	erasedPost := seedRecountPost(t, db, communityDID, "erased", recountCounts{upvotes: 2, downvotes: 0, score: 2}, 0, 0, false)

	// Legacy orphans: live votes naming subjects that have no row, one for each
	// collection whose votes are supposed to be counted. All three must be swept
	// — a repair that handled only the legacy post collection would leave postv2
	// and comment votes armed with the same latent decrement.
	orphanVoteURIs := map[string]string{
		"social.coves.community.post":    seedRecountOrphanVote(t, db, "orphan-legacy", "social.coves.community.post"),
		"social.coves.community.postv2":  seedRecountOrphanVote(t, db, "orphan-postv2", "social.coves.community.postv2"),
		"social.coves.community.comment": seedRecountOrphanVote(t, db, "orphan-comment", "social.coves.community.comment"),
	}

	// The bridged like: same shape as an orphan — a live vote whose subject has
	// no row anywhere — and it must survive, because no row is ever expected for
	// an app.bsky.feed.post subject.
	bridgedVoteURI := seedRecountOrphanVote(t, db, "bridged-like", "app.bsky.feed.post")

	testkit.MigrateUp(t, db)

	assert.Equal(t, recountCounts{upvotes: 2, downvotes: 1, score: 4}, postCountsOf(t, db, driftedPost),
		"the live post must be recounted from its three live votes in both directions, keeping the bridged 3/0 in the score (2-1+3-0)")

	assert.Equal(t, recountCounts{upvotes: 1, downvotes: 0, score: 2}, commentCountsOf(t, db, deletedComment),
		"a soft-deleted subject must be recounted too, and its bridged 2/1 must survive the recount (1-0+2-1); skipping deleted rows leaves corrupt counts to be revived if the comment is ever restored")

	assert.Equal(t, recountCounts{upvotes: 0, downvotes: 0, score: 0}, postCountsOf(t, db, erasedPost),
		"a post whose voters were erased has no live vote rows left, so its recounted totals are zero; erasure-inflated counts are exactly what nothing else repairs")

	for collection, voteURI := range orphanVoteURIs {
		assert.Falsef(t, voteIsLive(t, db, voteURI),
			"the legacy orphan vote on %s is still live; every live vote on a countable collection whose subject has no row is a decrement waiting to fire the moment the voter withdraws it", collection)
	}

	assert.True(t, voteIsLive(t, db, bridgedVoteURI),
		"the vote on an app.bsky.feed.post subject was swept; bridged likes are deliberately uncounted viewer state with no local subject row by design, and sweeping on subject-absence alone erases all of them")
}

// seedRecountPost writes one post row with the counts a drifted subject carries
// before the repair. Direct SQL rather than the post repository: what is under
// test is a migration acting on rows already in the table, and stored counts
// that disagree with the vote rows are a state no writer will produce on
// request.
func seedRecountPost(t *testing.T, db *sql.DB, communityDID, name string, stored recountCounts, bridgedUp, bridgedDown int, deleted bool) string {
	t.Helper()

	rkey := testkit.TID()
	uri := "at://" + communityDID + "/social.coves.community.post/" + rkey
	var deletedAt *time.Time
	if deleted {
		when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		deletedAt = &when
	}

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, deleted_at,
		                   upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, uri, "bafyrecount"+name, rkey, fixtures.DID("recountauthor"), communityDID, "recount fixture "+name,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), deletedAt,
		stored.upvotes, stored.downvotes, stored.score, bridgedUp, bridgedDown)
	require.NoErrorf(t, err, "seeding the %s post fixture", name)
	return uri
}

// seedRecountComment is seedRecountPost's counterpart for the other countable
// subject kind. Comments are recounted by the same migration and are the only
// place the soft-deleted-subject case is pinned, so the two cannot share one
// fixture.
func seedRecountComment(t *testing.T, db *sql.DB, name string, stored recountCounts, bridgedUp, bridgedDown int, deleted bool) string {
	t.Helper()

	rkey := testkit.TID()
	commenterDID := fixtures.DID("recountcommenter")
	uri := "at://" + commenterDID + "/social.coves.community.comment/" + rkey
	var deletedAt *time.Time
	if deleted {
		when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		deletedAt = &when
	}

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content,
		                      created_at, deleted_at, upvote_count, downvote_count, score,
		                      bridged_upvote_count, bridged_downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`, uri, "bafyrecount"+name, rkey, commenterDID,
		"at://"+commenterDID+"/social.coves.community.post/recountroot", "bafyrecountroot",
		"recount fixture "+name, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), deletedAt,
		stored.upvotes, stored.downvotes, stored.score, bridgedUp, bridgedDown)
	require.NoErrorf(t, err, "seeding the %s comment fixture", name)
	return uri
}

// seedRecountVote writes one vote row against an existing subject. Each vote
// gets its own voter DID: the unique index on (voter_did, subject_uri) covers
// live rows only, and distinct voters keep a withdrawn vote from reading as the
// same person's changed mind when the fixture is about the count.
func seedRecountVote(t *testing.T, db *sql.DB, name, subjectURI, direction string, live bool) string {
	t.Helper()

	voterDID := fixtures.DID("recountvoter" + name)
	uri := "at://" + voterDID + "/social.coves.feed.vote/" + testkit.TID()
	var deletedAt *time.Time
	if !live {
		when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		deletedAt = &when
	}

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9)
	`, uri, "bafyrecountvote"+name, name, voterDID, subjectURI, "bafyrecountsubject", direction,
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), deletedAt)
	require.NoErrorf(t, err, "seeding the %s vote fixture", name)
	return uri
}

// seedRecountOrphanVote writes a live vote naming a subject in collection that
// has no row in this database — the shape the old consumer left behind, and the
// shape a bridged like has permanently. Nothing is inserted for the subject
// repo: an AT-URI is fully constructible before (or without) the record it
// names, which is how these rows come to exist in the first place.
func seedRecountOrphanVote(t *testing.T, db *sql.DB, name, collection string) string {
	t.Helper()

	subjectURI := "at://" + fixtures.DID("recountsubject"+name) + "/" + collection + "/" + testkit.TID()
	return seedRecountVote(t, db, name, subjectURI, "up", true)
}

func postCountsOf(t *testing.T, db *sql.DB, uri string) recountCounts {
	t.Helper()

	var counts recountCounts
	require.NoErrorf(t, db.QueryRowContext(context.Background(), `
		SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1
	`, uri).Scan(&counts.upvotes, &counts.downvotes, &counts.score), "reading back the counts of post %s", uri)
	return counts
}

func commentCountsOf(t *testing.T, db *sql.DB, uri string) recountCounts {
	t.Helper()

	var counts recountCounts
	require.NoErrorf(t, db.QueryRowContext(context.Background(), `
		SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1
	`, uri).Scan(&counts.upvotes, &counts.downvotes, &counts.score), "reading back the counts of comment %s", uri)
	return counts
}

// voteIsLive reports whether the vote row is still active. The row must still
// EXIST either way — the repair soft-deletes orphans so the withdrawal path can
// still find them and so the sweep is auditable; a hard delete would be
// indistinguishable from a vote that was never indexed.
func voteIsLive(t *testing.T, db *sql.DB, uri string) bool {
	t.Helper()

	var deletedAt sql.NullTime
	require.NoErrorf(t, db.QueryRowContext(context.Background(), `
		SELECT deleted_at FROM votes WHERE uri = $1
	`, uri).Scan(&deletedAt), "the vote row %s is gone; the repair must soft-delete, never hard-delete", uri)
	return !deletedAt.Valid
}
