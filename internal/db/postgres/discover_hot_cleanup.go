package postgres

import (
	"context"
	"fmt"

	"Coves/internal/core/discover"

	"github.com/lib/pq"
)

func (r *postgresDiscoverRepo) CleanupExpiredDiscoverHotState(ctx context.Context, derivedRowBatchSize int) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if derivedRowBatchSize <= 0 {
		return 0, fmt.Errorf("Discover Hot cleanup batch size must be positive")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin Discover Hot cleanup: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		WITH doomed AS (
			SELECT id
			FROM discover_hot_build_attempts
			WHERE created_at <= NOW() - INTERVAL '1 minute'
			ORDER BY created_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM discover_hot_build_attempts attempt
		USING doomed
		WHERE attempt.id = doomed.id
	`, derivedRowBatchSize)
	if err != nil {
		return 0, fmt.Errorf("delete expired Discover Hot build attempts: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired Discover Hot build attempts: %w", err)
	}
	remaining := int64(derivedRowBatchSize) - removed

	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM discover_hot_snapshots
		WHERE expires_at <= NOW()
		ORDER BY expires_at, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, remaining)
	if err != nil {
		return 0, fmt.Errorf("lock expired Discover Hot snapshots: %w", err)
	}

	snapshotIDs := make([]int64, 0, derivedRowBatchSize)
	for rows.Next() {
		var snapshotID int64
		if err := rows.Scan(&snapshotID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired Discover Hot snapshot: %w", err)
		}
		snapshotIDs = append(snapshotIDs, snapshotID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate expired Discover Hot snapshots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired Discover Hot snapshots: %w", err)
	}

	if len(snapshotIDs) > 0 {
		result, err = tx.ExecContext(ctx, `
			WITH doomed AS (
				SELECT candidate.snapshot_id, candidate.uri
				FROM discover_hot_candidates candidate
				WHERE candidate.snapshot_id = ANY($1)
				ORDER BY candidate.snapshot_id, candidate.uri
				LIMIT $2
			)
			DELETE FROM discover_hot_candidates candidate
			USING doomed
			WHERE candidate.snapshot_id = doomed.snapshot_id
				AND candidate.uri = doomed.uri
		`, pq.Array(snapshotIDs), remaining)
		if err != nil {
			return 0, fmt.Errorf("delete expired Discover Hot candidates: %w", err)
		}
		candidateRows, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count expired Discover Hot candidates: %w", err)
		}
		removed += candidateRows

		remaining = int64(derivedRowBatchSize) - removed
		if remaining > 0 {
			result, err = tx.ExecContext(ctx, `
				WITH doomed AS (
					SELECT checkpoint.id
					FROM discover_hot_checkpoints checkpoint
					WHERE checkpoint.snapshot_id = ANY($1)
					ORDER BY checkpoint.snapshot_id, checkpoint.id
					LIMIT $2
				)
				DELETE FROM discover_hot_checkpoints checkpoint
				USING doomed
				WHERE checkpoint.id = doomed.id
			`, pq.Array(snapshotIDs), remaining)
			if err != nil {
				return 0, fmt.Errorf("delete expired Discover Hot checkpoints: %w", err)
			}
			checkpointRows, err := result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("count expired Discover Hot checkpoints: %w", err)
			}
			removed += checkpointRows
		}

		_, err = tx.ExecContext(ctx, `
			WITH doomed AS (
				SELECT snapshot.id
				FROM discover_hot_snapshots snapshot
				WHERE snapshot.id = ANY($1)
					AND snapshot.expires_at <= NOW()
					AND NOT EXISTS (
						SELECT 1 FROM discover_hot_candidates candidate
						WHERE candidate.snapshot_id = snapshot.id
					)
					AND NOT EXISTS (
						SELECT 1 FROM discover_hot_checkpoints checkpoint
						WHERE checkpoint.snapshot_id = snapshot.id
					)
				ORDER BY snapshot.expires_at, snapshot.id
				LIMIT $2
			)
			DELETE FROM discover_hot_snapshots snapshot
			USING doomed
			WHERE snapshot.id = doomed.id
		`, pq.Array(snapshotIDs), derivedRowBatchSize)
		if err != nil {
			return 0, fmt.Errorf("delete expired Discover Hot snapshots: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit Discover Hot cleanup: %w", err)
	}
	return removed, nil
}

var _ discover.DiscoverHotStateCleaner = (*postgresDiscoverRepo)(nil)
