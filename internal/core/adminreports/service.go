package adminreports

import (
	"context"
)

// service implements the Service interface for admin reports
type service struct {
	repo Repository
}

// NewService creates a new admin reports service
func NewService(repo Repository) Service {
	return &service{
		repo: repo,
	}
}

// SubmitReport validates the report request and creates a new report
// Returns the report ID on success
func (s *service) SubmitReport(ctx context.Context, req SubmitReportRequest) (*SubmitReportResult, error) {
	// Use the constructor which handles validation and target type determination
	report, err := NewReport(req)
	if err != nil {
		return nil, err
	}

	// Create the report in the database
	if err := s.repo.Create(ctx, report); err != nil {
		return nil, err
	}

	return &SubmitReportResult{
		ReportID: report.ID,
	}, nil
}
