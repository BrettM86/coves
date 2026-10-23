//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// erasureLockWaiters waits until at least count backends in this test database
// are waiting on a row or relation lock (advisory waits excluded).
func erasureLockWaiters(t *testing.T, db *sql.DB, ctx context.Context, count int) {
	t.Helper()
	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var waiting int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND wait_event_type = 'Lock'
				AND wait_event <> 'advisory'`).Scan(&waiting)
		return waiting >= count, err
	}, testkit.WithDescription("%d erasures waiting on a lock", count))
}

func erasureResultWithin(t *testing.T, ctx context.Context, results <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-ctx.Done():
		t.Fatalf("erasure of %s did not finish: %v", name, ctx.Err())
		return nil
	}
}

// Two accounts that voted on each other's content are erased at the same time.
// Their erasure advisory locks are per DID and do not serialize them, so the
// row locks each erasure takes on its own content and on the subjects it
// recounts must be acquired in one global order, or one of them aborts with a
// deadlock (40P01) and the account deletion fails.
//
// The fixture transaction holds a row each erasure needs after it has started
// taking content locks, so both are in flight when it lets them go.
func TestUserRepo_Delete_ConcurrentErasuresOfInteractingAccountsDoNotDeadlock(t *testing.T) {
	for _, row := range []struct {
		name string
		// seed gives each account content the other voted on and returns the
		// statement the fixture transaction runs to hold both erasures.
		seed func(t *testing.T, db *sql.DB, communityDID, firstDID, secondDID string) (holdQuery string, holdArgs []any)
	}{
		{
			name: "own comments",
			seed: func(t *testing.T, db *sql.DB, communityDID, firstDID, secondDID string) (string, []any) {
				host := seedErasurePost(t, db, communityDID, "did:plc:"+testkit.UniqueIDWithPrefix(t, "host"), "host", recountCounts{}, 0, 0, 2)
				firstComment := seedErasureComment(t, db, firstDID, host, host, "firstcomment", recountCounts{upvotes: 1, score: 1}, 0, false)
				secondComment := seedErasureComment(t, db, secondDID, host, host, "secondcomment", recountCounts{upvotes: 1, score: 1}, 0, false)
				seedErasureVote(t, db, firstDID, secondComment, "up")
				seedErasureVote(t, db, secondDID, firstComment, "up")
				// Each erasure deletes its votes after its comments.
				return `SELECT 1 FROM votes WHERE voter_did = ANY($1) FOR UPDATE`, []any{pq.Array([]string{firstDID, secondDID})}
			},
		},
		{
			name: "own posts",
			seed: func(t *testing.T, db *sql.DB, communityDID, firstDID, secondDID string) (string, []any) {
				firstPost := seedErasurePost(t, db, communityDID, firstDID, "firstpost", recountCounts{upvotes: 1, score: 1}, 0, 0, 0)
				secondPost := seedErasurePost(t, db, communityDID, secondDID, "secondpost", recountCounts{upvotes: 1, score: 1}, 0, 0, 0)
				seedErasureVote(t, db, firstDID, secondPost, "up")
				seedErasureVote(t, db, secondDID, firstPost, "up")
				// Each erasure deletes its post admissions after recounting the
				// subjects of its votes and before deleting its posts.
				var admissionURIs []string
				for _, did := range []string{firstDID, secondDID} {
					uri := "at://" + did + "/social.coves.community.post/" + testkit.TID()
					_, err := db.ExecContext(context.Background(), `
						INSERT INTO community_post_admissions (community_did, post_uri, status)
						VALUES ($1, $2, 'pending')`, communityDID, uri)
					require.NoError(t, err)
					admissionURIs = append(admissionURIs, uri)
				}
				return `SELECT 1 FROM community_post_admissions WHERE post_uri = ANY($1) FOR UPDATE`, []any{pq.Array(admissionURIs)}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			db := testkit.DB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			repo := NewUserRepository(db)

			communityName := testkit.UniqueIDWithPrefix(t, "erasepair")
			communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
			require.NoError(t, err)

			firstDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "first")
			secondDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "second")
			for _, did := range []string{firstDID, secondDID} {
				_, err := repo.Create(ctx, &users.User{DID: did, Handle: testkit.UniqueIDWithPrefix(t, "pair") + ".test", PDSURL: "https://test.pds"})
				require.NoError(t, err)
			}
			holdQuery, holdArgs := row.seed(t, db, communityDID, firstDID, secondDID)

			// The fixture transaction and the two erasures fill the clone's
			// pool, so lock waits are observed on a separate connection.
			var databaseName string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName))
			observer, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(databaseName))
			require.NoError(t, err)
			observer.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, observer.Close()) })

			hold, holdProcessID := notificationRaceTransaction(t, db, ctx)
			_, err = hold.ExecContext(ctx, holdQuery, holdArgs...)
			require.NoError(t, err)

			firstResult := make(chan error, 1)
			go func() { firstResult <- repo.Delete(ctx, firstDID) }()
			notificationRaceBlockedDelete(t, observer, ctx, holdProcessID, "")

			secondResult := make(chan error, 1)
			go func() { secondResult <- repo.Delete(ctx, secondDID) }()
			erasureLockWaiters(t, observer, ctx, 2)
			notificationRaceRequirePending(t, firstResult)
			notificationRaceRequirePending(t, secondResult)

			require.NoError(t, hold.Rollback())

			firstErr := erasureResultWithin(t, ctx, firstResult, "first account")
			secondErr := erasureResultWithin(t, ctx, secondResult, "second account")
			assert.False(t, notificationRaceDeadlock(firstErr), "first erasure deadlocked: %v", firstErr)
			assert.False(t, notificationRaceDeadlock(secondErr), "second erasure deadlocked: %v", secondErr)
			require.NoError(t, firstErr)
			require.NoError(t, secondErr)
		})
	}
}

// A vote consumer that has incremented a subject's counts but not yet
// committed holds that subject row. Erasure's recount must not take its vote
// snapshot until that consumer commits, or it writes a tally that misses the
// new vote.
func TestUserRepo_Delete_RecountWaitsForUncommittedVote(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := NewUserRepository(db)

	communityName := testkit.UniqueIDWithPrefix(t, "erasevote")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)

	erasedDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "erased")
	survivorDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "survivor")
	lateVoterDID := "did:plc:" + testkit.UniqueIDWithPrefix(t, "late")
	_, err = repo.Create(ctx, &users.User{DID: erasedDID, Handle: testkit.UniqueIDWithPrefix(t, "erased") + ".test", PDSURL: "https://test.pds"})
	require.NoError(t, err)

	votedPost := seedErasurePost(t, db, communityDID, "did:plc:"+testkit.UniqueIDWithPrefix(t, "author"), "voted", recountCounts{upvotes: 2, score: 2}, 0, 0, 0)
	seedErasureVote(t, db, erasedDID, votedPost, "up")
	seedErasureVote(t, db, survivorDID, votedPost, "up")

	// The vote consumer's insert and post up-vote increment (vote_consumer.go),
	// left uncommitted.
	consumer, consumerProcessID := notificationRaceTransaction(t, db, ctx)
	rkey := testkit.TID()
	_, err = consumer.ExecContext(ctx, `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafyerasurevote', $2, $3, $4, 'bafyerasuresubject', 'up', $5, NOW())
		ON CONFLICT (uri) DO NOTHING`,
		"at://"+lateVoterDID+"/social.coves.feed.vote/"+rkey, rkey, lateVoterDID, votedPost,
		time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	_, err = consumer.ExecContext(ctx, `
		UPDATE posts
		SET upvote_count = upvote_count + 1,
		    score = upvote_count + 1 - downvote_count + bridged_upvote_count - bridged_downvote_count
		WHERE uri = $1`, votedPost)
	require.NoError(t, err)

	deleted := make(chan error, 1)
	go func() { deleted <- repo.Delete(ctx, erasedDID) }()
	notificationRaceBlockedDelete(t, db, ctx, consumerProcessID, "")
	notificationRaceRequirePending(t, deleted)

	require.NoError(t, consumer.Commit())
	require.NoError(t, erasureResultWithin(t, ctx, deleted, "erased account"))

	assert.Equal(t, recountCounts{upvotes: 2, downvotes: 0, score: 2}, postCountsOf(t, db, votedPost),
		"the survivor's and the late voter's up-votes are live once the erased vote is gone")
}
