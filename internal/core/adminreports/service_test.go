package adminreports

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockRepository implements Repository for testing
type mockRepository struct {
	createFunc       func(ctx context.Context, report *Report) error
	listByStatusFunc func(ctx context.Context, status string, limit, offset int) ([]*Report, error)
	updateStatusFunc func(ctx context.Context, id int64, status, resolvedBy, notes string) error
	createdReports   []*Report
}

func (m *mockRepository) Create(ctx context.Context, report *Report) error {
	if m.createFunc != nil {
		return m.createFunc(ctx, report)
	}
	// Default behavior: assign ID and store report
	report.ID = int64(len(m.createdReports) + 1)
	m.createdReports = append(m.createdReports, report)
	return nil
}

func (m *mockRepository) ListByStatus(ctx context.Context, status string, limit, offset int) ([]*Report, error) {
	if m.listByStatusFunc != nil {
		return m.listByStatusFunc(ctx, status, limit, offset)
	}
	return []*Report{}, nil
}

func (m *mockRepository) UpdateStatus(ctx context.Context, id int64, status, resolvedBy, notes string) error {
	if m.updateStatusFunc != nil {
		return m.updateStatusFunc(ctx, id, status, resolvedBy, notes)
	}
	return nil
}

func TestSubmitReport_Success(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: "This is spam content",
	}

	result, err := svc.SubmitReport(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("expected result, got nil")
	}

	if result.ReportID != 1 {
		t.Errorf("expected ReportID 1, got %d", result.ReportID)
	}

	if len(repo.createdReports) != 1 {
		t.Fatalf("expected 1 created report, got %d", len(repo.createdReports))
	}

	created := repo.createdReports[0]
	if created.ReporterDID != req.ReporterDID {
		t.Errorf("expected ReporterDID %q, got %q", req.ReporterDID, created.ReporterDID)
	}
	if created.TargetURI != req.TargetURI {
		t.Errorf("expected TargetURI %q, got %q", req.TargetURI, created.TargetURI)
	}
	if created.Reason != Reason(req.Reason) {
		t.Errorf("expected Reason %q, got %q", req.Reason, created.Reason)
	}
	if created.Explanation != req.Explanation {
		t.Errorf("expected Explanation %q, got %q", req.Explanation, created.Explanation)
	}
	if created.Status != StatusOpen {
		t.Errorf("expected Status %q, got %q", StatusOpen, created.Status)
	}
	if created.TargetType != TargetTypePost {
		t.Errorf("expected TargetType %q, got %q", TargetTypePost, created.TargetType)
	}
}

func TestSubmitReport_CommentTargetType(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.comment/xyz789",
		Reason:      "harassment",
		Explanation: "Harassing comment",
	}

	result, err := svc.SubmitReport(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("expected result, got nil")
	}

	created := repo.createdReports[0]
	if created.TargetType != TargetTypeComment {
		t.Errorf("expected TargetType %q, got %q", TargetTypeComment, created.TargetType)
	}
}

func TestSubmitReport_ValidationErrors(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	tests := []struct {
		name        string
		req         SubmitReportRequest
		expectedErr error
	}{
		{
			name: "missing reporter DID",
			req: SubmitReportRequest{
				ReporterDID: "",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "spam",
			},
			expectedErr: ErrReporterRequired,
		},
		{
			name: "invalid reason",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "invalid_reason",
			},
			expectedErr: ErrInvalidReason,
		},
		{
			name: "missing target URI prefix",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "https://example.com/post/123",
				Reason:      "spam",
			},
			expectedErr: ErrInvalidTarget,
		},
		{
			name: "incomplete AT URI - only prefix",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://",
				Reason:      "spam",
			},
			expectedErr: ErrInvalidTarget,
		},
		{
			name: "malformed AT URI - missing collection",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123",
				Reason:      "spam",
			},
			expectedErr: ErrInvalidTarget,
		},
		{
			name: "malformed AT URI - missing rkey",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post",
				Reason:      "spam",
			},
			expectedErr: ErrInvalidTarget,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.SubmitReport(context.Background(), tt.req)
			if !errors.Is(err, tt.expectedErr) {
				t.Errorf("expected error %v, got %v", tt.expectedErr, err)
			}
		})
	}
}

func TestSubmitReport_ExplanationTooLong(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	// Create explanation longer than 1000 characters
	longExplanation := strings.Repeat("a", MaxExplanationLength+1)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: longExplanation,
	}

	_, err := svc.SubmitReport(context.Background(), req)
	if !errors.Is(err, ErrExplanationTooLong) {
		t.Errorf("expected ErrExplanationTooLong, got %v", err)
	}
}

