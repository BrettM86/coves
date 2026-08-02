package adminreports

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// mockNotifier records the alerts a service raises.
//
// Guarded by a mutex because the concurrency-bound test drives it from several
// goroutines at once.
type mockNotifier struct {
	notifyFunc func(ctx context.Context, report *Report) error

	mu       sync.Mutex
	notified []*Report
	// contextErrs captures ctx.Err() as seen inside the notifier, so a test can
	// prove the alert is not riding the request's cancellation.
	contextErrs []error
}

func (m *mockNotifier) NotifyNewReport(ctx context.Context, report *Report) error {
	m.mu.Lock()
	m.notified = append(m.notified, report)
	m.contextErrs = append(m.contextErrs, ctx.Err())
	m.mu.Unlock()

	if m.notifyFunc != nil {
		return m.notifyFunc(ctx, report)
	}
	return nil
}

// alerts returns a snapshot of the reports the notifier was handed.
func (m *mockNotifier) alerts() []*Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Report(nil), m.notified...)
}

// countingRepo is a Repository safe to drive from many goroutines at once,
// which the shared mockRepository is not.
type countingRepo struct {
	mu      sync.Mutex
	created int
}

func (r *countingRepo) Create(ctx context.Context, report *Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created++
	report.ID = int64(r.created)
	return nil
}

func (r *countingRepo) ListByStatus(ctx context.Context, status string, limit, offset int) ([]*Report, error) {
	return nil, nil
}

func (r *countingRepo) UpdateStatus(ctx context.Context, id int64, status, resolvedBy, notes string) error {
	return nil
}

func (r *countingRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

func validSubmitRequest(reason string) SubmitReportRequest {
	return SubmitReportRequest{
		ReporterDID: "did:plc:reporter7890",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		Reason:      reason,
		Explanation: "explanation text",
	}
}

func TestSubmitReport_RaisesAlert(t *testing.T) {
	repo := &mockRepository{}
	notifier := &mockNotifier{}
	svc := NewService(repo, WithNotifier(notifier))

	result, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(notifier.notified) != 1 {
		t.Fatalf("expected exactly 1 alert, got %d", len(notifier.notified))
	}

	// The alert must carry the stored report, not the pre-insert one: the ID is
	// the only handle an operator has for finding the record.
	if got := notifier.notified[0].ID; got != result.ReportID {
		t.Errorf("alert carried report ID %d, want %d", got, result.ReportID)
	}
	if got := notifier.notified[0].Reason; got != ReasonCSAM {
		t.Errorf("alert carried reason %q, want %q", got, ReasonCSAM)
	}
}

// The row is committed before the alert is attempted. A failed alert must not
// be reported to the user as a failed report — that invites a retry, and the
// retry files a duplicate.
func TestSubmitReport_AlertFailureDoesNotFailSubmission(t *testing.T) {
	repo := &mockRepository{}
	notifier := &mockNotifier{
		notifyFunc: func(ctx context.Context, report *Report) error {
			return errors.New("telegram unreachable")
		},
	}
	svc := NewService(repo, WithNotifier(notifier))

	result, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam"))
	if err != nil {
		t.Fatalf("alert failure must not fail submission, got: %v", err)
	}
	if result == nil || result.ReportID == 0 {
		t.Fatal("expected a usable report ID despite the alert failure")
	}
	if len(repo.createdReports) != 1 {
		t.Fatalf("expected the report to be stored, got %d row(s)", len(repo.createdReports))
	}
}

// The mobile client fires this request and the user may close the app
// immediately. If the alert inherited the request context it would be
// cancelled before it ever left the process.
func TestSubmitReport_AlertSurvivesRequestCancellation(t *testing.T) {
	repo := &mockRepository{}
	notifier := &mockNotifier{}
	svc := NewService(repo, WithNotifier(notifier))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.SubmitReport(ctx, validSubmitRequest("csam")); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(notifier.notified) != 1 {
		t.Fatalf("expected the alert to be raised, got %d", len(notifier.notified))
	}
	if err := notifier.contextErrs[0]; err != nil {
		t.Errorf("alert context was already cancelled (%v); it must be detached from the request", err)
	}
}

func TestSubmitReport_NoAlertWhenValidationFails(t *testing.T) {
	repo := &mockRepository{}
	notifier := &mockNotifier{}
	svc := NewService(repo, WithNotifier(notifier))

	if _, err := svc.SubmitReport(context.Background(), validSubmitRequest("not-a-reason")); err == nil {
		t.Fatal("expected a validation error")
	}

	if len(notifier.notified) != 0 {
		t.Errorf("expected no alert for a rejected report, got %d", len(notifier.notified))
	}
}

func TestSubmitReport_NoAlertWhenStorageFails(t *testing.T) {
	repo := &mockRepository{
		createFunc: func(ctx context.Context, report *Report) error {
			return errors.New("database unavailable")
		},
	}
	notifier := &mockNotifier{}
	svc := NewService(repo, WithNotifier(notifier))

	if _, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam")); err == nil {
		t.Fatal("expected a storage error")
	}

	// Alerting on an unstored report would send an operator to a row that does
	// not exist.
	if len(notifier.notified) != 0 {
		t.Errorf("expected no alert when storage failed, got %d", len(notifier.notified))
	}
}

