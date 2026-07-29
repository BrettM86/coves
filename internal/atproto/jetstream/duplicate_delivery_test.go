//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests prove duplicate-delivery idempotency of the count-mutating
// consumers against the real test database. Duplicates are GUARANTEED in
// production: after every reconnect the connector rewinds the cursor 5s and
// replays events, and the DeadLetterRedriver re-invokes HandleEvent on events
// that already partially ran. A vote or comment delivered twice must mutate the
// denormalized counters exactly once.

const (
	dupTestPrefix    = "did:plc:jsdup"
	dupTestCommunity = dupTestPrefix + "community"
	dupTestAuthor    = dupTestPrefix + "author"
	dupTestVoter1    = dupTestPrefix + "voter1"
	dupTestVoter2    = dupTestPrefix + "voter2"
	dupTestCommenter = dupTestPrefix + "commenter"
)

func cleanupDupTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	_, _ = db.Exec("DELETE FROM votes WHERE voter_did LIKE $1", dupTestPrefix+"%")
	_, _ = db.Exec("DELETE FROM comments WHERE commenter_did LIKE $1 OR root_uri LIKE $2", dupTestPrefix+"%", "at://"+dupTestPrefix+"%")
	_, _ = db.Exec("DELETE FROM posts WHERE community_did LIKE $1", dupTestPrefix+"%")
	_, _ = db.Exec("DELETE FROM communities WHERE did LIKE $1", dupTestPrefix+"%")
	_, _ = db.Exec("DELETE FROM users WHERE did LIKE $1", dupTestPrefix+"%")
}

// setupDupFixtures indexes a user, community and one post, returning the post URI/CID.
func setupDupFixtures(t *testing.T, db *sql.DB) (postURI, postCID string) {
	t.Helper()
	insertBridgedUser(t, db, dupTestAuthor, "dupauthor.test")
	insertBridgedUser(t, db, dupTestCommenter, "dupcommenter.test")
	insertBridgedCommunity(t, db, dupTestCommunity, "dupcommunity.test", dupTestAuthor)

	us := newMockUserService()
	us.users[dupTestAuthor] = &users.User{DID: dupTestAuthor, Handle: "dupauthor.test"}
	pc := NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), us, db)

	postURI = "at://" + dupTestCommunity + "/social.coves.community.post/dup1"
	postCID = "bafdup1"
	require.NoError(t, pc.HandleEvent(context.Background(), &JetstreamEvent{
		Kind:   "commit",
		Did:    dupTestCommunity,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Operation:  "create",
			Collection: "social.coves.community.post",
			RKey:       "dup1",
			CID:        postCID,
			Record: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": dupTestCommunity,
				"author":    dupTestAuthor,
				"title":     "dup target",
				"content":   "body",
				"createdAt": "2026-03-01T00:00:00Z",
			},
		},
	}))
	return postURI, postCID
}

func dupVoteEvent(voterDID, rkey, op, subjectURI, subjectCID, direction string) *JetstreamEvent {
	var record map[string]interface{}
	if op != "delete" {
		record = map[string]interface{}{
			"subject":   map[string]interface{}{"uri": subjectURI, "cid": subjectCID},
			"direction": direction,
			"createdAt": "2026-03-01T01:00:00Z",
		}
	}
	return &JetstreamEvent{
		Kind:   "commit",
		Did:    voterDID,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Operation:  op,
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        "bafdupvote",
			Record:     record,
		},
	}
}

func readDupPostCounts(t *testing.T, db *sql.DB, uri string) (upvotes, downvotes, score, commentCount int) {
	t.Helper()
	require.NoError(t, db.QueryRow(
		`SELECT upvote_count, downvote_count, score, comment_count FROM posts WHERE uri=$1`, uri,
	).Scan(&upvotes, &downvotes, &score, &commentCount))
	return
}

