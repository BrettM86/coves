package aggregator

import (
	"net/http"

	"Coves/internal/core/aggregators"
)

// MetricsHandler provides API key service metrics for monitoring
type MetricsHandler struct {
	apiKeyService aggregators.APIKeyServiceInterface
}

// NewMetricsHandler creates a new metrics handler
func NewMetricsHandler(apiKeyService aggregators.APIKeyServiceInterface) *MetricsHandler {
	return &MetricsHandler{
		apiKeyService: apiKeyService,
	}
}

// MetricsResponse contains API key service operational metrics
type MetricsResponse struct {
	FailedLastUsedUpdates int64 `json:"failedLastUsedUpdates"`
	FailedNonceUpdates    int64 `json:"failedNonceUpdates"`
}

// HandleMetrics handles GET /xrpc/social.coves.aggregator.getMetrics
// Returns operational metrics for the API key service.
// This endpoint is intended for internal monitoring and health checks.
func (h *MetricsHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := MetricsResponse{
		FailedLastUsedUpdates: h.apiKeyService.GetFailedLastUsedUpdates(),
		FailedNonceUpdates:    h.apiKeyService.GetFailedNonceUpdates(),
	}

	writeJSONResponse(w, http.StatusOK, response)
}
