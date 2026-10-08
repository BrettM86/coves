//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type notificationGatePost struct {
	authorDID string
	uri       string
	cid       string
}

type notificationGateFixture struct {
	db           *sql.DB
	commenterDID string
	posts        []notificationGatePost
}

// Each fixture owns its activation row and indexed post(s), so its time gates
// cannot change the meaning of a parallel case's notification assertion. The
// commenter is indexed on commenterPDS and the control recipient on a native PDS.
func newNotificationGateFixture(t *testing.T, recipientPDS, commenterPDS string, controlRecipient bool) notificationGateFixture {
	t.Helper()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	commenterDID := "did:plc:" + uniqueID + "commenter"
	communityDID := "did:plc:" + uniqueID + "community"
	recipients := []struct{ did, handle, pdsURL string }{
		{"did:plc:" + uniqueID + "recipient", uniqueID + "recipient.test", recipientPDS},
	}
	if controlRecipient {
		recipients = append(recipients, struct{ did, handle, pdsURL string }{
			"did:plc:" + uniqueID + "control", uniqueID + "control.test", bridgedTestNativePDS,
		})
	}
	for _, user := range append(recipients, struct{ did, handle, pdsURL string }{
		commenterDID, uniqueID + "commenter.test", commenterPDS,
	}) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())`,
			user.did, user.handle, user.pdsURL)
		require.NoError(t, err, "index notification recipient or commenter")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		 VALUES ($1, $2, $3, $4, $4, $4, $5, NOW())`,
		communityDID, uniqueID+"community.test", "Notification gate community", recipients[0].did, bridgedTestNativePDS)
	require.NoError(t, err, "index the posts' community")

	userService := newMockUserService()
	for _, recipient := range recipients {
		userService.users[recipient.did] = &users.User{DID: recipient.did, Handle: recipient.handle}
	}
	postConsumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		userService, db, WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
	fixture := notificationGateFixture{db: db, commenterDID: commenterDID}
	for _, recipient := range recipients {
		postKey := testkit.TID()
		post := notificationGatePost{authorDID: recipient.did, uri: pv2URI(recipient.did, postKey), cid: "bafyreicommentnotificationgatepost"}
		require.NoError(t, postConsumer.HandleEvent(ctx, pv2Event(
			recipient.did, "create", postKey, testkit.TID(), post.cid, time.Now().UnixMicro(),
			pv2Record(communityDID, "Reply notification gate target", "A post to reply to"),
		)), "index recipient's author-owned post")
		require.Equal(t, 1, countRows(t, db,
			`SELECT count(*) FROM posts WHERE uri = $1 AND author_did = $2`, post.uri, recipient.did))
		fixture.posts = append(fixture.posts, post)
	}
	return fixture
}

func (fixture notificationGateFixture) commentConsumer(options ...CommentEventConsumerOption) *CommentEventConsumer {
	return NewCommentEventConsumer(postgres.NewCommentRepository(fixture.db), fixture.db,
		append([]CommentEventConsumerOption{WithCommentNotifications(postgres.NewNotificationRepository(fixture.db))}, options...)...)
}

func replyForNotificationGate(t *testing.T, fixture notificationGateFixture, consumer *CommentEventConsumer, post notificationGatePost, createdAt string) string {
	t.Helper()
	commentKey := testkit.TID()
	commentURI := "at://" + fixture.commenterDID + "/" + CommentCollection + "/" + commentKey
	commentRecord := revCommentRecord("A replies to the recipient's post", post.uri, post.cid, post.uri, post.cid)
	commentRecord["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(context.Background(), revCommitEvent(
		fixture.commenterDID, CommentCollection, "create", commentKey, testkit.TID(),
		"bafyreicommentnotificationgatereply", time.Now().UnixMicro(), commentRecord,
	)), "index A's top-level reply")
	require.Equal(t, 1, countRows(t, fixture.db, `SELECT count(*) FROM comments WHERE uri = $1`, commentURI),
		"the reply must be indexed even if its notification is suppressed")
	return commentURI
}

