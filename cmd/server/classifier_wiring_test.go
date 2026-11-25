package main

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/classification"
	"Coves/internal/core/imageproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildClassificationWorkerPassesImageFetcherOnlyWithProxy(t *testing.T) {
	tests := []struct {
		name            string
		proxy           *imageproxy.ImageProxyService
		withConstructor bool
		wantOptions     int
		wantWorker      bool
	}{
		{name: "proxy enabled passes the image fetcher", proxy: &imageproxy.ImageProxyService{}, withConstructor: true, wantOptions: 1, wantWorker: true},
		{name: "proxy disabled passes no options", proxy: nil, withConstructor: true, wantOptions: 0, wantWorker: true},
		{name: "classifier disabled builds no worker", proxy: &imageproxy.ImageProxyService{}, withConstructor: false, wantOptions: -1, wantWorker: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recordedOptions := -1
			a := &application{imageProxyService: test.proxy}
			if test.withConstructor {
				a.newClassificationWorker = func(options ...classification.WorkerOption) *classification.Worker {
					recordedOptions = len(options)
					return &classification.Worker{}
				}
			}

			a.buildClassificationWorker()

			assert.Equal(t, test.wantOptions, recordedOptions)
			assert.Equal(t, test.wantWorker, a.classificationWorker != nil)
		})
	}
}

// claimDeadlineRepository reports the deadline of the first ClaimDue context
// and claims nothing.
type claimDeadlineRepository struct {
	classification.Repository
	deadlines chan time.Duration
}

func (r *claimDeadlineRepository) ClaimDue(ctx context.Context, _ classification.Evaluation, _ time.Time, _ int, _ time.Duration) ([]classification.ClaimedJob, error) {
	remaining := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	select {
	case r.deadlines <- remaining:
	default:
	}
	return nil, nil
}

