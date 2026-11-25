package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/classification"
)

// ClassificationScoreAlertRepository stores the delivery state of
// classification_score_alerts. The classification repository inserts the
// alerts with their result (StoreResult).
type ClassificationScoreAlertRepository struct {
	db *sql.DB
}

// NewClassificationScoreAlertRepository builds the score alert repository.
func NewClassificationScoreAlertRepository(db *sql.DB) *ClassificationScoreAlertRepository {
	return &ClassificationScoreAlertRepository{db: db}
}

var _ classification.ScoreAlertRepository = (*ClassificationScoreAlertRepository)(nil)

// claimDueScoreAlertQuery claims the oldest due alert in one statement: the
// due CTE locks the row it selects (skipping rows another claim holds), and
// the claimed CTE moves its next_attempt_at to the lease expiry so no other
// claim sees it as due. A NULL next_attempt_at is a new alert, due at once.
// Parameters: $1 now, $2 lease expiry, $3 attempt limit, $4 job ID (NULL for
// every job). The comment join supplies a comment alert's root post URI for
// its web link; it is a LEFT JOIN so an alert whose comment row is gone still
// claims, with an empty root URI.
const claimDueScoreAlertQuery = `
	WITH due AS (
		SELECT a.id
		FROM classification_score_alerts a
		JOIN classification_results r ON r.id = a.result_id
		WHERE a.delivered_at IS NULL AND a.attempts < $3
		  AND (a.next_attempt_at IS NULL OR a.next_attempt_at <= $1)
		  AND ($4::bigint IS NULL OR r.job_id = $4)
		ORDER BY a.next_attempt_at NULLS FIRST, a.id
		LIMIT 1
		FOR UPDATE OF a SKIP LOCKED
	), claimed AS (
		UPDATE classification_score_alerts a
		SET next_attempt_at = $2
		FROM due
		WHERE a.id = due.id
		RETURNING a.id, a.result_id, a.question_key, a.subject_kind, a.attempts
	)
	SELECT c.id, c.result_id, r.job_id, c.question_key, c.subject_kind, c.attempts,
	       r.subject_uri, r.community_did, COALESCE(cm.root_uri, '')
	FROM claimed c
	JOIN classification_results r ON r.id = c.result_id
	LEFT JOIN comments cm ON c.subject_kind = 'comment' AND cm.uri = r.subject_uri`

// ClaimDueAlert claims one due alert; jobID 0 means any job.
func (r *ClassificationScoreAlertRepository) ClaimDueAlert(ctx context.Context, jobID int64, now, leaseUntil time.Time, attemptLimit int) (classification.ScoreAlert, bool, error) {
	var job any
	if jobID != 0 {
		job = jobID
	}
	var alert classification.ScoreAlert
	var subjectKind string
	err := r.db.QueryRowContext(ctx, claimDueScoreAlertQuery, now, leaseUntil, attemptLimit, job).Scan(
		&alert.ID, &alert.ResultID, &alert.JobID, &alert.QuestionKey, &subjectKind, &alert.Attempts,
		&alert.SubjectURI, &alert.CommunityDID, &alert.RootURI)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.ScoreAlert{}, false, nil
	}
	if err != nil {
		return classification.ScoreAlert{}, false, fmt.Errorf("failed to claim classification score alert: %w", err)
	}
	alert.SubjectKind = classification.SubjectKind(subjectKind)
	return alert, true, nil
}

// MarkAlertDelivered and MarkAlertFailed match the claim's lease, so a caller
// whose lease expired and was reclaimed cannot overwrite the newer claim.
func (r *ClassificationScoreAlertRepository) MarkAlertDelivered(ctx context.Context, id int64, lease, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE classification_score_alerts SET delivered_at = $3
		WHERE id = $1 AND next_attempt_at = $2 AND delivered_at IS NULL`, id, lease, at)
	if err != nil {
		return fmt.Errorf("failed to mark classification score alert delivered: %w", err)
	}
	return scoreAlertLeaseHeld(result)
}

func (r *ClassificationScoreAlertRepository) MarkAlertFailed(ctx context.Context, id int64, lease time.Time, errorCode string, nextAttemptAt time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE classification_score_alerts
		SET attempts = attempts + 1, last_error_code = $3, next_attempt_at = $4
		WHERE id = $1 AND next_attempt_at = $2 AND delivered_at IS NULL`, id, lease, errorCode, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("failed to mark classification score alert failed: %w", err)
	}
	return scoreAlertLeaseHeld(result)
}

func scoreAlertLeaseHeld(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read classification score alert update count: %w", err)
	}
	if rows == 0 {
		return classification.ErrScoreAlertLeaseLost
	}
	return nil
}
