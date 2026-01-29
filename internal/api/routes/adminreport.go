package routes

import (
	"Coves/internal/api/handlers/adminreport"
	"Coves/internal/api/middleware"
	"Coves/internal/core/adminreports"
	"time"

	"github.com/go-chi/chi/v5"
)

// RegisterAdminReportRoutes registers admin report XRPC endpoints on the router
// Implements social.coves.admin.* lexicon endpoints for content reporting
// All endpoints require authentication and are rate limited
func RegisterAdminReportRoutes(r chi.Router, service adminreports.Service, authMiddleware *middleware.OAuthAuthMiddleware) {
	// Initialize handlers
	submitHandler := adminreport.NewSubmitHandler(service)

	// Create rate limiter for report submission
	// Allow 10 reports per minute per user to prevent abuse
	// This is intentionally restrictive since report submission is a sensitive operation
	reportRateLimiter := middleware.NewRateLimiter(10, time.Minute)

	// Procedure endpoints (POST) - require authentication and rate limiting
	// social.coves.admin.submitReport - submit a report for admin review
	r.With(
		reportRateLimiter.Middleware,
		authMiddleware.RequireAuth,
	).Post(
		"/xrpc/social.coves.admin.submitReport",
		submitHandler.HandleSubmit)
}
