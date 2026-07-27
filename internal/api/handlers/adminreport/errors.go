package adminreport

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/adminreports"
)

// errorMapper maps admin report service errors to XRPC responses.
//
// Each validation sentinel gets a static message: the enumerated values in
// these are the useful part, and spelling them out beats echoing the error.
var errorMapper = xrpc.NewMapper("adminreport",
	xrpc.Sentinel(adminreports.ErrInvalidReason, http.StatusBadRequest, "InvalidReason",
		"Invalid report reason. Must be one of: csam, doxing, harassment, spam, illegal, other"),
	xrpc.Sentinel(adminreports.ErrInvalidStatus, http.StatusBadRequest, "InvalidStatus",
		"Invalid report status. Must be one of: open, reviewing, resolved, dismissed"),
	xrpc.Sentinel(adminreports.ErrInvalidTarget, http.StatusBadRequest, "InvalidTarget",
		"Invalid target URI. Must be a valid AT Protocol URI starting with at://"),
	xrpc.Sentinel(adminreports.ErrExplanationTooLong, http.StatusBadRequest, "ExplanationTooLong",
		"Explanation exceeds maximum length of 1000 characters"),
	xrpc.Sentinel(adminreports.ErrReporterRequired, http.StatusBadRequest, "ReporterRequired",
		"Reporter DID is required"),
	xrpc.Sentinel(adminreports.ErrInvalidTargetType, http.StatusBadRequest, "InvalidTargetType",
		"Invalid target type. Must be one of: post, comment"),

	xrpc.Match(adminreports.IsNotFound, http.StatusNotFound,
		"NotFound", "Report not found"),

	// Catch-all so a validation sentinel added to the domain without a rule
	// above still answers 400 rather than 500.
	xrpc.Match(adminreports.IsValidationError, http.StatusBadRequest,
		"InvalidRequest", "The request contains invalid data"),
)

// writeError writes a JSON error response with the given status code.
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// handleServiceError maps service-layer errors to HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	errorMapper.Write(w, err)
}