func TestCommentConsumer_NotificationWriteGates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
	}{
		{"a_erased_recipient"},
		{"b_aggregator_recipient"},
		{"d1_recipient_blocks_commenter"},
		{"d2_commenter_blocks_recipient"},
		{"f_older_than_seven_days"},
		{"g_before_activation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
			post := fixture.posts[0]
			createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
			switch test.name {
			case "a_erased_recipient":
				_, err := fixture.db.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, post.authorDID)
				require.NoError(t, err, "retain both the users row and the erasure marker")
			case "b_aggregator_recipient":
				_, err := fixture.db.ExecContext(ctx,
					`INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
					post.authorDID, "Aggregator recipient", "at://"+post.authorDID+"/social.coves.aggregator.service/self", "bafyreigateservice")
				require.NoError(t, err, "retain both the users row and the aggregator declaration")
			case "d1_recipient_blocks_commenter", "d2_commenter_blocks_recipient":
				blockerDID, blockedDID := post.authorDID, fixture.commenterDID
				if test.name == "d2_commenter_blocks_recipient" {
					blockerDID, blockedDID = blockedDID, blockerDID
				}
				_, err := fixture.db.ExecContext(ctx,
					`INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
					blockerDID, blockedDID, "at://"+blockerDID+"/"+CovesActorBlockCollection+"/"+testkit.TID(), "bafyreigateblock")
				require.NoError(t, err, "index the pre-existing directional block")
			case "f_older_than_seven_days", "g_before_activation":
				var databaseNow time.Time
				require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT NOW()`).Scan(&databaseNow))
				var activationTime, recordTime time.Time
				if test.name == "f_older_than_seven_days" {
					activationTime, recordTime = databaseNow.Add(-30*24*time.Hour), databaseNow.Add(-8*24*time.Hour)
				} else {
					activationTime, recordTime = databaseNow.Add(-time.Hour), databaseNow.Add(-2*time.Hour)
				}
				_, err := fixture.db.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, activationTime)
				require.NoError(t, err)
				createdAt = recordTime.UTC().Format(time.RFC3339Nano)
			}
			commentURI := replyForNotificationGate(t, fixture, fixture.commentConsumer(), post, createdAt)
			require.Zero(t, countRows(t, fixture.db,
				`SELECT count(*) FROM notifications WHERE record_uri = $1`, commentURI),
				"the reply to an ineligible recipient must not write any notification")
		})
	}

	// Only the recipient's hosting suppresses: the commenter is also hosted on
	// the trusted bridge, so the native control proves a bridged actor still
	// notifies a native recipient.
	t.Run("c_bridge_hosted_recipient_and_untrusted_control", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		fixture := newNotificationGateFixture(t, bridgedTestPDS, bridgedTestPDS, true)
		createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
		consumer := fixture.commentConsumer(WithCommentBridgeTrust(NewBridgeTrust([]string{bridgedTestPDS})))
		bridgePost, controlPost := fixture.posts[0], fixture.posts[1]
		bridgeReplyURI := replyForNotificationGate(t, fixture, consumer, bridgePost, createdAt)
		assert.Zero(t, countRows(t, fixture.db,
			`SELECT count(*) FROM notifications WHERE record_uri = $1`, bridgeReplyURI),
			"a reply to a trusted bridge-hosted recipient must not write any notification")

		controlReplyURI := replyForNotificationGate(t, fixture, consumer, controlPost, createdAt)
		require.Equal(t, 1, countRows(t, fixture.db,
			`SELECT count(*) FROM notifications WHERE recipient_did = $1 AND reason = 'postReply' AND actor_did = $2 AND record_uri = $3`,
			controlPost.authorDID, fixture.commenterDID, controlReplyURI),
			"a native-PDS recipient must still receive the postReply from a bridge-hosted commenter")
	})

	t.Run("e_later_block_keeps_existing_notification", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		fixture := newNotificationGateFixture(t, bridgedTestNativePDS, bridgedTestNativePDS, false)
		post := fixture.posts[0]
		createdAt := activatedCommentNotificationTime(t, fixture.db, ctx)
		commentURI := replyForNotificationGate(t, fixture, fixture.commentConsumer(), post, createdAt)

		type storedNotification struct {
			id              int64
			recipientDID    string
			reason          string
			actorDID        string
			recordURI       string
			recordCID       string
			subjectURI      string
			rootPostURI     string
			recordCreatedAt time.Time
			sortAt          time.Time
		}
		readNotification := func() storedNotification {
			t.Helper()
			var notification storedNotification
			require.NoError(t, fixture.db.QueryRowContext(ctx,
				`SELECT id, recipient_did, reason, actor_did, record_uri, record_cid, subject_uri,
				        root_post_uri, record_created_at, sort_at FROM notifications
				 WHERE recipient_did = $1 AND reason = 'postReply' AND record_uri = $2`,
				post.authorDID, commentURI,
			).Scan(&notification.id, &notification.recipientDID, &notification.reason,
				&notification.actorDID, &notification.recordURI, &notification.recordCID,
				&notification.subjectURI, &notification.rootPostURI,
				&notification.recordCreatedAt, &notification.sortAt))
			return notification
		}
		beforeBlock := readNotification()
		require.Equal(t, fixture.commenterDID, beforeBlock.actorDID)
		require.Equal(t, post.uri, beforeBlock.subjectURI)

		blockConsumer := NewUserEventConsumer(nil, nil,
			WithUserBlockRepo(postgres.NewUserBlockRepository(fixture.db)))
		require.NoError(t, blockConsumer.HandleEvent(ctx,
			userBlockEvent(post.authorDID, testkit.TID(), "create", fixture.commenterDID)))
		require.Equal(t, 1, countRows(t, fixture.db,
			`SELECT count(*) FROM user_blocks WHERE blocker_did = $1 AND blocked_did = $2`,
			post.authorDID, fixture.commenterDID), "the real block consumer must index B's block of A")
		require.Equal(t, beforeBlock, readNotification(), "a later block must not erase or change an existing postReply")
	})
}
