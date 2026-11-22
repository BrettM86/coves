//go:build integration

package postgres

import (
	"database/sql"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

var classificationMigration095Tables = []string{
	"classification_jobs",
	"classification_results",
	"classification_usage",
}

func TestClassificationMigration095_DownDropsTablesAndUpRestoresThem(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),

		"100 (classification partial results) must be rolled back first")

	require.EqualValues(t, 99, testkit.MigrateDownOne(t, db, 99),
		"099 (classification deferral backoff) must be rolled back first")
	require.EqualValues(t, 98, testkit.MigrateDownOne(t, db, 98),
		"098 (classification score alerts) must be rolled back first")
	require.EqualValues(t, 97, testkit.MigrateDownOne(t, db, 97),
		"097 (classification verdicts) must be rolled back first")
	require.EqualValues(t, 96, testkit.MigrateDownOne(t, db, 96),
		"096 (classification image batches) must be rolled back first")
	require.EqualValues(t, 95, testkit.MigrateDownOne(t, db, 95),
		"095 (classification queue) must be the migration rolled back")
	for _, table := range classificationMigration095Tables {
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		require.False(t, relation.Valid, "095's Down must drop %s", table)
	}

	testkit.MigrateUp(t, db)
	for _, table := range classificationMigration095Tables {
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		require.True(t, relation.Valid, "095's Up must create %s", table)
	}
}

func TestClassificationMigration096_DownDropsBatchTableAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	relationExists := func(table string) bool {
		t.Helper()
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		return relation.Valid
	}

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),

		"100 (classification partial results) must be rolled back first")

	require.EqualValues(t, 99, testkit.MigrateDownOne(t, db, 99),
		"099 (classification deferral backoff) must be rolled back first")
	require.EqualValues(t, 98, testkit.MigrateDownOne(t, db, 98),
		"098 (classification score alerts) must be rolled back first")
	require.EqualValues(t, 97, testkit.MigrateDownOne(t, db, 97),
		"097 (classification verdicts) must be rolled back first")
	require.EqualValues(t, 96, testkit.MigrateDownOne(t, db, 96),
		"096 (classification image batches) must be the migration rolled back")
	require.False(t, relationExists("classification_job_batches"), "096's Down must drop classification_job_batches")
	require.True(t, relationExists("classification_jobs"), "096's Down must leave classification_jobs")

	testkit.MigrateUp(t, db)
	require.True(t, relationExists("classification_job_batches"), "096's Up must create classification_job_batches")

	var jobID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO classification_jobs
		    (subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id, author_did, community_did)
		VALUES ('at://did:plc:clsmig082/social.coves.community.postv2/3lclsmig0821', 'post', 'bafyclsmig082', 'v1',
		        '@cf/cloudflare/clef-flash', '@cf/cloudflare/clef', 'did:plc:clsmig082', 'did:plc:clsmig082community')
		RETURNING id
	`).Scan(&jobID))
	_, err := db.Exec(`
		INSERT INTO classification_job_batches (job_id, batch_index, model_id, probabilities, images_sent, images_skipped)
		VALUES ($1, 0, '@cf/cloudflare/clef', '{"spam_scam": 0.06}', 4, 0), ($1, 1, '@cf/cloudflare/clef', '{"spam_scam": 0.16}', 1, 0)
	`, jobID)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM classification_jobs WHERE id = $1`, jobID)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM classification_job_batches WHERE job_id = $1`, jobID).Scan(&remaining))
	require.Equal(t, 0, remaining, "deleting a job must delete its batch rows")
}

func TestClassificationMigration097_DownDropsVerdictTableAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	relationExists := func(table string) bool {
		t.Helper()
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		return relation.Valid
	}

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),

		"100 (classification partial results) must be rolled back first")

	require.EqualValues(t, 99, testkit.MigrateDownOne(t, db, 99),
		"099 (classification deferral backoff) must be rolled back first")
	require.EqualValues(t, 98, testkit.MigrateDownOne(t, db, 98),
		"098 (classification score alerts) must be rolled back first")
	require.EqualValues(t, 97, testkit.MigrateDownOne(t, db, 97),
		"097 (classification verdicts) must be the migration rolled back")
	require.False(t, relationExists("classification_verdicts"), "097's Down must drop classification_verdicts")
	require.True(t, relationExists("classification_results"), "097's Down must leave classification_results")

	testkit.MigrateUp(t, db)
	require.True(t, relationExists("classification_verdicts"), "097's Up must create classification_verdicts")
}

func TestClassificationMigration098_DownDropsScoreAlertTableAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	relationExists := func(table string) bool {
		t.Helper()
		var relation sql.NullString
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, table).Scan(&relation))
		return relation.Valid
	}

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),

		"100 (classification partial results) must be rolled back first")

	require.EqualValues(t, 99, testkit.MigrateDownOne(t, db, 99),
		"099 (classification deferral backoff) must be rolled back first")
	require.EqualValues(t, 98, testkit.MigrateDownOne(t, db, 98),
		"098 (classification score alerts) must be the migration rolled back")
	require.False(t, relationExists("classification_score_alerts"), "098's Down must drop classification_score_alerts")
	require.True(t, relationExists("classification_results"), "098's Down must leave classification_results")
	require.True(t, relationExists("classification_verdicts"), "098's Down must leave classification_verdicts")

	testkit.MigrateUp(t, db)
	require.True(t, relationExists("classification_score_alerts"), "098's Up must create classification_score_alerts")
}

func TestClassificationMigration099_DownDropsDeferralCountAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	columnExists := func() bool {
		t.Helper()
		var exists bool
		require.NoError(t, db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'classification_jobs' AND column_name = 'deferral_count'
			)`).Scan(&exists))
		return exists
	}
	require.True(t, columnExists(), "099's Up must add classification_jobs.deferral_count")

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),

		"100 (classification partial results) must be rolled back first")

	require.EqualValues(t, 99, testkit.MigrateDownOne(t, db, 99),
		"099 (classification deferral backoff) must be the migration rolled back")
	require.False(t, columnExists(), "099's Down must drop classification_jobs.deferral_count")

	testkit.MigrateUp(t, db)
	require.True(t, columnExists(), "099's Up must restore classification_jobs.deferral_count")
}

func TestClassificationMigration100_DownDropsPartialAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	columnExists := func() bool {
		t.Helper()
		var exists bool
		require.NoError(t, db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'classification_results' AND column_name = 'partial'
			)`).Scan(&exists))
		return exists
	}
	require.True(t, columnExists(), "100's Up must add classification_results.partial")

	require.EqualValues(t, 100, testkit.MigrateDownOne(t, db, 100),
		"100 (classification partial results) must be the migration rolled back")
	require.False(t, columnExists(), "100's Down must drop classification_results.partial")

	testkit.MigrateUp(t, db)
	require.True(t, columnExists(), "100's Up must restore classification_results.partial")
}
