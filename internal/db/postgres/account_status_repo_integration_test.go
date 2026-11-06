//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const accountStatusRepoDID = "did:plc:accountstatusrepoaaaaaaa"

// accountStatusRepoNoon is the fixed instant recheck times are written around.
var accountStatusRepoNoon = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// utcState returns state with its recheck time in UTC, so a stored row read
// back in the session time zone compares equal to a literal.
func utcState(state accountstatus.State) accountstatus.State {
	if state.Recheck != nil {
		recheck := *state.Recheck
		recheck.NextAttemptAt = recheck.NextAttemptAt.UTC()
		state.Recheck = &recheck
	}
	return state
}

// requireStoredRestriction compares accountDID's stored restriction with want
// (nil: no row).
func requireStoredRestriction(t *testing.T, store *postgres.AccountStatusRepository, accountDID string, want *accountstatus.Status) {
	t.Helper()
	status, found, err := store.Restriction(t.Context(), accountDID)
	require.NoError(t, err)
	if want == nil {
		assert.False(t, found, "expected no stored status restriction, found %q", status)
		return
	}
	require.True(t, found, "expected stored status restriction %q, found none", *want)
	assert.Equal(t, *want, status)
}

// TestAccountStatusRepository_ReconcileAppliesRecheck pins that Reconcile
// hands decide the stored restriction and recheck row, and applies the
// returned restriction change and recheck change together: a Schedule inserts
// or replaces the row, a Clear deletes it, a Keep leaves it.
func TestAccountStatusRepository_ReconcileAppliesRecheck(t *testing.T) {
	takendown := accountstatus.Takendown
	pending := accountstatus.PendingRecheck{
		Attempts:      1,
		NextAttemptAt: time.Date(2026, 11, 5, 12, 1, 0, 0, time.UTC),
		LastErrorCode: accountstatus.FailurePDSErrorStatus,
	}
	rescheduled := accountstatus.PendingRecheck{
		Attempts:      2,
		NextAttemptAt: time.Date(2026, 11, 5, 12, 3, 0, 0, time.UTC),
		LastErrorCode: accountstatus.FailurePDSUnreachable,
	}
	tests := []struct {
		name            string
		existingStatus  *accountstatus.Status
		existingRecheck *accountstatus.PendingRecheck
		change          accountstatus.Change
		wantSeen        accountstatus.State
		wantStatus      *accountstatus.Status
		wantRecheck     *accountstatus.PendingRecheck
	}{
		{
			name: "schedule with no recheck row inserts it",
			change: accountstatus.Change{Kind: accountstatus.ChangeNone, Recheck: accountstatus.RecheckChange{
				Action: accountstatus.RecheckSchedule, Attempts: 1,
				NextAttemptAt: pending.NextAttemptAt, LastErrorCode: pending.LastErrorCode,
			}},
			wantSeen:    accountstatus.State{},
			wantRecheck: &pending,
		},
		{
			name:            "schedule over a recheck row replaces attempts, next attempt and code",
			existingStatus:  &takendown,
			existingRecheck: &pending,
			change: accountstatus.Change{Kind: accountstatus.ChangeNone, Recheck: accountstatus.RecheckChange{
				Action: accountstatus.RecheckSchedule, Attempts: 2,
				NextAttemptAt: rescheduled.NextAttemptAt, LastErrorCode: rescheduled.LastErrorCode,
			}},
			wantSeen:    accountstatus.State{Status: accountstatus.Takendown, Found: true, Recheck: &pending},
			wantStatus:  &takendown,
			wantRecheck: &rescheduled,
		},
		{
			name:            "clear deletes the recheck row and keeps the restriction",
			existingStatus:  &takendown,
			existingRecheck: &rescheduled,
			change: accountstatus.Change{
				Kind:    accountstatus.ChangeNone,
				Recheck: accountstatus.RecheckChange{Action: accountstatus.RecheckClear},
			},
			wantSeen:   accountstatus.State{Status: accountstatus.Takendown, Found: true, Recheck: &rescheduled},
			wantStatus: &takendown,
		},
		{
			name:            "keep leaves the recheck row",
			existingRecheck: &rescheduled,
			change:          accountstatus.Change{Kind: accountstatus.ChangeNone},
			wantSeen:        accountstatus.State{Recheck: &rescheduled},
			wantRecheck:     &rescheduled,
		},
		{
			name:            "set restriction and clear recheck apply together",
			existingRecheck: &pending,
			change: accountstatus.Change{
				Kind: accountstatus.ChangeSet, Status: accountstatus.Takendown,
				Recheck: accountstatus.RecheckChange{Action: accountstatus.RecheckClear},
			},
			wantSeen:   accountstatus.State{Recheck: &pending},
			wantStatus: &takendown,
		},
		{
			name:           "clear restriction and schedule recheck apply together",
			existingStatus: &takendown,
			change: accountstatus.Change{Kind: accountstatus.ChangeClear, Recheck: accountstatus.RecheckChange{
				Action: accountstatus.RecheckSchedule, Attempts: 1,
				NextAttemptAt: pending.NextAttemptAt, LastErrorCode: pending.LastErrorCode,
			}},
			wantSeen:    accountstatus.State{Status: accountstatus.Takendown, Found: true},
			wantRecheck: &pending,
		},
		{
			name:           "schedule with the repo_not_found code is stored",
			existingStatus: &takendown,
			change: accountstatus.Change{Kind: accountstatus.ChangeNone, Recheck: accountstatus.RecheckChange{
				Action: accountstatus.RecheckSchedule, Attempts: 2,
				NextAttemptAt: rescheduled.NextAttemptAt, LastErrorCode: accountstatus.FailureRepoNotFound,
			}},
			wantSeen:   accountstatus.State{Status: accountstatus.Takendown, Found: true},
			wantStatus: &takendown,
			wantRecheck: &accountstatus.PendingRecheck{
				Attempts: 2, NextAttemptAt: rescheduled.NextAttemptAt, LastErrorCode: accountstatus.FailureRepoNotFound,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			if tt.existingStatus != nil {
				accountstatusfixture.Set(t, db, accountStatusRepoDID, *tt.existingStatus)
			}
			if tt.existingRecheck != nil {
				accountstatusfixture.SetRecheck(t, db, accountStatusRepoDID, *tt.existingRecheck)
			}

			var seen accountstatus.State
			err := store.Reconcile(t.Context(), accountStatusRepoDID,
				func(_ context.Context, current accountstatus.State) (accountstatus.Change, error) {
					seen = current
					return tt.change, nil
				})
			require.NoError(t, err)

			assert.Equal(t, tt.wantSeen, utcState(seen), "the state decide was handed")
			requireStoredRestriction(t, store, accountStatusRepoDID, tt.wantStatus)
			assert.Equal(t, tt.wantRecheck, accountstatusfixture.Recheck(t, db, accountStatusRepoDID),
				"the stored recheck row")
		})
	}
}

