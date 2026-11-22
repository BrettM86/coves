package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"Coves/internal/classifier/cloudflare"
	"Coves/internal/core/classification"
	postgresRepo "Coves/internal/db/postgres"
	"Coves/internal/notify/telegram"
)

// classifierRequestTimeout bounds one Workers AI request. A run claims
// classifierBatchesPerConcurrency jobs per in-flight slot, and each slot sends
// its requests in turn, so a run takes up to 4 × 30s = 2 minutes of requests.
// classifierCycleTimeout bounds the run: the batch plus classifierDatabaseMargin
// for each job's subject read and request reservation. It must stay inside the
// worker's 5-minute claim lease, so a job is never claimed twice while the run
// that claimed it still holds it. A job with images sends one request per
// image batch, so a run can reach its deadline mid-subject; the finished
// batches are stored and the next run continues from the first unfinished one.
const (
	classifierRequestTimeout        = 30 * time.Second
	classifierBatchesPerConcurrency = 4
	classifierDatabaseMargin        = 30 * time.Second
	classifierCycleTimeout          = classifierBatchesPerConcurrency*classifierRequestTimeout + classifierDatabaseMargin
)

// buildClassifier builds the content classification queue repository and the
// worker constructor when CLASSIFIER_ENABLED is true; buildClassificationWorker
// builds the worker after the image proxy. When disabled it leaves both nil:
// the post and comment consumers and the community repository then enqueue
// nothing and no worker runs.
func (a *application) buildClassifier() error {
	cfg := a.cfg.Classifier
	if !cfg.Enabled {
		slog.Info("content classifier disabled", "enable_with", "CLASSIFIER_ENABLED=true")
		return nil
	}
	policy, err := classification.LoadPolicy()
	if err != nil {
		return fmt.Errorf("content classifier: %w", err)
	}
	scoreAlerts, err := classification.ParseScoreAlerts(cfg.ScoreAlerts, policy)
	if err != nil {
		return fmt.Errorf("content classifier: %w", err)
	}
	evaluation := classification.Evaluation{
		PolicyVersion: policy.Version,
		TextModelID:   cfg.TextModelID,
		ImageModelID:  cfg.ImageModelID,
	}
	// coves:allow-bare-client: fixed Cloudflare Workers AI API host, never an untrusted URL
	httpClient := &http.Client{Timeout: classifierRequestTimeout}
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:    cfg.AccountID,
		APIToken:     cfg.APIToken,
		TextModelID:  cfg.TextModelID,
		ImageModelID: cfg.ImageModelID,
	}, httpClient)

	telegramConfig, err := telegram.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("content classifier: telegram: %w", err)
	}
	// Untyped nil when Telegram is disabled: a nil *telegram.Client inside the
	// interface would not compare equal to nil and would be called.
	var alertSender classification.MessageSender
	if telegramConfig.Enabled {
		telegramClient, err := telegram.NewClient(telegramConfig)
		if err != nil {
			return fmt.Errorf("content classifier: telegram: %w", err)
		}
		alertSender = telegramClient
	} else {
		slog.Warn("content classifier: Telegram is not enabled; score alerts are recorded and NOBODY is notified",
			"fix", "set TELEGRAM_ALERTS_ENABLED=true")
	}
	scoreAlertService := classification.NewScoreAlertService(
		postgresRepo.NewClassificationScoreAlertRepository(a.db), alertSender,
		classification.ScoreAlertDeliveryConfig{
			AttemptLimit:   cfg.ScoreAlertAttemptLimit,
			RetryBaseDelay: cfg.ScoreAlertRetryBaseDelay,
		}, time.Now, slog.Default())

	a.classificationRepo = postgresRepo.NewClassificationRepository(a.db, a.cfg.Instance.DID)
	a.classificationEvaluation = evaluation
	a.classificationScoreAlerts = scoreAlerts
	a.classificationScoreAlertService = scoreAlertService
	a.classificationAuthPause = classification.NewProviderAuthPause()
	authPause := a.classificationAuthPause
	repository := a.classificationRepo
	a.newClassificationWorker = func(options ...classification.WorkerOption) *classification.Worker {
		options = append([]classification.WorkerOption{
			classification.WithScoreAlerts(scoreAlerts, scoreAlertService),
			classification.WithProviderAuthPause(authPause),
		}, options...)
		return classification.NewWorker(repository, client, policy, classification.WorkerConfig{
			Evaluation:      evaluation,
			Concurrency:     cfg.Concurrency,
			BatchSize:       cfg.Concurrency * classifierBatchesPerConcurrency,
			TextCharLimit:   cfg.TextCharLimit,
			AttemptLimit:    cfg.AttemptLimit,
			RetryBaseDelay:  cfg.RetryBaseDelay,
			PendingDeferral: cfg.PendingDeferral,
			DailyRequestCap: cfg.DailyRequestCap,
			RequestTimeout:  classifierRequestTimeout,
		}, time.Now, slog.Default(), options...)
	}
	slog.Info("content classifier enabled", "policy_version", policy.Version,
		"text_model_id", cfg.TextModelID, "image_model_id", cfg.ImageModelID, "daily_request_cap", cfg.DailyRequestCap,
		"worker_interval", cfg.WorkerInterval, "concurrency", cfg.Concurrency,
		"score_alerts", cfg.ScoreAlerts, "score_alert_sweep_interval", cfg.ScoreAlertSweepInterval)
	return nil
}

