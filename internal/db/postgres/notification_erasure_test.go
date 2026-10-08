//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type erasureNotification struct {
	recipientDID string
	reason       string
	recordURI    string
	actorDID     string
	subjectURI   string
}

func notificationRowsForErasure(t *testing.T, db *sql.DB) []erasureNotification {
	t.Helper()
	rows, err := db.Query(`
		SELECT recipient_did, reason, COALESCE(record_uri, ''), COALESCE(actor_did, ''), COALESCE(subject_uri, '')
		FROM notifications ORDER BY recipient_did, reason, record_uri, subject_uri`)
	require.NoError(t, err)
	defer rows.Close()

	var notifications []erasureNotification
	for rows.Next() {
		var notification erasureNotification
		require.NoError(t, rows.Scan(&notification.recipientDID, &notification.reason,
			&notification.recordURI, &notification.actorDID, &notification.subjectURI))
		notifications = append(notifications, notification)
	}
	require.NoError(t, rows.Err())
	return notifications
}

func TestUserRepo_Delete_ErasesNotificationActorAndRecipient(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	repo := NewUserRepository(db)
	suffix := testkit.UniqueID(t)
	actorA := "did:plc:notifyactor" + suffix
	recipientB := "did:plc:notifyrecipientb" + suffix
	recipientC := "did:plc:notifyrecipientc" + suffix
	actorD := "did:plc:notifyactord" + suffix
	for _, user := range []struct{ handle, did string }{
		{"notifyactor" + suffix + ".test", actorA},
		{"notifyrecipientb" + suffix + ".test", recipientB},
		{"notifyrecipientc" + suffix + ".test", recipientC},
		{"notifyactord" + suffix + ".test", actorD},
	} {
		createTestUser(t, db, user.handle, user.did)
	}

	postB := "at://" + recipientB + "/social.coves.community.post/owned"
	postC := "at://" + recipientC + "/social.coves.community.post/owned"
	replyB := "at://" + actorA + "/social.coves.community.comment/replyb"
	mentionB := "at://" + actorD + "/social.coves.community.comment/mentionb"
	replyCFromA := "at://" + actorA + "/social.coves.community.comment/replyc"
	mentionCFromA := "at://" + actorA + "/social.coves.community.comment/mentionc"
	replyCFromD := "at://" + actorD + "/social.coves.community.comment/replyc"

	for _, notification := range []struct {
		recipientDID, reason, recordURI, actorDID, subjectURI, rootPostURI string
	}{
		{recipientB, "postReply", replyB, actorA, postB, postB},
		{recipientB, "mention", mentionB, actorD, "", postB},
		{recipientC, "postReply", replyCFromA, actorA, postC, postC},
		{recipientC, "mention", mentionCFromA, actorA, "", postC},
		{recipientC, "postReply", replyCFromD, actorD, postC, postC},
	} {
		_, err := db.Exec(`INSERT INTO notifications
			(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri)
			VALUES ($1, $2, $3, 'bafytestnotification', $4, NULLIF($5, ''), $6)`,
			notification.recipientDID, notification.reason, notification.recordURI,
			notification.actorDID, notification.subjectURI, notification.rootPostURI)
		require.NoError(t, err)
	}
	for _, group := range []struct{ recipientDID, postURI string }{
		{recipientB, postB}, {recipientC, postC},
	} {
		_, err := db.Exec(`INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
			VALUES ($1, 'upvote', $2, $2)`, group.recipientDID, group.postURI)
		require.NoError(t, err)
	}
	for _, did := range []string{recipientB, recipientC} {
		_, err := db.Exec(`INSERT INTO notification_state (did, seen_at) VALUES ($1, NOW())`, did)
		require.NoError(t, err)
	}
	stateRowCount := func(did string) int {
		t.Helper()
		var stateCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notification_state WHERE did = $1`, did).Scan(&stateCount))
		return stateCount
	}

	expectedBefore := []erasureNotification{
		{recipientB, "mention", mentionB, actorD, ""},
		{recipientB, "postReply", replyB, actorA, postB},
		{recipientB, "upvote", "", "", postB},
		{recipientC, "mention", mentionCFromA, actorA, ""},
		{recipientC, "postReply", replyCFromA, actorA, postC},
		{recipientC, "postReply", replyCFromD, actorD, postC},
		{recipientC, "upvote", "", "", postC},
	}
	require.ElementsMatch(t, expectedBefore, notificationRowsForErasure(t, db))

	require.NoError(t, repo.Delete(ctx, actorA))
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE actor_did = $1`, actorA).Scan(&count))
	require.Zero(t, count, "erased actor must leave no notifications for other recipients")
	expectedAfterActor := []erasureNotification{
		{recipientB, "mention", mentionB, actorD, ""},
		{recipientB, "upvote", "", "", postB},
		{recipientC, "postReply", replyCFromD, actorD, postC},
		{recipientC, "upvote", "", "", postC},
	}
	require.ElementsMatch(t, expectedAfterActor, notificationRowsForErasure(t, db))
	require.Equal(t, 1, stateRowCount(recipientB), "erasing actor A must not delete B's notification_state row")
	require.Equal(t, 1, stateRowCount(recipientC), "erasing actor A must not delete C's notification_state row")

	require.NoError(t, repo.Delete(ctx, recipientB))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE recipient_did = $1`, recipientB).Scan(&count))
	require.Zero(t, count, "erased recipient must leave no inbox rows")
	require.Zero(t, stateRowCount(recipientB), "erased recipient must leave no state")
	require.Equal(t, 1, stateRowCount(recipientC), "erasing recipient B must not delete C's notification_state row")
	require.ElementsMatch(t, expectedAfterActor[2:], notificationRowsForErasure(t, db), "C's notifications must survive both erasures")
}

func TestUserRepo_Delete_VoterErasurePreservesRecipientUpvoteGroup(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	suffix := testkit.UniqueID(t)
	voterDID := "did:plc:notifyvoter" + suffix
	recipientDID := "did:plc:notifyowner" + suffix
	createTestUser(t, db, "notifyvoter"+suffix+".test", voterDID)
	createTestUser(t, db, "notifyowner"+suffix+".test", recipientDID)
	postURI := "at://" + recipientDID + "/social.coves.community.post/owned"
	voteURI := "at://" + voterDID + "/social.coves.feed.vote/onowned"
	_, err := db.Exec(`INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, 'bafytestvote', 'onowned', $2, $3, 'bafytestpost', 'up', NOW())`,
		voteURI, voterDID, postURI)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri)
		VALUES ($1, 'upvote', $2, $2)`, recipientDID, postURI)
	require.NoError(t, err)
	group := []erasureNotification{{recipientDID, "upvote", "", "", postURI}}
	require.ElementsMatch(t, group, notificationRowsForErasure(t, db))

	require.NoError(t, NewUserRepository(db).Delete(ctx, voterDID))
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM votes WHERE uri = $1`, voteURI).Scan(&count))
	require.Zero(t, count, "erased voter's live vote must be deleted")
	require.ElementsMatch(t, group, notificationRowsForErasure(t, db), "recipient's upvote group must not be deleted with the voter")
}

type publicWithdrawalMarker struct {
	postURI string
	kind    string
}

func publicWithdrawalMarkers(t *testing.T, db *sql.DB) []publicWithdrawalMarker {
	t.Helper()
	rows, err := db.Query(`SELECT post_uri, kind FROM notification_public_post_withdrawals`)
	require.NoError(t, err)
	defer rows.Close()

	var markers []publicWithdrawalMarker
	for rows.Next() {
		var marker publicWithdrawalMarker
		require.NoError(t, rows.Scan(&marker.postURI, &marker.kind))
		markers = append(markers, marker)
	}
	require.NoError(t, rows.Err())
	return markers
}

func TestUserRepo_Delete_ErasesUsersPublicWithdrawalMarkersOfBothKinds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		failDeletion bool
	}{
		{"successful erasure", false},
		{"failed erasure rolls back marker deletion", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			ctx := context.Background()
			suffix := testkit.UniqueID(t)
			authorA := "did:plc:withdrawala" + suffix
			authorB := "did:plc:withdrawalb" + suffix
			createTestUser(t, db, "withdrawala"+suffix+".test", authorA)
			createTestUser(t, db, "withdrawalb"+suffix+".test", authorB)

			postA1 := "at://" + authorA + "/social.coves.community.postv2/first"
			postA2 := "at://" + authorA + "/social.coves.community.postv2/second"
			postB := "at://" + authorB + "/social.coves.community.postv2/other"
			before := []publicWithdrawalMarker{
				{postA1, "authorDelete"},
				{postA2, "communityWithdrawal"},
				{postB, "authorDelete"},
				{postB, "communityWithdrawal"},
			}
			for _, marker := range []struct {
				publicWithdrawalMarker
				communityRev any
			}{
				{before[0], nil},
				{before[1], "rev-a"},
				{before[2], nil},
				{before[3], "rev-b"},
			} {
				_, err := db.ExecContext(ctx, `INSERT INTO notification_public_post_withdrawals
					(post_uri, kind, community_rev) VALUES ($1, $2, $3)`,
					marker.postURI, marker.kind, marker.communityRev)
				require.NoError(t, err)
			}
			require.ElementsMatch(t, before, publicWithdrawalMarkers(t, db))

			if test.failDeletion {
				_, err := db.ExecContext(ctx, `CREATE FUNCTION reject_public_withdrawal_erasure() RETURNS trigger
					LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced users deletion failure'; END $$`)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER reject_public_withdrawal_erasure
					BEFORE DELETE ON users FOR EACH ROW WHEN (OLD.did = '%s')
					EXECUTE FUNCTION reject_public_withdrawal_erasure()`, authorA))
				require.NoError(t, err)
				require.Error(t, NewUserRepository(db).Delete(ctx, authorA))
				require.ElementsMatch(t, before, publicWithdrawalMarkers(t, db),
					"failed user deletion must roll back withdrawal marker deletions")
				var users int
				require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE did = $1`, authorA).Scan(&users))
				require.Equal(t, 1, users)
				return
			}

			require.NoError(t, NewUserRepository(db).Delete(ctx, authorA))
			require.ElementsMatch(t, []publicWithdrawalMarker{
				{postB, "authorDelete"},
				{postB, "communityWithdrawal"},
			}, publicWithdrawalMarkers(t, db), "only the erased author's markers must be removed")
		})
	}
}
