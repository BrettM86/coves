package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// This file implements rev-gating: the ordering guard that makes it safe to
// consume multiple Jetstream feeds carrying the same repos (see migration
// 033_create_jetstream_record_revs.sql for the full rationale).
//
// Every commit event carries rev, the repo's monotonic TID (lexicographically
// ordered string). The last APPLIED rev per record URI is stored in
// jetstream_record_revs; an incoming create/update/delete applies only when
// its rev is strictly greater. Equal rev = the same event replayed (no-op,
// subsumes duplicate handling); smaller rev = a stale cross-feed copy
// (skipped). The gate row survives hard deletes, acting as a tombstone that
// rejects the stale create that would otherwise resurrect a deleted record.
//
// Two integration patterns, chosen per consumer:
//
//  1. Transactional (posts, comments, votes — the count-mutating consumers):
//     tryAdvanceRecordRev runs as the FIRST statement of the consumer's
//     existing transaction. The conditional upsert takes a row lock, so
//     concurrent handlers of the same record serialize, and a rollback
//     reverts the gate together with the writes. Fully atomic.
//
//  2. Transactional claim held across apply (users' blocks, communities,
//     aggregators — the repo-method consumers, via applyGated): the gate row
//     is claimed as the FIRST statement of a dedicated transaction on the
//     gate's own DB handle, apply() runs while that claim (a row lock on the
//     record's gate row) is held, and the transaction commits only after
//     apply succeeds. Same-URI handlers on other feeds serialize on the gate
//     row lock for the full duration of apply, so there is no check→write
//     window for a concurrent feed to interleave in; an apply failure rolls
//     the claim back un-advanced so retries/redrives replay the event.
//     Exception: the user consumer's PROFILE path keeps the older
//     check→write→advance protocol (RevGate.IsStale before the write,
//     RevGate.Advance after) because its write is an idempotent last-write-
//     wins profile update where a brief unguarded window is acceptable.
//
// Events with an empty rev bypass the gate, preserving previous behavior.
// In practice only synthetic test events are rev-less: real Jetstream frames
// always carry rev, and dead letters store the raw frame, so redriven events
// keep theirs.

// revGateQuerier is the subset of *sql.DB / *sql.Tx the gate needs, so the
// same statements run standalone or inside a consumer's transaction.
type revGateQuerier interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// tryAdvanceRecordRev atomically claims rev for the record URI. It returns
// true when the event wins (no gate row yet, or a strictly greater rev) and
// the gate row was written; false when the stored rev is greater or equal —
// a duplicate replay or a stale cross-feed copy the caller must skip.
// An empty rev bypasses the gate (returns true without writing).
func tryAdvanceRecordRev(ctx context.Context, q revGateQuerier, uri, rev string) (bool, error) {
	if rev == "" {
		return true, nil
	}
	result, err := q.ExecContext(ctx, `
		INSERT INTO jetstream_record_revs (record_uri, rev)
		VALUES ($1, $2)
		ON CONFLICT (record_uri) DO UPDATE
		SET rev = EXCLUDED.rev, updated_at = NOW()
		WHERE jetstream_record_revs.rev < EXCLUDED.rev
	`, uri, rev)
	if err != nil {
		return false, fmt.Errorf("failed to advance record rev for %s: %w", uri, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check record rev advance for %s: %w", uri, err)
	}
	return rows > 0, nil
}

// recordRevIsStale reports whether the stored rev for the record URI is
// greater than or equal to the incoming one (i.e. the event must be skipped).
// No gate row, or an empty incoming rev, means not stale.
func recordRevIsStale(ctx context.Context, q revGateQuerier, uri, rev string) (bool, error) {
	if rev == "" {
		return false, nil
	}
	var stale bool
	err := q.QueryRowContext(ctx,
		`SELECT rev >= $2 FROM jetstream_record_revs WHERE record_uri = $1`, uri, rev,
	).Scan(&stale)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check record rev for %s: %w", uri, err)
	}
	return stale, nil
}

