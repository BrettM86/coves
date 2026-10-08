//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestVoteConsumer_RemovedPostCannotCreateOrBumpGroup(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"new group", "existing group", "downvote replaced by upvote"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newUpvoteGroupFixture(t)
			consumer := f.consumer()
			var groupID int64
			var originalSort time.Time
			if kind != "new group" {
				deliverGroupVote(t, consumer, f.voter(t), f.post, "up", f.createdAt)
				groupID, _ = maintenanceGroup(t, f)
				ageMaintenanceGroup(t, f.db, groupID)
				require.NoError(t, f.db.QueryRow(`SELECT sort_at FROM notifications WHERE id = $1`, groupID).Scan(&originalSort))
			}
			voter := f.voter(t)
			var oldVote string
			if kind == "downvote replaced by upvote" {
				oldVote = deliverGroupVote(t, consumer, voter, f.post, "down", f.createdAt)
				require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM votes WHERE uri = $1 AND direction = 'down' AND deleted_at IS NULL`, oldVote))
			}
			removeNotificationReferencePost(t, f.db, f.post)
			if oldVote == "" {
				deliverGroupVote(t, consumer, voter, f.post, "up", f.createdAt)
			} else {
				replaceMaintenanceVote(t, f, consumer, voter, "up", oldVote)
			}
			if kind == "new group" {
				require.Zero(t, groupCount(t, f.db, f.author, f.post), "removed post must not start a group")
				return
			}
			var storedID int64
			var storedSort time.Time
			require.NoError(t, f.db.QueryRow(`SELECT id, sort_at FROM notifications WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
				f.author, f.post).Scan(&storedID, &storedSort))
			require.Equal(t, groupID, storedID)
			require.Truef(t, storedSort.Equal(originalSort), "removed post group sort_at changed from %s to %s", originalSort, storedSort)
		})
	}
}

func TestVoteConsumer_WithdrawnCommentRootCannotCreateGroup(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"deleted root", "removed root"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newUpvoteGroupFixture(t)
			commenter := f.voter(t)
			key := testkit.TID()
			commentURI := "at://" + commenter + "/" + CommentCollection + "/" + key
			_, err := f.db.Exec(`INSERT INTO comments
				(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
				VALUES ($1, 'bafupvotesubject', $2, $3, $4, 'bafupvoteroot', $4, 'bafupvoteroot', 'comment', NOW())`,
				commentURI, key, commenter, f.post)
			require.NoError(t, err)
			if kind == "removed root" {
				removeNotificationReferencePost(t, f.db, f.post)
			} else {
				_, err = f.db.Exec(`UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, f.post)
				require.NoError(t, err)
			}
			deliverGroupVote(t, f.consumer(), f.voter(t), commentURI, "up", f.createdAt)
			require.Zero(t, groupCount(t, f.db, commenter, commentURI), "withdrawn root must suppress the comment upvote group")
		})
	}
}

func TestVoteConsumer_RetractionStillDeletesLastGroupAfterRemoval(t *testing.T) {
	t.Parallel()
	f := newUpvoteGroupFixture(t)
	consumer := f.consumer()
	voter := f.voter(t)
	key := testkit.TID()
	created := maintenanceVoteAtKey(voter, f.post, f.createdAt, revA, key)
	require.NoError(t, consumer.HandleEvent(context.Background(), created))
	var groupID int64
	var sortAt time.Time
	require.NoError(t, f.db.QueryRow(`SELECT id, sort_at FROM notifications WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`,
		f.author, f.post).Scan(&groupID, &sortAt))
	require.NotZero(t, groupID)
	removeNotificationReferencePost(t, f.db, f.post)
	require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(voter, "social.coves.feed.vote", "delete", key,
		revB, "", created.TimeUS+1_000_000, nil)))
	var deletedAt sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deleted_at FROM votes WHERE uri = $1`,
		"at://"+voter+"/social.coves.feed.vote/"+key).Scan(&deletedAt))
	require.True(t, deletedAt.Valid)
	require.Zero(t, groupCount(t, f.db, f.author, f.post), "retracting the last upvote deletes the old group even after removal")
}
