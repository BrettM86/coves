//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func snapshotActionCount(t *testing.T, db *sql.DB, actionID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions WHERE id = $1`, actionID).Scan(&count))
	return count
}

// Erasing an account deletes the snapshots of its removed content, found by
// the human author rather than by the repository that holds the record: a
// legacy post lives in its community's repository. The action rows stay, and
// other authors' snapshots are untouched.
func TestModerationContentSnapshotAuthorErasure(t *testing.T) {
	db := testkit.DB(t)
	communityDID, erasedDID := moderationPostActors(t, db)
	keptName := testkit.UniqueIDWithPrefix(t, "snapkept")
	keptDID := fixtures.DID(keptName)
	fixtures.User(t, db, keptName+".test", keptDID)
	service := restrictedLogService(db)

	keptPost := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, keptDID, "Kept author title", "")
	legacyPost := indexedModerationPost(t, db, moderation.LegacyPostCollection, communityDID, erasedDID, "Erased legacy title", "")
	erasedComment := restrictedLogComment(t, db, erasedDID, keptPost)
	erasedRemovals := map[string]string{
		"legacy post": restrictedLogRemove(t, service, legacyPost, snapshotReasonSpam),
		"comment":     restrictedLogRemove(t, service, erasedComment, snapshotReasonSpam),
	}
	keptRemoval := restrictedLogRemove(t, service, keptPost, snapshotReasonSpam)
	for name, id := range erasedRemovals {
		_, admin := snapshotLogEntries(t, service, id)
		require.Contains(t, admin, "contentSnapshot", "the %s removal must keep a snapshot before erasure", name)
	}

	require.NoError(t, postgres.NewUserRepository(db).Delete(t.Context(), erasedDID))

	for name, id := range erasedRemovals {
		_, admin := snapshotLogEntries(t, service, id)
		assert.NotContains(t, admin, "contentSnapshot", "the erased author's %s snapshot must be gone", name)
		assert.Contains(t, admin, "privateSubject", "the %s log entry stays for admins", name)
	}
	_, admin := snapshotLogEntries(t, service, keptRemoval)
	assert.Equal(t, map[string]any{"title": "Kept author title", "text": "indexed post content", "capturedCid": moderationPostCID}, admin["contentSnapshot"])

	for name, id := range erasedRemovals {
		assert.Equal(t, 1, snapshotActionCount(t, db, id), "the %s action row stays", name)
		assert.Equal(t, 0, snapshotRowCount(t, db, id), "the %s snapshot row is deleted", name)
	}
	assert.Equal(t, 1, snapshotRowCount(t, db, keptRemoval), "another author's snapshot stays")
}

// An erasure that runs while a removal of the author's post is in flight must
// still delete the snapshot that removal commits: erasure waits on the
// removal's share lock on the post row, and sweeps snapshots after that.
func TestModerationContentSnapshotErasureWaitsForInFlightRemoval(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "In-flight removal title", "")
	gate, release := holdRestoreBeforeCommit(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	removed := make(chan moderationConcurrentOutcome, 1)
	go func() {
		result, err := service.RemoveContent(ctx, fixtures.DID("snapshotraceadmin"), moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "snapshot-race-remove", Reason: snapshotReasonSpam,
		})
		removed <- moderationConcurrentOutcome{result: result, err: err}
	}()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		var parked int
		err := gate.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND wait_event = 'advisory'
			  AND query LIKE '%INSERT INTO moderation_idempotency_keys%'
		`).Scan(&parked)
		return parked == 1, err
	}, testkit.WithDescription("the removal parked at its last statement, after its snapshot write"))

	erased := make(chan error, 1)
	go func() { erased <- postgres.NewUserRepository(db).Delete(ctx, authorDID) }()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		var waiting int
		err := gate.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND wait_event <> 'advisory'
			  AND query LIKE '%DELETE FROM posts WHERE author_did%'
		`).Scan(&waiting)
		return waiting == 1, err
	}, testkit.WithDescription("the erasure waiting on the in-flight removal's lock on the post row"))

	release()
	outcome := <-removed
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, moderation.OutcomeApplied, outcome.result.Outcome)
	require.NotNil(t, outcome.result.Action)
	require.NoError(t, <-erased)

	assert.Equal(t, 1, snapshotActionCount(t, db, outcome.result.Action.ID), "the removal's action row stays")
	assert.Equal(t, 0, snapshotRowCount(t, db, outcome.result.Action.ID), "no snapshot of the erased author may survive the race")
}
