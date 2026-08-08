//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The denormalized counters a post row carries — upvote_count, downvote_count,
// score, comment_count — when the post is an AUTHOR-OWNED one
// (social.coves.community.postv2, docs/PRD_AUTHOR_OWNED_POSTS.md §3.1).
//
// Votes and comments name their subject by AT-URI, and neither consumer has any
// other way to learn which table to count into: it reads the collection segment
// out of the subject's URI and routes on it. The author-owned flip changed that
// segment for every new post, so a router that knows only the deprecated NSID
// silently stops counting — the vote row and the comment row are both indexed,
// the counter is simply never touched, and nothing anywhere reports an error.
// That is the failure these tests exist to make loud.
//
// They run against real SQL because the counting IS the SQL: the increment and
// the row it lands on are one statement inside the consumer's transaction, so a
// fake repository would be asserting that the fake counts.

const (
	aggPrefix    = "did:plc:agg"
	aggCommunity = aggPrefix + "community"
	aggAuthor    = aggPrefix + "author"
	aggVoter     = aggPrefix + "voter"
	aggCommenter = aggPrefix + "commenter"
)

// indexAuthorOwnedPost drives a real postv2 create through the post consumer and
// returns the indexed post's URI and CID.
//
// The post is written by the consumer rather than by an INSERT so that the row
// under test is exactly the row production produces — in particular its URI,
// which is the only input the vote and comment consumers route on.
func indexAuthorOwnedPost(t *testing.T, db *sql.DB) (postURI, postCID string) {
	t.Helper()

	insertBridgedUser(t, db, aggAuthor, "aggauthor.test")
	insertBridgedUser(t, db, aggVoter, "aggvoter.test")
	insertBridgedUser(t, db, aggCommenter, "aggcommenter.test")
	insertBridgedCommunity(t, db, aggCommunity, "aggcommunity.test", aggAuthor)

	userService := newMockUserService()
	userService.users[aggAuthor] = &users.User{DID: aggAuthor, Handle: "aggauthor.test"}

	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		userService,
		db,
		WithAdmissions(postgres.NewAdmissionRepository(db)),
	)

	const rkey = "aggpost"
	postURI = pv2URI(aggAuthor, rkey)
	postCID = "bafyreiaggpost"

	require.NoError(t, consumer.HandleEvent(context.Background(), pv2Event(
		aggAuthor, "create", rkey, testkit.TID(), postCID, time.Now().UnixMicro(),
		pv2Record(aggCommunity, "an author-owned post", "the words being voted on"),
	)))
	return postURI, postCID
}

// aggVoteEvent builds a vote commit in the VOTER's repo. The subject is passed
// whole so a test can point a vote at either post collection.
func aggVoteEvent(op, rkey, direction, subjectURI, subjectCID string) *JetstreamEvent {
	return revCommitEvent(aggVoter, "social.coves.feed.vote", op, rkey, testkit.TID(),
		"bafyreivote"+rkey, time.Now().UnixMicro(), map[string]interface{}{
			"$type":     "social.coves.feed.vote",
			"subject":   map[string]interface{}{"uri": subjectURI, "cid": subjectCID},
			"direction": direction,
			"createdAt": "2026-03-02T00:00:00Z",
		})
}

// aggCommentEvent builds a top-level comment commit in the COMMENTER's repo:
// root and parent are both the post, which is what makes it top-level and what
// sends the increment at posts.comment_count rather than at a parent comment.
func aggCommentEvent(rkey, postURI, postCID string) *JetstreamEvent {
	return revCommitEvent(aggCommenter, CommentCollection, "create", rkey, testkit.TID(),
		"bafyreicomment"+rkey, time.Now().UnixMicro(),
		commentRecord("a reply to an author-owned post", postURI, postCID, postURI, postCID, nil))
}

// readAggregates returns the counter columns of a post row.
func readAggregates(t *testing.T, db *sql.DB, uri string) (upvotes, downvotes, score, commentCount int) {
	t.Helper()
	require.NoErrorf(t, db.QueryRow(
		`SELECT upvote_count, downvote_count, score, comment_count FROM posts WHERE uri = $1`, uri,
	).Scan(&upvotes, &downvotes, &score, &commentCount), "no post row for %s", uri)
	return upvotes, downvotes, score, commentCount
}

// A vote on an author-owned post must move that post's counters, in every
// direction and on every operation the consumer handles.
//
// The whole arc is one test because the counters are cumulative state: proving
// the increment in isolation would leave the decrement free to subtract from a
// number it never added to, which is the shape of the defect this covers.
func TestVoteConsumer_CountsVotesOnAuthorOwnedPosts(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	postURI, postCID := indexAuthorOwnedPost(t, db)
	votes := newVoteConsumer(db)

	require.NoError(t, votes.HandleEvent(ctx, aggVoteEvent("create", "up1", "up", postURI, postCID)))
	upvotes, downvotes, score, _ := readAggregates(t, db, postURI)
	require.Equalf(t, 1, upvotes,
		"an upvote on %s must increment upvote_count: the vote consumer routes on the collection in the subject URI, and an author-owned post's URI names postv2", postURI)
	require.Equal(t, 0, downvotes)
	require.Equal(t, 1, score, "score is stored, not derived, so a missed increment is invisible to every feed that sorts by it")

	// The change path: a second vote on the same subject from the same voter,
	// under a different rkey. The consumer soft-deletes the standing vote and
	// must UNDO its contribution before applying the new one — a decrement that
	// routes nowhere leaves the old direction counted forever.
	require.NoError(t, votes.HandleEvent(ctx, aggVoteEvent("create", "down1", "down", postURI, postCID)))
	upvotes, downvotes, score, _ = readAggregates(t, db, postURI)
	require.Equal(t, 0, upvotes, "switching to a downvote must retract the upvote it replaces")
	require.Equal(t, 1, downvotes)
	require.Equal(t, -1, score)

	// The delete path.
	require.NoError(t, votes.HandleEvent(ctx, aggVoteEvent("delete", "down1", "down", postURI, postCID)))
	upvotes, downvotes, score, _ = readAggregates(t, db, postURI)
	require.Equal(t, 0, upvotes)
	require.Equal(t, 0, downvotes, "deleting the vote must decrement the count it added")
	require.Equal(t, 0, score)
}

// A top-level comment on an author-owned post must increment that post's
// comment_count.
func TestCommentConsumer_CountsCommentsOnAuthorOwnedPosts(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	postURI, postCID := indexAuthorOwnedPost(t, db)

	require.NoError(t, newCommentConsumer(db).HandleEvent(ctx, aggCommentEvent("aggc1", postURI, postCID)))

	_, _, _, commentCount := readAggregates(t, db, postURI)
	require.Equalf(t, 1, commentCount,
		"a top-level comment on %s must increment posts.comment_count: the comment consumer routes on the collection in the parent URI, and an author-owned post's URI names postv2", postURI)
}
