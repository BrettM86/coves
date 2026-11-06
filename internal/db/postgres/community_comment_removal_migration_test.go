//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"Coves/internal/db/migrations"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const communityCommentRemovalsMigration = "067_community_comment_removals.sql"

// rollBackToMigration rolls db back one asserted step at a time from its
// current head until version target is the latest applied migration, so the
// test keeps exercising target's Down as later migrations land.
func rollBackToMigration(t *testing.T, db *sql.DB, target int64) {
	t.Helper()
	for {
		var current int64
		require.NoError(t, db.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&current))
		require.GreaterOrEqual(t, current, target, "the schema is already below migration %d", target)
		if current == target {
			return
		}
		require.Equal(t, current, testkit.MigrateDownOne(t, db, current))
	}
}

// migrationDownSQL is the -- +goose Down section of an embedded migration.
func migrationDownSQL(t *testing.T, name string) string {
	t.Helper()
	source, err := migrations.FS.ReadFile(name)
	require.NoError(t, err)
	_, down, found := strings.Cut(string(source), "-- +goose Down")
	require.True(t, found, "%s has a Down section", name)
	return down
}

func communityCommentRemovalCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM community_comment_removals`).Scan(&count))
	return count
}

func communityCommentRemovalsExists(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('community_comment_removals') IS NOT NULL`).Scan(&exists))
	return exists
}

// Rolling back 067 would drop the only (repository DID, rkey) to subject
// mapping a community removal's later delete commit can be resolved through,
// so its Down refuses while any mapping exists, held or applied, and leaves the
// table and its rows in place. With no mapping it drops the table, and
// re-applying restores it.
func TestMigration067DownRefusesWhileCommentRemovalMappingsExist(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		state string // "" leaves the table empty
	}{
		{name: "a held removal mapping", state: "held"},
		{name: "an applied removal mapping", state: "applied"},
		{name: "no mappings", state: ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := testkit.DB(t)
			rollBackToMigration(t, db, 67)

			if row.state == "" {
				require.EqualValues(t, 67, testkit.MigrateDownOne(t, db, 67), "067's Down runs on an empty table")
				assert.False(t, communityCommentRemovalsExists(t, db), "067's Down drops the empty table")
				testkit.MigrateUp(t, db)
				assert.True(t, communityCommentRemovalsExists(t, db), "re-applying restores the table")
				return
			}

			var actionID any
			if row.state == "applied" {
				actionID = "migration067-applied-action"
				_, err := db.ExecContext(ctx, `INSERT INTO moderation_actions
					(id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection,
					 action, origin, created_at)
					VALUES ($1, 'did:plc:migration067community', 'did:plc:migration067community', 'community',
					        'did:plc:migration067community', 'at://did:plc:migration067author/social.coves.community.comment/3lsubject',
					        'social.coves.community.comment', 'apply-removal', 'inherited', NOW())`, actionID)
				require.NoError(t, err)
			}
			_, err := db.ExecContext(ctx, `INSERT INTO community_comment_removals
				(repository_did, record_key, record_cid, subject_uri, subject_cid, state, apply_action_id)
				VALUES ('did:plc:migration067community', '3lremoval', 'bafyreimigration067removal',
				        'at://did:plc:migration067author/social.coves.community.comment/3lsubject', 'bafyreimigration067subject', $1, $2)`,
				row.state, actionID)
			require.NoError(t, err)

			transaction, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, downErr := transaction.ExecContext(ctx, migrationDownSQL(t, communityCommentRemovalsMigration))
			require.NoError(t, transaction.Rollback())

			require.Error(t, downErr, "067's Down must refuse while the %s mapping exists", row.state)
			require.True(t, communityCommentRemovalsExists(t, db), "the table survives the refused Down")
			assert.Equal(t, 1, communityCommentRemovalCount(t, db), "the mapping survives the refused Down")
		})
	}
}

// moderation_actions refuses an empty actor_did except on the one system
// action that has no actor: the re-application of a community comment removal
// after the comment moved back under the removing community's post.
func TestModerationActionsEmptyActorOnlyForSubjectMovedSystemAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		actor   string
		reason  sql.NullString
		refused bool
	}{
		{name: "empty actor with no reason", actor: "", refused: true},
		{
			name: "empty actor with another reason", actor: "",
			reason: sql.NullString{String: "social.coves.moderation.defs#reasonMisinformation", Valid: true}, refused: true,
		},
		{
			name: "empty actor on the subject-moved system action", actor: "",
			reason: sql.NullString{String: "social.coves.moderation.defs#reasonSubjectMoved", Valid: true},
		},
		{name: "actor DID with no reason", actor: "did:plc:emptyactorcommunity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			_, err := db.ExecContext(context.Background(), `INSERT INTO moderation_actions
				(id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection,
				 action, reason, origin, created_at)
				VALUES ('emptyactoraction', $1, 'did:plc:emptyactorcommunity', 'community',
				        'did:plc:emptyactorcommunity', 'at://did:plc:emptyactorauthor/social.coves.community.comment/3lsubject',
				        'social.coves.community.comment', 'apply-removal', $2, 'inherited', NOW())`, tt.actor, tt.reason)

			if !tt.refused {
				require.NoError(t, err)
				return
			}
			var pqErr *pq.Error
			require.ErrorAs(t, err, &pqErr, "an empty actor outside the subject-moved system action must be refused")
			assert.Equal(t, pqerror.Code("23514"), pqErr.Code, "the refusal must be a check violation")
		})
	}
}
