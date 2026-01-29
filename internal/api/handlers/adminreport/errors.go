package adminreport

import (
	"Coves/internal/core/adminreports"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// errorResponse represents a standardized JSON error response
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON error response with the given status code
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(errorResponse{
		Error:   errorType,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service-layer errors to HTTP responses
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case adminreports.IsValidationError(err):
		// Map specific validation errors to appropriate messages
		switch {
		case errors.Is(err, adminreports.ErrInvalidReason):
			writeError(w, http.StatusBadRequest, "InvalidReason",
				"Invalid report reason. Must be one of: csam, doxing, harassment, spam, illegal, other")
		case errors.Is(err, adminreports.ErrInvalidStatus):
			writeError(w, http.StatusBadRequest, "InvalidStatus",
				"Invalid report status. Must be one of: open, reviewing, resolved, dismissed")
		case errors.Is(err, adminreports.ErrInvalidTarget):
			writeError(w, http.StatusBadRequest, "InvalidTarget",
				"Invalid target URI. Must be a valid AT Protocol URI starting with at://")
		case errors.Is(err, adminreports.ErrExplanationTooLong):
			writeError(w, http.StatusBadRequest, "ExplanationTooLong",
				"Explanation exceeds maximum length of 1000 characters")
		case errors.Is(err, adminreports.ErrReporterRequired):
			writeError(w, http.StatusBadRequest, "ReporterRequired",
				"Reporter DID is required")
		case errors.Is(err, adminreports.ErrInvalidTargetType):
			writeError(w, http.StatusBadRequest, "InvalidTargetType",
				"Invalid target type. Must be one of: post, comment")
		default:
			// SECURITY: Don't expose internal error messages to clients
			// Log the actual error for debugging, but return a generic message
			log.Printf("Unhandled validation error in admin report handler: %v", err)
			writeError(w, http.StatusBadRequest, "InvalidRequest",
				"The request contains invalid data")
		}

	case adminreports.IsNotFound(err):
		writeError(w, http.StatusNotFound, "NotFound", "Report not found")

	default:
		// SECURITY: Don't leak internal error details to clients
		log.Printf("Unexpected error in admin report handler: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
