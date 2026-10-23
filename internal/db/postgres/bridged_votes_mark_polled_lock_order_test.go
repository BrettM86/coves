//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Account erasure locks comments before posts, each in URI order. MarkPolled
// must take the same order: with an erasure-shaped transaction already holding
// the comment, MarkPolled must wait without holding the post, so the erasure
// transaction can still lock the post instead of one side aborting with 40P01.
func TestBridgedVotesRepository_MarkPolledLocksCommentsBeforePostsLikeErasure(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedBridgedVotesCandidates(t, ctx, db)
	subjects := []string{fixture.p1, fixture.c1}

	erasure, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = erasure.Rollback() }()
	var erasurePID int
	require.NoError(t, erasure.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&erasurePID))
	_, err = erasure.ExecContext(ctx,
		`SELECT 1 FROM comments WHERE uri = ANY($1) ORDER BY uri FOR NO KEY UPDATE`, pq.Array(subjects))
	require.NoError(t, err, "erasure-shaped transaction locks the comment first")

	marked := make(chan error, 1)
	go func() { marked <- NewBridgedVotesRepository(db).MarkPolled(ctx, subjects) }()
	bridgedRaceBlocked(t, db, ctx, erasurePID, "bridged_polled_at")

	_, err = erasure.ExecContext(ctx,
		`SELECT 1 FROM posts WHERE uri = ANY($1) ORDER BY uri FOR NO KEY UPDATE`, pq.Array(subjects))
	require.NoError(t, err, "erasure-shaped transaction must lock the post while MarkPolled waits")
	require.NoError(t, erasure.Commit())

	select {
	case err := <-marked:
		require.False(t, notificationRaceDeadlock(err), "MarkPolled deadlocked against erasure lock order: %v", err)
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("MarkPolled did not finish: %v", ctx.Err())
	}
	require.True(t, readBridgedPolledAt(t, ctx, db, "posts", fixture.p1).Valid, "post watermark advances after the wait")
	require.True(t, readBridgedPolledAt(t, ctx, db, "comments", fixture.c1).Valid, "comment watermark advances after the wait")
}
