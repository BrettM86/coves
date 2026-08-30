package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/core/users"
)

// The marker store is optional on UserRepository and reached by type assertion,
// so nothing but this line makes the compiler check that the one implementation
// that can answer still does. Without it, a signature drifting out of step with
// the interface would surface as a service failing closed at runtime.
var _ users.ErasureMarkerStore = (*postgresUserRepo)(nil)

// DeletedAccountRepository reads the migration-036 erasure markers.
//
// It satisfies jetstream.DeletedAccountLookup structurally rather than by
// importing it: the interface is declared where it is CONSUMED (the ingestion
// consumer), which is what keeps the storage layer from depending on the
// firehose layer for a single method.
type DeletedAccountRepository struct {
	db *sql.DB
}

// NewDeletedAccountRepository wires the lookup over the AppView database.
func NewDeletedAccountRepository(db *sql.DB) *DeletedAccountRepository {
	return &DeletedAccountRepository{db: db}
}

// IsAccountDeleted reports whether this DID names an account the AppView was
// asked to erase.
//
// A query failure must come back as an error, never as false. Under
// author-owned posts an unknown author indexes normally (§5.3), so a false here
// is indistinguishable from a healthy answer — a database blip would silently
// re-index the content a deletion erased, which is the exact outcome the marker
// table exists to prevent.
func (r *postgresUserRepo) IsAccountDeleted(ctx context.Context, did string) (bool, error) {
	return accountIsErased(ctx, r.db, did)
}

// ReinstateAccount removes the erasure marker for a DID, and it is the marker's
// only exit. See users.ErasureMarkerStore for why the exit is a named method
// rather than a side effect of the users insert, and why absence is success.
//
// The bool reports whether a marker was actually there, which is how the caller
// tells an account returning from erasure — rare, and the AppView reversing a
// deletion it promised to keep — from the ordinary login that removes nothing.
func (r *postgresUserRepo) ReinstateAccount(ctx context.Context, did string) (bool, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM deleted_accounts WHERE did = $1`, did)
	if err != nil {
		return false, fmt.Errorf("removing the erasure marker for %s: %w", did, err)
	}

	// Reported rather than assumed: a driver that cannot count rows must not
	// have this claim an account came back from erasure, or the audit log says
	// a deletion was reversed when nothing happened.
	cleared, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting the erasure markers removed for %s: %w", did, err)
	}
	return cleared > 0, nil
}

// IsAccountDeleted implements the same lookup for the standalone repository.
func (r *DeletedAccountRepository) IsAccountDeleted(ctx context.Context, did string) (bool, error) {
	return accountIsErased(ctx, r.db, did)
}

// accountIsErased is the single statement behind both lookups above.
//
// It is one function because the two callers are the two halves of the same
// guard — the ingestion consumer refusing an erased author's events, and the
// user service refusing to re-index them — and a second spelling would be a
// second chance for one of them to drift into failing open.
func accountIsErased(ctx context.Context, db *sql.DB, did string) (bool, error) {
	var deleted bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM deleted_accounts WHERE did = $1)`, did,
	).Scan(&deleted); err != nil {
		return false, fmt.Errorf("checking whether %s was erased: %w", did, err)
	}
	return deleted, nil
}
