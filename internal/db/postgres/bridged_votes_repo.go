package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/core/bridgedvotes"

	"github.com/lib/pq"
)

// BridgedVotesRepository implements bridgedvotes.Store over posts, comments and
// communities.
type BridgedVotesRepository struct {
	db *sql.DB
}

// NewBridgedVotesRepository builds the postgres-backed bridgedvotes.Store.
func NewBridgedVotesRepository(db *sql.DB) *BridgedVotesRepository {
	return &BridgedVotesRepository{db: db}
}

// SelectCandidates implements bridgedvotes.Store: it selects the oldest eligible subjects in poll-rotation order.
func (r *BridgedVotesRepository) SelectCandidates(ctx context.Context, storedHosts []string, lookback time.Duration, limit int) ([]bridgedvotes.Candidate, error) {
	// Exact pds_url equality is deliberate here. The poller normalizes operator
	// config, matches it to stored community values, then passes those exact stored
	// strings back so this persistence layer never turns an untrusted URL into a dial
	// target.
	//
	// Comments carry no community column, so their community and PDS must be inherited
	// from an indexed root post. The inner join and root soft-delete gate also prevent a
	// dangling comment or a comment on removed content from entering the poll rotation.
	query := `
		SELECT uri, pds_url
		FROM (
			SELECT p.uri, co.pds_url, p.bridged_polled_at, p.created_at
			FROM posts p
			JOIN communities co ON co.did = p.community_did
			WHERE co.pds_url = ANY($1)
				AND p.deleted_at IS NULL
				AND p.created_at > NOW() - make_interval(secs => $2)

			UNION ALL

			SELECT c.uri, co.pds_url, c.bridged_polled_at, c.created_at
			FROM comments c
			JOIN posts p ON p.uri = c.root_uri AND p.deleted_at IS NULL
			JOIN communities co ON co.did = p.community_did
			WHERE co.pds_url = ANY($1)
				AND c.deleted_at IS NULL
				AND c.created_at > NOW() - make_interval(secs => $2)
		) AS candidates
		-- A sweep cap smaller than the never-polled cohort used to leave which
		-- rows entered rotation first up to the query plan. Creation time and URI
		-- make that first pass stable across indexes, restarts and equal stamps.
		ORDER BY bridged_polled_at ASC NULLS FIRST, created_at ASC, uri ASC
		LIMIT $3
	`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(storedHosts), lookback.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to select bridged vote candidates: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close bridged vote candidate rows", "error", closeErr)
		}
	}()

	candidates := make([]bridgedvotes.Candidate, 0)
	for rows.Next() {
		var candidate bridgedvotes.Candidate
		if err := rows.Scan(&candidate.URI, &candidate.StoredPDSURL); err != nil {
			return nil, fmt.Errorf("failed to scan bridged vote candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating bridged vote candidates: %w", err)
	}

	return candidates, nil
}

// DistinctCommunityPDSURLs implements bridgedvotes.Store: it lists the stored community PDS values eligible for trust matching.
func (r *BridgedVotesRepository) DistinctCommunityPDSURLs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT pds_url
		FROM communities
		WHERE pds_url IS NOT NULL AND pds_url <> ''
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list distinct community PDS URLs: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close community PDS URL rows", "error", closeErr)
		}
	}()

	urls := make([]string, 0)
	for rows.Next() {
		var pdsURL string
		if err := rows.Scan(&pdsURL); err != nil {
			return nil, fmt.Errorf("failed to scan community PDS URL: %w", err)
		}
		urls = append(urls, pdsURL)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating community PDS URLs: %w", err)
	}

	return urls, nil
}

