//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/adminreports"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A report that bumps the communityReport group of a moderator whose account
// erasure is in flight must serialize with that erasure instead of
// deadlocking it. The transaction below takes account erasure's lock order
// (user_repo.go Delete): the exclusive erasure gate and the marker, then the
// moderator's notifications, and the users row only after the report is
// waiting on it.
func TestCommunityReportNotification_ModeratorErasureDuringReportDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	f := newReportNotificationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	community := f.rosteredCommunity(t, hostedCommunity, "rnerase", 1)
	moderator := community.Moderators[0]
	postURI := seedVisibilityPost(t, f.db, community.DID, "did:plc:rnauthor", "rnerasepost", "reported", communityQueuePostCreated)
	newReporter := func(prefix string) string {
		id := testkit.UniqueIDWithPrefix(t, prefix)
		did := "did:plc:" + id
		createTestUser(t, f.db, id+".test", did)
		return did
	}
	f.report(t, newReporter("rnerasea"), postURI, adminreports.TargetTypePost, adminreports.ReasonSpam, reportNotificationAt)
	require.Equal(t, 1, storedCommunityReportRows(t, f.db, moderator, community.DID), "the first report gives the moderator a group row")

	erasure, erasureProcessID := notificationRaceTransaction(t, f.db, ctx)
	_, err := erasure.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", moderator)
	require.NoError(t, err)
	_, err = erasure.ExecContext(ctx, `INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, moderator)
	require.NoError(t, err)
	_, err = erasure.ExecContext(ctx, `DELETE FROM notifications WHERE recipient_did = $1 OR actor_did = $1`, moderator)
	require.NoError(t, err)

	secondReporter := newReporter("rneraseb")
	results := make(chan error, 1)
	go func() {
		results <- f.reports.Create(ctx, &adminreports.Report{
			ReporterDID: secondReporter, TargetURI: postURI,
			TargetType: adminreports.TargetTypePost, Reason: adminreports.ReasonSpam,
			Status: adminreports.StatusOpen,
		})
	}()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var processID int
		err := f.db.QueryRowContext(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND $1 = ANY(pg_blocking_pids(pid))
				AND wait_event_type = 'Lock'
			LIMIT 1`, erasureProcessID).Scan(&processID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}, testkit.WithDescription("the second report waiting on the moderator's erasure"))
	notificationRaceRequirePending(t, results)

	_, err = erasure.ExecContext(ctx, `DELETE FROM community_roles WHERE subject_did = $1`, moderator)
	require.NoError(t, err, "erasure deletes the moderator's roles")
	_, err = erasure.ExecContext(ctx, `DELETE FROM users WHERE did = $1`, moderator)
	require.NoError(t, err, "erasure deletes the moderator's users row without a deadlock")
	require.NoError(t, erasure.Commit(), "erasure commits")
	require.NoError(t, <-results, "the report submitted during the erasure is stored")

	assert.Zero(t, storedCommunityReportRows(t, f.db, moderator, community.DID), "the erased moderator has no communityReport group")
	assert.Equal(t, unreadGroup(community.DID, 2), f.listed(t, community.Owner), "the owner's group counts both reports")
}
