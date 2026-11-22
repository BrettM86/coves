package routes

import (
	"Coves/internal/api/handlers/classificationadmin"
	"Coves/internal/api/middleware"

	"github.com/go-chi/chi/v5"
)

// RegisterClassificationAdminRoutes registers the instance admin content
// classifier review endpoints. They are AppView-served, never PDS-proxied, so
// the PDS OAuth scopes in cmd/server remain unchanged.
func RegisterClassificationAdminRoutes(r chi.Router, service classificationadmin.ReviewService, adminAuth *middleware.InstanceAdminMiddleware) {
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.listClassificationResults", classificationadmin.NewListResultsHandler(service).ServeHTTP)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.recordClassificationVerdict", classificationadmin.NewRecordVerdictHandler(service).ServeHTTP)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getClassificationPrecision", classificationadmin.NewPrecisionHandler(service).ServeHTTP)
}

// RegisterClassificationOperationsRoutes registers the instance admin content
// classifier operations endpoints: re-enqueueing existing content and the
// per-community summary. Like the review endpoints they are AppView-served
// and registered whether or not the classifier is enabled.
func RegisterClassificationOperationsRoutes(r chi.Router, reenqueue classificationadmin.ReenqueueService, summary classificationadmin.SummaryService, adminAuth *middleware.InstanceAdminMiddleware) {
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.enqueueClassification", classificationadmin.NewEnqueueHandler(reenqueue).ServeHTTP)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getCommunityClassificationSummary", classificationadmin.NewSummaryHandler(summary).ServeHTTP)
}

// RegisterClassificationMetricsRoutes registers the instance admin content
// classifier metrics endpoint. Like the review endpoints it is AppView-served
// and registered whether or not the classifier is enabled.
func RegisterClassificationMetricsRoutes(r chi.Router, service classificationadmin.MetricsService, adminAuth *middleware.InstanceAdminMiddleware) {
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getClassificationMetrics", classificationadmin.NewMetricsHandler(service).ServeHTTP)
}
