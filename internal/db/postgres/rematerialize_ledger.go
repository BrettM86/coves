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
//
// THE COMMUNITY SCOPE IS PART OF THE SCHEMA, not a filter callers remember to
// apply. `ListResumable`, `CountByState` and `ReopenFallback` all take it
// explicitly, because the tool's staged rollout mode is only meaningful if the
// destructive half of the run cannot reach outside it — an unscoped resume for
// community A will happily drive, and delete, community B's rows.

// rematerializeLedger is the migration-037-backed posts.RematerializeLedger.
type rematerializeLedger struct {
	db *sql.DB
}

// NewRematerializeLedger returns the Postgres re-materialization ledger.
func NewRematerializeLedger(db *sql.DB) posts.RematerializeLedger {
	return &rematerializeLedger{db: db}
}

// ledgerColumns is the one SELECT list every read uses, so a column added to the
// row struct cannot be scanned by one query and forgotten by another.
const ledgerColumns = `old_uri, state, author_did, community_did, source_cid, new_uri, new_cid, new_rkey, reason, created_at, updated_at`

// Discover upserts the row for oldURI in state discovered, idempotently: a
// re-run finds the existing row (whatever state it stands in) rather than
// resetting it, then reads it back so the caller resumes from where it stopped.
func (l *rematerializeLedger) Discover(ctx context.Context, oldURI, communityDID, authorDID string) (posts.RematerializeLedgerRow, error) {
	// ON CONFLICT DO NOTHING keeps a resumed row untouched; a plain INSERT would
	// reset an in-flight row back to discovered and re-do the whole migration.
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO post_rematerialization_ledger (old_uri, state, author_did, community_did, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (old_uri) DO NOTHING
	`, oldURI, string(posts.RematerializeDiscovered), nullString(authorDID), nullString(communityDID))
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
	row, err := scanLedgerRow(l.db.QueryRowContext(ctx, `
		SELECT `+ledgerColumns+`
		FROM post_rematerialization_ledger
		WHERE old_uri = $1
	`, oldURI))
	if err == sql.ErrNoRows {
		return posts.RematerializeLedgerRow{}, false, nil
	}
	if err != nil {
		return posts.RematerializeLedgerRow{}, false, fmt.Errorf("reading ledger row %s: %w", oldURI, err)
	}
	return row, true, nil
}

// ListResumable returns every row still in a non-terminal state — the ledger-
// driven resume set (whole-branch review, P7) — restricted to communityDID when
// it is non-empty.
//
// A migrated row whose delete succeeded but whose MarkDone crashed is GONE from
// the community repo, so only this query — never the source's listRecords — can
// rediscover it. And a staged run must not rediscover ANOTHER community's row,
// because the very next thing the tool does with one is delete its record.
func (l *rematerializeLedger) ListResumable(ctx context.Context, communityDID string) ([]posts.RematerializeLedgerRow, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT `+ledgerColumns+`
		FROM post_rematerialization_ledger
		WHERE state NOT IN ('done', 'fallback_left_legacy')
		  AND ($1 = '' OR community_did = $1)
		ORDER BY created_at
	`, communityDID)
	if err != nil {
		return nil, fmt.Errorf("listing resumable ledger rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []posts.RematerializeLedgerRow
	for rows.Next() {
		row, err := scanLedgerRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning a resumable ledger row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecordPostV2Written moves discovered → postv2_written and records both the
// postv2 coordinates the resume path reads back and the SOURCE CID the postv2
// was built from — the value every later pre-delete check is made against.
func (l *rematerializeLedger) RecordPostV2Written(ctx context.Context, oldURI, sourceCID, newURI, newCID, newRkey string) error {
	if sourceCID == "" {
		return fmt.Errorf("recording the postv2 of %s: no source CID was supplied, so no later delete could be guarded against a concurrent edit", oldURI)
	}
	return l.guardedTransition(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $2, source_cid = $3, new_uri = $4, new_cid = $5, new_rkey = $6, updated_at = NOW()
		WHERE old_uri = $1 AND state = $7
	`, "postv2_written", oldURI,
		string(posts.RematerializePostV2Written), sourceCID, newURI, newCID, newRkey, string(posts.RematerializeDiscovered))
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

// MarkFallback moves a discovered row to a terminal fallback state with a
// reason. The from-state guard is discovered ONLY: a row that has already had a
// postv2 written for it is past the point where "leave it as legacy" is a
// coherent verdict, and re-marking a row that is already a fallback would
// overwrite the reason the operator is about to read.
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

// ReopenFallback moves fallback rows back to discovered so a later run can retry
// them, restricted to communityDID when it is non-empty.
//
// THIS IS THE ONLY WAY BACK OUT OF A FALLBACK, and it exists because without it
// there is none. A missing author grant sentences a row terminally; the operator
// re-authorizes the author and every subsequent run is a permanent no-op over
// those posts, with the only remedy an UPDATE statement typed by hand against a
// production table. Zero rows moved is NOT an error here — "there was nothing to
// reopen" is a legitimate, common answer, and this is not one of the ordered
// transitions whose no-op means divergence.
//
// It moves rows only from a fallback state to discovered. It cannot resurrect a
// done row, it clears no postv2 coordinates, and it writes to no repo.
func (l *rematerializeLedger) ReopenFallback(ctx context.Context, communityDID string) (int, error) {
	res, err := l.db.ExecContext(ctx, `
		UPDATE post_rematerialization_ledger
		SET state = $1, reason = NULL, updated_at = NOW()
		WHERE state = $2
		  AND ($3 = '' OR community_did = $3)
	`, string(posts.RematerializeDiscovered), string(posts.RematerializeFallbackLeftLegacy), communityDID)
	if err != nil {
		return 0, fmt.Errorf("reopening fallback rows: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reopening fallback rows: reading rows affected: %w", err)
	}
	return int(affected), nil
}

// CountByState is the census: how many rows sit in each state, restricted to
// communityDID when it is non-empty, so a staged run can report on its own scope
// and on the whole migration as two separate facts.
func (l *rematerializeLedger) CountByState(ctx context.Context, communityDID string) (map[posts.RematerializeState]int, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT state, COUNT(*)
		FROM post_rematerialization_ledger
		WHERE ($1 = '' OR community_did = $1)
		GROUP BY state
	`, communityDID)
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

// scanLedgerRow reads one row in the ledgerColumns order. It takes the package's
// shared rowScanner (post_repo.go) so one scan body serves both the single-row
// Get and the batched ListResumable.
func scanLedgerRow(src rowScanner) (posts.RematerializeLedgerRow, error) {
	var (
		row          posts.RematerializeLedgerRow
		state        string
		authorDID    sql.NullString
		communityDID sql.NullString
		sourceCID    sql.NullString
		newURI       sql.NullString
		newCID       sql.NullString
		newRkey      sql.NullString
		reason       sql.NullString
	)
	if err := src.Scan(&row.OldURI, &state, &authorDID, &communityDID, &sourceCID,
		&newURI, &newCID, &newRkey, &reason, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return posts.RematerializeLedgerRow{}, err
	}
	row.State = posts.RematerializeState(state)
	row.AuthorDID = authorDID.String
	row.CommunityDID = communityDID.String
	row.SourceCID = sourceCID.String
	row.NewURI = newURI.String
	row.NewCID = newCID.String
	row.NewRkey = newRkey.String
	row.Reason = reason.String
	return row, nil
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
