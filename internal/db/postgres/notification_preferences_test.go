//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func preferenceState(t *testing.T, db *sql.DB, did string) (sql.NullTime, []string) {
	t.Helper()
	var seenAt sql.NullTime
	var disabled []string
	require.NoError(t, db.QueryRow(`SELECT seen_at, disabled_reasons FROM notification_state WHERE did = $1`, did).
		Scan(&seenAt, pq.Array(&disabled)))
	sort.Strings(disabled)
	return seenAt, disabled
}

func TestNotificationPreferences_GetDefaultsAndDisabledReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		disabled []string
		hasState bool
		want     notifications.Preferences
	}{
		{"no state defaults to enabled", nil, false, notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}},
		{"disabled post reply only", []string{"postReply"}, true, notifications.Preferences{PostReply: false, CommentReply: true, Mention: true, Upvote: true}},
		{"disabled comment reply only", []string{"commentReply"}, true, notifications.Preferences{PostReply: true, CommentReply: false, Mention: true, Upvote: true}},
		{"disabled mention only", []string{"mention"}, true, notifications.Preferences{PostReply: true, CommentReply: true, Mention: false, Upvote: true}},
		{"disabled upvote only", []string{"upvote"}, true, notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}},
		{"disabled mention and upvote", []string{"mention", "upvote"}, true, notifications.Preferences{PostReply: true, CommentReply: true, Mention: false, Upvote: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := retentionUser(t, db)
			if tc.hasState {
				_, err := db.Exec(`INSERT INTO notification_state (did, disabled_reasons) VALUES ($1, $2)`, did, pq.Array(tc.disabled))
				require.NoError(t, err)
			}
			repo := NewNotificationRepository(db).(notifications.PreferencesRepository)
			got, err := repo.GetPreferences(context.Background(), did)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNotificationPreferences_PutChangesOnlyNamedReasons(t *testing.T) {
	t.Parallel()
	falseValue, trueValue := false, true
	seenAt := time.Date(2026, time.September, 30, 12, 0, 0, 123456000, time.UTC).Truncate(time.Microsecond)
	type step struct {
		update   notifications.PreferencesUpdate
		want     notifications.Preferences
		disabled []string
	}
	for _, tc := range []struct {
		name            string
		hasState        bool
		initialSeenAt   any
		initialDisabled []string
		steps           []step
	}{
		{
			name: "no row creates state without advancing seen at",
			steps: []step{{notifications.PreferencesUpdate{Mention: &falseValue},
				notifications.Preferences{PostReply: true, CommentReply: true, Mention: false, Upvote: true}, []string{"mention"}}},
		},
		{
			name:     "existing seen at survives mention disable",
			hasState: true, initialSeenAt: seenAt, initialDisabled: []string{},
			steps: []step{{notifications.PreferencesUpdate{Mention: &falseValue},
				notifications.Preferences{PostReply: true, CommentReply: true, Mention: false, Upvote: true}, []string{"mention"}}},
		},
		{
			name:     "mention reenabled and upvote disabled",
			hasState: true, initialSeenAt: seenAt, initialDisabled: []string{"mention"},
			steps: []step{{notifications.PreferencesUpdate{Mention: &trueValue, Upvote: &falseValue},
				notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}, []string{"upvote"}}},
		},
		{
			name:     "repeated put does not duplicate upvote",
			hasState: true, initialSeenAt: seenAt, initialDisabled: []string{"upvote"},
			steps: []step{
				{notifications.PreferencesUpdate{Mention: &trueValue, Upvote: &falseValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}, []string{"upvote"}},
				{notifications.PreferencesUpdate{Mention: &trueValue, Upvote: &falseValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}, []string{"upvote"}},
			},
		},
		{
			name:     "omitted upvote remains disabled",
			hasState: true, initialSeenAt: seenAt, initialDisabled: []string{"mention", "upvote"},
			steps: []step{{notifications.PreferencesUpdate{Mention: &trueValue},
				notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}, []string{"upvote"}}},
		},
		{
			name:     "empty update preserves disabled comment reply",
			hasState: true, initialSeenAt: seenAt, initialDisabled: []string{"commentReply"},
			steps: []step{{notifications.PreferencesUpdate{},
				notifications.Preferences{PostReply: true, CommentReply: false, Mention: true, Upvote: true}, []string{"commentReply"}}},
		},
		{
			name: "post reply disabled then reenabled",
			steps: []step{
				{notifications.PreferencesUpdate{PostReply: &falseValue},
					notifications.Preferences{PostReply: false, CommentReply: true, Mention: true, Upvote: true}, []string{"postReply"}},
				{notifications.PreferencesUpdate{PostReply: &trueValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}, []string{}},
			},
		},
		{
			name: "comment reply disabled then reenabled",
			steps: []step{
				{notifications.PreferencesUpdate{CommentReply: &falseValue},
					notifications.Preferences{PostReply: true, CommentReply: false, Mention: true, Upvote: true}, []string{"commentReply"}},
				{notifications.PreferencesUpdate{CommentReply: &trueValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}, []string{}},
			},
		},
		{
			name: "upvote disabled then reenabled",
			steps: []step{
				{notifications.PreferencesUpdate{Upvote: &falseValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: false}, []string{"upvote"}},
				{notifications.PreferencesUpdate{Upvote: &trueValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}, []string{}},
			},
		},
		{
			name: "mention disabled then reenabled",
			steps: []step{
				{notifications.PreferencesUpdate{Mention: &falseValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: false, Upvote: true}, []string{"mention"}},
				{notifications.PreferencesUpdate{Mention: &trueValue},
					notifications.Preferences{PostReply: true, CommentReply: true, Mention: true, Upvote: true}, []string{}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := retentionUser(t, db)
			if tc.hasState {
				_, err := db.Exec(`INSERT INTO notification_state (did, seen_at, disabled_reasons) VALUES ($1, $2, $3)`,
					did, tc.initialSeenAt, pq.Array(tc.initialDisabled))
				require.NoError(t, err)
			}
			repo := NewNotificationRepository(db).(notifications.PreferencesRepository)
			for index, change := range tc.steps {
				got, err := repo.PutPreferences(context.Background(), did, change.update)
				require.NoError(t, err, "step %d", index+1)
				require.Equal(t, change.want, got, "step %d return", index+1)
				storedSeenAt, storedDisabled := preferenceState(t, db, did)
				if tc.initialSeenAt == nil {
					require.False(t, storedSeenAt.Valid, "step %d must not advance seen_at", index+1)
				} else {
					require.Equal(t, sql.NullTime{Time: seenAt, Valid: true}, storedSeenAt, "step %d seen_at", index+1)
				}
				require.Equal(t, change.disabled, storedDisabled, "step %d disabled_reasons", index+1)
			}
		})
	}
}

func TestNotificationPreferences_PutForUnindexedAccountReturnsAccountNotIndexed(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	did := "did:plc:prefunindexed" + testkit.UniqueID(t)
	falseValue := false
	repo := NewNotificationRepository(db).(notifications.PreferencesRepository)
	for _, update := range []notifications.PreferencesUpdate{{}, {Mention: &falseValue}} {
		_, err := repo.PutPreferences(context.Background(), did, update)
		require.True(t, errors.Is(err, notifications.ErrAccountNotIndexed), "want ErrAccountNotIndexed, got %v", err)
	}
	var stateRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM notification_state WHERE did = $1`, did).Scan(&stateRows))
	require.Zero(t, stateRows)
	var userRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM users WHERE did = $1`, did).Scan(&userRows))
	require.Zero(t, userRows, "putPreferences must never create a users row")
}
