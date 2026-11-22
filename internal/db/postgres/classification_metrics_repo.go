package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"Coves/internal/core/classification"
)

// ClassificationMetricsRepository reads the classifier metrics.
type ClassificationMetricsRepository struct {
	db *sql.DB
}

// NewClassificationMetricsRepository builds the metrics repository.
func NewClassificationMetricsRepository(db *sql.DB) *ClassificationMetricsRepository {
	return &ClassificationMetricsRepository{db: db}
}

// ReadMetrics reads the classifier metrics at now: job counts by status (a
// classified job counts as sent), every usage row (day descending, then
// model), the worker lag (now minus the oldest due non-terminal job's
// next_attempt_at, in whole seconds, zero when none is due) and the number of
// undelivered community flag events and score alerts.
//
// With evaluation set (the classifier is enabled), the queued, deferred and
// retrying counts and the worker lag cover only jobs under that evaluation,
// the ones the worker claims: a job left under an earlier policy or model is
// never claimed, so counting it would grow the lag without bound. With
// evaluation nil (the classifier is disabled), they cover every non-terminal
// job; no worker runs then, so the lag grows while any job is due. The
// terminal counts always cover every evaluation.
func (r *ClassificationMetricsRepository) ReadMetrics(ctx context.Context, evaluation *classification.Evaluation, now time.Time) (classification.Metrics, error) {
	var metrics classification.Metrics
	var lagSeconds int64
	var policyVersion, textModelID, imageModelID sql.NullString
	if evaluation != nil {
		policyVersion = sql.NullString{String: evaluation.PolicyVersion, Valid: true}
		textModelID = sql.NullString{String: evaluation.TextModelID, Valid: true}
		imageModelID = sql.NullString{String: evaluation.ImageModelID, Valid: true}
	}
	// $2 NULL selects every evaluation; otherwise only jobs under $2-$4.
	err := r.db.QueryRowContext(ctx, `
		WITH jobs AS (
		    SELECT status, close_reason, next_attempt_at,
		           ($2::text IS NULL OR (policy_version = $2 AND text_model_id = $3 AND image_model_id = $4)) AS active
		    FROM classification_jobs
		)
		SELECT
		    COUNT(*) FILTER (WHERE status = 'queued' AND active),
		    COUNT(*) FILTER (WHERE status = 'deferred' AND active),
		    COUNT(*) FILTER (WHERE status = 'retrying' AND active),
		    COUNT(*) FILTER (WHERE status = 'classified'),
		    COUNT(*) FILTER (WHERE status = 'failed'),
		    COUNT(*) FILTER (WHERE status = 'closed' AND close_reason = 'superseded'),
		    COUNT(*) FILTER (WHERE status = 'closed' AND close_reason = 'not_visible'),
		    COALESCE(FLOOR(EXTRACT(EPOCH FROM ($1::timestamptz - MIN(next_attempt_at)
		        FILTER (WHERE status IN ('queued', 'deferred', 'retrying') AND active AND next_attempt_at <= $1)))), 0)::BIGINT,
		    (SELECT COUNT(*) FROM community_nsfw_flag_events WHERE delivered_at IS NULL),
		    (SELECT COUNT(*) FROM classification_score_alerts WHERE delivered_at IS NULL)
		FROM jobs
	`, now, policyVersion, textModelID, imageModelID).Scan(
		&metrics.Jobs.Queued,
		&metrics.Jobs.Deferred,
		&metrics.Jobs.Retrying,
		&metrics.Jobs.Sent,
		&metrics.Jobs.Failed,
		&metrics.Jobs.ClosedSuperseded,
		&metrics.Jobs.ClosedNotVisible,
		&lagSeconds,
		&metrics.UndeliveredFlagEvents,
		&metrics.UndeliveredScoreAlerts,
	)
	if err != nil {
		return classification.Metrics{}, fmt.Errorf("read classification job metrics: %w", err)
	}
	metrics.WorkerLag = time.Duration(lagSeconds) * time.Second

	rows, err := r.db.QueryContext(ctx, `
		SELECT model_id, to_char(usage_date, 'YYYY-MM-DD'), requests, input_tokens
		FROM classification_usage
		ORDER BY usage_date DESC, model_id ASC
	`)
	if err != nil {
		return classification.Metrics{}, fmt.Errorf("read classification usage metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	metrics.Usage = []classification.UsageDay{}
	for rows.Next() {
		var day classification.UsageDay
		if err := rows.Scan(&day.ModelID, &day.Day, &day.Requests, &day.InputTokens); err != nil {
			return classification.Metrics{}, fmt.Errorf("scan classification usage metrics: %w", err)
		}
		metrics.Usage = append(metrics.Usage, day)
	}
	if err := rows.Err(); err != nil {
		return classification.Metrics{}, fmt.Errorf("iterate classification usage metrics: %w", err)
	}
	return metrics, nil
}
