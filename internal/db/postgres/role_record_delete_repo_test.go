//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B3: the Postgres store of pending role record deletes claims due rows under
// a lease, removes a finished row, reschedules a failed one, and refuses a
// claim another claim took over, including one taken on a row that was
// finished and then enqueued again at the same record key.

const (
	roleDeleteCommunity = "did:plc:roledeletequeue"
	roleDeleteLease     = 10 * time.Minute
)

// roleDeleteBase is the cutoff the claims run at, a minute past the wall clock
// so a database clock a little ahead of the test host still finds rows "due".
func roleDeleteBase() time.Time {
	return time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
}

// seedRoleRecordDelete inserts a pending delete due at nextAttemptAt.
func seedRoleRecordDelete(t *testing.T, db *sql.DB, rkey, cid string, nextAttemptAt time.Time) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO community_role_record_deletes
		(community_did, record_key, record_cid, next_attempt_at) VALUES ($1, $2, $3, $4)`,
		roleDeleteCommunity, rkey, cid, nextAttemptAt)
	require.NoError(t, err)
}

func claimRoleRecordDeletes(t *testing.T, store *RoleRecordDeleteRepository, dueAt time.Time, limit int) []moderation.RoleRecordDelete {
	t.Helper()
	claimed, err := store.ClaimDueRoleRecordDeletes(t.Context(), moderation.RoleRecordDeleteClaim{
		DueAt: dueAt, Lease: roleDeleteLease, Limit: limit,
	})
	require.NoError(t, err)
	return claimed
}

// roleDeleteKeys is each claimed row's record key and CID.
func roleDeleteKeys(claimed []moderation.RoleRecordDelete) map[string]string {
	keys := map[string]string{}
	for _, d := range claimed {
		keys[d.RecordKey] = d.RecordCID
	}
	return keys
}

func newRoleDeleteStore(t *testing.T) (*sql.DB, *RoleRecordDeleteRepository) {
	t.Helper()
	db := testkit.DB(t)
	insertRoleTestCommunity(t, db, roleDeleteCommunity)
	return db, NewRoleRecordDeleteRepository(db)
}

func TestRoleRecordDeleteRepository_ClaimsDueRowsUnderALease(t *testing.T) {
	t.Parallel()
	db, store := newRoleDeleteStore(t)
	base := roleDeleteBase()
	seedRoleRecordDelete(t, db, "pastrolekey", "bafyreipastrole", base.Add(-10*time.Minute))
	seedRoleRecordDelete(t, db, "duerolekey", "bafyreiduerole", base)
	seedRoleRecordDelete(t, db, "futurerolekey", "bafyreifuturerole", base.Add(30*time.Minute))

	first := claimRoleRecordDeletes(t, store, base, 10)
	assert.Equal(t, map[string]string{"pastrolekey": "bafyreipastrole", "duerolekey": "bafyreiduerole"},
		roleDeleteKeys(first), "rows due at or before the cutoff are claimed, a future row is not")
	for _, d := range first {
		assert.Equal(t, roleDeleteCommunity, d.CommunityDID)
		assert.NotZero(t, d.ID, "a claimed row carries its row identity")
		assert.Equal(t, 0, d.Attempts)
		assert.Empty(t, d.LastFailureCode)
	}

	assert.Empty(t, claimRoleRecordDeletes(t, store, base.Add(time.Minute), 10),
		"rows under an unexpired lease are not claimed again")

	again := claimRoleRecordDeletes(t, store, base.Add(11*time.Minute), 10)
	assert.Equal(t, map[string]string{"pastrolekey": "bafyreipastrole", "duerolekey": "bafyreiduerole"},
		roleDeleteKeys(again), "rows are claimable again once the lease has passed")

	limited := claimRoleRecordDeletes(t, store, base.Add(22*time.Minute), 1)
	assert.Len(t, limited, 1, "a claim takes at most Limit rows")
}

func TestRoleRecordDeleteRepository_CompleteAndReschedule(t *testing.T) {
	t.Parallel()
	db, store := newRoleDeleteStore(t)
	base := roleDeleteBase()
	seedRoleRecordDelete(t, db, "completerolekey", "bafyreicompleterole", base.Add(-time.Minute))
	seedRoleRecordDelete(t, db, "retryrolekey", "bafyreiretryrole", base.Add(-time.Minute))

	claimed := claimRoleRecordDeletes(t, store, base, 10)
	require.Len(t, claimed, 2)
	byKey := map[string]moderation.RoleRecordDelete{}
	for _, d := range claimed {
		byKey[d.RecordKey] = d
	}

	require.NoError(t, store.CompleteRoleRecordDelete(t.Context(), byKey["completerolekey"]))
	assert.Equal(t, "[]", erasureRoleRows(t, db, "community_role_record_deletes", "record_key = 'completerolekey'"),
		"a completed delete's row is removed")

	retryAt := base.Add(12 * time.Minute)
	require.NoError(t, store.RescheduleRoleRecordDelete(t.Context(), byKey["retryrolekey"], retryAt, "pds_delete_failed"))
	assert.Empty(t, claimRoleRecordDeletes(t, store, base.Add(11*time.Minute), 10),
		"a rescheduled row is not due before its next attempt")
	retried := claimRoleRecordDeletes(t, store, retryAt, 10)
	require.Len(t, retried, 1, "a rescheduled row is due at its next attempt")
	assert.Equal(t, "retryrolekey", retried[0].RecordKey)
	assert.Equal(t, "bafyreiretryrole", retried[0].RecordCID)
	assert.Equal(t, 1, retried[0].Attempts)
	assert.Equal(t, "pds_delete_failed", retried[0].LastFailureCode)
	assert.True(t, retryAt.Equal(retried[0].NextAttemptAt), "next attempt at %s, want %s", retried[0].NextAttemptAt, retryAt)
}

func TestRoleRecordDeleteRepository_StaleClaimIsLost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// takeOver leaves the stale claim's record key held by a newer claim.
		takeOver func(t *testing.T, db *sql.DB, store *RoleRecordDeleteRepository, stale moderation.RoleRecordDelete, base time.Time)
	}{
		{
			name: "another claim took the row after the lease passed",
			takeOver: func(t *testing.T, _ *sql.DB, store *RoleRecordDeleteRepository, _ moderation.RoleRecordDelete, base time.Time) {
				require.Len(t, claimRoleRecordDeletes(t, store, base.Add(11*time.Minute), 10), 1)
			},
		},
		{
			// R5: the stale claim completed the row, then the same record
			// key was enqueued again as a fresh row and claimed.
			name: "the row was completed and enqueued again",
			takeOver: func(t *testing.T, db *sql.DB, store *RoleRecordDeleteRepository, stale moderation.RoleRecordDelete, base time.Time) {
				require.NoError(t, store.CompleteRoleRecordDelete(t.Context(), stale))
				seedRoleRecordDelete(t, db, "stalerolekey", "bafyreirecreatedrole", base.Add(-time.Minute))
				recreated := claimRoleRecordDeletes(t, store, base, 10)
				require.Len(t, recreated, 1)
				require.Equal(t, "bafyreirecreatedrole", recreated[0].RecordCID)
				require.NotEqual(t, stale.ID, recreated[0].ID, "the recreated row has a new identity")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, store := newRoleDeleteStore(t)
			base := roleDeleteBase()
			seedRoleRecordDelete(t, db, "stalerolekey", "bafyreistalerole", base.Add(-time.Minute))
			claimed := claimRoleRecordDeletes(t, store, base, 10)
			require.Len(t, claimed, 1)
			stale := claimed[0]
			tc.takeOver(t, db, store, stale, base)
			before := erasureRoleRows(t, db, "community_role_record_deletes", "true")

			assert.ErrorIs(t, store.CompleteRoleRecordDelete(t.Context(), stale), moderation.ErrRoleRecordDeleteClaimLost)
			assert.ErrorIs(t, store.RescheduleRoleRecordDelete(t.Context(), stale, base.Add(time.Hour), "pds_delete_failed"),
				moderation.ErrRoleRecordDeleteClaimLost)
			assert.Equal(t, before, erasureRoleRows(t, db, "community_role_record_deletes", "true"),
				"a stale claim changes nothing")
			assert.NotEqual(t, "[]", before, "checkpoint: the newer claim's row is still pending")
		})
	}
}

// An erasure that enqueues a record key already pending replaces the pending
// row's CID, and a claim taken before the re-enqueue cannot finish the row.
func TestRoleRecordDeleteRepository_ReenqueueReplacesThePendingCID(t *testing.T) {
	t.Parallel()
	db, store := newRoleDeleteStore(t)
	base := roleDeleteBase()
	userRepo := NewUserRepository(db)
	indexErasedRole := func(cid string) {
		createTestUser(t, db, "reenqueue"+testkit.UniqueID(t)+".test", erasedRoleAccount)
		_, err := db.ExecContext(t.Context(), `INSERT INTO community_roles
			(community_did, subject_did, role, record_key, record_cid, record_created_at)
			VALUES ($1, $2, 'moderator', $3, $4, '2026-11-01T10:00:00Z')`,
			roleDeleteCommunity, erasedRoleAccount, erasedRoleRecordKey, cid)
		require.NoError(t, err)
	}
	_, err := db.ExecContext(t.Context(), `INSERT INTO community_subject_versions (community_did, subject, version)
		VALUES ($1, $1, 7)`, roleDeleteCommunity)
	require.NoError(t, err)

	indexErasedRole("bafyreifirstrolecid")
	require.NoError(t, userRepo.Delete(t.Context(), erasedRoleAccount))
	claimed := claimRoleRecordDeletes(t, store, base, 10)
	require.Len(t, claimed, 1)
	require.Equal(t, "bafyreifirstrolecid", claimed[0].RecordCID, "checkpoint: the first erasure queued its CID")
	stale := claimed[0]

	reinstated, err := userRepo.(users.ErasureMarkerStore).ReinstateAccount(t.Context(), erasedRoleAccount)
	require.NoError(t, err)
	require.True(t, reinstated)
	indexErasedRole("bafyreisecondrolecid")
	require.NoError(t, userRepo.Delete(t.Context(), erasedRoleAccount))

	var cids []string
	rows, err := db.QueryContext(t.Context(), `SELECT record_cid FROM community_role_record_deletes
		WHERE community_did = $1 AND record_key = $2`, roleDeleteCommunity, erasedRoleRecordKey)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var cid string
		require.NoError(t, rows.Scan(&cid))
		cids = append(cids, cid)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"bafyreisecondrolecid"}, cids, "the re-enqueue replaces the pending row's CID")

	assert.ErrorIs(t, store.CompleteRoleRecordDelete(t.Context(), stale), moderation.ErrRoleRecordDeleteClaimLost,
		"a claim taken before the re-enqueue cannot finish the new delete")
	reclaimed := claimRoleRecordDeletes(t, store, base.Add(11*time.Minute), 10)
	require.Len(t, reclaimed, 1, "the re-enqueued delete is still pending")
	assert.Equal(t, "bafyreisecondrolecid", reclaimed[0].RecordCID)
	assert.Equal(t, 0, reclaimed[0].Attempts)
}