// TestAccountStatusRepository_ReconcileRollsBackBothOnRecheckFailure pins
// that the restriction and recheck changes commit together: a recheck
// schedule the table refuses (an unknown failure code) fails Reconcile and
// leaves neither the restriction nor the recheck row changed.
func TestAccountStatusRepository_ReconcileRollsBackBothOnRecheckFailure(t *testing.T) {
	db := testkit.DB(t)
	store := postgres.NewAccountStatusRepository(db)
	existing := accountstatus.PendingRecheck{
		Attempts:      3,
		NextAttemptAt: time.Date(2026, 11, 5, 12, 4, 0, 0, time.UTC),
		LastErrorCode: accountstatus.FailurePDSUnreachable,
	}
	accountstatusfixture.SetRecheck(t, db, accountStatusRepoDID, existing)

	err := store.Reconcile(t.Context(), accountStatusRepoDID,
		func(_ context.Context, _ accountstatus.State) (accountstatus.Change, error) {
			return accountstatus.Change{
				Kind: accountstatus.ChangeSet, Status: accountstatus.Takendown,
				Recheck: accountstatus.RecheckChange{
					Action: accountstatus.RecheckSchedule, Attempts: 4,
					NextAttemptAt: time.Date(2026, 11, 5, 12, 12, 0, 0, time.UTC),
					LastErrorCode: accountstatus.ConfirmationFailure("not_a_failure_code"),
				},
			}, nil
		})
	require.Error(t, err, "a recheck schedule with an unknown failure code must fail Reconcile")

	requireStoredRestriction(t, store, accountStatusRepoDID, nil)
	assert.Equal(t, &existing, accountstatusfixture.Recheck(t, db, accountStatusRepoDID),
		"the recheck row must be unchanged by the failed Reconcile")
}

