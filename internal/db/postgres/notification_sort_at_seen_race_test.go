//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// An ingest transaction can start, wait on row locks, and write after the
// recipient has marked notifications seen. The written row was not visible when
// seen_at advanced, so it must stay unread however early the transaction began.
func TestNotificationSortAt_WriteCommittedAfterSeenAtIsUnread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *unreadVisibilityFixture)
		write func(t *testing.T, f *unreadVisibilityFixture, repository notifications.Repository, transaction *sql.Tx)
	}{
		{
			name:  "new reply inserted by ApplyTx",
			setup: func(t *testing.T, f *unreadVisibilityFixture) {},
			write: func(t *testing.T, f *unreadVisibilityFixture, repository notifications.Repository, transaction *sql.Tx) {
				require.NoError(t, repository.ApplyTx(context.Background(), transaction, []notifications.Intent{{
					Reason: notifications.ReasonPostReply, RecipientDID: f.recipient, ActorDID: f.actor,
					RecordURI: f.comment(t, f.root), RecordCID: "bafyunreadrecord",
					SubjectURI: f.root, RootPostURI: f.root, RecordCreatedAt: f.sortAt,
				}}))
			},
		},
		{
			name: "existing upvote group bumped by ApplyUpvoteGroupTx",
			setup: func(t *testing.T, f *unreadVisibilityFixture) {
				f.insertVote(t, "did:plc:sortatvoter"+testkit.UniqueID(t), f.root, false)
				f.notify(t, "upvote", "", f.root, f.root)
			},
			write: func(t *testing.T, f *unreadVisibilityFixture, repository notifications.Repository, transaction *sql.Tx) {
				require.NoError(t, repository.ApplyUpvoteGroupTx(context.Background(), transaction, notifications.UpvoteGroupIntent{
					Action: notifications.UpvoteGroupBump, RecipientDID: f.recipient,
					SubjectURI: f.root, RootPostURI: f.root,
				}))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newUnreadCountFixture(t)
			tc.setup(t, f)
			repository := NewNotificationRepository(f.db)

			transaction, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			defer transaction.Rollback()
			var transactionStart time.Time
			require.NoError(t, transaction.QueryRowContext(ctx, `SELECT now()`).Scan(&transactionStart))

			// Poll the database clock until it is past the transaction start, so
			// the seen time is after that start and not clamped to server now.
			var seenAt time.Time
			for attempt := 0; !seenAt.After(transactionStart); attempt++ {
				require.Less(t, attempt, 100000, "database clock never passed the transaction start")
				require.NoError(t, f.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&seenAt))
			}
			require.NoError(t, repository.(notifications.ReadRepository).UpdateSeen(ctx, f.recipient, seenAt))

			tc.write(t, f, repository, transaction)
			require.NoError(t, transaction.Commit())

			f.requireCount(t, 1)
			page := f.listPage(t, f.recipient, "", 10)
			require.Len(t, page.Notifications, 1)
			require.False(t, page.Notifications[0].IsRead, "a notification committed after seen_at advanced must be unread")
		})
	}
}
