//go:build integration

package postgres

import (
	"context"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationRepository_IsAggregatorReadsUncommittedDeclaration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	uniqueID := testkit.UniqueID(t)
	aggregatorDID := "did:plc:notificationaggregator" + uniqueID
	otherDID := "did:plc:notificationother" + uniqueID
	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx,
		`INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, $2, $3, $4)`,
		aggregatorDID, "Notification test aggregator", "at://"+aggregatorDID+"/social.coves.aggregator.service/self", "bafyreirecipientgateaggregator")
	require.NoError(t, err)

	lookups := NewNotificationRepository(db).LookupsTx(transaction)
	aggregator, err := lookups.IsAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	require.True(t, aggregator, "an uncommitted aggregator declaration must gate its recipient")
	aggregator, err = lookups.IsAggregator(ctx, otherDID)
	require.NoError(t, err)
	require.False(t, aggregator, "an unrelated DID must not appear to be an aggregator")
}