// buildClassificationReview builds the admin review service over the stored
// results and verdicts, with the current policy so a precision report lists
// its questions even before anything is verdicted. It does not depend on
// CLASSIFIER_ENABLED.
func (a *application) buildClassificationReview() error {
	policy, err := classification.LoadPolicy()
	if err != nil {
		return fmt.Errorf("content classifier review: %w", err)
	}
	a.classificationReview = classification.NewReviewService(
		postgresRepo.NewClassificationReviewRepository(a.db, a.cfg.Instance.DID), policy, time.Now)
	return nil
}

// buildClassificationOperations builds the re-enqueue, summary and metrics
// services for the admin operations endpoints, whether or not the classifier is
// enabled. With the classifier disabled the re-enqueue service has no
// evaluation and refuses every request with ErrClassifierNotConfigured; the
// summary reads only stored results and flag events.
func (a *application) buildClassificationOperations() error {
	policy, err := classification.LoadPolicy()
	if err != nil {
		return fmt.Errorf("content classifier operations: %w", err)
	}
	var evaluation *classification.Evaluation
	if a.classificationRepo != nil {
		current := a.classificationEvaluation
		evaluation = &current
	}
	a.classificationReenqueue = classification.NewReenqueueService(
		postgresRepo.NewClassificationReenqueueRepository(a.db, 0), evaluation)
	a.classificationSummary = classification.NewSummaryService(
		postgresRepo.NewClassificationSummaryRepository(a.db), policy, time.Now)
	var metricsOptions []classification.MetricsOption
	if a.classificationAuthPause != nil {
		metricsOptions = append(metricsOptions, classification.WithMetricsProviderAuthPause(a.classificationAuthPause))
	}
	a.classificationMetrics = classification.NewMetricsService(
		postgresRepo.NewClassificationMetricsRepository(a.db), evaluation, time.Now, metricsOptions...)
	return nil
}

// buildClassificationWorker builds the classification worker once the image
// proxy exists, so the worker reads images through the proxy as a viewer
// would. With the proxy disabled the worker has no image fetcher and counts
// every image as skipped.
func (a *application) buildClassificationWorker() {
	if a.newClassificationWorker == nil {
		return
	}
	if a.imageProxyService == nil {
		slog.Warn("content classifier: image proxy disabled, images will be counted as skipped and not classified")
		a.classificationWorker = a.newClassificationWorker()
		return
	}
	a.classificationWorker = a.newClassificationWorker(
		classification.WithImageFetcher(classification.NewProxyImageFetcher(a.imageProxyService, a.identityResolver)))
}

// startClassifierJob runs the classification worker every interval, each run
// bounded by classifierCycleTimeout rather than the interval.
func startClassifierJob(ctx context.Context, waitGroup *sync.WaitGroup, worker *classification.Worker, interval time.Duration) {
	runTickerWithTimeout(ctx, waitGroup, "content-classifier", interval, classifierCycleTimeout, func(ctx context.Context) {
		if err := worker.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// The run's error can wrap subject detail, so only a fixed code
			// is logged; the worker logs each failure by job ID and a fixed
			// per-step code.
			slog.Error("content classification run failed", "error_code", "classification_run_failed")
		}
	})
}

// startScoreAlertSweepJob runs the score alert retry sweep every interval.
func startScoreAlertSweepJob(ctx context.Context, waitGroup *sync.WaitGroup, service *classification.ScoreAlertService, interval time.Duration) {
	runTicker(ctx, waitGroup, "classification-score-alert-sweep", interval, func(ctx context.Context) {
		err := service.Sweep(ctx)
		switch {
		case err == nil || errors.Is(err, context.Canceled):
		case errors.Is(err, context.DeadlineExceeded):
			slog.Warn("classification score alert sweep outlasted its interval", "error_code", "sweep_timed_out")
		default:
			slog.Error("classification score alert sweep failed", "error", err)
		}
	})
}
