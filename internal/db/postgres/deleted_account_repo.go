package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

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
