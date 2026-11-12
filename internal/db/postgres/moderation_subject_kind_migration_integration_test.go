//go:build integration

package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moderationKindAction is one instance action row written directly. Empty
// strings and a nil expiresAt are stored as NULL.
type moderationKindAction struct {
	id               string
	subject          string
	collection       string
	action           string
	subjectKind      string
	reversesActionID string
	expiresAt        *time.Time
}

// insertPreSubjectKindAction writes row under the 074 schema, which has no
// subject_kind column; row.subjectKind is ignored.
func insertPreSubjectKindAction(t *testing.T, db *sql.DB, row moderationKindAction) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, action, reason, origin, created_at,
		     expires_at, reverses_action_id)
		VALUES ($1, $2, $3, 'instance', $4, NULLIF($5, ''), $6, 'social.coves.moderation.defs#reasonSpam', 'local',
		        '2026-10-22T12:00:00Z', $7, NULLIF($8, ''))
	`, row.id, fixtures.DID("migrationadmin"), fixtures.InstanceDID(), row.subject, row.collection, row.action,
		row.expiresAt, row.reversesActionID)
	require.NoError(t, err)
}

// insertSubjectKindAction writes row with its subject_kind.
func insertSubjectKindAction(t *testing.T, db *sql.DB, row moderationKindAction) error {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, subject_kind, action, reason, origin,
		     created_at, expires_at, reverses_action_id)
		VALUES ($1, $2, $3, 'instance', $4, NULLIF($5, ''), NULLIF($6, ''), $7, 'social.coves.moderation.defs#reasonSpam',
		        'local', '2026-10-22T12:00:00Z', $8, NULLIF($9, ''))
	`, row.id, fixtures.DID("migrationadmin"), fixtures.InstanceDID(), row.subject, row.collection, row.subjectKind,
		row.action, row.expiresAt, row.reversesActionID)
	return err
}

// readSubjectKinds returns subject_kind by action id for every stored action.
func readSubjectKinds(t *testing.T, db *sql.DB) map[string]sql.NullString {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT id, subject_kind FROM moderation_actions`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	kinds := map[string]sql.NullString{}
	for rows.Next() {
		var id string
		var kind sql.NullString
		require.NoError(t, rows.Scan(&id, &kind))
		kinds[id] = kind
	}
	require.NoError(t, rows.Err())
	return kinds
}

// subjectKindColumnCount is 1 while moderation_actions has subject_kind, 0 after it is dropped.
func subjectKindColumnCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'moderation_actions' AND column_name = 'subject_kind'
	`).Scan(&count))
	return count
}

// migrationFailureRecorder is a TestingT that records Fatalf and Errorf
// instead of stopping the test, so a migration that must fail can be run.
type migrationFailureRecorder struct {
	*testing.T
	failures []string
}

func (r *migrationFailureRecorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *migrationFailureRecorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestModerationMigration075_BackfillsSubjectKind(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),
		"080 (moderation community record writes) must be rolled back first")
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
	timedCommunity := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kindtimed"))
	removedCommunity := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kindremoved"))
	plainAccount := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kindplain"))
	insertCommunityRowForDID(t, db, timedCommunity)
	insertCommunityRowForDID(t, db, removedCommunity)
	expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []moderationKindAction{
		{id: "m075-timed", subject: "at://" + timedCommunity, action: moderation.ActionRemove, expiresAt: &expiresAt},
		{id: "m075-timed-restore", subject: "at://" + timedCommunity, action: moderation.ActionRestore, reversesActionID: "m075-timed"},
		{id: "m075-community", subject: "at://" + removedCommunity, action: moderation.ActionRemove},
		{id: "m075-community-restore", subject: "at://" + removedCommunity, action: moderation.ActionRestore, reversesActionID: "m075-community"},
		{id: "m075-account", subject: "at://" + plainAccount, action: moderation.ActionRemove},
		{
			id: "m075-record", subject: "at://" + plainAccount + "/" + moderation.CommentCollection + "/3lrecord",
			collection: moderation.CommentCollection, action: moderation.ActionRemove,
		},
	} {
		insertPreSubjectKindAction(t, db, row)
	}

	testkit.MigrateUp(t, db)

	assert.Equal(t, map[string]sql.NullString{
		"m075-timed":             {String: "account", Valid: true},
		"m075-timed-restore":     {String: "account", Valid: true},
		"m075-community":         {String: "community", Valid: true},
		"m075-community-restore": {String: "community", Valid: true},
		"m075-account":           {String: "account", Valid: true},
		"m075-record":            {},
	}, readSubjectKinds(t, db))
}

