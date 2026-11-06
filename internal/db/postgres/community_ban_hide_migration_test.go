//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 069 adds the index-time ban hide markers without rewriting a row:
// comments and votes that existed before it stay visible and counted even when
// their author is under an active community ban, because nobody classified them
// at index time. The ban history the classification reads is initialized from
// the bans already in community_bans, so a comment the banned author writes
// after the migration is hidden.
func TestMigration069PreservesExistingRowsAndInitializesBanHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
		"076 (account status rechecks) must be rolled back first")
	require.EqualValues(t, 75, testkit.MigrateDownOne(t, db, 75),
		"075 (moderation subject kind) must be rolled back first")
	require.EqualValues(t, 74, testkit.MigrateDownOne(t, db, 74),
		"074 (account status restrictions) must be rolled back first")
	require.EqualValues(t, 73, testkit.MigrateDownOne(t, db, 73),
		"073 (account ban hide reason) must be rolled back first")
	require.EqualValues(t, 72, testkit.MigrateDownOne(t, db, 72),
		"072 (account moderation subjects) must be rolled back first")
	require.EqualValues(t, 71, testkit.MigrateDownOne(t, db, 71),
		"071 (labels) must be rolled back first")
	require.EqualValues(t, 70, testkit.MigrateDownOne(t, db, 70),
		"070 (pending ban-check indexes) must be rolled back first")
	require.EqualValues(t, 69, testkit.MigrateDownOne(t, db, 69),
		"069 (ban hide markers) must be rolled back to seed rows at version 68")

	testID := testkit.UniqueID(t)
	communityDID, err := fixtures.Community(ctx, db, "bh8-"+testID, "bh8owner-"+testID+".test")
	require.NoError(t, err)
	authorDID := "did:plc:bh8author" + testID
	fixtures.User(t, db, "bh8author-"+testID+".test", authorDID)
	postURI := fixtures.Post(t, db, communityDID, "did:plc:bh8poster"+testID, "post in C", 0,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	const postCID = "bafytest" // fixtures.Post's CID

	commentURI := "at://" + authorDID + "/" + jetstream.CommentCollection + "/bh8existing"
	_, err = db.Exec(`
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid,
		                      parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafyreibh8existing', 'bh8existing', $2, $3, $4, $3, $4, 'written before 069', $5)`,
		commentURI, authorDID, postURI, postCID, time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC))
	require.NoError(t, err, "seeding A's existing comment")
	voteURI := "at://" + authorDID + "/social.coves.feed.vote/bh8existingvote"
	_, err = db.Exec(`
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafyreibh8vote', 'bh8existingvote', $2, $3, $4, 'up', $5, NOW())`,
		voteURI, authorDID, postURI, postCID, time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC))
	require.NoError(t, err, "seeding A's existing vote")
	_, err = db.Exec(`UPDATE posts SET upvote_count = 1, downvote_count = 0, score = 1, comment_count = 1 WHERE uri = $1`,
		postURI)
	require.NoError(t, err, "setting the post's counts to include A's comment and vote")
	_, err = db.Exec(`
		INSERT INTO community_bans (community_did, subject_did, record_key, record_cid, reason)
		VALUES ($1, $2, $3, 'bafyreibh8ban', 'spam')`,
		communityDID, authorDID, communities.BanRecordKey(authorDID))
	require.NoError(t, err, "seeding A's active ban in C")

	testkit.MigrateUp(t, db)

	var commentReason, voteReason sql.NullString
	require.NoError(t, db.QueryRow(`SELECT hide_reason FROM comments WHERE uri = $1`, commentURI).Scan(&commentReason))
	assert.Equal(t, sql.NullString{}, commentReason, "a comment that predates 069 is not hidden")
	require.NoError(t, db.QueryRow(`SELECT hide_reason FROM votes WHERE uri = $1`, voteURI).Scan(&voteReason))
	assert.Equal(t, sql.NullString{}, voteReason, "a vote that predates 069 is not hidden")

	var upvotes, downvotes, score, commentCount int
	require.NoError(t, db.QueryRow(`SELECT upvote_count, downvote_count, score, comment_count FROM posts WHERE uri = $1`,
		postURI).Scan(&upvotes, &downvotes, &score, &commentCount))
	assert.Equal(t, []int{1, 0, 1, 1}, []int{upvotes, downvotes, score, commentCount},
		"post upvote_count, downvote_count, score, comment_count unchanged by 069")

	consumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	newCommentURI := "at://" + authorDID + "/" + jetstream.CommentCollection + "/bh8new"
	require.NoError(t, consumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Kind:   "commit",
		Did:    authorDID,
		TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev:        testkit.TID(),
			Operation:  "create",
			Collection: jetstream.CommentCollection,
			RKey:       "bh8new",
			CID:        "bafyreibh8new",
			Record: map[string]interface{}{
				"$type":   jetstream.CommentCollection,
				"content": "written after 069",
				"reply": map[string]interface{}{
					"root":   map[string]interface{}{"uri": postURI, "cid": postCID},
					"parent": map[string]interface{}{"uri": postURI, "cid": postCID},
				},
				"createdAt": "2026-03-01T02:00:00Z",
			},
		},
	}), "A's new comment on the C post")

	var newReason sql.NullString
	require.NoError(t, db.QueryRow(`SELECT hide_reason FROM comments WHERE uri = $1`, newCommentURI).Scan(&newReason))
	assert.Equal(t, sql.NullString{String: "community_ban", Valid: true}, newReason,
		"a comment indexed after 069 under a ban that predates it is hidden")
}