func TestSubmitReport_ExplanationExactlyAtLimit(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	// Create explanation at exactly 1000 characters
	exactExplanation := strings.Repeat("a", MaxExplanationLength)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: exactExplanation,
	}

	_, err := svc.SubmitReport(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for explanation at limit, got: %v", err)
	}
}

func TestSubmitReport_ExplanationWithMultibyteCharacters(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	// Create explanation with 1001 multibyte characters (should fail)
	// Each emoji is 1 character but multiple bytes
	multibyteExplanation := strings.Repeat("🔥", MaxExplanationLength+1)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: multibyteExplanation,
	}

	_, err := svc.SubmitReport(context.Background(), req)
	if !errors.Is(err, ErrExplanationTooLong) {
		t.Errorf("expected ErrExplanationTooLong for multibyte characters exceeding limit, got %v", err)
	}
}

func TestSubmitReport_ExplanationWithMultibyteCharactersAtLimit(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	// Create explanation with exactly 1000 multibyte characters (should pass)
	multibyteExplanation := strings.Repeat("🔥", MaxExplanationLength)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: multibyteExplanation,
	}

	_, err := svc.SubmitReport(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for multibyte explanation at limit, got: %v", err)
	}
}

func TestSubmitReport_RepositoryError(t *testing.T) {
	expectedErr := errors.New("database connection failed")
	repo := &mockRepository{
		createFunc: func(ctx context.Context, report *Report) error {
			return expectedErr
		},
	}
	svc := NewService(repo)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
	}

	_, err := svc.SubmitReport(context.Background(), req)
	if !errors.Is(err, expectedErr) {
		t.Errorf("expected repository error, got %v", err)
	}
}

func TestSubmitReport_AllValidReasons(t *testing.T) {
	validReasons := []string{"csam", "doxing", "harassment", "spam", "illegal", "other"}

	for _, reason := range validReasons {
		t.Run("reason_"+reason, func(t *testing.T) {
			repo := &mockRepository{}
			svc := NewService(repo)

			req := SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      reason,
			}

			result, err := svc.SubmitReport(context.Background(), req)
			if err != nil {
				t.Fatalf("expected no error for reason %q, got: %v", reason, err)
			}
			if result == nil {
				t.Fatalf("expected result for reason %q, got nil", reason)
			}
		})
	}
}

func TestSubmitReport_DidWebURI(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:web:example.com/social.coves.post/abc123",
		Reason:      "spam",
	}

	result, err := svc.SubmitReport(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for did:web URI, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
}

func TestIsValidReason(t *testing.T) {
	tests := []struct {
		reason   string
		expected bool
	}{
		{"csam", true},
		{"doxing", true},
		{"harassment", true},
		{"spam", true},
		{"illegal", true},
		{"other", true},
		{"invalid", false},
		{"CSAM", false}, // case-sensitive
		{"Spam", false}, // case-sensitive
		{"", false},
		{" spam", false}, // with space
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			if got := IsValidReason(tt.reason); got != tt.expected {
				t.Errorf("IsValidReason(%q) = %v, want %v", tt.reason, got, tt.expected)
			}
		})
	}
}

func TestIsValidStatus(t *testing.T) {
	tests := []struct {
		status   string
		expected bool
	}{
		{"open", true},
		{"reviewing", true},
		{"resolved", true},
		{"dismissed", true},
		{"invalid", false},
		{"OPEN", false},     // case-sensitive
		{"Resolved", false}, // case-sensitive
		{"", false},
		{" open", false}, // with space
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := IsValidStatus(tt.status); got != tt.expected {
				t.Errorf("IsValidStatus(%q) = %v, want %v", tt.status, got, tt.expected)
			}
		})
	}
}

func TestIsValidTargetType(t *testing.T) {
	tests := []struct {
		targetType string
		expected   bool
	}{
		{"post", true},
		{"comment", true},
		{"invalid", false},
		{"POST", false},    // case-sensitive
		{"Comment", false}, // case-sensitive
		{"", false},
		{" post", false}, // with space
	}

	for _, tt := range tests {
		t.Run(tt.targetType, func(t *testing.T) {
			if got := IsValidTargetType(tt.targetType); got != tt.expected {
				t.Errorf("IsValidTargetType(%q) = %v, want %v", tt.targetType, got, tt.expected)
			}
		})
	}
}

