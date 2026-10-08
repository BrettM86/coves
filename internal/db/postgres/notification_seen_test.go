//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func notificationSeenState(t *testing.T, db *sql.DB, did string) (sql.NullTime, []string) {
	t.Helper()
	var seenAt sql.NullTime
	var disabled []string
	// The left join makes a missing state row observable as a NULL seen_at.
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT state.seen_at,
		COALESCE(state.disabled_reasons, ARRAY[]::text[])
		FROM users account LEFT JOIN notification_state state ON state.did = account.did
		WHERE account.did = $1`, did).Scan(&seenAt, pq.Array(&disabled)))
	return seenAt, disabled
}

func TestNotificationSeen_UpdateSeenIsMonotonic(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 9, 1, 12, 34, 56, 123456000, time.UTC)
	for _, tc := range []struct {
		name     string
		initial  []string
		updates  []time.Time
		wantSeen time.Time
		wantOff  []string
	}{
		{"no state row", nil, []time.Time{instant}, instant, []string{}},
		{"preferences state with null seen at", []string{"mention", "upvote"},
			[]time.Time{instant.Add(-time.Hour), instant}, instant, []string{"mention", "upvote"}},
		{"backwards update ignored", nil, []time.Time{instant, instant.Add(-time.Hour)}, instant, []string{}},
		{"later update advances", nil, []time.Time{instant.Add(-time.Hour), instant}, instant, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := retentionUser(t, db)
			if tc.initial != nil {
				_, err := db.ExecContext(context.Background(), `INSERT INTO notification_state (did, disabled_reasons)
					VALUES ($1, $2)`, did, pq.Array(tc.initial))
				require.NoError(t, err)
			}
			repo := NewNotificationRepository(db).(notifications.ReadRepository)
			for _, seenAt := range tc.updates {
				require.NoError(t, repo.UpdateSeen(context.Background(), did, seenAt))
			}
			stored, disabled := notificationSeenState(t, db, did)
			require.True(t, stored.Valid, "stored seen_at must be non-NULL")
			require.True(t, stored.Time.UTC().Equal(tc.wantSeen), "stored seen_at = %s, want %s", stored.Time.UTC(), tc.wantSeen)
			require.Equal(t, tc.wantOff, disabled, "disabled_reasons")
		})
	}
}

func TestNotificationSeen_FutureSeenAtIsClampedToServerTime(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 9, 1, 12, 34, 56, 123456000, time.UTC)
	for _, tc := range []struct {
		name      string
		seenFirst bool
	}{
		{"no state row", false},
		{"existing seen at", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := retentionUser(t, db)
			repo := NewNotificationRepository(db).(notifications.ReadRepository)
			if tc.seenFirst {
				require.NoError(t, repo.UpdateSeen(context.Background(), did, instant))
			}
			var serverNow, future time.Time
			require.NoError(t, db.QueryRowContext(context.Background(),
				`SELECT now(), now() + interval '1 hour'`).Scan(&serverNow, &future))
			require.NoError(t, repo.UpdateSeen(context.Background(), did, future))
			var serverAfter time.Time
			require.NoError(t, db.QueryRowContext(context.Background(),
				`SELECT clock_timestamp()`).Scan(&serverAfter))
			stored, _ := notificationSeenState(t, db, did)
			require.True(t, stored.Valid, "stored seen_at must be non-NULL")
			require.False(t, stored.Time.UTC().Before(serverNow.UTC()), "stored seen_at %s is before server now %s", stored.Time.UTC(), serverNow.UTC())
			require.False(t, stored.Time.UTC().After(serverAfter.UTC()), "stored seen_at %s is after server clock %s", stored.Time.UTC(), serverAfter.UTC())
		})
	}
}

// countReplyAt indexes at sortAt and also sets record_created_at to sortAt;
// delay the record time explicitly so the count must use index time instead.
func (f *unreadVisibilityFixture) delayedReplyAt(t *testing.T, sortAt, recordCreatedAt time.Time) {
	t.Helper()
	record := f.countReplyAt(t, sortAt, f.root)
	result, err := f.db.ExecContext(context.Background(), `UPDATE notifications
		SET record_created_at = $3 WHERE recipient_did = $1 AND record_uri = $2`,
		f.recipient, record, recordCreatedAt)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated, "delayed reply notification must exist")
}

func TestNotificationSeen_DelayedReplyAfterSeenAtIsUnread(t *testing.T) {
	t.Parallel()
	seenAt := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := newUnreadCountFixture(t)
	repo := NewNotificationRepository(f.db).(notifications.ReadRepository)
	require.NoError(t, repo.UpdateSeen(context.Background(), f.recipient, seenAt))
	f.countReplyAt(t, seenAt.Add(-time.Hour), f.root)
	f.delayedReplyAt(t, seenAt.Add(time.Minute), seenAt.Add(-time.Hour))
	f.delayedReplyAt(t, seenAt.Add(2*time.Minute), seenAt.Add(-time.Hour))
	count, err := repo.CountUnread(context.Background(), f.recipient)
	require.NoError(t, err)
	require.Equal(t, 2, count, "delayed replies indexed after seen_at must be unread")
}
