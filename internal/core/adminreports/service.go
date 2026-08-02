package adminreports

import (
	"context"
	"log/slog"
	"time"
)

const (
	// alertTimeout is the ceiling on how long report submission will wait for
	// the alert channel. A channel normally imposes its own, shorter timeout
	// (Telegram's default is 5s); this is the backstop that keeps a misbehaving
	// or unconfigured transport from pinning the request forever. A channel
	// timeout above this value is capped by it — see TELEGRAM_TIMEOUT_SECONDS.
	alertTimeout = 10 * time.Second

	// maxConcurrentAlerts bounds how many submissions may be parked on the
	// alert channel at once.
	//
	// This bound has to exist here because nothing upstream provides one: the
	// route's rate limiter keys on client IP (middleware.GetClientIP), so N
	// source addresses buy N independent budgets, and delivery is synchronous.
	// Without a cap, a slow or unreachable channel converts report submission
	// into an unbounded supply of request goroutines each parked for up to
	// alertTimeout. Saturation drops the alert loudly rather than queueing,
	// because a queued alert delivered minutes late is worth less than the log
	// line saying it was dropped.
	maxConcurrentAlerts = 4
)

// service implements the Service interface for admin reports
type service struct {
	repo     Repository
	notifier Notifier

	// alertSlots is a counting semaphore enforcing maxConcurrentAlerts.
	alertSlots chan struct{}
}

// ServiceOption configures optional service dependencies. Options keep the
// constructor stable as new optional collaborators are added.
type ServiceOption func(*service)

// WithNotifier raises an operator alert for every accepted report. Without it
// the service is storage-only: reports are recorded and nobody is told.
func WithNotifier(notifier Notifier) ServiceOption {
	return func(s *service) {
		s.notifier = notifier
	}
}

// NewService creates a new admin reports service
func NewService(repo Repository, opts ...ServiceOption) Service {
	s := &service{
		repo:       repo,
		alertSlots: make(chan struct{}, maxConcurrentAlerts),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SubmitReport validates the report request and creates a new report
// Returns the report ID on success
func (s *service) SubmitReport(ctx context.Context, req SubmitReportRequest) (*SubmitReportResult, error) {
	// Use the constructor which handles validation and target type determination
	report, err := NewReport(req)
	if err != nil {
		return nil, err
	}

	// Create the report in the database
	if err := s.repo.Create(ctx, report); err != nil {
		return nil, err
	}

	s.raiseAlert(ctx, report)

	return &SubmitReportResult{
		ReportID: report.ID,
	}, nil
}

// raiseAlert tells an operator that a report has been filed.
//
// Two decisions worth stating, because both look like bugs from the outside:
//
// Delivery is synchronous. The obvious alternative — hand it to a goroutine so
// the reporter never waits — drops the alert silently whenever the process
// exits before it lands, and losing a CSAM alert to a routine deploy is a
// worse outcome than making one reporter wait a few seconds. maxConcurrentAlerts
// is what keeps that choice from being unbounded.
//
// Nothing here can fail the submission. The row is already committed: answering
// an error would tell reporters their report failed when it did not, and invite
// a retry that files a duplicate. That has to hold for panics too, not just
// errors — a panicking notifier would otherwise reach chi's Recoverer and turn
// a stored report into a 500, producing exactly the duplicate this avoids.
func (s *service) raiseAlert(ctx context.Context, report *Report) {
	if s.notifier == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("admin report stored but operator alert panicked",
				"report_id", report.ID,
				"reason", report.Reason,
				"panic", recovered,
			)
		}
	}()

	select {
	case s.alertSlots <- struct{}{}:
		defer func() { <-s.alertSlots }()
	default:
		slog.Error("admin report stored but operator alert dropped: alert channel saturated",
			"report_id", report.ID,
			"reason", report.Reason,
			"target_uri", report.TargetURI,
			"in_flight", maxConcurrentAlerts,
		)
		return
	}

	// Detach from the request's cancellation. The report is already stored, and
	// a reporter who closes the app the instant they hit submit must not also
	// cancel the alert about them doing it. Context values (trace IDs) survive;
	// only the cancellation signal is dropped.
	alertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertTimeout)
	defer cancel()

	if err := s.notifier.NotifyNewReport(alertCtx, report); err != nil {
		// The report ID is the recovery path: it is enough to find the full
		// record in Postgres once someone reads this line.
		slog.Error("admin report stored but operator alert failed",
			"report_id", report.ID,
			"reason", report.Reason,
			"target_uri", report.TargetURI,
			"error", err,
		)
	}
}
