package postgres

import (
	"Coves/internal/core/adminreports"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"
)

type postgresAdminReportRepo struct {
	db *sql.DB
}

// NewAdminReportRepository creates a new PostgreSQL admin report repository
func NewAdminReportRepository(db *sql.DB) adminreports.Repository {
	return &postgresAdminReportRepo{db: db}
}

// Create inserts a new admin report into the database
// Returns the created report with ID and CreatedAt populated
//
// KNOWN DEFECT (issue 2026-07-31-repo-minor-pins-batch.md, item 7): the branch below that
// maps a "valid_target_type" violation to ErrInvalidTargetType is dead code — migration
// 028 declares no such constraint and target_type is bare TEXT. Masked today because
// adminreports.NewReport derives the value from the reported URI rather than taking it
// from the client. (see TestAdminReportRepo_TargetTypeIsUnconstrained)
func (r *postgresAdminReportRepo) Create(ctx context.Context, report *adminreports.Report) error {
	query := `
		INSERT INTO admin_reports (
			reporter_did, target_uri, target_type,
			reason, explanation, status
		) VALUES (
			$1, $2, $3,
			$4, $5, $6
		)
		RETURNING id, created_at
	`

	// Default status to 'open' if not set
	status := report.Status
	if status == "" {
		status = adminreports.StatusOpen
	}

	// Handle empty explanation as NULL
	var explanation *string
	if report.Explanation != "" {
		explanation = &report.Explanation
	}

	err := r.db.QueryRowContext(
		ctx, query,
		report.ReporterDID, report.TargetURI, string(report.TargetType),
		string(report.Reason), explanation, string(status),
	).Scan(&report.ID, &report.CreatedAt)

	if err != nil {
		// Check for constraint violations using pq.Error type
		if pqErr := extractPQError(err); pqErr != nil {
			if strings.Contains(pqErr.Constraint, "valid_reason") {
				return adminreports.ErrInvalidReason
			}
			if strings.Contains(pqErr.Constraint, "valid_status") {
				return adminreports.ErrInvalidStatus
			}
			if strings.Contains(pqErr.Constraint, "valid_target_type") {
				return adminreports.ErrInvalidTargetType
			}
		}
		return fmt.Errorf("failed to create admin report: %w", err)
	}

	report.Status = status
	return nil
}

// ListByStatus retrieves reports filtered by status with pagination
// Results are ordered by created_at DESC (newest first)
func (r *postgresAdminReportRepo) ListByStatus(ctx context.Context, status string, limit, offset int) ([]*adminreports.Report, error) {
	query := `
		SELECT
			id, reporter_did, target_uri, target_type,
			reason, explanation, status,
			resolved_by, resolution_notes,
			created_at, resolved_at
		FROM admin_reports
		WHERE status = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list admin reports by status: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close rows in ListByStatus",
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	var reports []*adminreports.Report
	for rows.Next() {
		report, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating admin reports: %w", err)
	}

	return reports, nil
}

// UpdateStatus updates a report's status and resolution details
// Sets resolved_by, resolution_notes, and resolved_at when resolving or dismissing
//
// KNOWN DEFECT (issue 2026-07-31-admin-report-reopen-keeps-stale-resolution.md): the non-terminal branch
// touches status alone, so resolved_by, resolution_notes and resolved_at SURVIVE a move
// back to "open". The row then reads as an open report resolved by someone at a specific
// time, and any time-to-resolution metric counts it as closed.
// (see TestAdminReportRepo_ReopeningKeepsTheStaleResolution)
func (r *postgresAdminReportRepo) UpdateStatus(ctx context.Context, id int64, status, resolvedBy, notes string) error {
	var query string
	var args []interface{}

	// When resolving or dismissing, set resolved_at and resolution fields
	if status == string(adminreports.StatusResolved) || status == string(adminreports.StatusDismissed) {
		query = `
			UPDATE admin_reports
			SET status = $1,
				resolved_by = $2,
				resolution_notes = $3,
				resolved_at = $4
			WHERE id = $5
		`
		args = []interface{}{status, resolvedBy, notes, time.Now(), id}
	} else {
		// For other status changes (e.g., open -> reviewing), don't set resolution fields
		query = `
			UPDATE admin_reports
			SET status = $1
			WHERE id = $2
		`
		args = []interface{}{status, id}
	}

	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		// Check for constraint violations using pq.Error type
		if pqErr := extractPQError(err); pqErr != nil {
			if strings.Contains(pqErr.Constraint, "valid_status") {
				return adminreports.ErrInvalidStatus
			}
			if strings.Contains(pqErr.Constraint, "valid_reason") {
				return adminreports.ErrInvalidReason
			}
		}
		return fmt.Errorf("failed to update admin report status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	if rowsAffected == 0 {
		return adminreports.ErrReportNotFound
	}

	return nil
}

// scanReport scans a single report from a database row
func scanReport(rows *sql.Rows) (*adminreports.Report, error) {
	var report adminreports.Report
	var targetType, reason, status string
	var explanation sql.NullString
	var resolvedBy sql.NullString
	var resolutionNotes sql.NullString
	var resolvedAt sql.NullTime

	err := rows.Scan(
		&report.ID, &report.ReporterDID, &report.TargetURI, &targetType,
		&reason, &explanation, &status,
		&resolvedBy, &resolutionNotes,
		&report.CreatedAt, &resolvedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan admin report: %w", err)
	}

	// Convert string values to typed enums
	report.TargetType = adminreports.TargetType(targetType)
	report.Reason = adminreports.Reason(reason)
	report.Status = adminreports.Status(status)

	// Convert nullable fields
	if explanation.Valid {
		report.Explanation = explanation.String
	}
	if resolvedBy.Valid {
		report.ResolvedBy = &resolvedBy.String
	}
	if resolutionNotes.Valid {
		report.ResolutionNotes = &resolutionNotes.String
	}
	if resolvedAt.Valid {
		report.ResolvedAt = &resolvedAt.Time
	}

	return &report, nil
}

// extractPQError extracts a pq.Error from an error if present
// Returns nil if the error is not a pq.Error
func extractPQError(err error) *pq.Error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr
	}
	return nil
}
