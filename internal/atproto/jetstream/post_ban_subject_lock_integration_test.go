//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post indexing for author D waits while another transaction holds D's
// ban-subject advisory lock (communities.BanSubjectLockKeySQL), so a
// concurrent community create on D either sees the post or commits first.
func TestPostConsumer_WaitsOnAuthorBanSubjectLock(t *testing.T) {
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	connection, err := db.Conn(t.Context())
	require.NoError(t, err)
	holder, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := holder.Rollback()
		assert.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone), "rolling back the holder: %v", rollbackError)
		assert.NoError(t, connection.Close())
	})
	var holderProcessID int
	require.NoError(t, holder.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&holderProcessID))
	_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, pv2Author)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	rkey := testkit.TID()
	results := make(chan error, 1)
	go func() {
		results <- fixture.consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", rkey, testkit.TID(),
			"bafyreibansubjectlock", time.Now().UnixMicro(), pv2Record(pv2Community, "locked post", "body")))
	}()
	var waiterProcessID int
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE((
				SELECT pid FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'
				  AND $1 = ANY(pg_blocking_pids(pid))
				LIMIT 1
			), 0)`, holderProcessID).Scan(&waiterProcessID)
		return waiterProcessID != 0, err
	}, testkit.WithDescription("post indexing waits on the author's ban-subject advisory lock"))
	// The lock comes before the post row: the waiting transaction has not
	// touched posts, so a community create holding the lock never waits on it.
	var postsLocks int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND relation = 'posts'::regclass`, waiterProcessID).Scan(&postsLocks))
	assert.Zero(t, postsLocks, "the waiting post transaction holds no lock on posts")

	require.NoError(t, holder.Commit())
	select {
	case err = <-results:
	case <-ctx.Done():
		t.Fatalf("post indexing did not finish after the holder committed: %v", ctx.Err())
	}
	require.NoError(t, err)
	author, community, _, _, _ := readPV2Post(t, db, pv2URI(pv2Author, rkey))
	assert.Equal(t, pv2Author, author)
	assert.Equal(t, pv2Community, community)
}
