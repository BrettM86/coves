//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForGatedInsertAndOneWaiter waits until one request is held inside its
// action insert by holdModerationActionInsert and exactly one other request is
// blocked on a lock. With the actor lock the second request waits in
// pg_advisory_xact_lock before reading its key; without it the second request
// waits on the subject row or the same insert gate, and the outcome differs.
func waitForGatedInsertAndOneWaiter(t *testing.T, connection *sql.Conn) {
	t.Helper()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		var waiting, inserting int
		err := connection.QueryRowContext(t.Context(), `
			SELECT count(*), count(*) FILTER (WHERE query LIKE '%INSERT INTO moderation_actions%')
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
		`).Scan(&waiting, &inserting)
		return waiting == 2 && inserting >= 1, err
	}, testkit.WithDescription("one action insert gated while the other request of the same actor waits on a lock"))
}

func TestModerationConcurrentIdenticalKeyRetryAppliesOnce(t *testing.T) {
	db := testkit.DB(t)
	subject, _, _ := indexedModerationComment(t, db, true, "")
	service := newPostgresModerationService(db)
	actor := fixtures.DID("retryraceadmin")
	request := moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "retry-race-key",
		Reason: moderationConcurrencyReason,
	}
	connection, release := holdModerationActionInsert(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	remove := func() (*moderation.MutationResult, error) {
		return service.RemoveContent(ctx, actor, request)
	}
	results := startConcurrentModerationCalls(remove, remove)
	waitForGatedInsertAndOneWaiter(t, connection)
	release()
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotNil(t, first.result)
	require.NotNil(t, second.result)
	assert.Equal(t, moderation.OutcomeApplied, first.result.Outcome)
	assert.Equal(t, first.result, second.result, "the retry must replay the stored result of the first request")
	assert.Equal(t, 1, countModerationActions(t, db, subject.URI, moderation.ActionRemove))
	var keys int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1`, actor).Scan(&keys))
	assert.Equal(t, 1, keys)
}
