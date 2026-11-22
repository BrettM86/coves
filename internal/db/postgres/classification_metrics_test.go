//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The classifier metrics read (docs/PRD_CONTENT_CLASSIFICATION.md, chunk 07
// Q3): job counts, usage, worker lag and undelivered operator messages,
// derived from the tables.

// classificationMetricsNow is the injected read time.
var classificationMetricsNow = time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)

// classificationMetricsEvaluation is the active evaluation the metrics read
// under; classificationMetricsOlderEvaluation is an earlier one the worker no
// longer claims.
var (
	classificationMetricsEvaluation      = classification.Evaluation{PolicyVersion: "v1", TextModelID: "@cf/test/model-a", ImageModelID: "@cf/test/image"}
	classificationMetricsOlderEvaluation = classification.Evaluation{PolicyVersion: "v0", TextModelID: "@cf/test/model-old", ImageModelID: "@cf/test/image"}
)

// insertMetricsJob inserts one job of label under the active evaluation in
// status with closeReason (empty for none) due at nextAttemptAt.
func insertMetricsJob(t *testing.T, db *sql.DB, label, status, closeReason string, nextAttemptAt time.Time) {
	t.Helper()
	insertMetricsJobUnder(t, db, classificationMetricsEvaluation, label, status, closeReason, nextAttemptAt)
}

// insertMetricsJobUnder inserts one job of label under evaluation in status
// with closeReason (empty for none) due at nextAttemptAt.
func insertMetricsJobUnder(t *testing.T, db *sql.DB, evaluation classification.Evaluation, label, status, closeReason string, nextAttemptAt time.Time) {
	t.Helper()
	var reason sql.NullString
	if closeReason != "" {
		reason = sql.NullString{String: closeReason, Valid: true}
	}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO classification_jobs
		    (subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id, status,
		     close_reason, author_did, community_did, next_attempt_at)
		VALUES ($1, 'post', $2, $3, $4, $5, $6, $7,
		        'did:plc:clsmetricsauthor', 'did:plc:clsmetricscommunity', $8)
	`, "at://did:plc:clsmetricsauthor/social.coves.community.postv2/"+label, "bafy"+label,
		evaluation.PolicyVersion, evaluation.TextModelID, evaluation.ImageModelID, status, reason, nextAttemptAt)
	require.NoErrorf(t, err, "inserting metrics job %s", label)
}

// TestClassificationMetricsReadsCountsUsageLagAndUndelivered (B8).
func TestClassificationMetricsReadsCountsUsageLagAndUndelivered(t *testing.T) {
	db := testkit.DB(t)
	ctx := t.Context()

	// Non-terminal jobs: three due (the retrying one 180s overdue, the oldest,
	// then queued jobs 90s and 30s overdue) and one not yet due.
	insertMetricsJob(t, db, "queuedlate", "queued", "", time.Date(2026, 11, 12, 17, 58, 30, 0, time.UTC))
	insertMetricsJob(t, db, "queuedrecent", "queued", "", time.Date(2026, 11, 12, 17, 59, 30, 0, time.UTC))
	insertMetricsJob(t, db, "deferred", "deferred", "", time.Date(2026, 11, 12, 18, 10, 0, 0, time.UTC))
	insertMetricsJob(t, db, "retrying", "retrying", "", time.Date(2026, 11, 12, 17, 57, 0, 0, time.UTC))
	// Non-terminal jobs left under an earlier evaluation, far overdue: the
	// worker never claims them, so they count toward neither the pending
	// counts nor the lag.
	insertMetricsJobUnder(t, db, classificationMetricsOlderEvaluation, "olderqueued", "queued", "", time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC))
	insertMetricsJobUnder(t, db, classificationMetricsOlderEvaluation, "olderdeferred", "deferred", "", time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC))
	insertMetricsJobUnder(t, db, classificationMetricsOlderEvaluation, "olderretrying", "retrying", "", time.Date(2026, 11, 12, 16, 30, 0, 0, time.UTC))
	// Terminal jobs, far overdue, which never count toward the lag.
	insertMetricsJob(t, db, "failed", "failed", "", time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	insertMetricsJob(t, db, "superseded", "closed", "superseded", time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	insertMetricsJob(t, db, "notvisible1", "closed", "not_visible", time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	insertMetricsJob(t, db, "notvisible2", "closed", "not_visible", time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	var resultIDs []int64
	for _, label := range []string{"metricsclassified1", "metricsclassified2"} {
		resultIDs = append(resultIDs, seedClassificationResult(t, db, classificationResultSeed{
			label:         label,
			kind:          classification.SubjectKindPost,
			subjectURI:    "at://did:plc:clsmetricsauthor/social.coves.community.postv2/" + label,
			scoredCID:     "bafy" + label,
			communityDID:  "did:plc:clsmetricscommunity",
			authorDID:     "did:plc:clsmetricsauthor",
			probabilities: map[string]float64{"sexual_minors": 0.9},
			createdAt:     time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC),
		}))
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO classification_usage (model_id, usage_date, requests, input_tokens) VALUES
		    ('@cf/test/model-a', '2026-11-12', 10, 1000),
		    ('@cf/test/model-a', '2026-11-11', 4, 400),
		    ('@cf/test/model-b', '2026-11-12', 3, 300),
		    ('@cf/test/model-b', '2026-11-11', 1, 100),
		    ('@cf/test/model-a', '2026-10-03', 2, 200)
	`)
	require.NoError(t, err)

	seedClassificationCommunity(t, db, "did:plc:clsmetricsflagged", "clsmetricsflagged.coves.social", "did:plc:clsmetricsowner",
		"Metrics flagged", "Metrics Flagged", "A community with flag events", "bafyclsmetricsflagged")
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_nsfw_flag_events
		    (community_did, old_nsfw, new_nsfw, occurred_at, delivered_at, attempts, last_error_code, next_attempt_at) VALUES
		    ('did:plc:clsmetricsflagged', false, true, '2026-11-12T16:00:00Z', '2026-11-12T16:00:05Z', 0, NULL, '2026-11-12T16:00:00Z'),
		    ('did:plc:clsmetricsflagged', true, false, '2026-11-12T17:00:00Z', NULL, 0, NULL, '2026-11-12T17:00:00Z'),
		    ('did:plc:clsmetricsflagged', false, true, '2026-11-12T17:30:00Z', NULL, 1, 'send_failed', '2026-11-12T19:00:00Z'),
		    ('did:plc:clsmetricsflagged', true, false, '2026-11-12T12:00:00Z', NULL, 3, 'send_failed', '2026-11-12T13:00:00Z')
	`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO classification_score_alerts
		    (result_id, question_key, subject_kind, created_at, delivered_at, attempts, last_error_code, next_attempt_at) VALUES
		    ($1, 'sexual_minors', 'post', '2026-11-12T15:00:00Z', '2026-11-12T15:00:01Z', 0, NULL, NULL),
		    ($1, 'spam_scam', 'post', '2026-11-12T15:00:00Z', NULL, 0, NULL, NULL),
		    ($2, 'sexual_minors', 'post', '2026-11-12T15:00:00Z', NULL, 1, 'send_failed', '2026-11-12T19:00:00Z'),
		    ($2, 'spam_scam', 'post', '2026-11-12T15:00:00Z', NULL, 8, 'send_failed', '2026-11-12T16:00:00Z')
	`, resultIDs[0], resultIDs[1])
	require.NoError(t, err)

	metrics, err := NewClassificationMetricsRepository(db).ReadMetrics(ctx, &classificationMetricsEvaluation, classificationMetricsNow)
	require.NoError(t, err)

	assert.Equal(t, classification.Metrics{
		Jobs: classification.JobCounts{
			Queued:           2,
			Deferred:         1,
			Retrying:         1,
			Sent:             2,
			Failed:           1,
			ClosedSuperseded: 1,
			ClosedNotVisible: 2,
		},
		Usage: []classification.UsageDay{
			{ModelID: "@cf/test/model-a", Day: "2026-11-12", Requests: 10, InputTokens: 1000},
			{ModelID: "@cf/test/model-b", Day: "2026-11-12", Requests: 3, InputTokens: 300},
			{ModelID: "@cf/test/model-a", Day: "2026-11-11", Requests: 4, InputTokens: 400},
			{ModelID: "@cf/test/model-b", Day: "2026-11-11", Requests: 1, InputTokens: 100},
			{ModelID: "@cf/test/model-a", Day: "2026-10-03", Requests: 2, InputTokens: 200},
		},
		WorkerLag:              180 * time.Second,
		UndeliveredFlagEvents:  3,
		UndeliveredScoreAlerts: 3,
	}, metrics)
}

