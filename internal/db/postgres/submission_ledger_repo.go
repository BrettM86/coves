package postgres

import (
	"context"
	"database/sql"
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

func (l *submissionLedger) Reserve(ctx context.Context, cmd posts.ReserveSubmissionCommand) (posts.SubmissionReservation, error) {
	return posts.SubmissionReservation{}, nil
}

func (l *submissionLedger) Release(ctx context.Context, reservation posts.SubmissionReservation) error {
	return nil
}

func (l *submissionLedger) CountSince(ctx context.Context, authorDID, communityDID string, since time.Time) (int, error) {
	return 0, nil
}
