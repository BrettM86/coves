package classificationadmin

import (
	"context"
	"log/slog"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/classification"
)

// MetricsService is the metrics service the metrics handler calls.
type MetricsService interface {
	Metrics(ctx context.Context) (classification.Metrics, error)
}

// MetricsHandler serves social.coves.moderation.getClassificationMetrics.
type MetricsHandler struct{ service MetricsService }

// NewMetricsHandler builds the metrics handler.
func NewMetricsHandler(service MetricsService) *MetricsHandler {
	return &MetricsHandler{service: service}
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metrics, err := h.service.Metrics(r.Context())
	if err != nil {
		// The error is not logged: only the fixed code is, so no store
		// detail reaches the logs or the response.
		slog.Error("classification metrics failure", "error_code", "classification_metrics_failed")
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
		return
	}
	output := metricsOutput{
		Jobs: jobCountsView{
			Queued:           metrics.Jobs.Queued,
			Deferred:         metrics.Jobs.Deferred,
			Retrying:         metrics.Jobs.Retrying,
			Sent:             metrics.Jobs.Sent,
			Failed:           metrics.Jobs.Failed,
			ClosedSuperseded: metrics.Jobs.ClosedSuperseded,
			ClosedNotVisible: metrics.Jobs.ClosedNotVisible,
		},
		Usage:                  make([]usageDayView, 0, len(metrics.Usage)),
		WorkerLagSeconds:       int64(metrics.WorkerLag.Seconds()),
		UndeliveredFlagEvents:  metrics.UndeliveredFlagEvents,
		UndeliveredScoreAlerts: metrics.UndeliveredScoreAlerts,
	}
	if metrics.AuthPausedUntil != nil {
		output.AuthPausedUntil = formatDatetime(*metrics.AuthPausedUntil)
	}
	for _, day := range metrics.Usage {
		output.Usage = append(output.Usage, usageDayView{
			ModelID: day.ModelID, Day: day.Day, Requests: day.Requests, InputTokens: day.InputTokens,
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

type jobCountsView struct {
	Queued           int64 `json:"queued"`
	Deferred         int64 `json:"deferred"`
	Retrying         int64 `json:"retrying"`
	Sent             int64 `json:"sent"`
	Failed           int64 `json:"failed"`
	ClosedSuperseded int64 `json:"closedSuperseded"`
	ClosedNotVisible int64 `json:"closedNotVisible"`
}

type usageDayView struct {
	ModelID     string `json:"modelId"`
	Day         string `json:"day"`
	Requests    int64  `json:"requests"`
	InputTokens int64  `json:"inputTokens"`
}

type metricsOutput struct {
	Jobs                   jobCountsView  `json:"jobs"`
	Usage                  []usageDayView `json:"usage"`
	WorkerLagSeconds       int64          `json:"workerLagSeconds"`
	UndeliveredFlagEvents  int64          `json:"undeliveredFlagEvents"`
	UndeliveredScoreAlerts int64          `json:"undeliveredScoreAlerts"`
	AuthPausedUntil        string         `json:"authPausedUntil,omitempty"`
}