// TestAccountStatusRepository_DueRechecks pins that DueRechecks lists the
// DIDs whose next attempt is at or before now, oldest next attempt first, at
// most limit, and never a DID with no recheck row.
func TestAccountStatusRepository_DueRechecks(t *testing.T) {
	db := testkit.DB(t)
	store := postgres.NewAccountStatusRepository(db)
	rows := map[string]time.Time{
		"did:plc:recheckdueatnowaaaaaaaa": accountStatusRepoNoon,
		"did:plc:recheckdueoldestaaaaaaa": time.Date(2026, 11, 5, 11, 30, 0, 0, time.UTC),
		"did:plc:recheckduesecondaaaaaaa": time.Date(2026, 11, 5, 11, 59, 59, 0, time.UTC),
		"did:plc:recheckjustlateraaaaaaa": time.Date(2026, 11, 5, 12, 0, 1, 0, time.UTC),
		"did:plc:recheckanhourlateraaaaa": time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC),
	}
	for did, next := range rows {
		accountstatusfixture.SetRecheck(t, db, did, accountstatus.PendingRecheck{
			Attempts: 1, NextAttemptAt: next, LastErrorCode: accountstatus.FailurePDSErrorStatus,
		})
	}
	accountstatusfixture.Set(t, db, "did:plc:restrictedwithnorecheckaa", accountstatus.Takendown)

	tests := []struct {
		name  string
		limit int
		want  []string
	}{
		{
			name:  "all due rows, oldest first, boundary included",
			limit: 10,
			want: []string{
				"did:plc:recheckdueoldestaaaaaaa",
				"did:plc:recheckduesecondaaaaaaa",
				"did:plc:recheckdueatnowaaaaaaaa",
			},
		},
		{
			name:  "limit keeps the oldest",
			limit: 2,
			want: []string{
				"did:plc:recheckdueoldestaaaaaaa",
				"did:plc:recheckduesecondaaaaaaa",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.DueRechecks(t.Context(), accountStatusRepoNoon, tt.limit)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAccountStatusRepository_ReconcileReportsPresence pins that the state
// Reconcile hands decide reports Present when the DID has Coves presence: a
// users row, a communities row, an authored post or an authored comment. Each
// row seeds one source only; a DID with none of them is not present.
func TestAccountStatusRepository_ReconcileReportsPresence(t *testing.T) {
	tests := []struct {
		name        string
		seed        func(t *testing.T, db *sql.DB, did string)
		wantPresent bool
	}{
		{name: "users row", seed: seedActivityUser, wantPresent: true},
		{name: "communities row", seed: insertActivityCommunityRow, wantPresent: true},
		{name: "authored post with no users row", seed: seedActivityPost, wantPresent: true},
		{name: "authored comment with no users row", seed: seedActivityComment, wantPresent: true},
		{name: "no presence", seed: func(*testing.T, *sql.DB, string) {}, wantPresent: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			tt.seed(t, db, accountStatusRepoDID)

			var seen accountstatus.State
			err := store.Reconcile(t.Context(), accountStatusRepoDID,
				func(_ context.Context, current accountstatus.State) (accountstatus.Change, error) {
					seen = current
					return accountstatus.Change{Kind: accountstatus.ChangeNone}, nil
				})
			require.NoError(t, err)

			assert.Equal(t, tt.wantPresent, seen.Present, "the Present flag decide was handed")
		})
	}
}

// TestAccountStatusRepository_DueRechecksFewestAttemptsFirst pins that
// DueRechecks orders the unreserved slots by attempts before next attempt
// time, so a long-failing DID cannot starve newer ones: with limit 2, the one
// reserved slot takes the nine-attempt row due longest ago, and the other slot
// goes to the one-attempt row due a minute ago ahead of the eight-attempt row.
func TestAccountStatusRepository_DueRechecksFewestAttemptsFirst(t *testing.T) {
	db := testkit.DB(t)
	store := postgres.NewAccountStatusRepository(db)
	rows := []struct {
		did      string
		attempts int
		next     time.Time
	}{
		{did: "did:plc:recheckninetriesaaaaaaa", attempts: 9, next: accountStatusRepoNoon.Add(-2 * time.Hour)},
		{did: "did:plc:recheckeighttriesaaaaaa", attempts: 8, next: accountStatusRepoNoon.Add(-time.Hour)},
		{did: "did:plc:recheckonetryaaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-time.Minute)},
	}
	for _, row := range rows {
		accountstatusfixture.SetRecheck(t, db, row.did, accountstatus.PendingRecheck{
			Attempts: row.attempts, NextAttemptAt: row.next, LastErrorCode: accountstatus.FailurePDSUnreachable,
		})
	}

	got, err := store.DueRechecks(t.Context(), accountStatusRepoNoon, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"did:plc:recheckninetriesaaaaaaa", "did:plc:recheckonetryaaaaaaaaaa"}, got)
}

// TestAccountStatusRepository_DueRechecksReservesOldestSlots pins that
// DueRechecks reserves max(1, limit/10) slots of each batch (none for limit 0)
// for the oldest due rows by next attempt time, whatever their attempts, so a
// sustained inflow of fresh rows cannot starve a long-failing DID forever, even
// at a limit under 10. The remaining slots go to the other due rows fewest
// attempts first, the reserved rows come first, and no DID is listed twice.
func TestAccountStatusRepository_DueRechecksReservesOldestSlots(t *testing.T) {
	type recheckRow struct {
		did      string
		attempts int
		next     time.Time
	}
	tests := []struct {
		name  string
		rows  []recheckRow
		limit int
		want  []string
	}{
		{
			name: "limit 10 reserves one slot for the oldest due row",
			rows: []recheckRow{
				{did: "did:plc:recheckninetriesoldestaa", attempts: 9, next: accountStatusRepoNoon.Add(-3 * time.Hour)},
				{did: "did:plc:recheckeighttriesolderaa", attempts: 8, next: accountStatusRepoNoon.Add(-2 * time.Hour)},
				{did: "did:plc:recheckfresh01aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-11 * time.Minute)},
				{did: "did:plc:recheckfresh02aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-10 * time.Minute)},
				{did: "did:plc:recheckfresh03aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-9 * time.Minute)},
				{did: "did:plc:recheckfresh04aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-8 * time.Minute)},
				{did: "did:plc:recheckfresh05aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-7 * time.Minute)},
				{did: "did:plc:recheckfresh06aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-6 * time.Minute)},
				{did: "did:plc:recheckfresh07aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-5 * time.Minute)},
				{did: "did:plc:recheckfresh08aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-4 * time.Minute)},
				{did: "did:plc:recheckfresh09aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-3 * time.Minute)},
				{did: "did:plc:recheckfresh10aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-2 * time.Minute)},
				{did: "did:plc:recheckfresh11aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-time.Minute)},
			},
			limit: 10,
			want: []string{
				"did:plc:recheckninetriesoldestaa",
				"did:plc:recheckfresh01aaaaaaaaaa",
				"did:plc:recheckfresh02aaaaaaaaaa",
				"did:plc:recheckfresh03aaaaaaaaaa",
				"did:plc:recheckfresh04aaaaaaaaaa",
				"did:plc:recheckfresh05aaaaaaaaaa",
				"did:plc:recheckfresh06aaaaaaaaaa",
				"did:plc:recheckfresh07aaaaaaaaaa",
				"did:plc:recheckfresh08aaaaaaaaaa",
				"did:plc:recheckfresh09aaaaaaaaaa",
			},
		},
		{
			name: "limit 5 still reserves one slot for the oldest due row",
			rows: []recheckRow{
				{did: "did:plc:recheckninetriesoldestaa", attempts: 9, next: accountStatusRepoNoon.Add(-3 * time.Hour)},
				{did: "did:plc:recheckfresh01aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-5 * time.Minute)},
				{did: "did:plc:recheckfresh02aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-4 * time.Minute)},
				{did: "did:plc:recheckfresh03aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-3 * time.Minute)},
				{did: "did:plc:recheckfresh04aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-2 * time.Minute)},
				{did: "did:plc:recheckfresh05aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-time.Minute)},
			},
			limit: 5,
			want: []string{
				"did:plc:recheckninetriesoldestaa",
				"did:plc:recheckfresh01aaaaaaaaaa",
				"did:plc:recheckfresh02aaaaaaaaaa",
				"did:plc:recheckfresh03aaaaaaaaaa",
				"did:plc:recheckfresh04aaaaaaaaaa",
			},
		},
		{
			name: "limit 0 lists nothing",
			rows: []recheckRow{
				{did: "did:plc:recheckninetriesoldestaa", attempts: 9, next: accountStatusRepoNoon.Add(-3 * time.Hour)},
				{did: "did:plc:recheckfresh01aaaaaaaaaa", attempts: 1, next: accountStatusRepoNoon.Add(-time.Minute)},
			},
			limit: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testkit.DB(t)
			store := postgres.NewAccountStatusRepository(db)
			for _, row := range tt.rows {
				accountstatusfixture.SetRecheck(t, db, row.did, accountstatus.PendingRecheck{
					Attempts: row.attempts, NextAttemptAt: row.next, LastErrorCode: accountstatus.FailurePDSUnreachable,
				})
			}

			got, err := store.DueRechecks(t.Context(), accountStatusRepoNoon, tt.limit)
			require.NoError(t, err)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAccountStatusRechecksIndexedByAttemptsThenNextAttempt pins that
// account_status_rechecks has an index keyed on (attempts, next_attempt_at),
// in that order, serving DueRechecks' ordering.
func TestAccountStatusRechecksIndexedByAttemptsThenNextAttempt(t *testing.T) {
	db := testkit.DB(t)
	rows, err := db.QueryContext(t.Context(), `
		SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM pg_index i
		CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = 'account_status_rechecks'::regclass AND k.ord <= i.indnkeyatts
		GROUP BY i.indexrelid
	`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var indexKeys []string
	for rows.Next() {
		var keys string
		require.NoError(t, rows.Scan(&keys))
		indexKeys = append(indexKeys, keys)
	}
	require.NoError(t, rows.Err())

	assert.Contains(t, indexKeys, "attempts,next_attempt_at",
		"account_status_rechecks index key columns: %v", indexKeys)
}
