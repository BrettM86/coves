//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func bridgedScopeGroup(t *testing.T, fixture *unreadVisibilityFixture, subject, root string, total int) int64 {
	t.Helper()
	kind := "post"
	if subject != root {
		kind = "comment"
	}
	setBridgedGroupTotals(t, fixture.db, kind, subject, total, 0)
	var id int64
	require.NoError(t, fixture.db.QueryRow(`INSERT INTO notifications
		(recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $3, $4) RETURNING id`, fixture.recipient, subject, root,
		fixture.sortAt.Add(time.Second)).Scan(&id))
	return id
}

func bridgedScopeRepository(fixture *unreadVisibilityFixture, enabled bool) notifications.Repository {
	if enabled {
		return NewNotificationRepository(fixture.db, WithBridgedUpvoteTotals())
	}
	return NewNotificationRepository(fixture.db)
}

func TestNotificationRepository_BridgedGateOffIgnoresTotals(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"delete_if_empty", "sweep", "count", "list", "mixed_list"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			fixture := newUnreadVisibilityFixture(t)
			subject := fixture.post(t, posts.AdmissionStatusAccepted, false)
			id := bridgedScopeGroup(t, fixture, subject, subject, 5)
			repository := bridgedScopeRepository(fixture, false)
			switch operation {
			case "delete_if_empty":
				transaction, err := fixture.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				require.NoError(t, err)
				defer transaction.Rollback()
				require.NoError(t, repository.ApplyUpvoteGroupTx(context.Background(), transaction, deleteIfEmptyIntent(fixture.recipient, subject)))
				require.NoError(t, transaction.Commit())
			case "sweep":
				deleted, err := retentionSweeper(t, fixture.db).SweepEmptyUpvoteGroups(context.Background())
				require.NoError(t, err)
				require.EqualValues(t, 1, deleted)
			case "count", "list":
				var stored int
				require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&stored))
				require.Equal(t, 1, stored, "the hidden group must remain stored")
				reader := repository.(notifications.ReadRepository)
				if operation == "count" {
					count, err := reader.CountUnread(context.Background(), fixture.recipient)
					require.NoError(t, err)
					require.Equal(t, 1, count, "only the fixture's reply is unread")
				} else {
					page, err := reader.List(context.Background(), fixture.recipient, "", 10)
					require.NoError(t, err)
					require.Len(t, page.Notifications, 1, "only the fixture's reply is listed")
					require.Equal(t, notifications.ReasonCommentReply, page.Notifications[0].Reason)
				}
			case "mixed_list":
				votes := qualifyingUpvoteFixture{db: fixture.db}
				votes.insertVote(t, "did:plc:"+testkit.UniqueID(t), subject, "up", fixture.sortAt, false)
				votes.insertVote(t, "did:plc:"+testkit.UniqueID(t), subject, "up", fixture.sortAt, false)
				page, err := repository.(notifications.ReadRepository).List(context.Background(), fixture.recipient, "", 10)
				require.NoError(t, err)
				require.Len(t, page.Notifications, 2)
				require.Equal(t, subject, page.Notifications[0].SubjectURI)
				require.Equal(t, 2, page.Notifications[0].UpvoteCount)
			}
			if operation == "delete_if_empty" || operation == "sweep" {
				var remaining int
				require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&remaining))
				require.Equal(t, 0, remaining)
			}
		})
	}
}

func TestNotificationRepository_BridgedGateOnDoesNotFilterCommunityHost(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"delete_if_empty", "sweep", "count", "list"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			fixture := newUnreadVisibilityFixture(t)
			result, err := fixture.db.Exec(`UPDATE communities SET pds_url = $2 WHERE did = $1`, fixture.community, "https://unrelated.example.test")
			require.NoError(t, err)
			updated, err := result.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, updated, "the subject community must use the unrelated host")
			subject := fixture.post(t, posts.AdmissionStatusAccepted, false)
			id := bridgedScopeGroup(t, fixture, subject, subject, 6)
			repository := bridgedScopeRepository(fixture, true)
			switch operation {
			case "delete_if_empty":
				transaction, err := fixture.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				require.NoError(t, err)
				defer transaction.Rollback()
				require.NoError(t, repository.ApplyUpvoteGroupTx(context.Background(), transaction, deleteIfEmptyIntent(fixture.recipient, subject)))
				require.NoError(t, transaction.Commit())
			case "sweep":
				deleted, err := bridgedRetentionSweeper(t, fixture.db).SweepEmptyUpvoteGroups(context.Background())
				require.NoError(t, err)
				require.EqualValues(t, 0, deleted)
			case "count":
				count, err := repository.(notifications.ReadRepository).CountUnread(context.Background(), fixture.recipient)
				require.NoError(t, err)
				require.Equal(t, 2, count, "reply and bridged group must both count")
			case "list":
				page, err := repository.(notifications.ReadRepository).List(context.Background(), fixture.recipient, "", 10)
				require.NoError(t, err)
				require.Len(t, page.Notifications, 2, "reply and bridged group must both be listed")
				require.Equal(t, subject, page.Notifications[0].SubjectURI)
				require.Equal(t, 6, page.Notifications[0].UpvoteCount)
			}
			var remaining int
			require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&remaining))
			require.Equal(t, 1, remaining, "the group must remain stored")
		})
	}
}

