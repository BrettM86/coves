package classification

import (
	"context"
	"fmt"
	"time"
)

// JobCounts are the current classification job counts by status.
type JobCounts struct {
	Queued           int64
	Deferred         int64
	Retrying         int64
	Sent             int64
	Failed           int64
	ClosedSuperseded int64
	ClosedNotVisible int64
}

// UsageDay is one model's usage on one UTC day.
type UsageDay struct {
	ModelID     string
	Day         string
	Requests    int64
	InputTokens int64
}

// Metrics are the classifier's operational metrics.
type Metrics struct {
	Jobs                   JobCounts
	Usage                  []UsageDay
	WorkerLag              time.Duration
	UndeliveredFlagEvents  int64
	UndeliveredScoreAlerts int64
	// AuthPausedUntil is when the worker's provider auth pause ends, nil when
	// the worker is not paused.
	AuthPausedUntil *time.Time
}

// MetricsRepository reads the classifier metrics. With evaluation set, the
// pending counts and the worker lag cover only jobs under it; with evaluation
// nil they cover every evaluation.
type MetricsRepository interface {
	ReadMetrics(ctx context.Context, evaluation *Evaluation, now time.Time) (Metrics, error)
}

// MetricsService serves the classifier metrics.
type MetricsService struct {
	repo       MetricsRepository
	evaluation *Evaluation
	now        func() time.Time
	// authPause is the worker's provider auth pause, nil when no worker runs.
	authPause *ProviderAuthPause
}

// MetricsOption configures a MetricsService.
type MetricsOption func(*MetricsService)

// WithMetricsProviderAuthPause makes the metrics report the worker's provider
// auth pause.
func WithMetricsProviderAuthPause(pause *ProviderAuthPause) MetricsOption {
	return func(s *MetricsService) { s.authPause = pause }
}

// NewMetricsService builds a MetricsService reading at the time now returns.
// evaluation is the active evaluation, nil when the classifier is disabled.
func NewMetricsService(repo MetricsRepository, evaluation *Evaluation, now func() time.Time, opts ...MetricsOption) *MetricsService {
	service := &MetricsService{repo: repo, evaluation: evaluation, now: now}
	for _, option := range opts {
		option(service)
	}
	return service
}

// Metrics reads the classifier metrics at the current time.
func (s *MetricsService) Metrics(ctx context.Context) (Metrics, error) {
	now := s.now().UTC()
	metrics, err := s.repo.ReadMetrics(ctx, s.evaluation, now)
	if err != nil {
		return Metrics{}, fmt.Errorf("read classification metrics: %w", err)
	}
	if s.authPause != nil {
		if until, paused := s.authPause.PausedUntil(now); paused {
			until = until.UTC()
			metrics.AuthPausedUntil = &until
		}
	}
	return metrics, nil
}
