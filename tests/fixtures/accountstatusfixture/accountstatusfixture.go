//go:build integration

// Package accountstatusfixture writes and removes an account's stored
// PDS-confirmed status restriction (account_status_restrictions) directly, the
// state the users consumer reaches after a confirmed #account takedown or
// suspension, for read-path tests that need the restriction without driving
// the consumer. It also writes and reads an account's pending confirmation
// recheck (account_status_rechecks).
package accountstatusfixture

import (
	"database/sql"
	"errors"
	"testing"

	"Coves/internal/core/accountstatus"

	"github.com/stretchr/testify/require"
)

// Set stores status as accountDID's restriction, replacing any earlier one.
func Set(t *testing.T, db *sql.DB, accountDID string, status accountstatus.Status) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO account_status_restrictions (did, status) VALUES ($1, $2)
		ON CONFLICT (did) DO UPDATE SET status = EXCLUDED.status, confirmed_at = NOW()
	`, accountDID, string(status))
	require.NoError(t, err)
}

// Clear removes accountDID's stored restriction, which must exist.
func Clear(t *testing.T, db *sql.DB, accountDID string) {
	t.Helper()
	result, err := db.ExecContext(t.Context(), `DELETE FROM account_status_restrictions WHERE did = $1`, accountDID)
	require.NoError(t, err)
	deleted, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted, "exactly one stored status restriction of the account is cleared")
}

// SetRecheck stores recheck as accountDID's pending confirmation recheck,
// replacing any earlier one.
func SetRecheck(t *testing.T, db *sql.DB, accountDID string, recheck accountstatus.PendingRecheck) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO account_status_rechecks (did, attempts, next_attempt_at, last_error_code)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (did) DO UPDATE SET attempts = EXCLUDED.attempts,
			next_attempt_at = EXCLUDED.next_attempt_at, last_error_code = EXCLUDED.last_error_code,
			updated_at = NOW()
	`, accountDID, recheck.Attempts, recheck.NextAttemptAt, string(recheck.LastErrorCode))
	require.NoError(t, err)
}

// Recheck reads accountDID's pending confirmation recheck, nil when there is
// none. NextAttemptAt is returned in UTC.
func Recheck(t *testing.T, db *sql.DB, accountDID string) *accountstatus.PendingRecheck {
	t.Helper()
	var recheck accountstatus.PendingRecheck
	var code string
	err := db.QueryRowContext(t.Context(), `
		SELECT attempts, next_attempt_at, last_error_code FROM account_status_rechecks WHERE did = $1
	`, accountDID).Scan(&recheck.Attempts, &recheck.NextAttemptAt, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	require.NoError(t, err)
	recheck.NextAttemptAt = recheck.NextAttemptAt.UTC()
	recheck.LastErrorCode = accountstatus.ConfirmationFailure(code)
	return &recheck
}
