package classification_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedClassifier answers its nth call with errs[n], or a fixed answer when
// errs[n] is nil or the script has run out, and counts its calls.
type scriptedClassifier struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (c *scriptedClassifier) Classify(context.Context, classification.Content, []classification.Question) (classification.Classification, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn := c.calls
	c.calls++
	if turn < len(c.errs) && c.errs[turn] != nil {
		return classification.Classification{}, c.errs[turn]
	}
	return classification.Classification{ModelID: "text-model", Probabilities: map[string]float64{"spam": 0.2}}, nil
}

func (c *scriptedClassifier) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func providerAuthError(status int) error {
	return &classification.ProviderError{Code: classification.ErrorCodeProviderAuth, HTTPStatus: status}
}

func textPostJob(id int64) classification.ClaimedJob {
	return classification.ClaimedJob{
		ID: id, SubjectURI: "at://did:plc:author/social.coves.community.post/auth", SubjectKind: classification.SubjectKindPost,
		RecordCID: "bafyauth",
	}
}

// providerAuthRecords returns the records logged with code provider_auth.
func providerAuthRecords(handler *recordingHandler) []slog.Record {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	var records []slog.Record
	for _, record := range handler.records {
		if attributes(record)["code"] == classification.ErrorCodeProviderAuth {
			records = append(records, record)
		}
	}
	return records
}

// A 401 or 403 from the provider is the token, not the job: the worker
// records no attempt and no failure, gives every claim back, sends nothing
// more that run, logs once at Error with the HTTP status, and claims nothing
// until the pause ends. The pause starts at five minutes and doubles while
// the token keeps failing; a success resets it (R2, R3).
func TestWorkerRunOnce_ProviderAuthPausesWithoutSpendingAttempts(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: t0}
	repo := &deadlineRepository{job: textPostJob(31), extraJobs: []classification.ClaimedJob{textPostJob(32)}}
	classifier := &scriptedClassifier{errs: []error{providerAuthError(401), providerAuthError(403)}}
	pause := classification.NewProviderAuthPause()
	handler := &recordingHandler{}
	worker := classification.NewWorker(repo, classifier, classification.Policy{Version: "v1"}, classification.WorkerConfig{
		Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
		Concurrency:     1,
		BatchSize:       2,
		TextCharLimit:   8000,
		AttemptLimit:    1,
		RetryBaseDelay:  time.Minute,
		PendingDeferral: time.Minute,
		DailyRequestCap: 100,
	}, clock.Now, slog.New(handler), classification.WithProviderAuthPause(pause))

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.Equal(t, 1, classifier.callCount(), "no request is sent after the auth failure")
	assert.Empty(t, repo.failures, "the auth failure spends no attempt")
	assert.Empty(t, repo.partialResults)
	assert.Empty(t, repo.results)
	assert.Equal(t, []int64{31, 32}, repo.released, "both claims are given back")
	assert.Equal(t, 1, repo.loads, "the run stops: the unreached job is given back unread")
	records := providerAuthRecords(handler)
	require.Len(t, records, 1, "the pause is logged once")
	assert.Equal(t, slog.LevelError, records[0].Level)
	assert.Equal(t, "401", attributes(records[0])["http_status"])
	until, paused := pause.PausedUntil(t0)
	require.True(t, paused)
	assert.Equal(t, t0.Add(classification.ProviderAuthPauseBase), until)
	assert.Equal(t, 5*time.Minute, classification.ProviderAuthPauseBase)

	clock.Advance(4 * time.Minute)
	require.NoError(t, worker.RunOnce(context.Background()))
	assert.Equal(t, 1, repo.claims, "a paused worker claims nothing")
	assert.Equal(t, 1, classifier.callCount())

	// The pause ends; the token still fails, so the next pause is doubled.
	clock.Advance(time.Minute)
	require.NoError(t, worker.RunOnce(context.Background()))
	assert.Equal(t, 2, repo.claims)
	assert.Equal(t, 2, classifier.callCount())
	assert.Empty(t, repo.failures)
	require.Len(t, providerAuthRecords(handler), 2)
	assert.Equal(t, "403", attributes(providerAuthRecords(handler)[1])["http_status"])
	until, paused = pause.PausedUntil(clock.Now())
	require.True(t, paused)
	assert.Equal(t, t0.Add(15*time.Minute), until, "the second pause is ten minutes")

	clock.Advance(9 * time.Minute)
	require.NoError(t, worker.RunOnce(context.Background()))
	assert.Equal(t, 2, repo.claims, "still paused")

	// The token works again: both jobs are classified and the pause resets.
	clock.Advance(time.Minute)
	require.NoError(t, worker.RunOnce(context.Background()))
	assert.Equal(t, 4, classifier.callCount())
	assert.Len(t, repo.results, 2)
	_, paused = pause.PausedUntil(clock.Now())
	assert.False(t, paused)

	classifier.mu.Lock()
	classifier.errs = append(classifier.errs, nil, nil, providerAuthError(401))
	classifier.mu.Unlock()
	require.NoError(t, worker.RunOnce(context.Background()))
	until, paused = pause.PausedUntil(clock.Now())
	require.True(t, paused)
	assert.Equal(t, clock.Now().Add(classification.ProviderAuthPauseBase), until, "a success reset the pause to five minutes")
}

