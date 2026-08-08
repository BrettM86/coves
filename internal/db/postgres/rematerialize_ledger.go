package postgres

import (
	"context"
	"database/sql"
	"errors"

	"Coves/internal/core/posts"
)

// PostgreSQL storage for the re-materialization ledger (migration 037): one row
// per legacy social.coves.community.post record, tracking its progress through
// the cutover state machine (docs/PRD_AUTHOR_OWNED_POSTS.md §11).
//
// # THIS FILE IS A RED STUB
//
// It exists so the state-machine tests compile and so cmd/rematerialize-posts
// has a concrete ledger to wire. Every method returns the not-implemented
// sentinel; GREEN implements the single-statement upserts and the census query
// against the migration-037 table.

// rematerializeLedger is the migration-037-backed posts.RematerializeLedger.
type rematerializeLedger struct {
	db *sql.DB
}

// NewRematerializeLedger returns the Postgres re-materialization ledger.
func NewRematerializeLedger(db *sql.DB) posts.RematerializeLedger {
	return &rematerializeLedger{db: db}
}

var errRematerializeLedgerNotImplemented = errors.New("rematerialize ledger: not implemented (RED stub)")

func (l *rematerializeLedger) Discover(ctx context.Context, oldURI, authorDID string) (posts.RematerializeLedgerRow, error) {
	return posts.RematerializeLedgerRow{}, errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) Get(ctx context.Context, oldURI string) (posts.RematerializeLedgerRow, bool, error) {
	return posts.RematerializeLedgerRow{}, false, errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) RecordPostV2Written(ctx context.Context, oldURI, newURI, newCID, newRkey string) error {
	return errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) MarkVerified(ctx context.Context, oldURI string) error {
	return errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) MarkMigrated(ctx context.Context, oldURI string) error {
	return errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) MarkDone(ctx context.Context, oldURI string) error {
	return errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) MarkFallback(ctx context.Context, oldURI string, state posts.RematerializeState, reason string) error {
	return errRematerializeLedgerNotImplemented
}

func (l *rematerializeLedger) CountByState(ctx context.Context) (map[posts.RematerializeState]int, error) {
	return nil, errRematerializeLedgerNotImplemented
}
