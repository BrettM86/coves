//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run one real redrive pass against PostgresStateStore with a
// backlog larger than two production batches (100 rows each), then read the
// table back without ListRetryable's attempts filter or page limit.

const passBudgetBacklogSize = 250

// passBudgetRow is one jetstream_dead_letters row as stored, keyed by the
// event's unique time_us.
type passBudgetRow struct {
	id       int64
	attempts int
}

func readAllDeadLettersByTimeUS(t *testing.T, db *sql.DB, consumer string) map[int64]passBudgetRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT id, event_time_us, attempts
		FROM jetstream_dead_letters
		WHERE consumer_name = $1
		ORDER BY id ASC`, consumer)
	require.NoError(t, err)
	defer func() {
		_ = rows.Close() // iteration errors surface via rows.Err()
	}()

	byTimeUS := make(map[int64]passBudgetRow)
	for rows.Next() {
		var row passBudgetRow
		var timeUS int64
		require.NoError(t, rows.Scan(&row.id, &timeUS, &row.attempts))
		_, duplicate := byTimeUS[timeUS]
		require.Falsef(t, duplicate, "two dead letters share event time_us=%d", timeUS)
		byTimeUS[timeUS] = row
	}
	require.NoError(t, rows.Err())
	return byTimeUS
}

func newPostgresPassRedriver(t *testing.T, store *PostgresStateStore, consumer string, handler EventHandler) *DeadLetterRedriver {
	t.Helper()
	redriver := NewDeadLetterRedriver(
		DeadLetterRedriveStore{Source: store, Mutator: store, Pruner: store},
		map[string]EventHandler{consumer: handler},
	)
	require.Greater(t, passBudgetBacklogSize, 2*redriver.batchSize,
		"backlog must span at least three redrive pages or a same-pass re-list is invisible")
	return redriver
}

// One pass over a persistently failing backlog spanning three production
// pages must spend exactly one attempt of each row's budget: no row may be
// re-listed and re-failed within the same pass, whatever its starting count.
func TestDeadLetterRedriver_PostgresPassAttemptsEachFailingRowExactlyOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "redrive-pass-budget-once"

	handler := newFakeEventHandler()
	redriver := newPostgresPassRedriver(t, store, consumer, handler)
	startingAttempts := make(map[int64]int, passBudgetBacklogSize)
	for i := 0; i < passBudgetBacklogSize; i++ {
		timeUS := int64(100_000 + i)
		// Mostly fresh rows, with partly spent budgets spread across every page.
		attempts := 0
		switch i % 10 {
		case 3:
			attempts = 4
		case 7:
			attempts = 8
		}
		startingAttempts[timeUS] = attempts
		handler.alwaysFailTimeUS[timeUS] = true
		require.NoError(t, store.AddDeadLetter(ctx, consumer, timeUS, testEventJSON(t, timeUS),
			"seeded transient failure", attempts))
	}
	seeded := readAllDeadLettersByTimeUS(t, db, consumer)
	require.Len(t, seeded, passBudgetBacklogSize, "every seeded dead letter must be stored")

	redriver.redriveAll(ctx)

	stored := readAllDeadLettersByTimeUS(t, db, consumer)
	assert.Len(t, stored, passBudgetBacklogSize, "a failing redrive must never delete its dead letter")

	var attemptMismatches, callMismatches []string
	for timeUS, starting := range startingAttempts {
		row, ok := stored[timeUS]
		if !ok {
			attemptMismatches = append(attemptMismatches,
				fmt.Sprintf("time_us=%d (id=%d): row missing after the pass, want attempts=%d",
					timeUS, seeded[timeUS].id, starting+1))
		} else if row.attempts != starting+1 {
			attemptMismatches = append(attemptMismatches,
				fmt.Sprintf("time_us=%d (id=%d): attempts=%d, want %d (seeded %d + one attempt)",
					timeUS, row.id, row.attempts, starting+1, starting))
		}
		if calls := handler.calls(timeUS); calls != 1 {
			callMismatches = append(callMismatches,
				fmt.Sprintf("time_us=%d (id=%d): handled %d times, want 1", timeUS, seeded[timeUS].id, calls))
		}
	}
	assert.Emptyf(t, attemptMismatches,
		"%d of %d rows did not record exactly one attempt in one pass", len(attemptMismatches), passBudgetBacklogSize)
	assert.Emptyf(t, callMismatches,
		"%d of %d rows were not handled exactly once in one pass", len(callMismatches), passBudgetBacklogSize)
}

// A mixed backlog spanning three production pages drains in one pass: every
// success is deleted wherever its page falls, and every failure is attempted
// exactly once rather than re-listed ahead of the rows behind it.
func TestDeadLetterRedriver_PostgresPassDrainsSuccessesAcrossPages(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	store := NewPostgresStateStore(db)
	ctx := context.Background()
	const consumer = "redrive-pass-budget-drain"

	handler := newFakeEventHandler()
	redriver := newPostgresPassRedriver(t, store, consumer, handler)
	succeeds := make(map[int64]bool, passBudgetBacklogSize)
	for i := 0; i < passBudgetBacklogSize; i++ {
		timeUS := int64(200_000 + i)
		// Every third row succeeds, so successes sit on pages 1, 2 and 3.
		succeeds[timeUS] = i%3 == 0
		if !succeeds[timeUS] {
			handler.alwaysFailTimeUS[timeUS] = true
		}
		require.NoError(t, store.AddDeadLetter(ctx, consumer, timeUS, testEventJSON(t, timeUS),
			"seeded transient failure", 0))
	}
	seeded := readAllDeadLettersByTimeUS(t, db, consumer)
	require.Len(t, seeded, passBudgetBacklogSize, "every seeded dead letter must be stored")

	redriver.redriveAll(ctx)

	stored := readAllDeadLettersByTimeUS(t, db, consumer)
	var rowMismatches, callMismatches []string
	for timeUS, succeeded := range succeeds {
		row, present := stored[timeUS]
		switch {
		case succeeded && present:
			rowMismatches = append(rowMismatches,
				fmt.Sprintf("time_us=%d (id=%d): succeeding row still present with attempts=%d, want deleted",
					timeUS, row.id, row.attempts))
		case !succeeded && !present:
			rowMismatches = append(rowMismatches,
				fmt.Sprintf("time_us=%d (id=%d): failing row missing, want present with attempts=1",
					timeUS, seeded[timeUS].id))
		case !succeeded && row.attempts != 1:
			rowMismatches = append(rowMismatches,
				fmt.Sprintf("time_us=%d (id=%d): failing row attempts=%d, want 1", timeUS, row.id, row.attempts))
		}
		if calls := handler.calls(timeUS); calls != 1 {
			callMismatches = append(callMismatches,
				fmt.Sprintf("time_us=%d (id=%d, succeeds=%t): handled %d times, want 1",
					timeUS, seeded[timeUS].id, succeeded, calls))
		}
	}
	assert.Emptyf(t, rowMismatches,
		"%d of %d rows were not drained or attempted once in one pass", len(rowMismatches), passBudgetBacklogSize)
	assert.Emptyf(t, callMismatches,
		"%d of %d rows were not handled exactly once in one pass", len(callMismatches), passBudgetBacklogSize)
}
