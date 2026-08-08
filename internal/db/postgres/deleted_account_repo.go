package postgres

import (
	"context"
	"database/sql"
)

// RED STUB (task 5, cycle 1). Signatures only; the query is GREEN's.

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
func (r *DeletedAccountRepository) IsAccountDeleted(ctx context.Context, did string) (bool, error) {
	return false, nil
}
