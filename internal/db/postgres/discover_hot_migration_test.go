//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var discoverHotTables = []string{
	"discover_hot_snapshots",
	"discover_hot_build_attempts",
	"discover_hot_candidates",
	"discover_hot_checkpoints",
}

func TestMigration048DiscoverHotSnapshots(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	assertDiscoverHotMigrationShape(t, db)
	assertDiscoverHotMigrationConstraints(t, db)

	_, err := db.ExecContext(ctx, `CREATE TABLE discover_hot_migration_sentinel (id BIGINT PRIMARY KEY)`)
	require.NoError(t, err)

	require.EqualValues(t, 48, testkit.MigrateDownOne(t, db, 48),
		"this test must exercise migration 048's Down section")
	for _, table := range discoverHotTables {
		assert.Falsef(t, discoverHotTableExists(t, db, table),
			"migration 048 Down must remove %s", table)
	}
	assert.True(t, discoverHotTableExists(t, db, "discover_hot_migration_sentinel"),
		"migration 048 Down must not remove unrelated tables")
	assert.True(t, discoverHotTableExists(t, db, "posts"),
		"migration 048 Down must leave the pre-existing application schema intact")

	testkit.MigrateUp(t, db)
	assertDiscoverHotMigrationShape(t, db)
	assert.True(t, discoverHotTableExists(t, db, "discover_hot_migration_sentinel"),
		"reapplying migration 048 must not disturb unrelated tables")
}

func assertDiscoverHotMigrationShape(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, table := range discoverHotTables {
		requireTableExists(t, db, table)
	}

	assert.Equal(t, []string{"id"}, primaryKeyColumns(t, db, "discover_hot_snapshots"))
	assert.Equal(t, []string{"id"}, primaryKeyColumns(t, db, "discover_hot_build_attempts"))
	assert.Equal(t, []string{"snapshot_id", "uri"}, primaryKeyColumns(t, db, "discover_hot_candidates"))
	assert.Equal(t, []string{"id"}, primaryKeyColumns(t, db, "discover_hot_checkpoints"))

	var buildAttemptDataType, buildAttemptNullable, buildAttemptDefault string
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
			AND table_name = 'discover_hot_build_attempts'
			AND column_name = 'created_at'
	`).Scan(&buildAttemptDataType, &buildAttemptNullable, &buildAttemptDefault))
	assert.Equal(t, "timestamp with time zone", buildAttemptDataType)
	assert.Equal(t, "NO", buildAttemptNullable)
	assert.Contains(t, buildAttemptDefault, "now()", "build attempts must timestamp omitted created_at values")

	assertDiscoverHotForeignKey(t, db, "discover_hot_candidates", []string{"snapshot_id"},
		"discover_hot_snapshots", []string{"id"}, "c")
	assertDiscoverHotForeignKey(t, db, "discover_hot_checkpoints", []string{"snapshot_id"},
		"discover_hot_snapshots", []string{"id"}, "c")

	for _, column := range []struct {
		name     string
		dataType string
	}{
		{name: "state_digest", dataType: "bytea"},
		{name: "community_positions", dataType: "jsonb"},
		{name: "recent_communities", dataType: "ARRAY"},
	} {
		var dataType, nullable string
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT data_type, is_nullable
			FROM information_schema.columns
			WHERE table_schema = current_schema()
				AND table_name = 'discover_hot_checkpoints'
				AND column_name = $1
		`, column.name).Scan(&dataType, &nullable))
		assert.Equal(t, column.dataType, dataType, "%s must retain its storage type", column.name)
		assert.Equal(t, "NO", nullable, "%s must be required", column.name)
	}

	uniqueKeys := discoverHotConstraintColumnSets(t, db, "discover_hot_checkpoints", "u")
	assert.Contains(t, uniqueKeys, []string{"snapshot_id", "state_digest"},
		"checkpoint state digest must be unique within a snapshot")

	checkpointChecks := checkConstraintDefinitions(t, db, "discover_hot_checkpoints")
	var digestSizeCheck, stateSizeCheck string
	for _, definition := range checkpointChecks {
		if strings.Contains(definition, "state_digest") && strings.Contains(definition, "= 32") {
			digestSizeCheck = definition
		}
		if strings.Contains(definition, "community_positions") &&
			strings.Contains(definition, "recent_communities") &&
			strings.Contains(definition, "<= 65536") {
			stateSizeCheck = definition
		}
	}
	assert.NotEmptyf(t, digestSizeCheck, "state_digest must be fixed at 32 bytes; checks found: %v", checkpointChecks)
	assert.NotEmptyf(t, stateSizeCheck, "full checkpoint state must retain its 64 KiB limit; checks found: %v", checkpointChecks)

	checks := checkConstraintDefinitions(t, db, "discover_hot_candidates")
	var finiteRankCheck string
	for _, definition := range checks {
		if strings.Contains(definition, "base_rank") &&
			strings.Contains(definition, "'-Infinity'") &&
			strings.Contains(definition, "'Infinity'") {
			finiteRankCheck = definition
		}
	}
	assert.NotEmptyf(t, finiteRankCheck,
		"discover_hot_candidates.base_rank must reject non-finite values; checks found: %v", checks)

	assertDiscoverHotIndex(t, db, "discover_hot_snapshots",
		[]string{"algorithm_version", "viewer_scope", "created_at"}, []bool{false, false, true})
	assertDiscoverHotIndex(t, db, "discover_hot_snapshots",
		[]string{"expires_at"}, []bool{false})
	assertDiscoverHotIndex(t, db, "discover_hot_build_attempts",
		[]string{"created_at"}, []bool{false})
	assertDiscoverHotIndex(t, db, "discover_hot_candidates",
		[]string{"snapshot_id", "community_did", "base_rank", "created_at", "uri"},
		[]bool{false, false, true, true, true})
	assertDiscoverHotIndex(t, db, "discover_hot_checkpoints",
		[]string{"snapshot_id", "state_digest"}, []bool{false, false})
	assertDiscoverHotIndex(t, db, "discover_hot_checkpoints",
		[]string{"snapshot_id"}, []bool{false})
}

func assertDiscoverHotMigrationConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	var snapshotID int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO discover_hot_snapshots (
			algorithm_version, viewer_scope, ranking_time, expires_at
		) VALUES (1, 'global', NOW(), NOW() + INTERVAL '5 minutes')
		RETURNING id
	`).Scan(&snapshotID)
	require.NoError(t, err)

	insertCandidate := func(uri string, rank float64) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO discover_hot_candidates (
				snapshot_id, uri, community_did, base_rank, created_at
			) VALUES ($1, $2, 'did:plc:m48community', $3, NOW())
		`, snapshotID, uri, rank)
		return err
	}
	require.NoError(t, insertCandidate("at://did:plc:m48/post/finite", 1.25))
	assert.Error(t, insertCandidate("at://did:plc:m48/post/nan", math.NaN()),
		"base_rank must reject NaN")
	assert.Error(t, insertCandidate("at://did:plc:m48/post/positive-infinity", math.Inf(1)),
		"base_rank must reject positive infinity")
	assert.Error(t, insertCandidate("at://did:plc:m48/post/negative-infinity", math.Inf(-1)),
		"base_rank must reject negative infinity")

	firstDigest := make([]byte, 32)
	secondDigest := make([]byte, 32)
	secondDigest[0] = 1
	_, err = db.ExecContext(ctx, `
		INSERT INTO discover_hot_checkpoints (
			snapshot_id, state_digest, community_positions, recent_communities
		) VALUES ($1, $2, '{"did:plc:m48community": 1}'::jsonb, ARRAY['did:plc:m48community'])
	`, snapshotID, firstDigest)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO discover_hot_checkpoints (
			snapshot_id, state_digest, community_positions, recent_communities
		) VALUES ($1, $2, '{"did:plc:m48community": 2}'::jsonb, ARRAY['did:plc:m48community'])
	`, snapshotID, secondDigest)
	require.NoError(t, err, "distinct checkpoint state and digest must coexist within one snapshot")
	_, err = db.ExecContext(ctx, `
		INSERT INTO discover_hot_checkpoints (
			snapshot_id, state_digest, community_positions, recent_communities
		) VALUES ($1, $2, '{"did:plc:m48community": 1}'::jsonb, ARRAY['did:plc:m48community'])
	`, snapshotID, firstDigest)
	assert.Error(t, err, "duplicate checkpoint state within one snapshot must be rejected")

	_, err = db.ExecContext(ctx, `DELETE FROM discover_hot_snapshots WHERE id = $1`, snapshotID)
	require.NoError(t, err)
	for _, table := range []string{"discover_hot_candidates", "discover_hot_checkpoints"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE snapshot_id = $1`, snapshotID).Scan(&count))
		assert.Zerof(t, count, "deleting a snapshot must cascade to its owned %s rows", table)
	}
}

func discoverHotTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1
		)
	`, table).Scan(&exists))
	return exists
}

func discoverHotConstraintColumnSets(t *testing.T, db *sql.DB, table, constraintType string) [][]string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT array_agg(a.attname ORDER BY key.ord)
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS key(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = key.attnum
		WHERE c.conrelid = $1::regclass AND c.contype = $2
		GROUP BY c.oid
	`, table, constraintType)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var sets [][]string
	for rows.Next() {
		var columns []string
		require.NoError(t, rows.Scan(pq.Array(&columns)))
		sets = append(sets, columns)
	}
	require.NoError(t, rows.Err())
	return sets
}

func assertDiscoverHotForeignKey(
	t *testing.T,
	db *sql.DB,
	table string,
	columns []string,
	referencedTable string,
	referencedColumns []string,
	deleteAction string,
) {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT c.confrelid::regclass::text, c.confdeltype::text,
			array_agg(child.attname ORDER BY child_key.ord),
			array_agg(parent.attname ORDER BY child_key.ord)
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS child_key(attnum, ord) ON true
		JOIN unnest(c.confkey) WITH ORDINALITY AS parent_key(attnum, ord)
			ON parent_key.ord = child_key.ord
		JOIN pg_attribute child
			ON child.attrelid = c.conrelid AND child.attnum = child_key.attnum
		JOIN pg_attribute parent
			ON parent.attrelid = c.confrelid AND parent.attnum = parent_key.attnum
		WHERE c.conrelid = $1::regclass AND c.contype = 'f'
		GROUP BY c.oid
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	matched := false
	for rows.Next() {
		var gotReferencedTable, gotDeleteAction string
		var gotColumns, gotReferencedColumns []string
		require.NoError(t, rows.Scan(
			&gotReferencedTable,
			&gotDeleteAction,
			pq.Array(&gotColumns),
			pq.Array(&gotReferencedColumns),
		))
		if gotReferencedTable == referencedTable &&
			gotDeleteAction == deleteAction &&
			assert.ObjectsAreEqual(columns, gotColumns) &&
			assert.ObjectsAreEqual(referencedColumns, gotReferencedColumns) {
			matched = true
		}
	}
	require.NoError(t, rows.Err())
	assert.Truef(t, matched,
		"%s(%v) must reference %s(%v) with ON DELETE CASCADE",
		table, columns, referencedTable, referencedColumns)
}

func assertDiscoverHotIndex(t *testing.T, db *sql.DB, table string, columns []string, descending []bool) {
	t.Helper()
	require.Len(t, descending, len(columns))

	rows, err := db.QueryContext(context.Background(), `
		SELECT array_agg(a.attname ORDER BY key.ord),
			array_agg((i.indoption[(key.ord - 1)::integer] & 1) = 1 ORDER BY key.ord)
		FROM pg_index i
		JOIN unnest(i.indkey) WITH ORDINALITY AS key(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = key.attnum
		WHERE i.indrelid = $1::regclass AND i.indpred IS NULL
		GROUP BY i.indexrelid, i.indoption
	`, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	matched := false
	for rows.Next() {
		var gotColumns []string
		var gotDescending []bool
		require.NoError(t, rows.Scan(pq.Array(&gotColumns), pq.Array(&gotDescending)))
		if assert.ObjectsAreEqual(columns, gotColumns) && assert.ObjectsAreEqual(descending, gotDescending) {
			matched = true
		}
	}
	require.NoError(t, rows.Err())
	assert.Truef(t, matched, "%s must have an index on %v with descending flags %v", table, columns, descending)
}
