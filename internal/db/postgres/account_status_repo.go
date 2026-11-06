package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/accountstatus"
)

// accountStatusLockKeySQL is the transaction-scoped advisory lock serializing
// one DID's confirm-and-apply, namespaced away from other advisory lock users.
const accountStatusLockKeySQL = "hashtext('account-status:' || $1)"

// AccountStatusRepository stores accounts' PDS-confirmed takendown or
// suspended status restrictions.
type AccountStatusRepository struct {
	db *sql.DB
}

var _ accountstatus.Store = (*AccountStatusRepository)(nil)

// NewAccountStatusRepository returns the account status store over db.
func NewAccountStatusRepository(db *sql.DB) *AccountStatusRepository {
	return &AccountStatusRepository{db: db}
}

// Restriction implements accountstatus.Reader.
func (r *AccountStatusRepository) Restriction(ctx context.Context, did string) (accountstatus.Status, bool, error) {
	return readAccountStatus(ctx, r.db, did)
}

type accountStatusQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readAccountStatus(ctx context.Context, querier accountStatusQuerier, did string) (accountstatus.Status, bool, error) {
	var status string
	err := querier.QueryRowContext(ctx,
		`SELECT status FROM account_status_restrictions WHERE did = $1`, did).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the account status restriction: %w", err)
	}
	return accountstatus.Status(status), true, nil
}