// TestClassificationMetricsWorkerLagIsZeroWhenNothingIsDue: with every
// non-terminal job due in the future, the lag is 0, not negative.
func TestClassificationMetricsWorkerLagIsZeroWhenNothingIsDue(t *testing.T) {
	db := testkit.DB(t)

	insertMetricsJob(t, db, "futurequeued", "queued", "", time.Date(2026, 11, 12, 18, 5, 0, 0, time.UTC))
	insertMetricsJob(t, db, "futuredeferred", "deferred", "", time.Date(2026, 11, 12, 18, 10, 0, 0, time.UTC))
	insertMetricsJob(t, db, "futureretrying", "retrying", "", time.Date(2026, 11, 12, 18, 1, 0, 0, time.UTC))

	metrics, err := NewClassificationMetricsRepository(db).ReadMetrics(t.Context(), &classificationMetricsEvaluation, classificationMetricsNow)
	require.NoError(t, err)

	assert.Equal(t, classification.JobCounts{Queued: 1, Deferred: 1, Retrying: 1}, metrics.Jobs)
	assert.Equal(t, time.Duration(0), metrics.WorkerLag)
}

// TestClassificationMetricsWithoutEvaluationCoversEveryEvaluation: with the
// classifier disabled (no evaluation), the pending counts and the lag cover
// jobs under every evaluation.
func TestClassificationMetricsWithoutEvaluationCoversEveryEvaluation(t *testing.T) {
	db := testkit.DB(t)

	insertMetricsJob(t, db, "currentqueued", "queued", "", time.Date(2026, 11, 12, 17, 59, 0, 0, time.UTC))
	insertMetricsJobUnder(t, db, classificationMetricsOlderEvaluation, "olderqueued", "queued", "", time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC))
	insertMetricsJobUnder(t, db, classificationMetricsOlderEvaluation, "olderretrying", "retrying", "", time.Date(2026, 11, 12, 18, 30, 0, 0, time.UTC))

	metrics, err := NewClassificationMetricsRepository(db).ReadMetrics(t.Context(), nil, classificationMetricsNow)
	require.NoError(t, err)

	assert.Equal(t, classification.JobCounts{Queued: 2, Retrying: 1}, metrics.Jobs)
	assert.Equal(t, time.Hour, metrics.WorkerLag)
}