func TestModerationMigration075_SubjectKindConstraints(t *testing.T) {
	db := testkit.DB(t)
	account := "at://" + fixtures.DID(testkit.UniqueIDWithPrefix(t, "kindcheck"))
	record := account + "/" + moderation.CommentCollection + "/3lcheck"
	expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		row        moderationKindAction
		wantReject bool
	}{
		{name: "account subject recorded as account", row: moderationKindAction{subject: account, subjectKind: "account"}},
		{name: "account subject recorded as community", row: moderationKindAction{subject: account, subjectKind: "community"}},
		{name: "record subject without a kind", row: moderationKindAction{subject: record, collection: moderation.CommentCollection}},
		{name: "account subject without a kind", row: moderationKindAction{subject: account}, wantReject: true},
		{
			name: "record subject recorded as account", wantReject: true,
			row: moderationKindAction{subject: record, collection: moderation.CommentCollection, subjectKind: "account"},
		},
		{name: "account subject with an unknown kind", row: moderationKindAction{subject: account, subjectKind: "banana"}, wantReject: true},
		// RemoveCommunity never sets an expiry; a timed community decision
		// would read as an account ban under the backfill rule.
		{
			name: "community kind with an expiry", wantReject: true,
			row: moderationKindAction{subject: account, subjectKind: "community", expiresAt: &expiresAt},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := test.row
			row.id = testkit.UniqueIDWithPrefix(t, "kind")
			row.action = moderation.ActionRemove
			err := insertSubjectKindAction(t, db, row)
			if !test.wantReject {
				require.NoError(t, err)
				return
			}
			var pqErr *pq.Error
			require.True(t, errors.As(err, &pqErr), "want a check violation, got %v", err)
			assert.Equal(t, postgresCheckViolation, string(pqErr.Code), "want a check violation, got %v", err)
		})
	}
}

func TestModerationMigration075_DownRequiresBackfillAgreement(t *testing.T) {
	t.Run("kinds that match the backfill rule roll back", func(t *testing.T) {
		db := testkit.DB(t)
		community := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kinddowncom"))
		account := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kinddownacct"))
		insertCommunityRowForDID(t, db, community)
		expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
		for _, row := range []moderationKindAction{
			{id: "down-community", subject: "at://" + community, action: moderation.ActionRemove, subjectKind: "community"},
			{
				id: "down-community-restore", subject: "at://" + community, action: moderation.ActionRestore,
				subjectKind: "community", reversesActionID: "down-community",
			},
			{id: "down-timed", subject: "at://" + account, action: moderation.ActionRemove, subjectKind: "account", expiresAt: &expiresAt},
			{id: "down-account", subject: "at://" + account, action: moderation.ActionRemove, subjectKind: "account"},
		} {
			require.NoError(t, insertSubjectKindAction(t, db, row))
		}

		require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),

			"080 (moderation community record writes) must be rolled back first")

		require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
			"079 (community provisioning accounts) must be rolled back first")
		require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),

			"078 (untyped gallery $type stamp) must be rolled back first")

		require.EqualValues(t, 77, testkit.MigrateDownOne(t, db, 77),
			"077 (Bluesky profile import dismissal) must be rolled back first")
		require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
			"076 (account status rechecks) must be rolled back first")
		require.EqualValues(t, 75, testkit.MigrateDownOne(t, db, 75))
		assert.Zero(t, subjectKindColumnCount(t, db), "the down migration drops subject_kind")
	})
	expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		communityRow bool
		rows         func(subject string) []moderationKindAction
	}{
		// A permanent account ban on a DID with a communities row: the 074
		// schema would read it as a community removal.
		{
			name: "a permanent account ban on a community DID refuses to roll back", communityRow: true,
			rows: func(subject string) []moderationKindAction {
				return []moderationKindAction{{id: "down-disagree", subject: subject, action: moderation.ActionRemove, subjectKind: "account"}}
			},
		},
		// A timed account ban on a DID with a communities row: the 074 schema
		// would read it as neither an account ban nor a community removal.
		{
			name: "a timed account ban on a community DID refuses to roll back", communityRow: true,
			rows: func(subject string) []moderationKindAction {
				return []moderationKindAction{{
					id: "down-disagree", subject: subject, action: moderation.ActionRemove, subjectKind: "account", expiresAt: &expiresAt,
				}}
			},
		},
		// A community removal on a DID with no communities row: the 074 schema
		// would read it as an account ban. No account action is on a
		// community DID and no restore is involved.
		{
			name: "a community removal on a DID with no communities row refuses to roll back",
			rows: func(subject string) []moderationKindAction {
				return []moderationKindAction{{id: "down-orphan-removal", subject: subject, action: moderation.ActionRemove, subjectKind: "community"}}
			},
		},
		// A restore whose kind differs from the account ban it reverses: the
		// 074 schema has no kind for it to keep. The ban itself matches the
		// backfill rule and the DID has no communities row.
		{
			name: "a restore whose kind differs from the action it reverses refuses to roll back",
			rows: func(subject string) []moderationKindAction {
				return []moderationKindAction{
					{id: "down-ban", subject: subject, action: moderation.ActionRemove, subjectKind: "account"},
					{id: "down-ban-restore", subject: subject, action: moderation.ActionRestore, subjectKind: "community", reversesActionID: "down-ban"},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kinddownban"))
			if test.communityRow {
				insertCommunityRowForDID(t, db, did)
			}
			wantKinds := map[string]sql.NullString{}
			for _, row := range test.rows("at://" + did) {
				require.NoError(t, insertSubjectKindAction(t, db, row))
				wantKinds[row.id] = sql.NullString{String: row.subjectKind, Valid: true}
			}

			require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),

				"080 (moderation community record writes) must be rolled back first")

			require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
				"079 (community provisioning accounts) must be rolled back first")
			require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),

				"078 (untyped gallery $type stamp) must be rolled back first")

			require.EqualValues(t, 77, testkit.MigrateDownOne(t, db, 77),
				"077 (Bluesky profile import dismissal) must be rolled back first")
			require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
				"076 (account status rechecks) must be rolled back first")
			recorder := &migrationFailureRecorder{T: t}
			assert.Zero(t, testkit.MigrateDownOne(recorder, db, 75), "the down migration must not complete")
			assert.NotEmpty(t, recorder.failures, "the down migration must fail")
			assert.Equal(t, 1, subjectKindColumnCount(t, db), "a refused down migration keeps subject_kind")
			assert.Equal(t, wantKinds, readSubjectKinds(t, db))
		})
	}
}