// The contract is "an alert failure is never a report failure" — which has to
// cover panics, not just errors. Without recovery a panicking notifier reaches
// chi's Recoverer and turns a committed report into a 500, so the reporter
// retries and files the duplicate this design exists to avoid.
func TestSubmitReport_AlertPanicDoesNotFailSubmission(t *testing.T) {
	repo := &mockRepository{}
	notifier := &mockNotifier{
		notifyFunc: func(ctx context.Context, report *Report) error {
			panic("notifier exploded")
		},
	}
	svc := NewService(repo, WithNotifier(notifier))

	result, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam"))
	if err != nil {
		t.Fatalf("a panicking notifier must not fail submission, got: %v", err)
	}
	if result == nil || result.ReportID == 0 {
		t.Fatal("expected a usable report ID despite the panic")
	}
	if len(repo.createdReports) != 1 {
		t.Fatalf("expected the report to be stored, got %d row(s)", len(repo.createdReports))
	}
}

// Delivery is synchronous and the route's rate limiter keys on client IP, so
// without this bound a slow channel converts report submission into an
// unbounded supply of parked request goroutines.
func TestSubmitReport_BoundsConcurrentAlerts(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, maxConcurrentAlerts+2)

	repo := &countingRepo{}
	notifier := &mockNotifier{
		notifyFunc: func(ctx context.Context, report *Report) error {
			entered <- struct{}{}
			<-release
			return nil
		},
	}
	svc := NewService(repo, WithNotifier(notifier))

	// Fill every slot and leave them parked.
	var wg sync.WaitGroup
	for range maxConcurrentAlerts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam")); err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		}()
	}
	for range maxConcurrentAlerts {
		<-entered
	}

	// One more must not block: it is dropped rather than queued, and the report
	// still succeeds.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam")); err != nil {
			t.Errorf("expected no error on the saturated path, got: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submission blocked while the alert channel was saturated; it must drop, not queue")
	}

	close(release)
	wg.Wait()

	if got := repo.count(); got != maxConcurrentAlerts+1 {
		t.Errorf("expected all %d reports stored, got %d", maxConcurrentAlerts+1, got)
	}
	if got := len(notifier.alerts()); got != maxConcurrentAlerts {
		t.Errorf("expected %d alerts to reach the notifier (the last dropped), got %d",
			maxConcurrentAlerts, got)
	}
}

// Alerting is opt-in; the default build must not require a notifier.
func TestSubmitReport_WithoutNotifier(t *testing.T) {
	repo := &mockRepository{}
	svc := NewService(repo)

	result, err := svc.SubmitReport(context.Background(), validSubmitRequest("csam"))
	if err != nil {
		t.Fatalf("expected no error without a notifier, got: %v", err)
	}
	if result.ReportID == 0 {
		t.Error("expected a report ID")
	}
}
