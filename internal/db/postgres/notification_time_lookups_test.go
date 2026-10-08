//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_ActivatedAtReadsUncommittedTransactionValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()

	activatedAt := time.Date(2026, time.September, 28, 12, 34, 56, 123456000, time.UTC)
	result, err := transaction.ExecContext(ctx, `UPDATE notification_activation SET activated_at = $1`, activatedAt)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rowsAffected, "the fixture must update the singleton activation row")

	actual, err := NewNotificationRepository(db).LookupsTx(transaction).ActivatedAt(ctx)
	require.NoError(t, err)
	require.Truef(t, actual.Equal(activatedAt), "ActivatedAt = %s, want uncommitted transaction value %s", actual, activatedAt)
}

func TestNotificationRepository_ActivatedAtMissingRowReturnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()

	result, err := transaction.ExecContext(ctx, `DELETE FROM notification_activation`)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rowsAffected, "the fixture must remove the singleton activation row")

	_, err = NewNotificationRepository(db).LookupsTx(transaction).ActivatedAt(ctx)
	require.ErrorIs(t, err, ErrNotificationActivationMissing, "a missing activation row must not become a zero-time success")
}

func TestNotificationRepository_IndexTimeIsTransactionNow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()

	var transactionTime time.Time
	require.NoError(t, transaction.QueryRowContext(ctx, `SELECT now()`).Scan(&transactionTime))
	actual, err := NewNotificationRepository(db).LookupsTx(transaction).IndexTime(ctx)
	require.NoError(t, err)
	require.Truef(t, actual.Equal(transactionTime), "IndexTime = %s, want transaction now() = %s", actual, transactionTime)
}
