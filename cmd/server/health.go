package main

import (
	"Coves/internal/atproto/jetstream"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// consumerStalledThreshold is how long a consumer may be disconnected before
// /health/consumers reports "stalled" (503).
const consumerStalledThreshold = 60 * time.Second

// consumerHealth is one consumer's entry in the /health/consumers response.
//
// LastEventAgeSeconds and CursorAgeSeconds are informational signals for
// operator alerting, NOT auto-503 inputs: a quiet local stream legitimately
// receives no events, so a large age alone cannot distinguish "nothing to
// index" from "connected but wedged". Operators who know their stream's
// expected cadence can alert on these externally.
type consumerHealth struct {
	jetstream.ConnectorStatus
	DeadLetterBacklog   int64  `json:"deadLetterBacklog"`
	LastEventAgeSeconds *int64 `json:"lastEventAgeSeconds,omitempty"` // omitted if no event received yet
	CursorAgeSeconds    *int64 `json:"cursorAgeSeconds,omitempty"`    // omitted if the cursor is still 0
}

// consumerHealthResponse is the /health/consumers response body.
type consumerHealthResponse struct {
	Status string `json:"status"` // "ok", "degraded", or "stalled"
	// DeadLetterBacklogUnknown distinguishes "backlog is 0" from "the backlog
	// could not be counted" (e.g. Postgres is down): without it the endpoint
	// would look healthier the sicker the database gets.
	DeadLetterBacklogUnknown bool             `json:"deadLetterBacklogUnknown,omitempty"`
	Consumers                []consumerHealth `json:"consumers"`
}

// buildConsumerHealthResponse is the pure decision core of /health/consumers,
// extracted so tests can drive it with hand-built statuses. Rules:
//   - any consumer disconnected longer than consumerStalledThreshold →
//     "stalled" + 503.
//   - dead letter backlog uncountable → "degraded" + 200 (stalled wins).
//   - otherwise "ok" + 200.
//
// A connector reporting no DisconnectedSince is not stalled, but note that in
// production this only describes a connector that was never *started*:
// Connector.Start sets disconnected-since-boot as its first action precisely
// so that a consumer which never achieves its first connection still surfaces
// as stalled after the threshold. The never-started case exists only in tests.
func buildConsumerHealthResponse(statuses []jetstream.ConnectorStatus, backlogs map[string]int64, backlogUnknown bool, now time.Time) (consumerHealthResponse, int) {
	// Pre-allocated rather than left nil so the JSON field is always [] and
	// never null, which keeps the response shape stable for typed clients.
	response := consumerHealthResponse{
		Status:    "ok",
		Consumers: make([]consumerHealth, 0, len(statuses)),
	}
	httpCode := http.StatusOK
	if backlogUnknown {
		response.Status = "degraded"
		response.DeadLetterBacklogUnknown = true
	}

	for _, status := range statuses {
		if !status.Connected && status.DisconnectedSince != nil &&
			now.Sub(*status.DisconnectedSince) > consumerStalledThreshold {
			response.Status = "stalled"
			httpCode = http.StatusServiceUnavailable
		}

		entry := consumerHealth{
			ConnectorStatus:   status,
			DeadLetterBacklog: backlogs[status.Name],
		}
		if status.LastEventAt != nil {
			age := int64(now.Sub(*status.LastEventAt).Seconds())
			entry.LastEventAgeSeconds = &age
		}
		if status.CursorTimeUS != 0 {
			age := int64(now.Sub(time.UnixMicro(status.CursorTimeUS)).Seconds())
			entry.CursorAgeSeconds = &age
		}
		response.Consumers = append(response.Consumers, entry)
	}
	return response, httpCode
}

// consumerHealthHandler reports Jetstream consumer health as JSON: connection
// state, cursor position, processed/dead-lettered counts, event/cursor ages,
// and the dead letter backlog per consumer. Responds 503 when any consumer
// has been disconnected longer than consumerStalledThreshold (indexing is
// stalled) so monitoring can alert on it.
func consumerHealthHandler(connectors []*jetstream.Connector, deadLetterQueue jetstream.DeadLetterQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		backlogs, err := deadLetterQueue.CountDeadLetters(r.Context())
		backlogUnknown := err != nil
		if backlogUnknown {
			// Log the error server-side only: this is a public endpoint, so
			// the response carries just the deadLetterBacklogUnknown flag.
			slog.Error("failed to count dead letters for health check", "error", err)
		}

		statuses := make([]jetstream.ConnectorStatus, 0, len(connectors))
		for _, connector := range connectors {
			statuses = append(statuses, connector.Status())
		}

		response, httpCode := buildConsumerHealthResponse(statuses, backlogs, backlogUnknown, time.Now())

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpCode)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("failed to write consumer health response", "error", err)
		}
	}
}

// livenessHandler answers /health and /xrpc/_health.
//
// These stay pure liveness checks — they are the container healthcheck target,
// and a Jetstream outage must not restart-loop the whole AppView. Indexing
// health is reported separately by /health/consumers.
func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("OK")); err != nil {
		slog.Error("failed to write health check response", "error", err)
	}
}
