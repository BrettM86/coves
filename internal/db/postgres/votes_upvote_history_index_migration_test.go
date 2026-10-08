//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestVotesUpvoteHistoryIndexMigration054_DownAndUpRoundTrip(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	assertVotesUpvoteHistoryIndex(t, db, "after 054 Up")
	require.EqualValues(t, 55, testkit.MigrateDownOne(t, db, 55),
		"055 (notification public post withdrawals) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 54, testkit.MigrateDownOne(t, db, 54),
		"this must exercise 054's Down section, not an earlier migration")
	require.False(t, indexExists(t, db, "idx_votes_voter_subject_upvotes"),
		"054 Down must remove the upvote history index")

	testkit.MigrateUp(t, db)
	assertVotesUpvoteHistoryIndex(t, db, "after reapplying 054")
}

func assertVotesUpvoteHistoryIndex(t *testing.T, db *sql.DB, stage string) {
	t.Helper()
	var valid, ready, unique bool
	var keyCount, attributeCount int
	var firstColumn, secondColumn, predicate string
	err := db.QueryRowContext(context.Background(), `
		SELECT i.indisvalid, i.indisready, i.indisunique, i.indnkeyatts, i.indnatts,
			pg_get_indexdef(i.indexrelid, 1, true), pg_get_indexdef(i.indexrelid, 2, true),
			pg_get_expr(i.indpred, i.indrelid)
		FROM pg_index i
		JOIN pg_class index_class ON index_class.oid = i.indexrelid
		JOIN pg_class table_class ON table_class.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = table_class.relnamespace
		WHERE n.nspname = 'public' AND table_class.relname = 'votes'
			AND index_class.relname = 'idx_votes_voter_subject_upvotes'
	`).Scan(&valid, &ready, &unique, &keyCount, &attributeCount,
		&firstColumn, &secondColumn, &predicate)
	require.NoErrorf(t, err, "%s: reading idx_votes_voter_subject_upvotes on votes", stage)
	require.True(t, valid, stage+": index must be valid")
	require.True(t, ready, stage+": index must be ready for inserts")
	require.False(t, unique, stage+": index must not be unique")
	require.Equal(t, 2, keyCount, stage+": exactly two key columns")
	require.Equal(t, 2, attributeCount, stage+": no included columns")
	require.Equal(t, "voter_did", firstColumn, stage+": first key column")
	require.Equal(t, "subject_uri", secondColumn, stage+": second key column")
	require.Equal(t, "(direction = 'up'::text)", predicate,
		stage+": deleted upvotes must remain indexed for history lookups")
}