func TestVoteConsumer_DuplicateCreate_IncrementsExactlyOnce(t *testing.T) {
	db := setupBridgedTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupDupTestData(t, db)
	cleanupDupTestData(t, db)

	postURI, postCID := setupDupFixtures(t, db)
	vc := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	ctx := context.Background()

	// Deliver the SAME vote-create commit twice (rewind duplicate).
	event := dupVoteEvent(dupTestVoter1, "dv1", "create", postURI, postCID, "up")
	require.NoError(t, vc.HandleEvent(ctx, event))
	require.NoError(t, vc.HandleEvent(ctx, event))

	up, down, score, _ := readDupPostCounts(t, db, postURI)
	assert.Equal(t, 1, up, "duplicate vote create must increment upvote_count exactly once")
	assert.Equal(t, 0, down)
	assert.Equal(t, 1, score, "score must reflect exactly one upvote")

	var voteRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE voter_did=$1 AND deleted_at IS NULL`, dupTestVoter1,
	).Scan(&voteRows))
	assert.Equal(t, 1, voteRows, "exactly one active vote row")
}

func TestVoteConsumer_DuplicateDelete_DecrementsExactlyOnce(t *testing.T) {
	db := setupBridgedTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupDupTestData(t, db)
	cleanupDupTestData(t, db)

	postURI, postCID := setupDupFixtures(t, db)
	vc := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	ctx := context.Background()

	// Two upvotes from distinct voters, so a double-decrement would be visible
	// (1 → -? ) instead of being masked by the GREATEST(0, ...) floor at zero.
	require.NoError(t, vc.HandleEvent(ctx, dupVoteEvent(dupTestVoter1, "dv1", "create", postURI, postCID, "up")))
	require.NoError(t, vc.HandleEvent(ctx, dupVoteEvent(dupTestVoter2, "dv2", "create", postURI, postCID, "up")))

	up, _, _, _ := readDupPostCounts(t, db, postURI)
	require.Equal(t, 2, up, "fixture: two upvotes indexed")

	// Deliver the SAME vote-delete commit twice (rewind duplicate).
	deleteEvent := dupVoteEvent(dupTestVoter1, "dv1", "delete", postURI, postCID, "")
	require.NoError(t, vc.HandleEvent(ctx, deleteEvent))
	require.NoError(t, vc.HandleEvent(ctx, deleteEvent))

	up, down, score, _ := readDupPostCounts(t, db, postURI)
	assert.Equal(t, 1, up, "duplicate vote delete must decrement upvote_count exactly once")
	assert.Equal(t, 0, down)
	assert.Equal(t, 1, score, "score must reflect exactly one remaining upvote")
}

func TestCommentConsumer_DuplicateCreate_CountsExactlyOnce(t *testing.T) {
	db := setupBridgedTestDB(t)
	defer func() { _ = db.Close() }()
	defer cleanupDupTestData(t, db)
	cleanupDupTestData(t, db)

	postURI, postCID := setupDupFixtures(t, db)
	cc := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	ctx := context.Background()

	event := &JetstreamEvent{
		Kind:   "commit",
		Did:    dupTestCommenter,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Operation:  "create",
			Collection: CommentCollection,
			RKey:       "dc1",
			CID:        "bafdupc1",
			Record: map[string]interface{}{
				"$type":   CommentCollection,
				"content": "hello once",
				"reply": map[string]interface{}{
					"root":   map[string]interface{}{"uri": postURI, "cid": postCID},
					"parent": map[string]interface{}{"uri": postURI, "cid": postCID},
				},
				"createdAt": "2026-03-01T02:00:00Z",
			},
		},
	}

	// Deliver the SAME comment-create commit twice (rewind duplicate).
	require.NoError(t, cc.HandleEvent(ctx, event))
	require.NoError(t, cc.HandleEvent(ctx, event))

	_, _, _, commentCount := readDupPostCounts(t, db, postURI)
	assert.Equal(t, 1, commentCount, "duplicate comment create must increment comment_count exactly once")

	var commentRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM comments WHERE commenter_did=$1`, dupTestCommenter,
	).Scan(&commentRows))
	assert.Equal(t, 1, commentRows, "exactly one comment row indexed")
}
