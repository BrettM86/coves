package adminreport

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/adminreports"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockService implements adminreports.Service for testing
type mockService struct {
	submitReportFunc func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error)
}

func (m *mockService) SubmitReport(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
	if m.submitReportFunc != nil {
		return m.submitReportFunc(ctx, req)
	}
	return &adminreports.SubmitReportResult{ReportID: 1}, nil
}

func TestHandleSubmit_Success(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			if req.ReporterDID != "did:plc:testuser123" {
				t.Errorf("expected ReporterDID %q, got %q", "did:plc:testuser123", req.ReporterDID)
			}
			if req.TargetURI != "at://did:plc:author123/social.coves.post/abc123" {
				t.Errorf("expected TargetURI %q, got %q", "at://did:plc:author123/social.coves.post/abc123", req.TargetURI)
			}
			if req.Reason != "spam" {
				t.Errorf("expected Reason %q, got %q", "spam", req.Reason)
			}
			if req.Explanation != "This is spam" {
				t.Errorf("expected Explanation %q, got %q", "This is spam", req.Explanation)
			}
			return &adminreports.SubmitReportResult{ReportID: 42}, nil
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: "This is spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var output SubmitReportOutput
	if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if !output.Success {
		t.Error("expected Success to be true")
	}
	if output.ReportID != 42 {
		t.Errorf("expected ReportID 42, got %d", output.ReportID)
	}
}

func TestHandleSubmit_MethodNotAllowed(t *testing.T) {
	handler := NewSubmitHandler(&mockService{})

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/xrpc/social.coves.admin.submitReport", nil)
			req = setTestUserDID(req, "did:plc:testuser123")

			w := httptest.NewRecorder()
			handler.HandleSubmit(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
			}
		})
	}
}

func TestHandleSubmit_Unauthenticated(t *testing.T) {
	handler := NewSubmitHandler(&mockService{})

	input := SubmitReportInput{
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: "This is spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No auth context - simulates unauthenticated request

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}

	if !strings.Contains(w.Body.String(), "AuthRequired") {
		t.Errorf("expected AuthRequired error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InvalidJSON(t *testing.T) {
	handler := NewSubmitHandler(&mockService{})

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", strings.NewReader("not valid json"))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InvalidRequest") {
		t.Errorf("expected InvalidRequest error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InvalidReason(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrInvalidReason
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "invalid_reason",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InvalidReason") {
		t.Errorf("expected InvalidReason error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InvalidTarget(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrInvalidTarget
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "https://example.com",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InvalidTarget") {
		t.Errorf("expected InvalidTarget error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_ExplanationTooLong(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrExplanationTooLong
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: strings.Repeat("a", 1001),
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "ExplanationTooLong") {
		t.Errorf("expected ExplanationTooLong error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InvalidStatus(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrInvalidStatus
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InvalidStatus") {
		t.Errorf("expected InvalidStatus error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InvalidTargetType(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrInvalidTargetType
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InvalidTargetType") {
		t.Errorf("expected InvalidTargetType error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_NotFound(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, adminreports.ErrReportNotFound
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}

	if !strings.Contains(w.Body.String(), "NotFound") {
		t.Errorf("expected NotFound error, got %s", w.Body.String())
	}
}

func TestHandleSubmit_InternalError(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			return nil, errors.New("database connection failed")
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}

	if !strings.Contains(w.Body.String(), "InternalServerError") {
		t.Errorf("expected InternalServerError error, got %s", w.Body.String())
	}

	// SECURITY: Verify that the actual error message is not leaked
	if strings.Contains(w.Body.String(), "database") {
		t.Error("internal error details should not be exposed to client")
	}
}

func TestHandleSubmit_EmptyExplanation(t *testing.T) {
	svc := &mockService{
		submitReportFunc: func(ctx context.Context, req adminreports.SubmitReportRequest) (*adminreports.SubmitReportResult, error) {
			if req.Explanation != "" {
				t.Errorf("expected empty Explanation, got %q", req.Explanation)
			}
			return &adminreports.SubmitReportResult{ReportID: 1}, nil
		},
	}
	handler := NewSubmitHandler(svc)

	input := SubmitReportInput{
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      "spam",
		Explanation: "",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHandleSubmit_ContentTypeHeader(t *testing.T) {
	handler := NewSubmitHandler(&mockService{})

	input := SubmitReportInput{
		TargetURI: "at://did:plc:author123/social.coves.post/abc123",
		Reason:    "spam",
	}
	body, _ := json.Marshal(input)

	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.admin.submitReport", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = setTestUserDID(req, "did:plc:testuser123")

	w := httptest.NewRecorder()
	handler.HandleSubmit(w, req)

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type %q, got %q", "application/json", contentType)
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "TestError", "Test message")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	var resp errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if resp.Error != "TestError" {
		t.Errorf("expected error %q, got %q", "TestError", resp.Error)
	}
	if resp.Message != "Test message" {
		t.Errorf("expected message %q, got %q", "Test message", resp.Message)
	}
}

func TestHandleServiceError_AllValidationErrors(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "ErrInvalidReason",
			err:            adminreports.ErrInvalidReason,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidReason",
		},
		{
			name:           "ErrInvalidStatus",
			err:            adminreports.ErrInvalidStatus,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidStatus",
		},
		{
			name:           "ErrInvalidTarget",
			err:            adminreports.ErrInvalidTarget,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidTarget",
		},
		{
			name:           "ErrExplanationTooLong",
			err:            adminreports.ErrExplanationTooLong,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "ExplanationTooLong",
		},
		{
			name:           "ErrReporterRequired",
			err:            adminreports.ErrReporterRequired,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "ReporterRequired",
		},
		{
			name:           "ErrInvalidTargetType",
			err:            adminreports.ErrInvalidTargetType,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "InvalidTargetType",
		},
		{
			name:           "ErrReportNotFound",
			err:            adminreports.ErrReportNotFound,
			expectedStatus: http.StatusNotFound,
			expectedError:  "NotFound",
		},
		{
			name:           "internal error",
			err:            errors.New("some internal error"),
			expectedStatus: http.StatusInternalServerError,
			expectedError:  "InternalServerError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handleServiceError(w, tt.err)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			var resp errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal error response: %v", err)
			}

			if resp.Error != tt.expectedError {
				t.Errorf("expected error %q, got %q", tt.expectedError, resp.Error)
			}
		})
	}
}

// setTestUserDID sets the user DID in the context for testing
func setTestUserDID(req *http.Request, userDID string) *http.Request {
	ctx := middleware.SetTestUserDID(req.Context(), userDID)
	return req.WithContext(ctx)
}
