//go:build integration

package postgres

import (
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func requireNotificationSQLState(t *testing.T, err error, code string) *pq.Error {
	t.Helper()
	require.Error(t, err)
	var databaseError *pq.Error
	require.ErrorAs(t, err, &databaseError)
	require.Equal(t, pq.ErrorCode(code), databaseError.Code)
	return databaseError
}

type migrationNotification struct {
	recipientDID string
	reason       string
	recordURI    any
	recordCID    any
	actorDID     any
	subjectURI   any
	rootPostURI  string
}

func insertMigrationNotification(db *sql.DB, notification migrationNotification) error {
	_, err := db.Exec(`INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, notification.recipientDID, notification.reason,
		notification.recordURI, notification.recordCID, notification.actorDID,
		notification.subjectURI, notification.rootPostURI)
	return err
}

func TestNotificationMigration053_ActivationSingleton(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notification_activation`).Scan(&count))
	require.Equal(t, 1, count, "a migrated clone starts with one activation row")

	_, err := db.Exec(`INSERT INTO notification_activation (activated_at) VALUES (NOW())`)
	requireNotificationSQLState(t, err, "23505")
	_, err = db.Exec(`INSERT INTO notification_activation (singleton, activated_at) VALUES (false, NOW())`)
	requireNotificationSQLState(t, err, "23514")
}

