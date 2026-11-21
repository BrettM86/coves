package routes

import (
	"time"

	"Coves/internal/api/handlers/adminreport"
	"Coves/internal/api/middleware"
	"Coves/internal/core/adminreports"

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

// RegisterCommunityReportQueueRoutes registers social.coves.community.listReports
// and social.coves.community.dismissReports. Both require an authenticated
// caller; the service decides community authority.
func RegisterCommunityReportQueueRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware,
	service adminreport.CommunityQueueService, viewer adminreport.ReportSubjectViewer,
) {
	list := adminreport.NewListCommunityReportsHandler(service, viewer)
	dismiss := adminreport.NewDismissCommunityReportsHandler(service)
	authenticated := r.With(adminAuth.RequireAuthenticated)
	authenticated.Get("/xrpc/social.coves.community.listReports", list.HandleListReports)
	authenticated.Post("/xrpc/social.coves.community.dismissReports", dismiss.HandleDismissReports)
}