// readAccountPresence reports whether did has Coves presence: a users or
// communities row, or an authored post or comment. Each probe is an indexed
// lookup. A users row counts whatever its index_source: a hydrated author row
// is shown in views, so its restriction matters. This is deliberately broader
// than accountActivitySQL, which decides what counts as an account for bans
// and community creation.
func readAccountPresence(ctx context.Context, querier accountStatusQuerier, did string) (bool, error) {
	var present bool
	err := querier.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE did = $1)
			OR EXISTS (SELECT 1 FROM communities WHERE did = $1)
			OR EXISTS (SELECT 1 FROM posts WHERE author_did = $1)
			OR EXISTS (SELECT 1 FROM comments WHERE commenter_did = $1)
	`, did).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("reading the account's Coves presence: %w", err)
	}
	return present, nil
}

func readAccountStatusRecheck(ctx context.Context, querier accountStatusQuerier, did string) (*accountstatus.PendingRecheck, error) {
	var recheck accountstatus.PendingRecheck
	var code string
	err := querier.QueryRowContext(ctx, `
		SELECT attempts, next_attempt_at, last_error_code FROM account_status_rechecks WHERE did = $1
	`, did).Scan(&recheck.Attempts, &recheck.NextAttemptAt, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the account status recheck: %w", err)
	}
	recheck.LastErrorCode = accountstatus.ConfirmationFailure(code)
	return &recheck, nil
}

// Reconcile implements accountstatus.Store. The advisory lock is held from the
// read of the current restriction and recheck rows through the commit of the
// applied change, so two callers for one DID cannot both decide on the same
// stale rows. The state handed to decide also reports whether the DID has
// Coves presence (accountstatus.State.Present), read under the same lock.
//
// decide, and the PDS confirmation inside it, deliberately runs inside this
// transaction while the lock is held, so read-confirm-apply is serialized per
// DID. Each caller holds one connection for that long; the callers are one
// users-lane connector per Jetstream feed, the redrive lane, and the recheck
// worker, each serial, so at most that many connections are held. Each
// confirmation is bounded by repoStatusTimeout (5s); waits for the lock
// itself are not bounded by that timeout.
func (r *AccountStatusRepository) Reconcile(ctx context.Context, did string, decide func(ctx context.Context, current accountstatus.State) (accountstatus.Change, error)) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning the account status transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+accountStatusLockKeySQL+`)`, did); err != nil {
		return fmt.Errorf("locking the account status: %w", err)
	}
	current, found, err := readAccountStatus(ctx, tx, did)
	if err != nil {
		return err
	}
	recheck, err := readAccountStatusRecheck(ctx, tx, did)
	if err != nil {
		return err
	}
	present, err := readAccountPresence(ctx, tx, did)
	if err != nil {
		return err
	}
	change, err := decide(ctx, accountstatus.State{Status: current, Found: found, Present: present, Recheck: recheck})
	if err != nil {
		return err
	}

	switch change.Kind {
	case accountstatus.ChangeNone:
	case accountstatus.ChangeSet:
		if change.Status != accountstatus.Takendown && change.Status != accountstatus.Suspended {
			return fmt.Errorf("setting an account status restriction: unknown status %q", change.Status)
		}
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO account_status_restrictions (did, status) VALUES ($1, $2)
			ON CONFLICT (did) DO UPDATE SET status = EXCLUDED.status, confirmed_at = NOW()
		`, did, string(change.Status)); err != nil {
			return fmt.Errorf("storing the account status restriction: %w", err)
		}
	case accountstatus.ChangeClear:
		if _, err = tx.ExecContext(ctx, `DELETE FROM account_status_restrictions WHERE did = $1`, did); err != nil {
			return fmt.Errorf("clearing the account status restriction: %w", err)
		}
	default:
		return fmt.Errorf("applying an account status change: unknown change kind %d", change.Kind)
	}

	switch change.Recheck.Action {
	case accountstatus.RecheckKeep:
	case accountstatus.RecheckSchedule:
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO account_status_rechecks (did, attempts, next_attempt_at, last_error_code, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (did) DO UPDATE SET attempts = EXCLUDED.attempts,
				next_attempt_at = EXCLUDED.next_attempt_at,
				last_error_code = EXCLUDED.last_error_code,
				updated_at = NOW()
		`, did, change.Recheck.Attempts, change.Recheck.NextAttemptAt, string(change.Recheck.LastErrorCode)); err != nil {
			return fmt.Errorf("scheduling the account status recheck: %w", err)
		}
	case accountstatus.RecheckClear:
		if _, err = tx.ExecContext(ctx, `DELETE FROM account_status_rechecks WHERE did = $1`, did); err != nil {
			return fmt.Errorf("clearing the account status recheck: %w", err)
		}
	default:
		return fmt.Errorf("applying an account status recheck change: unknown action %d", change.Recheck.Action)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing the account status change: %w", err)
	}
	return nil
}

// DueRechecks implements accountstatus.Store. max(1, limit/10) slots (none
// for limit 0) go to the oldest due rows by (next_attempt_at, did) whatever
// their attempts, and are listed first; the rest go to the other due rows by
// (attempts, next_attempt_at, did). The reserved scan has no index on
// next_attempt_at: it reads only due rows of a table that holds just the DIDs
// whose confirmation is failing.
func (r *AccountStatusRepository) DueRechecks(ctx context.Context, now time.Time, limit int) (dids []string, err error) {
	reserved := 0
	if limit > 0 {
		reserved = max(1, limit/10)
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH reserved AS (
			SELECT did, next_attempt_at FROM account_status_rechecks
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, did
			LIMIT $3
		)
		SELECT did FROM (
			SELECT did, 0 AS part, 0 AS attempts, next_attempt_at FROM reserved
			UNION ALL
			(SELECT did, 1, attempts, next_attempt_at FROM account_status_rechecks
			 WHERE next_attempt_at <= $1 AND did NOT IN (SELECT did FROM reserved)
			 ORDER BY attempts, next_attempt_at, did
			 LIMIT $2::int - $3::int)
		) due
		ORDER BY part, attempts, next_attempt_at, did
	`, now, limit, reserved)
	if err != nil {
		return nil, fmt.Errorf("listing due account status rechecks: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("closing the due account status rechecks: %w", closeErr)
		}
	}()
	for rows.Next() {
		var did string
		if err := rows.Scan(&did); err != nil {
			return nil, fmt.Errorf("scanning a due account status recheck: %w", err)
		}
		dids = append(dids, did)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing due account status rechecks: %w", err)
	}
	return dids, nil
}
