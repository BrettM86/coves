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
	require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
		"079 (community provisioning accounts) must be rolled back first")
	require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),
		"078 (untyped gallery $type stamp) must be rolled back first")
	require.EqualValues(t, 77, testkit.MigrateDownOne(t, db, 77),
		"077 (Bluesky profile import dismissal) must be rolled back first")
	require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
		"076 (account status rechecks) must be rolled back first")
	require.EqualValues(t, 75, testkit.MigrateDownOne(t, db, 75),
		"075 (moderation subject kind) must be rolled back first")
	require.EqualValues(t, 74, testkit.MigrateDownOne(t, db, 74),
		"074 (account status restrictions) must be rolled back first")
	require.EqualValues(t, 73, testkit.MigrateDownOne(t, db, 73),
		"073 (account ban hide reason) must be rolled back first")
	require.EqualValues(t, 72, testkit.MigrateDownOne(t, db, 72),
		"072 (account moderation subjects) must be rolled back first")
	require.EqualValues(t, 71, testkit.MigrateDownOne(t, db, 71),
		"071 (labels) must be rolled back first")
	require.EqualValues(t, 70, testkit.MigrateDownOne(t, db, 70),
		"070 (pending ban-check indexes) must be rolled back first")
	require.EqualValues(t, 69, testkit.MigrateDownOne(t, db, 69),
		"069 (ban hide markers) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 68, testkit.MigrateDownOne(t, db, 68),
		"068 (community ban index) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 67, testkit.MigrateDownOne(t, db, 67),
		"067 (community comment removals) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	require.EqualValues(t, 61, testkit.MigrateDownOne(t, db, 61))
	require.EqualValues(t, 60, testkit.MigrateDownOne(t, db, 60),
		"060 (users bio renamed to description) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 59, testkit.MigrateDownOne(t, db, 59),
		"059 (users origin username index) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 58, testkit.MigrateDownOne(t, db, 58),
		"058 (communities origin display) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 57, testkit.MigrateDownOne(t, db, 57),
		"057 (users profile origin) must be rolled back before testing earlier migrations")
	require.EqualValues(t, 56, testkit.MigrateDownOne(t, db, 56),
		"056 (moderation content snapshots) must be rolled back before testing earlier migrations")
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