// A users row written before 075 is backfilled as account-indexed, so it
// stays account activity: communityRepo.Create refuses its DID.
func TestModerationMigration075_BackfillsUsersIndexSourceAsAccount(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),
		"080 (moderation community record writes) must be rolled back first")
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
	did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "preindexsrc"))
	_, err := db.ExecContext(t.Context(), `INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
		did, testkit.UniqueIDWithPrefix(t, "preindexsrc")+".test", testkit.Endpoints().PDS.BaseURL)
	require.NoError(t, err, "fixture: a users row under the 074 schema, with no authored content")

	testkit.MigrateUp(t, db)

	var indexSource string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT index_source FROM users WHERE did = $1`, did).Scan(&indexSource))
	assert.Equal(t, "account", indexSource)
	_, err = postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
		Create(t.Context(), newActivityCommunity(t, did))
	require.ErrorIs(t, err, communities.ErrDIDHasAccountActivity)
}

// usersIndexSourceColumnCount is 1 while users has index_source, 0 after it is dropped.
func usersIndexSourceColumnCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'index_source'
	`).Scan(&count))
	return count
}

// 075 backfills index_source 'author_hydration' for every pre-existing users
// row whose DID has a communities row, and 'account' for every other row. A
// community DID with only a users row then reads as having no account
// activity; one with an authored post still does (guard: the post counts, so
// the backfill does not reopen ban evasion). Down drops the column (guard).
func TestModerationMigration075_BackfillsUsersIndexSourceForCommunityDIDs(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),
		"080 (moderation community record writes) must be rolled back first")
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

	communityWithPost := fixtures.DID(testkit.UniqueIDWithPrefix(t, "srcpost"))
	communityUserOnly := fixtures.DID(testkit.UniqueIDWithPrefix(t, "srcuser"))
	plainAccount := fixtures.DID(testkit.UniqueIDWithPrefix(t, "srcplain"))
	for _, did := range []string{communityWithPost, communityUserOnly, plainAccount} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
			did, testkit.UniqueIDWithPrefix(t, "srchandle")+".test", testkit.Endpoints().PDS.BaseURL)
		require.NoError(t, err, "fixture: a users row under the 074 schema")
	}
	insertCommunityRowForDID(t, db, communityWithPost)
	insertCommunityRowForDID(t, db, communityUserOnly)
	seedActivityPost(t, db, communityWithPost)

	testkit.MigrateUp(t, db)

	for _, test := range []struct {
		name                string
		did                 string
		wantIndexSource     string
		wantAccountActivity bool
	}{
		{name: "community DID with a users row and an authored post", did: communityWithPost, wantIndexSource: "author_hydration", wantAccountActivity: true},
		{name: "community DID with a users row only", did: communityUserOnly, wantIndexSource: "author_hydration", wantAccountActivity: false},
		{name: "ordinary account", did: plainAccount, wantIndexSource: "account", wantAccountActivity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var indexSource string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT index_source FROM users WHERE did = $1`, test.did).Scan(&indexSource))
			assert.Equal(t, test.wantIndexSource, indexSource)
			assert.Equal(t, test.wantAccountActivity, readAccountSubjectFacts(t, db, test.did).HasAccountActivity, "HasAccountActivity")
		})
	}

	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),

		"080 (moderation community record writes) must be rolled back first")

	require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
		"079 (community provisioning accounts) must be rolled back first")
	require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),

		"078 (untyped gallery $type stamp) must be rolled back first")

	require.EqualValues(t, 77, testkit.MigrateDownOne(t, db, 77),
		"077 (Bluesky profile import dismissal) must be rolled back first")
	require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
		"076 (account status rechecks) must be rolled back first")
	require.EqualValues(t, 75, testkit.MigrateDownOne(t, db, 75), "075's Down")
	assert.Equal(t, 0, usersIndexSourceColumnCount(t, db), "075's Down drops users.index_source")
}