// While the token keeps failing the pause doubles up to an hour.
func TestWorkerRunOnce_ProviderAuthPauseDoublesToAnHour(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	repo := &deadlineRepository{job: textPostJob(33)}
	failing := make([]error, 10)
	for index := range failing {
		failing[index] = providerAuthError(401)
	}
	classifier := &scriptedClassifier{errs: failing}
	pause := classification.NewProviderAuthPause()
	worker := newClockedWorker(repo, classifier, clock.Now, classification.WithProviderAuthPause(pause))

	var pauses []time.Duration
	for range 7 {
		require.NoError(t, worker.RunOnce(context.Background()))
		until, paused := pause.PausedUntil(clock.Now())
		require.True(t, paused)
		pauses = append(pauses, until.Sub(clock.Now()))
		clock.Advance(until.Sub(clock.Now()))
	}

	assert.Equal(t, []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour, time.Hour,
	}, pauses)
	assert.Equal(t, time.Hour, classification.ProviderAuthPauseMax)
	assert.Empty(t, repo.failures, "no attempt is spent however long the token fails")
}

// metricsRepositoryFake answers with fixed metrics.
type metricsRepositoryFake struct{ metrics classification.Metrics }

func (r metricsRepositoryFake) ReadMetrics(context.Context, *classification.Evaluation, time.Time) (classification.Metrics, error) {
	return r.metrics, nil
}

// The metrics report when the worker is paused on a provider auth failure,
// and report nothing once the pause has ended.
func TestMetricsService_ReportsTheProviderAuthPause(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: t0}
	pause := classification.NewProviderAuthPause()
	repo := &deadlineRepository{job: textPostJob(34)}
	worker := newClockedWorker(repo, &scriptedClassifier{errs: []error{providerAuthError(403)}}, clock.Now,
		classification.WithProviderAuthPause(pause))
	service := classification.NewMetricsService(metricsRepositoryFake{}, nil, clock.Now,
		classification.WithMetricsProviderAuthPause(pause))

	metrics, err := service.Metrics(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metrics.AuthPausedUntil, "no pause before any auth failure")

	require.NoError(t, worker.RunOnce(context.Background()))
	metrics, err = service.Metrics(context.Background())
	require.NoError(t, err)
	require.NotNil(t, metrics.AuthPausedUntil)
	assert.Equal(t, t0.Add(5*time.Minute), *metrics.AuthPausedUntil)

	clock.Advance(5 * time.Minute)
	metrics, err = service.Metrics(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metrics.AuthPausedUntil, "the pause has ended")
}

