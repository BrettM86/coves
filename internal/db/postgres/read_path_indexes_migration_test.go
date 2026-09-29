//go:build integration

package postgres

import (
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// Migration 049 adds the read-path indexes and drops the ones that duplicate a
// UNIQUE constraint's index. Both directions must leave every constraint that
// actually enforces something in place.
func TestMigration049ReadPathIndexes(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	added := []string{"idx_posts_created", "idx_posts_score", "idx_comments_root_rkey", "idx_subscriptions_record_uri"}
	dropped := []string{
		"idx_posts_uri", "idx_votes_uri", "idx_votes_voter_subject", "idx_comments_uri",
		"idx_comments_uri_lookup", "idx_subscriptions_user_community", "idx_subscriptions_user",
	}
	// The constraint indexes that make the dropped ones redundant.
	kept := []string{
		"posts_uri_key", "votes_uri_key", "unique_voter_subject_active", "comments_uri_key",
		"community_subscriptions_user_did_community_did_key",
	}

	for _, name := range append(added, kept...) {
		require.Truef(t, indexExists(t, db, name), "after 049 Up, %s must exist", name)
	}
	for _, name := range dropped {
		require.Falsef(t, indexExists(t, db, name), "after 049 Up, redundant %s must be gone", name)
	}
	assertReadPathIndexDefinitions(t, db, "after 049 Up")

	require.EqualValues(t, 50, testkit.MigrateDownOne(t, db, 50),
		"050 (moderation state) must be rolled back before testing 049")
	require.EqualValues(t, 49, testkit.MigrateDownOne(t, db, 49),
		"this test must exercise migration 049's Down section")
	for _, name := range append(dropped, kept...) {
		require.Truef(t, indexExists(t, db, name), "after 049 Down, %s must exist", name)
	}
	for _, name := range added {
		require.Falsef(t, indexExists(t, db, name), "after 049 Down, %s must be gone", name)
	}

	testkit.MigrateUp(t, db)
	for _, name := range append(added, kept...) {
		require.Truef(t, indexExists(t, db, name), "after reapplying 049, %s must exist", name)
	}
	assertReadPathIndexDefinitions(t, db, "after reapplying 049")
}

// assertReadPathIndexDefinitions checks that each index 049 builds is valid
// and has the intended column order and partial predicate. A CONCURRENTLY
// build that is interrupted leaves an INVALID index behind under the same
// name, which the planner never uses, so existence alone proves nothing.
func assertReadPathIndexDefinitions(t *testing.T, db *sql.DB, stage string) {
	t.Helper()
	expectedDefinitions := map[string]string{
		"idx_posts_created": "CREATE INDEX idx_posts_created ON public.posts USING btree " +
			"(created_at DESC, uri DESC) WHERE (deleted_at IS NULL)",
		"idx_posts_score": "CREATE INDEX idx_posts_score ON public.posts USING btree " +
			"(score DESC, created_at DESC, uri DESC) WHERE (deleted_at IS NULL)",
		"idx_comments_root_rkey": "CREATE INDEX idx_comments_root_rkey ON public.comments USING btree " +
			"(root_uri, rkey)",
		"idx_subscriptions_record_uri": "CREATE INDEX idx_subscriptions_record_uri ON public.community_subscriptions USING btree " +
			"(record_uri)",
	}
	for name, expectedDefinition := range expectedDefinitions {
		var valid bool
		var definition string
		err := db.QueryRowContext(context.Background(), `
			SELECT i.indisvalid, pg_get_indexdef(i.indexrelid)
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname = $1
		`, name).Scan(&valid, &definition)
		require.NoErrorf(t, err, "%s: reading %s from pg_index", stage, name)
		require.Truef(t, valid, "%s: %s must be a valid index (indisvalid)", stage, name)
		require.Equalf(t, expectedDefinition, definition, "%s: %s definition", stage, name)
	}
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, name).Scan(&exists)
	require.NoError(t, err)
	return exists
}