func TestNewReport_Success(t *testing.T) {
	req := SubmitReportRequest{
		ReporterDID: "did:plc:testuser123",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: "This is spam",
	}

	report, err := NewReport(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if report.ReporterDID != req.ReporterDID {
		t.Errorf("expected ReporterDID %q, got %q", req.ReporterDID, report.ReporterDID)
	}
	if report.TargetURI != req.TargetURI {
		t.Errorf("expected TargetURI %q, got %q", req.TargetURI, report.TargetURI)
	}
	if report.Reason != Reason(req.Reason) {
		t.Errorf("expected Reason %q, got %q", req.Reason, report.Reason)
	}
	if report.Explanation != req.Explanation {
		t.Errorf("expected Explanation %q, got %q", req.Explanation, report.Explanation)
	}
	if report.Status != StatusOpen {
		t.Errorf("expected Status %q, got %q", StatusOpen, report.Status)
	}
	if report.TargetType != TargetTypePost {
		t.Errorf("expected TargetType %q, got %q", TargetTypePost, report.TargetType)
	}
	if report.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestNewReport_ValidationError(t *testing.T) {
	req := SubmitReportRequest{
		ReporterDID: "",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
	}

	_, err := NewReport(req)
	if !errors.Is(err, ErrReporterRequired) {
		t.Errorf("expected ErrReporterRequired, got %v", err)
	}
}

func TestSubmitReportRequest_Validate(t *testing.T) {
	tests := []struct {
		name        string
		req         SubmitReportRequest
		expectedErr error
	}{
		{
			name: "valid request",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "spam",
			},
			expectedErr: nil,
		},
		{
			name: "empty reporter",
			req: SubmitReportRequest{
				ReporterDID: "",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "spam",
			},
			expectedErr: ErrReporterRequired,
		},
		{
			name: "invalid reason",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "bad_reason",
			},
			expectedErr: ErrInvalidReason,
		},
		{
			name: "invalid target URI",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "https://example.com",
				Reason:      "spam",
			},
			expectedErr: ErrInvalidTarget,
		},
		{
			name: "explanation too long",
			req: SubmitReportRequest{
				ReporterDID: "did:plc:testuser123",
				TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
				Reason:      "spam",
				Explanation: strings.Repeat("x", MaxExplanationLength+1),
			},
			expectedErr: ErrExplanationTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.expectedErr == nil {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if !errors.Is(err, tt.expectedErr) {
					t.Errorf("expected error %v, got %v", tt.expectedErr, err)
				}
			}
		})
	}
}

func TestValidReasons(t *testing.T) {
	reasons := ValidReasons()
	expected := []Reason{ReasonCSAM, ReasonDoxing, ReasonHarassment, ReasonSpam, ReasonIllegal, ReasonOther}

	if len(reasons) != len(expected) {
		t.Fatalf("expected %d reasons, got %d", len(expected), len(reasons))
	}

	for i, r := range expected {
		if reasons[i] != r {
			t.Errorf("expected reason[%d] = %q, got %q", i, r, reasons[i])
		}
	}
}

func TestValidStatuses(t *testing.T) {
	statuses := ValidStatuses()
	expected := []Status{StatusOpen, StatusReviewing, StatusResolved, StatusDismissed}

	if len(statuses) != len(expected) {
		t.Fatalf("expected %d statuses, got %d", len(expected), len(statuses))
	}

	for i, s := range expected {
		if statuses[i] != s {
			t.Errorf("expected status[%d] = %q, got %q", i, s, statuses[i])
		}
	}
}

func TestValidTargetTypes(t *testing.T) {
	types := ValidTargetTypes()
	expected := []TargetType{TargetTypePost, TargetTypeComment}

	if len(types) != len(expected) {
		t.Fatalf("expected %d target types, got %d", len(expected), len(types))
	}

	for i, tt := range expected {
		if types[i] != tt {
			t.Errorf("expected targetType[%d] = %q, got %q", i, tt, types[i])
		}
	}
}

func TestIsValidationError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"ErrInvalidReason", ErrInvalidReason, true},
		{"ErrInvalidStatus", ErrInvalidStatus, true},
		{"ErrInvalidTarget", ErrInvalidTarget, true},
		{"ErrExplanationTooLong", ErrExplanationTooLong, true},
		{"ErrReporterRequired", ErrReporterRequired, true},
		{"ErrInvalidTargetType", ErrInvalidTargetType, true},
		{"ErrReportNotFound", ErrReportNotFound, false},
		{"generic error", errors.New("some error"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidationError(tt.err); got != tt.expected {
				t.Errorf("IsValidationError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"ErrReportNotFound", ErrReportNotFound, true},
		{"ErrInvalidReason", ErrInvalidReason, false},
		{"generic error", errors.New("some error"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.expected {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
