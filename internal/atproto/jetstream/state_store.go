package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PostgresStateStore persists Jetstream consumer state: per-consumer cursors
// and the dead letter queue. It lives in this package (like the direct SQL in
// vote_consumer.go) because the state is private to the firehose pipeline —
// no other domain reads these tables.
type PostgresStateStore struct {
	db *sql.DB
}

// NewPostgresStateStore creates a store for Jetstream consumer state.
func NewPostgresStateStore(db *sql.DB) *PostgresStateStore {
	return &PostgresStateStore{db: db}
}

// Compile-time interface satisfaction checks
var (
	_ CursorStore     = (*PostgresStateStore)(nil)
	_ DeadLetterQueue = (*PostgresStateStore)(nil)
)

// GetCursor returns the persisted cursor for the consumer, or 0 if none exists.
func (s *PostgresStateStore) GetCursor(ctx context.Context, consumerName string) (int64, error) {
	var cursorTimeUS int64
	err := s.db.QueryRowContext(ctx,
		`SELECT cursor_time_us FROM jetstream_cursors WHERE consumer_name = $1`,
		consumerName,
	).Scan(&cursorTimeUS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get jetstream cursor for %s: %w", consumerName, err)
	}
	return cursorTimeUS, nil
}

// SaveCursor upserts the cursor for the consumer. The WHERE clause keeps the
// cursor monotonic even if an older value is flushed out of order.
func (s *PostgresStateStore) SaveCursor(ctx context.Context, consumerName string, cursorTimeUS int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jetstream_cursors (consumer_name, cursor_time_us, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (consumer_name) DO UPDATE
		SET cursor_time_us = EXCLUDED.cursor_time_us, updated_at = NOW()
		WHERE jetstream_cursors.cursor_time_us < EXCLUDED.cursor_time_us`,
		consumerName, cursorTimeUS,
	)
	if err != nil {
		return fmt.Errorf("failed to save jetstream cursor for %s: %w", consumerName, err)
	}
	return nil
}

// AddDeadLetter stores a failed event for later redrive. eventData is stored
// as raw bytes (BYTEA) so byte-corrupt frames are capturable. redriveAttempts
// seeds the redrive budget: 0 for transient failures (the redriver retries
// them), MaxRedriveAttempts for permanent failures (kept for forensics only).
// Re-adding an already-captured event (same consumer, time_us, and payload —
// e.g. a poison event replayed by the reconnect rewind) hits the dedup index
// and is a no-op success: the event is safely captured, so the cursor may
// advance.
func (s *PostgresStateStore) AddDeadLetter(ctx context.Context, consumerName string, eventTimeUS int64, eventData []byte, handleErr string, redriveAttempts int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jetstream_dead_letters (consumer_name, event_time_us, event_data, last_error, attempts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		consumerName, eventTimeUS, eventData, handleErr, redriveAttempts,
	)
	if err != nil {
		return fmt.Errorf("failed to add jetstream dead letter for %s: %w", consumerName, err)
	}
	return nil
}

// ListRetryable returns up to limit dead letters for the consumer that have
// not exhausted their redrive attempts, oldest first.
func (s *PostgresStateStore) ListRetryable(ctx context.Context, consumerName string, maxAttempts, limit int) ([]DeadLetterEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, consumer_name, event_time_us, event_data, last_error, attempts, created_at
		FROM jetstream_dead_letters
		WHERE consumer_name = $1 AND attempts < $2
		ORDER BY id ASC
		LIMIT $3`,
		consumerName, maxAttempts, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list jetstream dead letters for %s: %w", consumerName, err)
	}
	defer func() {
		_ = rows.Close() // iteration errors surface via rows.Err()
	}()

	var deadLetters []DeadLetterEvent
	for rows.Next() {
		var deadLetter DeadLetterEvent
		if err := rows.Scan(
			&deadLetter.ID,
			&deadLetter.ConsumerName,
			&deadLetter.EventTimeUS,
			&deadLetter.EventData,
			&deadLetter.LastError,
			&deadLetter.Attempts,
			&deadLetter.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan jetstream dead letter: %w", err)
		}
		deadLetters = append(deadLetters, deadLetter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate jetstream dead letters: %w", err)
	}
	return deadLetters, nil
}

// DeleteDeadLetter removes a successfully redriven event.
func (s *PostgresStateStore) DeleteDeadLetter(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM jetstream_dead_letters WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete jetstream dead letter %d: %w", id, err)
	}
	return nil
}

// MarkRedriveAttempt increments the attempt counter after a failed redrive.
func (s *PostgresStateStore) MarkRedriveAttempt(ctx context.Context, id int64, handleErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jetstream_dead_letters
		SET attempts = attempts + 1, last_error = $2, updated_at = NOW()
		WHERE id = $1`,
		id, handleErr,
	)
	if err != nil {
		return fmt.Errorf("failed to mark jetstream dead letter attempt %d: %w", id, err)
	}
	return nil
}

// RetireDeadLetter marks a dead letter as permanently exhausted in one step
// (attempts jump straight to MaxRedriveAttempts) so it stops consuming
// redrive passes. The row remains in the table for forensics and is still
// included in the CountDeadLetters backlog.
func (s *PostgresStateStore) RetireDeadLetter(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jetstream_dead_letters
		SET attempts = GREATEST(attempts, $2), last_error = $3, updated_at = NOW()
		WHERE id = $1`,
		id, MaxRedriveAttempts, reason,
	)
	if err != nil {
		return fmt.Errorf("failed to retire jetstream dead letter %d: %w", id, err)
	}
	return nil
}

// CountDeadLetters returns the dead letter backlog per consumer.
func (s *PostgresStateStore) CountDeadLetters(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT consumer_name, COUNT(*)
		FROM jetstream_dead_letters
		GROUP BY consumer_name`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to count jetstream dead letters: %w", err)
	}
	defer func() {
		_ = rows.Close() // iteration errors surface via rows.Err()
	}()

	counts := make(map[string]int64)
	for rows.Next() {
		var consumerName string
		var count int64
		if err := rows.Scan(&consumerName, &count); err != nil {
			return nil, fmt.Errorf("failed to scan jetstream dead letter count: %w", err)
		}
		counts[consumerName] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate jetstream dead letter counts: %w", err)
	}
	return counts, nil
}
