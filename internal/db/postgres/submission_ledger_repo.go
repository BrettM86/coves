package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/posts"
)

// PostgreSQL storage for the post_submissions ledger (migration 035) — the
// rows that both deduplicate submissions and meter the per-author quota of
// docs/PRD_AUTHOR_OWNED_POSTS.md §8.
//
// It mirrors the aggregator limiter of migration 012: a row per accepted
// submission, a rolling-window COUNT over an index that leads with the pair
// being metered. The difference is that Reserve is written BEFORE the PDS
// write rather than after it, because here the insert is also the dedupe gate
// — see posts.SubmissionLedger for why that ordering is the safe one.
type submissionLedger struct {
	db *sql.DB
}

// NewSubmissionLedger creates the post_submissions repository.
func NewSubmissionLedger(db *sql.DB) posts.SubmissionLedger {
	return &submissionLedger{db: db}
}

// Reserve inserts the ledger row, letting the unique key answer the dedupe
// question.
//
// ON CONFLICT DO NOTHING ... RETURNING id is the whole check in one statement:
// a fresh submission returns its new id, and a repeat returns no rows at all,
// which arrives here as sql.ErrNoRows. Two racing identical submissions
// therefore get different answers from the database rather than the same answer
// from two reads — which is the entire reason the insert, and not a preceding
// SELECT, is the gate.
//
// The conflict target names the dedupe key explicitly rather than being left
// bare, so that a unique constraint added to this table later cannot be
// silently absorbed into "that was a duplicate".
func (l *submissionLedger) Reserve(ctx context.Context, cmd posts.ReserveSubmissionCommand) (posts.SubmissionReservation, error) {
	var id int64
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO post_submissions (author_did, community_did, fingerprint, dedupe_bucket)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (author_did, community_did, fingerprint, dedupe_bucket) DO NOTHING
		RETURNING id
	`, cmd.AuthorDID, cmd.CommunityDID, cmd.Fingerprint, cmd.DedupeBucket).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		// The row was refused by the dedupe key, which is a policy answer rather
		// than a storage failure. It is reported as the domain sentinel so the
		// caller can tell "someone already posted this" from "the database is
		// unwell" — and so the wording never reaches posts.IsConflict's
		// substring match, which would report a refused submission to its author
		// as one that already exists in the index.
		return posts.SubmissionReservation{}, posts.ErrDuplicateSubmission
	}
	if err != nil {
		return posts.SubmissionReservation{}, fmt.Errorf("failed to reserve submission: %w", err)
	}

	return posts.SubmissionReservation{ID: id}, nil
}

// Release removes a reservation whose submission never became a post.
//
// Deleting by the surrogate id rather than by the natural key is what makes
// this safe under concurrency: it can only ever remove the row this request
// inserted, so a release racing another author's submission of identical
// content cannot take theirs instead.
//
// Deleting nothing is a success. The caller is already handling a failure when
// it reaches here, and an error over an absent row would replace the reason the
// submission actually failed with a second, less useful one.
func (l *submissionLedger) Release(ctx context.Context, reservation posts.SubmissionReservation) error {
	if _, err := l.db.ExecContext(ctx, `
		DELETE FROM post_submissions WHERE id = $1
	`, reservation.ID); err != nil {
		return fmt.Errorf("failed to release submission reservation: %w", err)
	}
	return nil
}

// CountSince counts one author's submissions to one community inside the
// rolling window — the query idx_post_submissions_rate_limit exists for.
func (l *submissionLedger) CountSince(ctx context.Context, authorDID, communityDID string, since time.Time) (int, error) {
	var count int
	if err := l.db.QueryRowContext(ctx, `
		SELECT count(*) FROM post_submissions
		WHERE author_did = $1 AND community_did = $2 AND created_at >= $3
	`, authorDID, communityDID, since).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count recent submissions: %w", err)
	}
	return count, nil
}
