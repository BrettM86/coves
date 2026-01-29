package adminreports

import "context"

// Repository defines the data access layer for admin reports
type Repository interface {
	// Create stores a new report in the database
	// Returns the report with ID populated after successful creation
	Create(ctx context.Context, report *Report) error

	// ListByStatus returns reports filtered by status with pagination
	ListByStatus(ctx context.Context, status string, limit, offset int) ([]*Report, error)

	// UpdateStatus updates a report's status and resolution details
	UpdateStatus(ctx context.Context, id int64, status, resolvedBy, notes string) error
}

// SubmitReportResult contains the result of submitting a report
type SubmitReportResult struct {
	// ReportID is the ID of the created report
	ReportID int64
}

// Service defines the business logic layer for admin reports
type Service interface {
	// SubmitReport validates and creates a new report
	// Returns the report ID on success
	SubmitReport(ctx context.Context, req SubmitReportRequest) (*SubmitReportResult, error)
}
