//go:build integration

package postgres_test

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cdnPurgeRowSnapshot struct {
	State                string
	Attempts             int
	NextAttemptAtMinimum bool
	NextAttemptAt        sql.NullTime
	EarliestCompletionAt sql.NullTime
	LastFailureCode      sql.NullString
	Generation           int64
	Claim                int64
}

func readCDNPurgeRow(t *testing.T, db *sql.DB, blob imageproxy.BlockedBlob) cdnPurgeRowSnapshot {
	t.Helper()
	var row cdnPurgeRowSnapshot
	err := db.QueryRowContext(t.Context(), `
		SELECT state, attempts, next_attempt_at = '-infinity',
		       CASE WHEN next_attempt_at = '-infinity' THEN NULL ELSE next_attempt_at END,
		       earliest_completion_at, last_failure_code, generation, claim
		FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2
	`, blob.OwnerDID, blob.CID).Scan(&row.State, &row.Attempts, &row.NextAttemptAtMinimum, &row.NextAttemptAt,
		&row.EarliestCompletionAt, &row.LastFailureCode, &row.Generation, &row.Claim)
	require.NoError(t, err)
	return row
}

// The standalone recording path runs on anonymous blocked-URL requests and
// hourly ownerless sweeps. Re-recording a pair that is still pending must not
// reset its backoff, void its claim or report it for a fresh immediate attempt;
// a completed pair goes back to pending and is reported.
func TestModerationRepositoryRecordCDNPurgeTargetsLeavesPendingTargetsUntouched(t *testing.T) {
	db := testkit.DB(t)
	repository := postgres.NewModerationRepository(db)
	owner := fixtures.DID(testkit.UniqueIDWithPrefix(t, "cdnrec"))
	backedOff := imageproxy.BlockedBlob{OwnerDID: owner, CID: moderationImageCIDOne}
	completed := imageproxy.BlockedBlob{OwnerDID: owner, CID: moderationImageCIDTwo}

	pended, err := repository.RecordCDNPurgeTargets(t.Context(), []imageproxy.BlockedBlob{backedOff, completed})
	require.NoError(t, err)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{backedOff, completed}, pended, "new targets are reported")

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	targets, err := repository.ClaimDueCDNPurgeTargets(t.Context(), moderation.CDNPurgeClaim{
		DueAt: now, Now: func() time.Time { return now }, Lease: 2 * time.Minute, WriteTimeout: 5 * time.Second,
		Limit: 10, Blobs: []imageproxy.BlockedBlob{backedOff, completed},
	})
	require.NoError(t, err)
	require.Len(t, targets, 2)
	for _, target := range targets {
		switch target.Blob {
		case backedOff:
			require.NoError(t, repository.RescheduleCDNPurgeTarget(t.Context(), target, now.Add(time.Hour), "http_500"))
		case completed:
			require.NoError(t, repository.CompleteCDNPurgeTarget(t.Context(), target))
		default:
			t.Fatalf("claimed an unrecorded target %+v", target.Blob)
		}
	}
	backedOffBefore := readCDNPurgeRow(t, db, backedOff)
	require.Equal(t, "pending", backedOffBefore.State)
	require.Equal(t, 1, backedOffBefore.Attempts)
	require.True(t, backedOffBefore.NextAttemptAt.Valid)
	completedBefore := readCDNPurgeRow(t, db, completed)
	require.Equal(t, "completed", completedBefore.State)

	pended, err = repository.RecordCDNPurgeTargets(t.Context(), []imageproxy.BlockedBlob{backedOff, completed})
	require.NoError(t, err)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{completed}, pended, "only the completed target is re-pended")

	assert.Equal(t, backedOffBefore, readCDNPurgeRow(t, db, backedOff),
		"a pending target keeps its attempts, backoff, claim and generation")
	completedAfter := readCDNPurgeRow(t, db, completed)
	assert.Equal(t, "pending", completedAfter.State)
	assert.Equal(t, 0, completedAfter.Attempts)
	assert.True(t, completedAfter.NextAttemptAtMinimum, "a re-pended target is due immediately")
	assert.False(t, completedAfter.EarliestCompletionAt.Valid)
	assert.Equal(t, completedBefore.Generation+1, completedAfter.Generation)
	assert.Equal(t, completedBefore.Claim+1, completedAfter.Claim)
}