// A classifier cycle processes ceil(BatchSize/Concurrency) requests in turn
// on each slot, each up to classifierRequestTimeout. The cycle deadline must
// cover that, not the worker interval, and must end before the claim lease so
// no claimed job is claimed twice while the cycle still holds it.
func TestStartClassifierJob_CycleDeadlineCoversABatchAndEndsBeforeTheLease(t *testing.T) {
	requestsPerSlot := time.Duration(classifierBatchesPerConcurrency)
	if classifierCycleTimeout < requestsPerSlot*classifierRequestTimeout {
		t.Fatalf("classifierCycleTimeout %s is shorter than a batch (%d × %s)",
			classifierCycleTimeout, classifierBatchesPerConcurrency, classifierRequestTimeout)
	}
	if classifierCycleTimeout >= classification.ClaimLease {
		t.Fatalf("classifierCycleTimeout %s is not shorter than the claim lease %s",
			classifierCycleTimeout, classification.ClaimLease)
	}

	repo := &claimDeadlineRepository{deadlines: make(chan time.Duration, 1)}
	worker := classification.NewWorker(repo, nil, classification.Policy{}, classification.WorkerConfig{
		Concurrency: 2,
		BatchSize:   2 * classifierBatchesPerConcurrency,
	}, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	interval := 30 * time.Second
	startClassifierJob(ctx, &waitGroup, worker, interval)

	var remaining time.Duration
	select {
	case remaining = <-repo.deadlines:
	case <-time.After(5 * time.Second):
		t.Fatal("classifier job never ran its first cycle")
	}
	cancel()
	waitGroup.Wait()

	if remaining < 0 {
		t.Fatal("classifier cycle context has no deadline")
	}
	if remaining <= interval || remaining > classifierCycleTimeout {
		t.Fatalf("classifier cycle deadline %s from start; want the cycle timeout %s, not the interval %s",
			remaining, classifierCycleTimeout, interval)
	}
}

// TestBuildClassificationOperationsWithClassifierDisabled: with
// CLASSIFIER_ENABLED false both operations services are built, and the
// re-enqueue service refuses a valid request as not configured instead of
// queueing jobs under a zero evaluation (B11).
func TestBuildClassificationOperationsWithClassifierDisabled(t *testing.T) {
	a := &application{cfg: &config.Config{}}

	require.NoError(t, a.buildClassificationOperations())

	require.NotNil(t, a.classificationReenqueue, "the re-enqueue service is built with the classifier disabled")
	require.NotNil(t, a.classificationSummary, "the summary service is built with the classifier disabled")
	_, err := a.classificationReenqueue.Reenqueue(t.Context(), classification.ReenqueueRequest{
		Target: classification.ReenqueuePostsAndComments,
		Since:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Until:  time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	})
	assert.ErrorIs(t, err, classification.ErrClassifierNotConfigured)
}

// TestBuildClassifierRefusesUnknownScoreAlertKey: a CLASSIFIER_SCORE_ALERTS
// entry naming a question the policy does not ask fails startup with an error
// naming the key (B3).
func TestBuildClassifierRefusesUnknownScoreAlertKey(t *testing.T) {
	a := &application{cfg: &config.Config{Classifier: config.ClassifierConfig{
		Enabled:                  true,
		AccountID:                "acct-123",
		APIToken:                 "cf-workers-ai-token-wiring",
		TextModelID:              "@cf/cloudflare/clef-flash",
		ImageModelID:             "@cf/cloudflare/clef",
		DailyRequestCap:          5000,
		WorkerInterval:           30 * time.Second,
		Concurrency:              2,
		TextCharLimit:            8000,
		AttemptLimit:             5,
		RetryBaseDelay:           time.Minute,
		PendingDeferral:          10 * time.Minute,
		ScoreAlerts:              "unknown_key:0.5",
		ScoreAlertAttemptLimit:   8,
		ScoreAlertRetryBaseDelay: time.Minute,
		ScoreAlertSweepInterval:  5 * time.Minute,
	}}}

	err := a.buildClassifier()

	require.Error(t, err, "an unknown score alert key must fail startup")
	assert.ErrorContains(t, err, "unknown_key")
}

// TestBuildClassifierWorkerInstallsScoreAlerts: the worker constructor that
// buildClassifier sets builds workers with the parsed CLASSIFIER_SCORE_ALERTS
// thresholds and the score alert service (whose alert links use
// APPVIEW_PUBLIC_URL), which the worker needs to record
// and deliver score alerts with a stored result. Worker keeps these fields
// unexported and the constructor's repository and Cloudflare client are not
// injectable, so the test reads the fields through reflection.
func TestBuildClassifierWorkerInstallsScoreAlerts(t *testing.T) {
	for _, key := range []string{"TELEGRAM_ALERTS_ENABLED", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "TELEGRAM_TIMEOUT_SECONDS", "TELEGRAM_ALERT_REASONS"} {
		t.Setenv(key, "")
	}
	a := &application{cfg: &config.Config{Classifier: config.ClassifierConfig{
		Enabled:                  true,
		AccountID:                "acct-123",
		APIToken:                 "cf-workers-ai-token-wiring",
		TextModelID:              "@cf/cloudflare/clef-flash",
		ImageModelID:             "@cf/cloudflare/clef",
		DailyRequestCap:          5000,
		WorkerInterval:           30 * time.Second,
		Concurrency:              2,
		TextCharLimit:            8000,
		AttemptLimit:             5,
		RetryBaseDelay:           time.Minute,
		PendingDeferral:          10 * time.Minute,
		ScoreAlerts:              "sexual_minors:0.3",
		ScoreAlertAttemptLimit:   8,
		ScoreAlertRetryBaseDelay: time.Minute,
		ScoreAlertSweepInterval:  5 * time.Minute,
	}, OAuth: config.OAuthConfig{PublicURL: "https://appview.wiring.test"}}}

	require.NoError(t, a.buildClassifier())
	require.NotNil(t, a.newClassificationWorker)
	require.NotEmpty(t, a.classificationScoreAlerts)
	require.NotNil(t, a.classificationScoreAlertService)

	worker := reflect.ValueOf(a.newClassificationWorker()).Elem()

	thresholds := worker.FieldByName("scoreAlertThresholds")
	require.True(t, thresholds.IsValid(), "Worker has no scoreAlertThresholds field")
	assert.Equal(t, len(a.classificationScoreAlerts), thresholds.Len(),
		"the production worker must carry the parsed score alert thresholds")

	notifier := worker.FieldByName("scoreAlertNotifier")
	require.True(t, notifier.IsValid(), "Worker has no scoreAlertNotifier field")
	require.False(t, notifier.IsNil(), "the production worker must have a score alert notifier")
	assert.Equal(t, reflect.ValueOf(a.classificationScoreAlertService).Pointer(), notifier.Elem().Pointer(),
		"the production worker must notify the score alert service buildClassifier built")

	// The score alert links point at the configured APPVIEW_PUBLIC_URL.
	alertConfig := reflect.ValueOf(a.classificationScoreAlertService).Elem().FieldByName("cfg")
	require.True(t, alertConfig.IsValid(), "ScoreAlertService has no cfg field")
	assert.Equal(t, "https://appview.wiring.test", alertConfig.FieldByName("WebBaseURL").String(),
		"the score alert service must build links from APPVIEW_PUBLIC_URL")

	// The worker and the metrics share one provider auth pause, so
	// getClassificationMetrics shows the pause the worker is under.
	require.NotNil(t, a.classificationAuthPause)
	pause := worker.FieldByName("authPause")
	require.True(t, pause.IsValid(), "Worker has no authPause field")
	assert.Equal(t, reflect.ValueOf(a.classificationAuthPause).Pointer(), pause.Pointer(),
		"the production worker must use the pause buildClassifier built")
	a.db = nil
	require.NoError(t, a.buildClassificationOperations())
	metricsPause := reflect.ValueOf(a.classificationMetrics).Elem().FieldByName("authPause")
	require.True(t, metricsPause.IsValid(), "MetricsService has no authPause field")
	assert.Equal(t, reflect.ValueOf(a.classificationAuthPause).Pointer(), metricsPause.Pointer(),
		"the metrics must report the worker's pause")
}
