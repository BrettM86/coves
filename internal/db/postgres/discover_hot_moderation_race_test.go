//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// insertDiscoverHotRemoval uses the same action/decision shape as
// moderationTransaction.SetRemovalDecision. It avoids reading the posts view
// while the hydration-boundary gate holds its advisory lock.
func insertDiscoverHotRemoval(t *testing.T, db *sql.DB, uri string) {
	t.Helper()
	actionID := "hot-removal-" + testkit.UniqueID(t)
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection,
		     action, reason, origin, created_at)
		VALUES ($1, 'did:plc:hotmoderator', 'did:plc:hotinstance', 'instance', $2,
		        'social.coves.community.post', 'remove',
		        'social.coves.moderation.defs#reasonSpam', 'local', NOW())
	`, actionID, uri)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ('did:plc:hotinstance', 'instance', NULL, $2, 'removal', NULL, $1, TRUE)
	`, actionID, uri)
	require.NoError(t, err)
}

func TestDiscoverRepo_HotRefillsAfterInstanceRemovalAtHydrationBoundary(t *testing.T) {
	for _, timing := range []string{"before-revalidation", "after-revalidation-before-hydration"} {
		t.Run(timing, func(t *testing.T) {
			db := testkit.DB(t)
			ctx := t.Context()
			const (
				authorDID = "did:plc:hotmoderationauthor"
				lockClass = 1_126_644_054
				lockID    = 1
			)
			createTestUser(t, db, "hot-moderation-author.test", authorDID)
			communities := []string{"did:plc:hotmoderationa", "did:plc:hotmoderationb", "did:plc:hotmoderationc"}
			for i, did := range communities {
				createTestCommunity(t, db, did, "hot-moderation-"+string(rune('a'+i))+".test", authorDID)
			}
			createdAt := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
			seed := func(communityDID, rkey string, score int) string {
				t.Helper()
				uri := seedFilterablePost(t, db, communityDID, authorDID, rkey, createdAt)
				_, err := db.ExecContext(ctx, `UPDATE posts SET score = $2, upvote_count = $2 WHERE uri = $1`, uri, score)
				require.NoError(t, err)
				return uri
			}
			a1 := seed(communities[0], "hotmoda1", 1_000_000_000)
			removedA2 := seed(communities[0], "hotmoda2", 1_000_000)
			a3 := seed(communities[0], "hotmoda3", 2_000)
			b1 := seed(communities[1], "hotmodb1", 5_000)
			c1 := seed(communities[2], "hotmodc1", 100)

			repo := NewDiscoverRepository(db, "hot-moderation-race-secret").(*postgresDiscoverRepo)
			repo.discoverHotWorkDeadline = 10 * time.Second
			first, cursor, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 1})
			require.NoError(t, err)
			require.Equal(t, []string{a1}, discoverURIs(first))
			require.NotNil(t, cursor)

			type pageResult struct {
				feed   []*discover.FeedViewPost
				cursor *string
				err    error
			}
			var second pageResult
			if timing == "before-revalidation" {
				insertDiscoverHotRemoval(t, db, removedA2)
				second.feed, second.cursor, second.err = repo.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 2, Cursor: cursor})
			} else {
				_, err := db.ExecContext(ctx, `
					CREATE TABLE hot_moderation_gate (candidate_uri TEXT PRIMARY KEY, lock_class INTEGER, lock_id INTEGER);
					CREATE FUNCTION hot_moderation_wait(candidate_uri TEXT) RETURNS BOOLEAN
					LANGUAGE plpgsql VOLATILE AS $$
					DECLARE gate hot_moderation_gate%ROWTYPE;
					BEGIN
						SELECT * INTO gate FROM hot_moderation_gate WHERE hot_moderation_gate.candidate_uri = $1;
						IF FOUND THEN PERFORM pg_advisory_xact_lock(gate.lock_class, gate.lock_id); END IF;
						RETURN TRUE;
					END;
					$$;
					ALTER TABLE posts RENAME TO hot_moderation_posts;
					CREATE VIEW posts WITH (security_barrier = true) AS
					SELECT stored.* FROM hot_moderation_posts stored WHERE hot_moderation_wait(stored.uri);
				`)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO hot_moderation_gate VALUES ($1, $2, $3)`, removedA2, lockClass, lockID)
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
				var holderPID int
				require.NoError(t, lockConnection.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
				results := make(chan pageResult, 1)
				go func() {
					feed, next, requestErr := repo.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 2, Cursor: cursor})
					results <- pageResult{feed: feed, cursor: next, err: requestErr}
				}()
				waitForDiscoverHotAdvisoryLock(t, db, holderPID)
				insertDiscoverHotRemoval(t, db, removedA2)
				_, err = lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockClass, lockID)
				require.NoError(t, err)
				locked = false
				second = <-results
			}

			require.NoError(t, second.err)
			require.Equal(t, []string{b1, a3}, discoverURIs(second.feed), "removal must refill without penalizing A's diversity slot")
			require.NotNil(t, second.cursor)
			third, end, err := repo.GetDiscover(ctx, discover.GetDiscoverRequest{Sort: "hot", Limit: 2, Cursor: second.cursor})
			require.NoError(t, err)
			require.Equal(t, []string{c1}, discoverURIs(third))
			require.Nil(t, end)
			all := append(append(discoverURIs(first), discoverURIs(second.feed)...), discoverURIs(third)...)
			require.Equal(t, []string{a1, b1, a3, c1}, all, "every eligible snapshot candidate exactly once in checkpoint order")
			require.NotContains(t, all, removedA2)
		})
	}
}
