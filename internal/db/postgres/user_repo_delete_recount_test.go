//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Account erasure hard-deletes the erased account's votes and comments. Every
// one of those rows was counted into a denormalized column on some other row
// (a post's or comment's vote counts, a post's comment_count, a comment's
// reply_count), and nothing else ever recomputes those columns. Erasure must
// therefore recount exactly the subjects the deleted rows counted into, and
// leave every other row alone.
//
// Count semantics pinned here: vote counts come from live vote rows only, and
// score keeps the origin-asserted bridged terms
// (upvote_count - downvote_count + bridged_upvote_count - bridged_downvote_count);
// comment_count and reply_count count every comment row with that root or
// parent, soft-deleted ones included.
func TestUserRepo_Delete_RecountsSubjectsOfErasedVotesAndComments(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	repo := NewUserRepository(db)

	communityName := testkit.UniqueIDWithPrefix(t, "erasecount")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)

	// A is erased; B is a surviving account; the author owns every subject row
	// except A's own comments, so erasure cannot remove the subjects themselves.
	erasedDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "erased")
	survivorDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "survivor")
	authorDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "author")
	// C withdrew its votes; its soft-deleted rows must not be counted.
	withdrawnDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "withdrawn")

	_, err = repo.Create(ctx, &users.User{DID: erasedDID, Handle: testkit.UniqueIDWithPrefix(t, "erased") + ".test", PDSURL: "https://test.pds"})
	require.NoError(t, err)

	// 1. Post subject: A up, B up, C's withdrawn up. Stored counts agree with
	// the live votes.
	votedPost := seedErasurePost(t, db, communityDID, authorDID, "voted", recountCounts{upvotes: 2, downvotes: 0, score: 2}, 0, 0, 0)
	seedErasureVote(t, db, erasedDID, votedPost, "up")
	seedErasureVote(t, db, survivorDID, votedPost, "up")
	seedErasureWithdrawnVote(t, db, withdrawnDID, votedPost, "up")

	// 2. Comment subject: A up, B down, C's withdrawn down.
	commentHost := seedErasurePost(t, db, communityDID, authorDID, "commenthost", recountCounts{}, 0, 0, 2)
	votedComment := seedErasureComment(t, db, authorDID, commentHost, commentHost, "voted", recountCounts{upvotes: 1, downvotes: 1, score: 0}, 0, false)
	seedErasureVote(t, db, erasedDID, votedComment, "up")
	seedErasureVote(t, db, survivorDID, votedComment, "down")
	seedErasureWithdrawnVote(t, db, withdrawnDID, votedComment, "down")

	// Neighbour comment: B voted here, A never touched it. Its stored vote
	// counts (5 stored, 1 live) and reply_count (4 stored, no replies) are
	// deliberately wrong, so a comment recount that reaches beyond the erased
	// account's subjects shows up.
	neighbourComment := seedErasureComment(t, db, authorDID, commentHost, commentHost, "neighbour", recountCounts{upvotes: 5, downvotes: 0, score: 5}, 4, false)
	seedErasureVote(t, db, survivorDID, neighbourComment, "up")

	// 3. Bridged post: origin asserts 3 up / 1 down. A up, B down, so the
	// stored score is 1-1+3-1 = 2.
	bridgedPost := seedErasurePost(t, db, communityDID, authorDID, "bridged", recountCounts{upvotes: 1, downvotes: 1, score: 2}, 3, 1, 0)
	seedErasureVote(t, db, erasedDID, bridgedPost, "up")
	seedErasureVote(t, db, survivorDID, bridgedPost, "down")

	// 4. Thread on post Q. Rows with root_uri = Q:
	//   D  (author, top-level)        <- the parent under test
	//   E  (survivor, top-level)
	//   A1 (erased, top-level)
	//   A2 (erased, reply to D)
	//   F  (survivor, reply to D)
	//   Fd (survivor, reply to D, soft-deleted; still counted)
	//   G  (survivor, reply to A1; its parent goes, it stays)
	// Before: Q.comment_count = 7, D.reply_count = 3 (A2, F, Fd).
	// After erasing A: Q.comment_count = 5, D.reply_count = 2.
	threadPost := seedErasurePost(t, db, communityDID, authorDID, "thread", recountCounts{}, 0, 0, 7)
	parentComment := seedErasureComment(t, db, authorDID, threadPost, threadPost, "parent", recountCounts{}, 3, false)
	seedErasureComment(t, db, survivorDID, threadPost, threadPost, "survivortop", recountCounts{}, 0, false)
	erasedTop := seedErasureComment(t, db, erasedDID, threadPost, threadPost, "erasedtop", recountCounts{}, 1, false)
	seedErasureComment(t, db, erasedDID, threadPost, parentComment, "erasedreply", recountCounts{}, 0, false)
	seedErasureComment(t, db, survivorDID, threadPost, parentComment, "survivorreply", recountCounts{}, 0, false)
	seedErasureComment(t, db, survivorDID, threadPost, parentComment, "survivordeleted", recountCounts{}, 0, true)
	seedErasureComment(t, db, survivorDID, threadPost, erasedTop, "replytoerased", recountCounts{}, 0, false)

	// 5. Neighbour: B voted here, A never touched it. Its stored counts are
	// deliberately NOT what its live votes say (5 stored, 1 live), so any
	// recount that reaches beyond the erased account's subjects shows up.
	// Same for its comment_count (4 stored, no comment rows).
	neighbourPost := seedErasurePost(t, db, communityDID, authorDID, "neighbour", recountCounts{upvotes: 5, downvotes: 0, score: 5}, 0, 0, 4)
	seedErasureVote(t, db, survivorDID, neighbourPost, "up")

	require.NoError(t, repo.Delete(ctx, erasedDID))

	t.Run("post vote counts drop the erased vote", func(t *testing.T) {
		assert.Equal(t, recountCounts{upvotes: 1, downvotes: 0, score: 1}, postCountsOf(t, db, votedPost),
			"after erasing A only B's up-vote is live")
	})

	t.Run("comment vote counts drop the erased vote", func(t *testing.T) {
		assert.Equal(t, recountCounts{upvotes: 0, downvotes: 1, score: -1}, commentCountsOf(t, db, votedComment),
			"after erasing A only B's down-vote is live")
	})

	t.Run("bridged terms survive the recount", func(t *testing.T) {
		assert.Equal(t, recountCounts{upvotes: 0, downvotes: 1, score: 1}, postCountsOf(t, db, bridgedPost),
			"score must be 0-1+3-1 = 1; the bridged 3/1 is not derivable from vote rows and must be kept")
	})

	t.Run("comment_count and reply_count drop the erased comments", func(t *testing.T) {
		assert.Equal(t, 5, postCommentCountOf(t, db, threadPost),
			"Q keeps D, E, F, soft-deleted Fd and G once A1 and A2 are erased")
		assert.Equal(t, 2, commentReplyCountOf(t, db, parentComment),
			"D keeps F and soft-deleted Fd once A2 is erased")
	})

	t.Run("unrelated subject is not recounted", func(t *testing.T) {
		assert.Equal(t, recountCounts{upvotes: 5, downvotes: 0, score: 5}, postCountsOf(t, db, neighbourPost),
			"A never voted here; erasure must not rewrite its stored counts")
		assert.Equal(t, 4, postCommentCountOf(t, db, neighbourPost),
			"A never commented here; erasure must not rewrite its stored comment_count")
		assert.Equal(t, recountCounts{upvotes: 5, downvotes: 0, score: 5}, commentCountsOf(t, db, neighbourComment),
			"A never voted on this comment; erasure must not rewrite its stored counts")
		assert.Equal(t, 4, commentReplyCountOf(t, db, neighbourComment),
			"A never replied here; erasure must not rewrite its stored reply_count")
	})
}