func TestNotificationMigration053_DownAndUpRoundTrip(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	require.EqualValues(t, 55, testkit.MigrateDownOne(t, db, 55),
		"055 (notification public post withdrawals) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 54, testkit.MigrateDownOne(t, db, 54),
		"054 (upvote history index) must be rolled back before testing 053's Down section")
	require.EqualValues(t, 53, testkit.MigrateDownOne(t, db, 53),
		"this must exercise 053's Down section, not an earlier migration")
	for _, table := range []string{"notifications", "notification_state", "notification_activation"} {
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		require.Falsef(t, relation.Valid, "%s must be gone after 053 Down", table)
	}
	testkit.MigrateUp(t, db)
	for _, table := range []string{"notifications", "notification_state", "notification_activation"} {
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		require.Truef(t, relation.Valid, "%s must be recreated by 053 Up", table)
	}
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM notification_activation`).Scan(&count))
	require.Equal(t, 1, count, "053 Up must seed exactly one activation row again")
}

func TestNotificationMigration055_DownAndUpRoundTrip(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	require.EqualValues(t, 55, testkit.MigrateDownOne(t, db, 55),
		"this must exercise 055's Down section, not an earlier migration")
	var relation sql.NullString
	require.NoError(t, db.QueryRow(`SELECT to_regclass('notification_public_post_withdrawals')`).Scan(&relation))
	require.False(t, relation.Valid, "055 Down must drop notification_public_post_withdrawals")

	testkit.MigrateUp(t, db)
	require.NoError(t, db.QueryRow(`SELECT to_regclass('notification_public_post_withdrawals')`).Scan(&relation))
	require.True(t, relation.Valid, "055 Up must recreate notification_public_post_withdrawals")

	postURI := "at://did:plc:withdrawalmigration/social.coves.community.postv2/one"
	_, err := db.Exec(`INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev)
		VALUES ($1, 'authorDelete', NULL)`, postURI)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev)
		VALUES ($1, 'communityWithdrawal', 'rev-one')`, postURI)
	require.NoError(t, err, "both withdrawal kinds must coexist for the same post URI")

	for _, invalid := range []struct {
		name         string
		kind         string
		communityRev any
	}{
		{"unknown kind", "other", nil},
		{"author delete with rev", "authorDelete", "rev-two"},
		{"community withdrawal without rev", "communityWithdrawal", nil},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev)
				VALUES ($1, $2, $3)`, postURI, invalid.kind, invalid.communityRev)
			requireNotificationSQLState(t, err, "23514")
		})
	}
	_, err = db.Exec(`INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev)
		VALUES ($1, 'authorDelete', NULL)`, postURI)
	requireNotificationSQLState(t, err, "23505")
}

func TestNotificationMigration053_ReasonShapeChecks(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	recipientDID := "did:plc:notificationcheck" + testkit.UniqueID(t)
	createTestUser(t, db, "notificationcheck"+testkit.UniqueID(t)+".test", recipientDID)
	actorDID := "did:plc:notificationactor" + testkit.UniqueID(t)
	rootPostURI := "at://" + recipientDID + "/social.coves.community.post/root"
	valid := []migrationNotification{
		{recipientDID, "postReply", "at://" + actorDID + "/social.coves.community.comment/postreply", "bafyreply", actorDID, rootPostURI, rootPostURI},
		{recipientDID, "commentReply", "at://" + actorDID + "/social.coves.community.comment/commentreply", "bafyreply", actorDID, rootPostURI, rootPostURI},
		{recipientDID, "mention", "at://" + actorDID + "/social.coves.community.comment/mention", "bafymention", actorDID, nil, rootPostURI},
		{recipientDID, "upvote", nil, nil, nil, rootPostURI, rootPostURI},
	}
	for _, notification := range valid {
		require.NoErrorf(t, insertMigrationNotification(db, notification), "%s valid shape", notification.reason)
	}

	for _, example := range []struct {
		name   string
		valid  migrationNotification
		change func(*migrationNotification)
	}{
		{"upvote with actor", valid[3], func(row *migrationNotification) { row.actorDID = actorDID }},
		{"mention with subject", valid[2], func(row *migrationNotification) { row.subjectURI = rootPostURI }},
		{"postReply without CID", valid[0], func(row *migrationNotification) { row.recordCID = nil }},
		{"commentReply without CID", valid[1], func(row *migrationNotification) { row.recordCID = nil }},
	} {
		t.Run(example.name, func(t *testing.T) {
			invalid := example.valid
			example.change(&invalid)
			requireNotificationSQLState(t, insertMigrationNotification(db, invalid), "23514")
		})
	}
}

func TestNotificationMigration053_UniqueIndexes(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	suffix := testkit.UniqueID(t)
	recipientDID := "did:plc:notificationunique" + suffix
	otherRecipientDID := "did:plc:notificationother" + suffix
	createTestUser(t, db, "notificationunique"+suffix+".test", recipientDID)
	createTestUser(t, db, "notificationother"+suffix+".test", otherRecipientDID)
	rootPostURI := "at://" + recipientDID + "/social.coves.community.post/root"
	reply := migrationNotification{
		recipientDID, "postReply", "at://did:plc:replyauthor/social.coves.community.comment/reply", "bafyreply",
		"did:plc:replyauthor", rootPostURI, rootPostURI,
	}
	require.NoError(t, insertMigrationNotification(db, reply))
	require.Equal(t, "uq_notifications_record",
		requireNotificationSQLState(t, insertMigrationNotification(db, reply), "23505").Constraint)

	differentReason := reply
	differentReason.reason = "commentReply"
	require.NoError(t, insertMigrationNotification(db, differentReason), "record URI is reusable under another reason")
	differentRecipient := reply
	differentRecipient.recipientDID = otherRecipientDID
	require.NoError(t, insertMigrationNotification(db, differentRecipient), "record URI and reason are reusable for another recipient")

	group := migrationNotification{recipientDID, "upvote", nil, nil, nil, rootPostURI, rootPostURI}
	require.NoError(t, insertMigrationNotification(db, group))
	require.Equal(t, "uq_notifications_upvote_group",
		requireNotificationSQLState(t, insertMigrationNotification(db, group), "23505").Constraint)
}

func TestNotificationMigration053_RecipientForeignKeys(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	missingDID := "did:plc:notificationmissing" + testkit.UniqueID(t)
	rootPostURI := "at://" + missingDID + "/social.coves.community.post/root"
	requireNotificationSQLState(t, insertMigrationNotification(db,
		migrationNotification{missingDID, "upvote", nil, nil, nil, rootPostURI, rootPostURI}), "23503")
	_, err := db.Exec(`INSERT INTO notification_state (did) VALUES ($1)`, missingDID)
	requireNotificationSQLState(t, err, "23503")
}

func TestNotificationMigration053_ErasedRecipientCannotReceiveNotifications(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	recipientDID := "did:plc:notificationerased" + testkit.UniqueID(t)
	createTestUser(t, db, "notificationerased"+testkit.UniqueID(t)+".test", recipientDID)
	require.NoError(t, NewUserRepository(db).Delete(context.Background(), recipientDID))
	rootPostURI := "at://" + recipientDID + "/social.coves.community.post/root"
	requireNotificationSQLState(t, insertMigrationNotification(db,
		migrationNotification{recipientDID, "upvote", nil, nil, nil, rootPostURI, rootPostURI}), "23503")
}
