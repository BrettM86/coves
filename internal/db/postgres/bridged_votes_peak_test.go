//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

func (f bridgedNotificationFixture) upvotePeak(t *testing.T) int {
	t.Helper()
	var peak int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		`SELECT bridged_upvote_peak FROM `+f.table+` WHERE uri = $1`, f.subject).Scan(&peak))
	return peak
}

func TestBridgedVotesPeak_OscillationBumpsOnlyAboveHighWaterMark(t *testing.T) {
	for _, kind := range []string{"post", "comment"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, kind, "native")
			repo := f.repository()
			require.Equal(t, 0, f.groupCount(t))
			require.Equal(t, 0, f.upvotePeak(t))
			f.nativeUpvote(t)
			f.apply(t, repo, 5, 0, f.now.Add(time.Minute), true)
			require.Equal(t, 1, f.groupCount(t))
			f.markSeen(t, repo)
			f.unread(t, repo, 0)
			sortAt := f.groupSort(t)

			for index, step := range []struct {
				total         int
				listedUpvotes int
			}{
				{total: 3, listedUpvotes: 4},
				{total: 5, listedUpvotes: 6},
				{total: 4, listedUpvotes: 5},
				{total: 5, listedUpvotes: 6},
			} {
				f.after(t, sortAt)
				f.apply(t, repo, step.total, 0, f.now.Add(time.Duration(index+2)*time.Minute), true)
				require.True(t, f.groupSort(t).Equal(sortAt), "total %d must preserve the previous group sort", step.total)
				f.unread(t, repo, 0)
				f.listedUpvotes(t, repo, step.listedUpvotes)
				require.Equal(t, 5, f.upvotePeak(t))
			}

			f.after(t, sortAt)
			f.apply(t, repo, 6, 0, f.now.Add(6*time.Minute), true)
			require.True(t, f.groupSort(t).After(sortAt))
			f.unread(t, repo, 1)
			require.Equal(t, 6, f.upvotePeak(t))
		})
	}
}

func TestBridgedVotesPeak_GroupDeletedAtZeroReturnsOnlyAbovePeak(t *testing.T) {
	t.Parallel()
	f := newBridgedNotificationFixture(t, "post", "native")
	repo := f.repository()
	seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 3, 0, f.now.Add(-time.Hour))
	_, err := f.db.ExecContext(f.ctx, `UPDATE posts SET bridged_upvote_peak = 3 WHERE uri = $1`, f.subject)
	require.NoError(t, err)
	f.seedGroup(t, f.now.Add(-time.Minute))

	f.apply(t, repo, 0, 0, f.now, true)
	require.Equal(t, 0, f.groupCount(t))
	require.Equal(t, 3, f.upvotePeak(t))
	f.apply(t, repo, 3, 0, f.now.Add(time.Minute), true)
	require.Equal(t, 0, f.groupCount(t))
	require.Equal(t, 3, f.upvotePeak(t))
	f.apply(t, repo, 4, 0, f.now.Add(2*time.Minute), true)
	require.Equal(t, 1, f.groupCount(t))
	var root string
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT root_post_uri FROM notifications
		WHERE recipient_did = $1 AND subject_uri = $2 AND reason = 'upvote'`, f.recipient, f.subject).Scan(&root))
	require.Equal(t, f.root, root)
	f.unread(t, repo, 1)
	require.Equal(t, 4, f.upvotePeak(t))
}

func TestBridgedVotesPeak_Bookkeeping(t *testing.T) {
	for _, row := range []struct {
		name             string
		recipientMode    string
		storedPeak       int
		incoming         int
		stale            bool
		unwired          bool
		groupFails       bool
		communityRemoved bool
		wantApplied      bool
		wantTotal        int
		wantPeak         int
	}{
		{name: "community removed", recipientMode: "native", storedPeak: 3, incoming: 6, communityRemoved: true, wantApplied: true, wantTotal: 6, wantPeak: 6},
		{name: "erased recipient", recipientMode: "erased", storedPeak: 3, incoming: 6, wantApplied: true, wantTotal: 6, wantPeak: 6},
		{name: "notifications not wired", recipientMode: "native", storedPeak: 3, incoming: 6, unwired: true, wantApplied: true, wantTotal: 6, wantPeak: 6},
		{name: "stale asOf", recipientMode: "native", storedPeak: 3, incoming: 6, stale: true, wantTotal: 3, wantPeak: 3},
		{name: "decrease", recipientMode: "native", storedPeak: 5, incoming: 2, wantApplied: true, wantTotal: 2, wantPeak: 5},
		{name: "group write fails", recipientMode: "native", storedPeak: 3, incoming: 6, groupFails: true, wantTotal: 3, wantPeak: 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, "post", row.recipientMode)
			baseline := f.now.Add(-time.Hour)
			seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 3, 0, baseline)
			_, err := f.db.ExecContext(f.ctx, `UPDATE posts SET bridged_upvote_peak = $2 WHERE uri = $1`, f.subject, row.storedPeak)
			require.NoError(t, err)
			if row.communityRemoved {
				seedVisibilityAdmission(t, f.db, f.community, f.root, posts.AdmissionStatusRemoved, "", "")
			}

			repo := f.repository()
			store := f.store(repo)
			var sentinel error
			if row.unwired {
				store = NewBridgedVotesRepository(f.db)
			}
			if row.groupFails {
				sentinel = errors.New("group write failed")
				store = f.store(bridgedNotificationRepositoryDecorator{Repository: repo,
					apply: func(context.Context, *sql.Tx, notifications.UpvoteGroupIntent) error { return sentinel },
				})
			}
			asOf := f.now.Add(time.Minute)
			if row.stale {
				asOf = baseline.Add(-time.Second)
			}
			applied, err := store.ApplyAggregate(f.ctx, bridgedvotes.Aggregate{URI: f.subject, Upvotes: row.incoming, AsOf: asOf})
			if row.groupFails {
				require.ErrorIs(t, err, sentinel)
				require.False(t, applied)
			} else {
				require.NoError(t, err)
				require.Equal(t, row.wantApplied, applied)
			}
			var total int
			require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT bridged_upvote_count FROM posts WHERE uri = $1`, f.subject).Scan(&total))
			require.Equal(t, row.wantTotal, total)
			require.Equal(t, row.wantPeak, f.upvotePeak(t))
			require.Equal(t, 0, f.groupCount(t))
		})
	}
}

func TestBridgedVotesPeak_CountWrittenWithoutPeak(t *testing.T) {
	for _, kind := range []string{"post", "comment"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, kind, "native")
			repo := f.repository()
			seedStoredAggregate(t, f.ctx, f.db, f.table, f.subject, 5, 0, f.now.Add(-time.Hour))
			require.Equal(t, 0, f.groupCount(t))
			f.apply(t, repo, 3, 0, f.now.Add(time.Minute), true)
			require.Equal(t, 0, f.groupCount(t))
			require.Equal(t, 5, f.upvotePeak(t))
			f.apply(t, repo, 5, 0, f.now.Add(2*time.Minute), true)
			require.Equal(t, 0, f.groupCount(t))
			require.Equal(t, 5, f.upvotePeak(t))
			f.apply(t, repo, 6, 0, f.now.Add(3*time.Minute), true)
			require.Equal(t, 1, f.groupCount(t))
			require.Equal(t, 6, f.upvotePeak(t))
		})
	}
}
