//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func insertDeleteTestGroup(t *testing.T, fixture upvoteGroupFixture, recipient, subject string, sortAt time.Time) int64 {
	t.Helper()
	var id int64
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $3, $4) RETURNING id`,
		recipient, subject, fixture.rootPostURI, sortAt).Scan(&id))
	return id
}

func deleteTestGroupCount(t *testing.T, fixture upvoteGroupFixture, recipient, subject string) int {
	t.Helper()
	var count int
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND subject_uri = $2 AND reason = 'upvote'`,
		recipient, subject).Scan(&count))
	return count
}

func deleteIfEmptyIntent(recipient, subject string) notifications.UpvoteGroupIntent {
	return notifications.UpvoteGroupIntent{
		Action:       notifications.UpvoteGroupDeleteIfEmpty,
		RecipientDID: recipient, SubjectURI: subject,
	}
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DeleteIfEmpty_QualifyingVotePreservesGroupAndSort(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	pastTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	groupID := insertDeleteTestGroup(t, fixture, fixture.recipientDID, fixture.subjectURI, pastTime)
	emptySubject := fixture.subjectURI + "-empty"
	insertDeleteTestGroup(t, fixture, fixture.recipientDID, emptySubject, pastTime)
	votes := qualifyingUpvoteFixture{db: fixture.db}
	votes.insertVote(t, "did:plc:"+testkit.UniqueID(t)+"voter", fixture.subjectURI, "up", time.Now().UTC(), false)

	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction,
		deleteIfEmptyIntent(fixture.recipientDID, emptySubject)),
		"positive control: an empty group in this fixture must be deletable")
	var emptyCount int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT count(*) FROM notifications WHERE id <> $1 AND reason = 'upvote'`, groupID).Scan(&emptyCount))
	require.Zero(t, emptyCount, "positive control: the empty group must have been deleted")
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction,
		deleteIfEmptyIntent(fixture.recipientDID, fixture.subjectURI)))
	var storedID int64
	var sortAt time.Time
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `
		SELECT id, sort_at FROM notifications
		WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&storedID, &sortAt),
		"a live qualifying vote must preserve its group")
	require.Equal(t, groupID, storedID, "the qualifying vote must preserve the same row")
	require.Truef(t, sortAt.Equal(pastTime), "delete-if-empty changed sort_at from %s to %s", pastTime, sortAt)
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DeleteIfEmpty_OnlyDisqualifiedVotesRemain(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*testing.T, upvoteGroupFixture, qualifyingUpvoteFixture, string, time.Time)
	}{
		{"recipient_self_upvote", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, _ string, now time.Time) {
			votes.insertVote(t, fixture.recipientDID, fixture.subjectURI, "up", now, false)
		}},
		{"erased_voter", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			_, err := fixture.db.ExecContext(fixture.ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, voter)
			require.NoError(t, err)
			votes.insertVote(t, voter, fixture.subjectURI, "up", now, false)
		}},
		{"aggregator_voter", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			_, err := fixture.db.ExecContext(fixture.ctx, `INSERT INTO aggregators (did, display_name, record_uri, record_cid)
				VALUES ($1, 'Aggregator voter', $2, 'bafydeleteaggregator')`, voter,
				"at://"+voter+"/social.coves.aggregator.service/self")
			require.NoError(t, err)
			votes.insertVote(t, voter, fixture.subjectURI, "up", now, false)
		}},
		{"recipient_blocks_voter", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			votes.insertBlock(t, fixture.recipientDID, voter)
			votes.insertVote(t, voter, fixture.subjectURI, "up", now, false)
		}},
		{"voter_blocks_recipient", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			votes.insertBlock(t, voter, fixture.recipientDID)
			votes.insertVote(t, voter, fixture.subjectURI, "up", now, false)
		}},
		{"downvotes", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			votes.insertVote(t, voter, fixture.subjectURI, "down", now, false)
		}},
		{"soft_deleted_upvotes", func(t *testing.T, fixture upvoteGroupFixture, votes qualifyingUpvoteFixture, voter string, now time.Time) {
			votes.insertVote(t, voter, fixture.subjectURI, "up", now, true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpvoteGroupFixture(t)
			insertDeleteTestGroup(t, fixture, fixture.recipientDID, fixture.subjectURI, time.Now().UTC())
			require.Equal(t, 1, deleteTestGroupCount(t, fixture, fixture.recipientDID, fixture.subjectURI),
				"the group must exist before attempting deletion")
			votes := qualifyingUpvoteFixture{db: fixture.db}
			test.setup(t, fixture, votes, "did:plc:"+testkit.UniqueID(t)+"voter", time.Now().UTC())
			transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
			require.NoError(t, err)
			defer transaction.Rollback()
			require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction,
				deleteIfEmptyIntent(fixture.recipientDID, fixture.subjectURI)))
			require.NoError(t, transaction.Commit())
			require.Zero(t, deleteTestGroupCount(t, fixture, fixture.recipientDID, fixture.subjectURI),
				"no live qualifying upvote remains on this subject")
		})
	}
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DeleteIfEmpty_OnlyDeletesMatchingUpvoteGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	pastTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	targetID := insertDeleteTestGroup(t, fixture, fixture.recipientDID, fixture.subjectURI, pastTime)
	otherRecipientID := insertDeleteTestGroup(t, fixture, fixture.otherRecipientDID, fixture.subjectURI, pastTime)
	otherSubject := fixture.subjectURI + "-other"
	otherSubjectID := insertDeleteTestGroup(t, fixture, fixture.recipientDID, otherSubject, pastTime)
	recordURI := "at://" + fixture.otherRecipientDID + "/social.coves.community.comment/reply"
	var replyID int64
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did,
			subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, 'postReply', $2, 'bafydeletegroupreply', $3, $4, $5, $6, $7)
		RETURNING id`, fixture.recipientDID, recordURI, fixture.otherRecipientDID,
		fixture.subjectURI, fixture.rootPostURI, time.Now().UTC(), pastTime).Scan(&replyID))
	require.Equal(t, 1, deleteTestGroupCount(t, fixture, fixture.recipientDID, fixture.subjectURI),
		"the target group must exist before attempting deletion")

	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction,
		deleteIfEmptyIntent(fixture.recipientDID, fixture.subjectURI)))
	require.NoError(t, transaction.Commit())
	var count int
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `SELECT count(*) FROM notifications WHERE id = $1`, targetID).Scan(&count))
	require.Zero(t, count, "positive control: the intended empty group must be deleted")
	for _, untouchedID := range []int64{otherRecipientID, otherSubjectID, replyID} {
		var storedID int64
		var sortAt time.Time
		require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `SELECT id, sort_at FROM notifications WHERE id = $1`,
			untouchedID).Scan(&storedID, &sortAt), "deletion must be scoped to the intended recipient, subject and reason")
		require.Equal(t, untouchedID, storedID)
		require.True(t, sortAt.Equal(pastTime), "an unrelated notification's sort_at must not change")
	}
}

func TestNotificationRepository_ApplyUpvoteGroupTx_DeleteIfEmpty_RollbackRestoresGroup(t *testing.T) {
	t.Parallel()
	fixture := newUpvoteGroupFixture(t)
	groupID := insertDeleteTestGroup(t, fixture, fixture.recipientDID, fixture.subjectURI, time.Now().UTC())
	require.Equal(t, 1, deleteTestGroupCount(t, fixture, fixture.recipientDID, fixture.subjectURI),
		"the group must exist before attempting deletion")
	transaction, err := fixture.db.BeginTx(fixture.ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	require.NoError(t, fixture.repository.ApplyUpvoteGroupTx(fixture.ctx, transaction,
		deleteIfEmptyIntent(fixture.recipientDID, fixture.subjectURI)))
	var count int
	require.NoError(t, transaction.QueryRowContext(fixture.ctx, `SELECT count(*) FROM notifications WHERE id = $1`, groupID).Scan(&count))
	require.Zero(t, count, "the caller's transaction must see the deletion")
	require.NoError(t, transaction.Rollback())
	var restoredID int64
	require.NoError(t, fixture.db.QueryRowContext(fixture.ctx, `
		SELECT id FROM notifications WHERE recipient_did = $1 AND reason = 'upvote' AND subject_uri = $2`,
		fixture.recipientDID, fixture.subjectURI).Scan(&restoredID))
	require.Equal(t, groupID, restoredID, "rolling back the caller's transaction must restore the same group")
}
