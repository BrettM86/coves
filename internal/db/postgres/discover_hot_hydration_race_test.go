//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRepo_HotRefillsWhenCandidateDisappearsBeforeHydration(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	const (
		authorDID = "did:plc:hothydrationauthor"
		lockClass = 1_126_644_053
		lockID    = 1
	)
	createTestUser(t, db, "hot-hydration-author.test", authorDID)

	communities := []string{
		"did:plc:hothydrationa",
		"did:plc:hothydrationb",
		"did:plc:hothydrationc",
	}
	for i, communityDID := range communities {
		createTestCommunity(t, db, communityDID, "hot-hydration-"+string(rune('a'+i))+".test", authorDID)
	}

	createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	seed := func(communityDID, rkey string, score int) string {
		t.Helper()
		uri := seedFilterablePost(t, db, communityDID, authorDID, rkey, createdAt)
		_, err := db.ExecContext(ctx, `
			UPDATE posts
			SET score = $2, upvote_count = $2
			WHERE uri = $1
		`, uri, score)
		require.NoError(t, err)
		return uri
	}

	a1 := seed(communities[0], "hothydrationa1", 1_000_000_000)
	vanishedA2 := seed(communities[0], "hothydrationa2", 1_000_000)
	a3 := seed(communities[0], "hothydrationa3", 2_000)
	b1 := seed(communities[1], "hothydrationb1", 5_000)
	c1 := seed(communities[2], "hothydrationc1", 100)

	repository := NewDiscoverRepository(db, "hot-hydration-race-secret").(*postgresDiscoverRepo)
	repository.discoverHotWorkDeadline = 10 * time.Second
	firstPage, inputCursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{a1}, discoverURIs(firstPage))
	require.NotNil(t, inputCursor)

	_, err = db.ExecContext(ctx, `
		CREATE TABLE discover_hot_hydration_test_gate (
			candidate_uri TEXT PRIMARY KEY,
			lock_class INTEGER NOT NULL,
			lock_id INTEGER NOT NULL
		);
		CREATE FUNCTION discover_hot_hydration_test_wait(candidate_uri TEXT)
		RETURNS BOOLEAN
		LANGUAGE plpgsql
		VOLATILE
		AS $$
		DECLARE
			gate discover_hot_hydration_test_gate%ROWTYPE;
		BEGIN
			SELECT * INTO gate
			FROM discover_hot_hydration_test_gate
			WHERE discover_hot_hydration_test_gate.candidate_uri = $1;
			IF FOUND THEN
				PERFORM pg_advisory_xact_lock(gate.lock_class, gate.lock_id);
			END IF;
			RETURN TRUE;
		END;
		$$;
		ALTER TABLE posts RENAME TO discover_hot_hydration_posts;
		CREATE VIEW posts WITH (security_barrier = true) AS
		SELECT stored.*
		FROM discover_hot_hydration_posts stored
		WHERE discover_hot_hydration_test_wait(stored.uri);
	`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO discover_hot_hydration_test_gate (candidate_uri, lock_class, lock_id)
		VALUES ($1, $2, $3)
	`, vanishedA2, lockClass, lockID)
	require.NoError(t, err)

	lockConnection, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lockConnection.Close()) })
	_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1, $2)`, lockClass, lockID)
	require.NoError(t, err)
	locked := true
	t.Cleanup(func() {
		if locked {
			_, unlockErr := lockConnection.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, lockClass, lockID)
			require.NoError(t, unlockErr)
		}
	})
	var lockHolderPID int
	require.NoError(t, lockConnection.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&lockHolderPID))

	type pageResult struct {
		feed   []*discover.FeedViewPost
		cursor *string
		err    error
	}
	results := make(chan pageResult, 1)
	go func() {
		feed, cursor, requestErr := repository.GetDiscover(ctx, discover.GetDiscoverRequest{
			Sort:   "hot",
			Limit:  2,
			Cursor: inputCursor,
		})
		results <- pageResult{feed: feed, cursor: cursor, err: requestErr}
	}()

	waitForDiscoverHotAdvisoryLock(t, db, lockHolderPID)
	mutation, err := db.ExecContext(ctx, `
		UPDATE discover_hot_hydration_posts
		SET deleted_at = clock_timestamp()
		WHERE uri = $1
	`, vanishedA2)
	require.NoError(t, err)
	mutated, err := mutation.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, mutated)
	_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockClass, lockID)
	require.NoError(t, err)
	locked = false

	secondPage := <-results
	require.NoError(t, secondPage.err, "a candidate disappearing after revalidation is an eligibility change, not a server error")
	assert.Equal(t, []string{b1, a3}, discoverURIs(secondPage.feed),
		"selection must restart from the input checkpoint so the vanished A post does not penalize A")
	assert.NotContains(t, discoverURIs(secondPage.feed), vanishedA2)
	require.NotNil(t, secondPage.cursor)

	finalPage, finalCursor, err := repository.GetDiscover(ctx, discover.GetDiscoverRequest{
		Sort:   "hot",
		Limit:  2,
		Cursor: secondPage.cursor,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{c1}, discoverURIs(finalPage),
		"the returned checkpoint must continue after exactly the posts that were emitted")
	assert.Nil(t, finalCursor)
	assert.NotContains(t, append(discoverURIs(secondPage.feed), discoverURIs(finalPage)...), vanishedA2)
}

func waitForDiscoverHotAdvisoryLock(t *testing.T, db *sql.DB, holderPID int) {
	t.Helper()
	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		var waiting bool
		err := db.QueryRowContext(context.Background(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks waiter
				INNER JOIN pg_locks holder
					ON holder.locktype = waiter.locktype
					AND holder.database = waiter.database
					AND holder.classid = waiter.classid
					AND holder.objid = waiter.objid
					AND holder.objsubid = waiter.objsubid
				WHERE holder.pid = $1
					AND holder.granted
					AND NOT waiter.granted
			)
		`, holderPID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("Discover Hot candidate revalidation waiting at the hydration boundary"))
}