// A request already under way in another job of the run when the token is
// refused sends nothing once the pause is in force: the job is given back
// with no attempt spent.
func TestWorkerRunOnce_ProviderAuthPauseStopsConcurrentJobsBeforeTheirRequest(t *testing.T) {
	pause := classification.NewProviderAuthPause()
	// The first job's fetch waits for the second job's fetch to start, and
	// the second job's fetch waits for the first job's refusal to pause the
	// worker, signalled by the pause's log line.
	secondStarted := make(chan struct{})
	paused := make(chan struct{})
	var fetchOnce, pausedOnce sync.Once
	first := true
	var mu sync.Mutex
	waitForPause := func() {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			select {
			case <-secondStarted:
			case <-time.After(5 * time.Second):
			}
			return
		}
		fetchOnce.Do(func() { close(secondStarted) })
		select {
		case <-paused:
		case <-time.After(5 * time.Second):
		}
	}
	logger := slog.New(signalHandler{code: classification.ErrorCodeProviderAuth, signal: func() {
		pausedOnce.Do(func() { close(paused) })
	}})
	// The fake shows every job the first job's subject, so both carry its CID.
	second := imagePostJob(36)
	second.RecordCID = imagePostJob(35).RecordCID
	repo := &deadlineRepository{
		job: imagePostJob(35), extraJobs: []classification.ClaimedJob{second},
		postImages: imageRefs(nineImageCIDs[0:1]...),
	}
	failing := []error{providerAuthError(401), providerAuthError(401)}
	classifier := &scriptedClassifier{errs: failing}
	worker := classification.NewWorker(repo, classifier, classification.Policy{Version: "v1"}, classification.WorkerConfig{
		Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
		Concurrency:     2,
		BatchSize:       2,
		TextCharLimit:   8000,
		AttemptLimit:    3,
		RetryBaseDelay:  time.Minute,
		PendingDeferral: time.Minute,
		DailyRequestCap: 100,
	}, time.Now, logger, classification.WithProviderAuthPause(pause),
		classification.WithImageFetcher(hookImageFetcher{onFetch: waitForPause}))

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.Equal(t, 1, classifier.callCount(), "the second job sends no request once paused")
	assert.Empty(t, repo.failures)
	assert.Empty(t, repo.closeReasons)
	assert.ElementsMatch(t, []int64{35, 36}, repo.released)
}

// signalHandler is a slog.Handler that calls signal for each record logged
// with code.
type signalHandler struct {
	code   string
	signal func()
}

func (h signalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h signalHandler) Handle(_ context.Context, record slog.Record) error {
	if attributes(record)["code"] == h.code {
		h.signal()
	}
	return nil
}

func (h signalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h signalHandler) WithGroup(string) slog.Handler { return h }

// releaseFailingRepository fails ReleaseClaim for one job.
type releaseFailingRepository struct {
	*deadlineRepository
	failJobID int64
}

func (r *releaseFailingRepository) ReleaseClaim(ctx context.Context, job classification.ClaimedJob) error {
	if job.ID == r.failJobID {
		return errors.New("release claim failed")
	}
	return r.deadlineRepository.ReleaseClaim(ctx, job)
}

// A refused token pauses the worker and stops the run even when the refused
// job's claim cannot be given back: the remaining jobs send nothing, and the
// metrics report the pause. The release failure is still returned.
func TestWorkerRunOnce_ProviderAuthPausesWhenTheReleaseFails(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: t0}
	pause := classification.NewProviderAuthPause()
	repo := &releaseFailingRepository{
		deadlineRepository: &deadlineRepository{job: textPostJob(37), extraJobs: []classification.ClaimedJob{textPostJob(38)}},
		failJobID:          37,
	}
	classifier := &scriptedClassifier{errs: []error{providerAuthError(401)}}
	handler := &recordingHandler{}
	worker := classification.NewWorker(repo, classifier, classification.Policy{Version: "v1"}, classification.WorkerConfig{
		Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
		Concurrency:     1,
		BatchSize:       2,
		TextCharLimit:   8000,
		AttemptLimit:    3,
		RetryBaseDelay:  time.Minute,
		PendingDeferral: time.Minute,
		DailyRequestCap: 100,
	}, clock.Now, slog.New(handler), classification.WithProviderAuthPause(pause))
	service := classification.NewMetricsService(metricsRepositoryFake{}, nil, clock.Now,
		classification.WithMetricsProviderAuthPause(pause))

	err := worker.RunOnce(context.Background())

	require.Error(t, err, "the release failure is returned")
	assert.Equal(t, 1, classifier.callCount(), "the remaining job sends nothing")
	assert.Empty(t, repo.failures)
	assert.Equal(t, []int64{38}, repo.released, "the unreached job is given back")
	require.Len(t, providerAuthRecords(handler), 1, "the pause is logged")
	metrics, err := service.Metrics(context.Background())
	require.NoError(t, err)
	require.NotNil(t, metrics.AuthPausedUntil)
	assert.Equal(t, t0.Add(classification.ProviderAuthPauseBase), *metrics.AuthPausedUntil)
}