// logSkippedStaleRev is the single, grep-able log line for gate skips.
// Rejected stale events are the system WORKING (e.g. the bsky feed's delayed
// copies of self-feed events); this line makes that observable and
// distinguishable from real trouble.
func logSkippedStaleRev(consumer, operation, uri, rev string) {
	log.Printf("rev-gate: %s skipped stale %s for %s (incoming rev %q <= stored)", consumer, operation, uri, rev)
}

// commitRecordURI builds the AT-URI of the record a commit event addresses.
func commitRecordURI(did string, commit *CommitEvent) string {
	return fmt.Sprintf("at://%s/%s/%s", did, commit.Collection, commit.RKey)
}

// RevGate carries the gate's own DB handle for the consumers that write
// through repository methods and therefore cannot join their writes into one
// transaction with the gate. applyGated uses it to open the claim transaction
// held across apply; IsStale/Advance expose the non-transactional
// check→write→advance flavor still used by the user consumer's profile path.
// A nil *RevGate disables gating entirely (tests, deployments consuming a
// single feed).
type RevGate struct {
	db *sql.DB
}

// NewRevGate creates a rev gate backed by the AppView database.
func NewRevGate(db *sql.DB) *RevGate {
	return &RevGate{db: db}
}

// IsStale reports whether the event's rev is superseded by the stored rev
// for the record URI. Nil-safe: a nil gate never reports stale.
func (g *RevGate) IsStale(ctx context.Context, uri, rev string) (bool, error) {
	if g == nil {
		return false, nil
	}
	return recordRevIsStale(ctx, g.db, uri, rev)
}

// Advance records rev as the last applied rev for the record URI, keeping
// whichever is greater. Called AFTER the consumer's idempotent write
// succeeds, so a failure in between replays the event instead of losing it.
// Nil-safe: a nil gate is a no-op.
func (g *RevGate) Advance(ctx context.Context, uri, rev string) error {
	if g == nil {
		return nil
	}
	_, err := tryAdvanceRecordRev(ctx, g.db, uri, rev)
	return err
}

// applyGated runs apply under a TRANSACTIONAL rev-gate claim for a commit
// event. The gate row is claimed (tryAdvanceRecordRev) as the first statement
// of a transaction on the gate's own DB handle, apply() runs while that claim
// is held, and the transaction commits only after apply succeeds. The claim's
// row lock makes two feeds' handlers for the SAME record URI serialize for
// the full duration of apply — the loser blocks on the claim, then observes
// the winner's rev and skips — so there is no check→write window for a stale
// cross-feed copy to sneak through, no matter how long apply takes.
//
// Deadlock note: apply's writes go through repository methods on their own
// connections, which is deliberate and safe — the gate transaction touches
// ONLY jetstream_record_revs, a table no repository write path ever touches,
// so the gate row lock acts as a pure per-record mutex around apply.
//
// An apply error (or panic — the deferred rollback covers both) releases the
// claim WITHOUT advancing, so the connector's retry/redrive replays the event
// instead of losing it behind its own gate entry. Events with an empty rev
// (synthetic test events, legacy dead letters) bypass the gate and run apply
// directly, as does a nil gate.
func applyGated(ctx context.Context, gate *RevGate, consumer, did string, commit *CommitEvent, apply func() error) error {
	if gate == nil || commit.Rev == "" {
		return apply()
	}
	uri := commitRecordURI(did, commit)
	tx, err := gate.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin rev-gate transaction for %s: %w", uri, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback rev-gate transaction for %s: %v", uri, rollbackErr)
		}
	}()

	won, err := tryAdvanceRecordRev(ctx, tx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(consumer, commit.Operation, uri, commit.Rev)
		return nil
	}
	if err := apply(); err != nil {
		return err // deferred rollback releases the claim un-advanced
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit rev-gate transaction for %s: %w", uri, err)
	}
	return nil
}