func seedErasurePost(t *testing.T, db *sql.DB, communityDID, authorDID, name string, stored recountCounts, bridgedUp, bridgedDown, commentCount int) string {
	t.Helper()

	rkey := testkit.TID()
	uri := "at://" + communityDID + "/social.coves.community.post/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at,
		                   upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count, comment_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, uri, "bafyerasure"+name, rkey, authorDID, communityDID, "erasure fixture "+name,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		stored.upvotes, stored.downvotes, stored.score, bridgedUp, bridgedDown, commentCount)
	require.NoErrorf(t, err, "seeding the %s post fixture", name)
	return uri
}

func seedErasureComment(t *testing.T, db *sql.DB, commenterDID, rootURI, parentURI, name string, stored recountCounts, replyCount int, deleted bool) string {
	t.Helper()

	rkey := testkit.TID()
	uri := "at://" + commenterDID + "/social.coves.community.comment/" + rkey
	var deletedAt *time.Time
	if deleted {
		when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		deletedAt = &when
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content,
		                      created_at, deleted_at, upvote_count, downvote_count, score, reply_count)
		VALUES ($1, $2, $3, $4, $5, 'bafyerasureroot', $6, 'bafyerasureparent', $7, $8, $9, $10, $11, $12, $13)
	`, uri, "bafyerasure"+name, rkey, commenterDID, rootURI, parentURI, "erasure fixture "+name,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), deletedAt,
		stored.upvotes, stored.downvotes, stored.score, replyCount)
	require.NoErrorf(t, err, "seeding the %s comment fixture", name)
	return uri
}

func seedErasureVote(t *testing.T, db *sql.DB, voterDID, subjectURI, direction string) {
	t.Helper()

	rkey := testkit.TID()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafyerasurevote', $2, $3, $4, 'bafyerasuresubject', $5, $6, NOW())
	`, "at://"+voterDID+"/social.coves.feed.vote/"+rkey, rkey, voterDID, subjectURI, direction,
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	require.NoErrorf(t, err, "seeding %s's vote on %s", voterDID, subjectURI)
}

func seedErasureWithdrawnVote(t *testing.T, db *sql.DB, voterDID, subjectURI, direction string) {
	t.Helper()

	rkey := testkit.TID()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at, deleted_at)
		VALUES ($1, 'bafyerasurevote', $2, $3, $4, 'bafyerasuresubject', $5, $6, NOW(), $7)
	`, "at://"+voterDID+"/social.coves.feed.vote/"+rkey, rkey, voterDID, subjectURI, direction,
		time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC))
	require.NoErrorf(t, err, "seeding %s's withdrawn vote on %s", voterDID, subjectURI)
}

func postCommentCountOf(t *testing.T, db *sql.DB, uri string) int {
	t.Helper()

	var count int
	require.NoErrorf(t, db.QueryRowContext(context.Background(),
		`SELECT comment_count FROM posts WHERE uri = $1`, uri).Scan(&count), "reading comment_count of post %s", uri)
	return count
}

func commentReplyCountOf(t *testing.T, db *sql.DB, uri string) int {
	t.Helper()

	var count int
	require.NoErrorf(t, db.QueryRowContext(context.Background(),
		`SELECT reply_count FROM comments WHERE uri = $1`, uri).Scan(&count), "reading reply_count of comment %s", uri)
	return count
}