// imageRefusingClassifier refuses the token for every request that carries
// images and answers every text-only request.
type imageRefusingClassifier struct{}

func (imageRefusingClassifier) Classify(_ context.Context, content classification.Content, _ []classification.Question) (classification.Classification, error) {
	if len(content.Images) > 0 {
		return classification.Classification{}, providerAuthError(403)
	}
	return classification.Classification{ModelID: "text-model", Probabilities: map[string]float64{"spam": 0.2}}, nil
}

// switchImageFetcher serves images while serving is set and fails every
// fetch otherwise.
type switchImageFetcher struct{ serving *atomic.Bool }

func (f switchImageFetcher) FetchImage(ctx context.Context, ref classification.ImageRef) (classification.Image, error) {
	if !f.serving.Load() {
		return classification.Image{}, errors.New("image fetch failed")
	}
	return cidImageFetcher{}.FetchImage(ctx, ref)
}

// When only the image model refuses the token, an accepted text request does
// not reset the pause: the refusals keep doubling it toward the hour.
func TestWorkerRunOnce_ProviderAuthPauseResetsOnlyOnTheRefusedModel(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	pause := classification.NewProviderAuthPause()
	repo := &deadlineRepository{job: imagePostJob(39), postImages: imageRefs(nineImageCIDs[0:1]...)}
	serving := &atomic.Bool{}
	serving.Store(true)
	worker := newClockedWorker(repo, imageRefusingClassifier{}, clock.Now,
		classification.WithProviderAuthPause(pause), classification.WithImageFetcher(switchImageFetcher{serving: serving}))

	require.NoError(t, worker.RunOnce(context.Background()))
	until, paused := pause.PausedUntil(clock.Now())
	require.True(t, paused)
	assert.Equal(t, clock.Now().Add(5*time.Minute), until, "the first pause is five minutes")

	// The pause ends; the image cannot be fetched, so a text-only request is
	// sent, and the text model accepts it.
	clock.Advance(5 * time.Minute)
	serving.Store(false)
	require.NoError(t, worker.RunOnce(context.Background()))
	_, paused = pause.PausedUntil(clock.Now())
	require.False(t, paused)
	require.Len(t, repo.results, 1, "the text request was accepted")

	serving.Store(true)
	require.NoError(t, worker.RunOnce(context.Background()))
	until, paused = pause.PausedUntil(clock.Now())
	require.True(t, paused)
	assert.Equal(t, clock.Now().Add(10*time.Minute), until, "the second pause is ten minutes")
}

// A request the provider refuses with 429 was never served, so it gives its
// daily-cap reservation back; any other failure was served and stays counted.
// Before this, 4,158 consecutive 429s used up the whole day's cap.
func TestWorkerRunOnce_RateLimitedRequestReleasesItsReservation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// wantCounted is the day's counted requests after the failed run.
		wantCounted int
		// wantResults is the results stored by a second run under the cap
		// of one.
		wantResults int
	}{
		{
			name:        "429 releases the reservation",
			err:         &classification.ProviderError{Code: classification.ErrorCodeRateLimited, HTTPStatus: 429},
			wantCounted: 0,
			wantResults: 1,
		},
		{
			name:        "500 keeps the reservation",
			err:         &classification.ProviderError{Code: classification.ErrorCodeProviderError, HTTPStatus: 500},
			wantCounted: 1,
			wantResults: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
			repo := &deadlineRepository{job: textPostJob(41)}
			classifier := &scriptedClassifier{errs: []error{tt.err}}
			worker := newConfiguredWorker(repo, classifier, clock.Now, func(cfg *classification.WorkerConfig) {
				cfg.DailyRequestCap = 1
			})

			require.NoError(t, worker.RunOnce(context.Background()))
			assert.Equal(t, tt.wantCounted, repo.requestsCounted("text-model", "2026-11-22"))

			clock.Advance(time.Minute)
			require.NoError(t, worker.RunOnce(context.Background()))
			assert.Len(t, repo.results, tt.wantResults)
			assert.Equal(t, 1, repo.requestsCounted("text-model", "2026-11-22"))
		})
	}
}
