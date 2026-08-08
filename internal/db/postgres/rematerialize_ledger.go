package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/core/posts"
)

// PostgreSQL storage for the re-materialization ledger (migration 037): one row
// per legacy social.coves.community.post record, tracking its progress through
// the cutover state machine (docs/PRD_AUTHOR_OWNED_POSTS.md §11).
//
// Every transition is a single parameterized statement. The state-advancing
// updates are GUARDED on the state they move FROM, and a guard that matches no
// row is an error rather than a silent no-op: the tool advances the machine in
// strict order, so a transition finding the row in an unexpected state means the
// ledger and the tool have diverged, and that is a fault to surface, not to
// swallow into a false success.

// rematerializeLedger is the migration-037-backed posts.RematerializeLedger.
type rematerializeLedger struct {
	db *sql.DB
}

// NewRematerializeLedger returns the Postgres re-materialization ledger.
func NewRematerializeLedger(db *sql.DB) posts.RematerializeLedger {
	return &rematerializeLedger{db: db}
}

// Discover upserts the row for oldURI in state discovered, idempotently: a
// re-run finds the existing row (whatever state it stands in) rather than
// resetting it, then reads it back so the caller resumes from where it stopped.
func (l *rematerializeLedger) Discover(ctx context.Context, oldURI, authorDID string) (posts.RematerializeLedgerRow, error) {
	// ON CONFLICT DO NOTHING keeps a resumed row untouched; a plain INSERT would
	// reset an in-flight row back to discovered and re-do the whole migration.
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO post_rematerialization_ledger (old_uri, state, author_did, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (old_uri) DO NOTHING
	`, oldURI, string(posts.RematerializeDiscovered), nullString(authorDID))
	if err != nil {
		return posts.RematerializeLedgerRow{}, fmt.Errorf("discovering %s: %w", oldURI, err)
	}

	row, found, err := l.Get(ctx, oldURI)
	if err != nil {
		return posts.RematerializeLedgerRow{}, err
	}
	if !found {
		return posts.RematerializeLedgerRow{}, fmt.Errorf("discovering %s: the row vanished between upsert and read", oldURI)
	}
	return row, nil
}

// Get reads one row. found is false when the URI has never been discovered.
func (l *rematerializeLedger) Get(ctx context.Context, oldURI string) (posts.RematerializeLedgerRow, bool, error) {
	var (
		row       posts.RematerializeLedgerRow
		state     string
		authorDID sql.NullString
		newURI    sql.NullString
		newCID    sql.NullString
		newRkey   sql.NullString
		reason    sql.NullString
	)
	err := l.db.QueryRowContext(ctx, `
		SELECT old_uri, state, author_did, new_uri, new_cid, new_rkey, reason, created_at, updated_at
		FROM post_rematerialization_ledger
		WHERE old_uri = $1
	`, oldURI).Scan(&row.OldURI, &state, &authorDID, &newURI, &newCID, &newRkey, &reason, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return posts.RematerializeLedgerRow{}, false, nil
	}
	if err != nil {
		return posts.RematerializeLedgerRow{}, false, fmt.Errorf("reading ledger row %s: %w", oldURI, err)
	}

	row.State = posts.RematerializeState(state)
	row.AuthorDID = authorDID.String
	row.NewURI = newURI.String
	row.NewCID = newCID.String
	row.NewRkey = newRkey.String
	row.Reason = reason.String
	return row, true, nil
}

// ListResumable returns every row still in a non-terminal state — the ledger-
// driven resume set (whole-branch review, P7). A migrated row whose delete
// succeeded but whose MarkDone crashed is GONE from the community repo, so only
// this query — never the source's listRecords — can rediscover it.
func (l *rematerializeLedger) ListResumable(ctx context.Context) ([]posts.RematerializeLedgerRow, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT old_uri, state, author_did, new_uri, new_cid, new_rkey, reason, created_at, updated_at
		FROM post_rematerialization_ledger
		WHERE state NOT IN ('done', 'fallback_left_legacy', 'fallback_no_creds')
		ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("listing resumable ledger rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []posts.RematerializeLedgerRow
	for rows.Next() {
		var (
			row       posts.RematerializeLedgerRow
			state     string
			authorDID sql.NullString
			newURI    sql.NullString
			newCID    sql.NullString
			newRkey   sql.NullString
			reason    sql.NullString
		)
		if err := rows.Scan(&row.OldURI, &state, &authorDID, &newURI, &newCID, &newRkey, &reason, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning a resumable ledger row: %w", err)
		}
		row.State = posts.RematerializeState(state)
		row.AuthorDID = authorDID.String
		row.NewURI = newURI.String
		row.NewCID = newCID.String
		row.NewRkey = newRkey.String
		row.Reason = reason.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecordPostV2Written moves discovered → postv2_written and records the postv2
// coordinates the resume path reads back.
func (l *rematerializeLedger) RecordPostV2Written(ctx context.Context, oldURI, newURI, newCID, newRkey string) error {
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, new_uri = $3, new_cid = $4, new_rkey = $5, updated_at = NOW()
		WHERE old_uri = $1 AND state = $6
	`, "postv2_written", oldURI,
		string(posts.RematerializePostV2Written), newURI, newCID, newRkey, string(posts.RematerializeDiscovered))
}

