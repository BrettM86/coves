package classificationadmin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"Coves/internal/api/handlers/classificationadmin"
	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const metricsNSID = "social.coves.moderation.getClassificationMetrics"

// metricsServiceFake answers with the configured metrics or error.
type metricsServiceFake struct {
	metrics classification.Metrics
	err     error
}

func (service *metricsServiceFake) Metrics(context.Context) (classification.Metrics, error) {
	return service.metrics, service.err
}

func metricsRequest() *http.Request {
	return adminRequest(http.MethodGet, "/xrpc/"+metricsNSID, nil)
}

type wireJobCounts struct {
	Queued           int64 `json:"queued"`
	Deferred         int64 `json:"deferred"`
	Retrying         int64 `json:"retrying"`
	Sent             int64 `json:"sent"`
	Failed           int64 `json:"failed"`
	ClosedSuperseded int64 `json:"closedSuperseded"`
	ClosedNotVisible int64 `json:"closedNotVisible"`
}

type wireUsageDay struct {
	ModelID     string `json:"modelId"`
	Day         string `json:"day"`
	Requests    int64  `json:"requests"`
	InputTokens int64  `json:"inputTokens"`
}

type wireMetrics struct {
	Jobs                   wireJobCounts  `json:"jobs"`
	Usage                  []wireUsageDay `json:"usage"`
	WorkerLagSeconds       int64          `json:"workerLagSeconds"`
	UndeliveredFlagEvents  int64          `json:"undeliveredFlagEvents"`
	UndeliveredScoreAlerts int64          `json:"undeliveredScoreAlerts"`
	AuthPausedUntil        *string        `json:"authPausedUntil"`
}

// TestMetricsHandlerOutput answers the service's metrics field for field,
// the worker lag in whole seconds, and a body that validates against the
// lexicon output (B13).
func TestMetricsHandlerOutput(t *testing.T) {
	service := &metricsServiceFake{metrics: classification.Metrics{
		Jobs: classification.JobCounts{
			Queued: 1, Deferred: 2, Retrying: 3, Sent: 4, Failed: 5, ClosedSuperseded: 6, ClosedNotVisible: 7,
		},
		Usage: []classification.UsageDay{
			{ModelID: "@cf/cloudflare/clef-flash", Day: "2026-11-12", Requests: 10, InputTokens: 1000},
			{ModelID: "@cf/cloudflare/clef", Day: "2026-11-11", Requests: 3, InputTokens: 300},
		},
		WorkerLag:              95 * time.Second,
		UndeliveredFlagEvents:  11,
		UndeliveredScoreAlerts: 13,
	}}

	response := serve(classificationadmin.NewMetricsHandler(service), metricsRequest())

	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	var output wireMetrics
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output), "body: %q", response.Body.String())
	assert.Equal(t, wireMetrics{
		Jobs: wireJobCounts{Queued: 1, Deferred: 2, Retrying: 3, Sent: 4, Failed: 5, ClosedSuperseded: 6, ClosedNotVisible: 7},
		Usage: []wireUsageDay{
			{ModelID: "@cf/cloudflare/clef-flash", Day: "2026-11-12", Requests: 10, InputTokens: 1000},
			{ModelID: "@cf/cloudflare/clef", Day: "2026-11-11", Requests: 3, InputTokens: 300},
		},
		WorkerLagSeconds:       95,
		UndeliveredFlagEvents:  11,
		UndeliveredScoreAlerts: 13,
	}, output)

	requireLexiconOutput(t, response, metricsNSID)
}

// TestMetricsHandlerReportsTheProviderAuthPause answers authPausedUntil as a
// datetime while the worker is paused on a provider auth failure, and leaves
// it out otherwise.
func TestMetricsHandlerReportsTheProviderAuthPause(t *testing.T) {
	pausedUntil := time.Date(2026, 11, 14, 12, 5, 0, 0, time.UTC)
	service := &metricsServiceFake{metrics: classification.Metrics{AuthPausedUntil: &pausedUntil}}

	response := serve(classificationadmin.NewMetricsHandler(service), metricsRequest())

	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	var output wireMetrics
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output))
	if assert.NotNil(t, output.AuthPausedUntil) {
		parsed, err := time.Parse(time.RFC3339Nano, *output.AuthPausedUntil)
		require.NoError(t, err)
		assert.True(t, pausedUntil.Equal(parsed), "authPausedUntil %s", *output.AuthPausedUntil)
	}
	requireLexiconOutput(t, response, metricsNSID)

	unpaused := serve(classificationadmin.NewMetricsHandler(&metricsServiceFake{}), metricsRequest())
	assert.NotContains(t, unpaused.Body.String(), "authPausedUntil", "no pause, no field")
}

// TestMetricsHandlerAnswersServiceErrorWith500 answers a service error with
// 500 InternalServerError and none of the error's text (B13).
func TestMetricsHandlerAnswersServiceErrorWith500(t *testing.T) {
	service := &metricsServiceFake{err: errors.New("metrics store unavailable")}

	response := serve(classificationadmin.NewMetricsHandler(service), metricsRequest())

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var body map[string]any
	if assert.NoError(t, json.Unmarshal(response.Body.Bytes(), &body), "XRPC error must be a JSON body: %q", response.Body.String()) {
		assert.Equal(t, "InternalServerError", body["error"])
		assert.NotContains(t, body["message"], "metrics store unavailable", "an internal error's detail stays out of the response")
	}
}