func TestNotificationRepository_BridgedGatePreservesVisibilityRestrictions(t *testing.T) {
	t.Parallel()
	for _, restriction := range []string{"pending_post", "hidden_comment_root", "disabled_upvote"} {
		for _, read := range []string{"count", "list"} {
			t.Run(restriction+"/"+read, func(t *testing.T) {
				t.Parallel()
				fixture := newUnreadVisibilityFixture(t)
				control := fixture.post(t, posts.AdmissionStatusAccepted, false)
				bridgedScopeGroup(t, fixture, control, control, 5)
				subject, root := "", ""
				var controlRecipient string
				switch restriction {
				case "pending_post":
					subject = fixture.post(t, posts.AdmissionStatusPending, false)
					root = subject
				case "hidden_comment_root":
					root = fixture.post(t, posts.AdmissionStatusPending, false)
					subject = seedActorComment(t, fixture.db, fixture.recipient, root, testkit.TID(), fixture.sortAt)
				case "disabled_upvote":
					fixture.setDisabledReasons(t, fixture.recipient, []string{"upvote"})
					// A reason-wide preference cannot leave an upvote visible to this recipient.
					// Use a second recipient in the same clone as the enabled control.
					controlRecipient = retentionUser(t, fixture.db)
					retentionSeenAt(t, fixture.db, controlRecipient, fixture.sortAt.Add(-time.Microsecond))
					_, err := fixture.db.Exec(`UPDATE notifications SET recipient_did = $2 WHERE subject_uri = $1`, control, controlRecipient)
					require.NoError(t, err)
					subject = fixture.post(t, posts.AdmissionStatusAccepted, false)
					root = subject
				}
				id := bridgedScopeGroup(t, fixture, subject, root, 5)
				var stored int
				require.NoError(t, fixture.db.QueryRow(`SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&stored))
				require.Equal(t, 1, stored, "the restricted group must still be in storage")
				reader := bridgedScopeRepository(fixture, true).(notifications.ReadRepository)
				if read == "count" {
					if controlRecipient != "" {
						controlCount, err := reader.CountUnread(context.Background(), controlRecipient)
						require.NoError(t, err)
						require.Equal(t, 1, controlCount, "the enabled recipient counts the bridged control")
					}
					count, err := reader.CountUnread(context.Background(), fixture.recipient)
					require.NoError(t, err)
					if restriction == "disabled_upvote" {
						require.Equal(t, 1, count, "only the reply counts when upvotes are disabled")
					} else {
						require.Equal(t, 2, count, "the reply and visible bridged control count, but not the restricted group")
					}
				} else {
					if controlRecipient != "" {
						controlPage, err := reader.List(context.Background(), controlRecipient, "", 10)
						require.NoError(t, err)
						require.Len(t, controlPage.Notifications, 1, "the enabled recipient lists the bridged control")
						require.Equal(t, control, controlPage.Notifications[0].SubjectURI)
					}
					page, err := reader.List(context.Background(), fixture.recipient, "", 10)
					require.NoError(t, err)
					if restriction == "disabled_upvote" {
						require.Len(t, page.Notifications, 1, "disabled upvotes are hidden; reply remains")
						require.Equal(t, notifications.ReasonCommentReply, page.Notifications[0].Reason)
					} else {
						require.Len(t, page.Notifications, 2, "reply and visible bridged control are listed")
						require.Equal(t, control, page.Notifications[0].SubjectURI)
						require.Equal(t, 5, page.Notifications[0].UpvoteCount)
					}
				}
			})
		}
	}
}