// MarkVerified moves postv2_written → verified.
func (l *rematerializeLedger) MarkVerified(ctx context.Context, oldURI string) error {
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, updated_at = NOW()
		WHERE old_uri = $1 AND state = $3
	`, "verified", oldURI,
		string(posts.RematerializeVerified), string(posts.RematerializePostV2Written))
}

// MarkMigrated moves verified → migrated — the checkpoint before delete.
func (l *rematerializeLedger) MarkMigrated(ctx context.Context, oldURI string) error {
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, updated_at = NOW()
		WHERE old_uri = $1 AND state = $3
	`, "migrated", oldURI,
		string(posts.RematerializeMigrated), string(posts.RematerializeVerified))
}

// MarkDone moves migrated → done, after the old record is deleted.
func (l *rematerializeLedger) MarkDone(ctx context.Context, oldURI string) error {
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, updated_at = NOW()
		WHERE old_uri = $1 AND state = $3
	`, "done", oldURI,
		string(posts.RematerializeDone), string(posts.RematerializeMigrated))
}

// MarkFallback moves a discovered row to a terminal fallback state with a reason.
// The from-state guard is intentionally broad — a fallback is only ever reached
// from discovered in cycle 1 — but the reason is always recorded for the census.
func (l *rematerializeLedger) MarkFallback(ctx context.Context, oldURI string, state posts.RematerializeState, reason string) error {
	if !posts.IsFallback(state) {
		return fmt.Errorf("marking %s as fallback: %q is not a fallback state", oldURI, state)
	}
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, reason = $3, updated_at = NOW()
		WHERE old_uri = $1 AND state = $4
	`, string(state), oldURI,
		string(state), reason, string(posts.RematerializeDiscovered))
}

// CountByState is the census: how many rows sit in each state, so the run can
// refuse "complete" while any fallback survives.
func (l *rematerializeLedger) CountByState(ctx context.Context) (map[posts.RematerializeState]int, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT state, COUNT(*) FROM post_rematerialization_ledger GROUP BY state
	`)
	if err != nil {
		return nil, fmt.Errorf("counting ledger rows by state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[posts.RematerializeState]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("scanning census row: %w", err)
		}
		counts[posts.RematerializeState(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating census rows: %w", err)
	}
	return counts, nil
}

// guardedTransition runs a from-state-guarded UPDATE and treats a no-op as the
// error it is: the tool only ever fires a transition when the row stands in the
// expected state, so matching no row means the ledger and the tool diverged.
func (l *rematerializeLedger) guardedTransition(ctx context.Context, query, toState, oldURI string, args ...any) error {
	full := append([]any{oldURI}, args...)
	res, err := l.db.ExecContext(ctx, query, full...)
	if err != nil {
		return fmt.Errorf("transitioning %s to %s: %w", oldURI, toState, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transitioning %s to %s: reading rows affected: %w", oldURI, toState, err)
	}
	if affected == 0 {
		return fmt.Errorf("transitioning %s to %s: no row in the expected prior state (the ledger and the tool have diverged)", oldURI, toState)
	}
	return nil
}
