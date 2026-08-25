package adminreport

import (
	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/adminreports"
	"encoding/json"
	"log"
	"net/http"
)

// SubmitHandler handles report submission requests
type SubmitHandler struct {
	service adminreports.Service
}

// NewSubmitHandler creates a new handler for submitting admin reports
func NewSubmitHandler(service adminreports.Service) *SubmitHandler {
	return &SubmitHandler{
		service: service,
	}
}

// SubmitReportInput matches the lexicon input schema for social.coves.admin.submitReport
type SubmitReportInput struct {
	TargetURI   string `json:"targetUri"`
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
}

// SubmitReportOutput matches the lexicon output schema
type SubmitReportOutput struct {
	Success  bool  `json:"success"`
	ReportID int64 `json:"reportId"`
}

// HandleSubmit handles report submission requests
// POST /xrpc/social.coves.admin.submitReport
//
// Request body: { "targetUri": "at://...", "reason": "csam", "explanation": "..." }
// Response: { "success": true, "reportId": 123 }
func (h *SubmitHandler) HandleSubmit(w http.ResponseWriter, r *http.Request) {
	// 1. Check method is POST
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2+3. Parse JSON body into SubmitReportInput under the small tier
	var input SubmitReportInput
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &input) {
		return
	}

	// 4. Get user DID from context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// 5. Convert input to SubmitReportRequest
	req := adminreports.SubmitReportRequest{
		ReporterDID: userDID,
		TargetURI:   input.TargetURI,
		Reason:      input.Reason,
		Explanation: input.Explanation,
	}

	// 6. Call service to submit report
	result, err := h.service.SubmitReport(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// 7. Return JSON response indicating success with report ID
	output := SubmitReportOutput{
		Success:  true,
		ReportID: result.ReportID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(output); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