// ApplyAggregate implements bridgedvotes.Store: it applies a non-regressing bridged tally to its post or comment.
func (r *BridgedVotesRepository) ApplyAggregate(ctx context.Context, agg bridgedvotes.Aggregate) error {
	if agg.AsOf.IsZero() {
		// The client never produces one (ParseAsOf rejects the zero time), so
		// this is a caller bug, and a silent success would let counts land
		// without the sampling instant the >= guard depends on.
		return fmt.Errorf("apply bridged vote aggregate to %q: %w", agg.URI, bridgedvotes.ErrMissingAsOf)
	}

	// Jetstream record stamps and this poller race through the same bridged columns.
	// Keeping the >= guard, count replacement, and score recomputation in one UPDATE
	// prevents a read-then-write race from letting an older aggregate overwrite a newer
	// one or recomputing score from counts that did not win the guard.
	result, err := r.db.ExecContext(ctx, `
		UPDATE posts
		SET bridged_upvote_count = $2,
			bridged_downvote_count = $3,
			bridged_stats_as_of = $4,
			score = (upvote_count + $2) - (downvote_count + $3)
		WHERE uri = $1
			AND deleted_at IS NULL
			AND (bridged_stats_as_of IS NULL OR $4 >= bridged_stats_as_of)
	`, agg.URI, agg.Upvotes, agg.Downvotes, agg.AsOf)
	if err != nil {
		return fmt.Errorf("failed to apply bridged vote aggregate to post: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check bridged vote post aggregate result: %w", err)
	}
	if rowsAffected > 0 {
		return nil
	}

	commentResult, err := r.db.ExecContext(ctx, `
		UPDATE comments
		SET bridged_upvote_count = $2,
			bridged_downvote_count = $3,
			bridged_stats_as_of = $4,
			score = (upvote_count + $2) - (downvote_count + $3)
		WHERE uri = $1
			AND deleted_at IS NULL
			AND (bridged_stats_as_of IS NULL OR $4 >= bridged_stats_as_of)
	`, agg.URI, agg.Upvotes, agg.Downvotes, agg.AsOf)
	if err != nil {
		return fmt.Errorf("failed to apply bridged vote aggregate to comment: %w", err)
	}
	commentRowsAffected, err := commentResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check bridged vote comment aggregate result: %w", err)
	}
	if commentRowsAffected == 0 {
		// The poller selected this subject as existing and non-deleted moments
		// ago, so zero rows in both tables is almost always the stale-guard
		// case: a stored bridged_stats_as_of newer than what the bridge just
		// served. Once is a race with a Jetstream stamp; a sustained stream on
		// one URI is a stored stamp that will never be beaten, and Warn is the
		// level production actually emits.
		slog.Warn("bridged vote aggregate matched no writable subject",
			"uri", agg.URI,
			"incoming_as_of", agg.AsOf,
		)
	}

	// A subject may disappear after selection, and stale asOf values are expected to
	// lose the guard. Neither case may wedge the sweep, so zero matches in both tables
	// are a successful no-op just like a bridge response that omits a subject.
	return nil
}

// MarkPolled implements bridgedvotes.Store: it advances rotation watermarks for every attempted subject.
func (r *BridgedVotesRepository) MarkPolled(ctx context.Context, uris []string) error {
	if len(uris) == 0 {
		return nil
	}

	// Every attempted candidate advances even when the bridge omitted it from its
	// response, otherwise permanently absent aggregates would monopolize the oldest
	// rotation slots. Unknown or concurrently removed URIs therefore deliberately
	// affect zero rows without turning the sweep into an error. Both tables move
	// in one transaction so a batch is never left half-advanced, with its posts
	// out of the oldest slot and its comments still in it.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin bridged vote mark transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Warn("failed to roll back bridged vote mark transaction", "error", rollbackErr)
		}
	}()

	if _, err := tx.ExecContext(ctx, `
		UPDATE posts
		SET bridged_polled_at = NOW()
		WHERE uri = ANY($1)
	`, pq.Array(uris)); err != nil {
		return fmt.Errorf("failed to mark bridged vote posts polled: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE comments
		SET bridged_polled_at = NOW()
		WHERE uri = ANY($1)
	`, pq.Array(uris)); err != nil {
		return fmt.Errorf("failed to mark bridged vote comments polled: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit bridged vote mark transaction: %w", err)
	}
	return nil
}
