package classification_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineRepository is an in-memory Repository that, like a database
// driver, refuses any call made on a done context. It holds one visible
// claimed job.
type deadlineRepository struct {
	mu          sync.Mutex
	job         classification.ClaimedJob
	extraJobs   []classification.ClaimedJob
	released    []int64
	failures    []classification.Failure
	results     []classification.Result
	inputTokens int
	reservedAt  []time.Time
	// postImages are the image refs the post subject shows; postImagesSkipped
	// the image slots the subject itself skips.
	postImages        []classification.ImageRef
	postImagesSkipped int
	// postVisibility and postCID, when set, override the visible subject at
	// the job's CID.
	postVisibility classification.SubjectVisibility
	postCID        string
	// closeReasons are the reasons the job was closed with.
	closeReasons []string
	// batches is the job's stored batch progress; batchStoredAt the now each
	// batch was stored with.
	batches       []classification.BatchProgress
	batchStoredAt []time.Time
	// deferredUntil are the due times the job was deferred to.
	deferredUntil []time.Time
	// claims counts ClaimDue calls; loads counts post subject loads.
	claims int
	loads  int
	// partialResults and partialFailures are the partial results stored on a
	// final failure and the failures they were stored with.
	partialResults  []classification.Result
	partialFailures []classification.Failure
}

func (r *deadlineRepository) ClaimDue(ctx context.Context, _ classification.Evaluation, _ time.Time, _ int, _ time.Duration) ([]classification.ClaimedJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims++
	return append([]classification.ClaimedJob{r.job}, r.extraJobs...), nil
}

func (r *deadlineRepository) LoadPostSubject(ctx context.Context, _ string) (classification.PostSubject, error) {
	if err := ctx.Err(); err != nil {
		return classification.PostSubject{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads++
	visibility := classification.VisibilityVisible
	if r.postVisibility != "" {
		visibility = r.postVisibility
	}
	cid := r.job.RecordCID
	if r.postCID != "" {
		cid = r.postCID
	}
	return classification.PostSubject{
		Visibility:    visibility,
		CID:           cid,
		Fields:        classification.PostFields{Title: "Harbor seals"},
		ImageRefs:     r.postImages,
		ImagesSkipped: r.postImagesSkipped,
	}, nil
}

func (r *deadlineRepository) LoadCommentSubject(context.Context, string) (classification.CommentSubject, error) {
	return classification.CommentSubject{}, nil
}

func (r *deadlineRepository) LoadCommunitySubject(context.Context, string) (classification.CommunitySubject, error) {
	return classification.CommunitySubject{}, nil
}

func (r *deadlineRepository) Close(ctx context.Context, _ int64, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeReasons = append(r.closeReasons, reason)
	// Closing a job discards its stored batches.
	r.batches = nil
	r.batchStoredAt = nil
	return nil
}

func (r *deadlineRepository) Defer(ctx context.Context, _ int64, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferredUntil = append(r.deferredUntil, until)
	return nil
}

func (r *deadlineRepository) ReleaseClaim(ctx context.Context, job classification.ClaimedJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, job.ID)
	return nil
}

func (r *deadlineRepository) RecordFailure(ctx context.Context, _ int64, failure classification.Failure) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, failure)
	if !failure.Final {
		return 0, nil
	}
	// A final failure discards the job's stored batches.
	discarded := len(r.batches)
	r.batches = nil
	r.batchStoredAt = nil
	return discarded, nil
}

func (r *deadlineRepository) ReserveRequest(ctx context.Context, _ string, now time.Time, _ int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reservedAt = append(r.reservedAt, now)
	return true, nil
}

func (r *deadlineRepository) AddInputTokens(ctx context.Context, _ string, _ time.Time, tokens int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputTokens += tokens
	return nil
}

func (r *deadlineRepository) StoreResult(ctx context.Context, _ classification.ClaimedJob, result classification.Result) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
	// Storing the result discards the job's stored batches; batchStoredAt
	// keeps when each was stored.
	r.batches = nil
	return true, nil
}

func (r *deadlineRepository) StorePartialResult(ctx context.Context, _ classification.ClaimedJob, result classification.Result, failure classification.Failure) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partialResults = append(r.partialResults, result)
	r.partialFailures = append(r.partialFailures, failure)
	// Storing the partial result discards the job's stored batches.
	r.batches = nil
	r.batchStoredAt = nil
	return true, nil
}

func (r *deadlineRepository) LoadBatchProgress(ctx context.Context, _ int64) ([]classification.BatchProgress, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]classification.BatchProgress(nil), r.batches...), nil
}

func (r *deadlineRepository) StoreBatchProgress(ctx context.Context, _ int64, progress classification.BatchProgress, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, progress)
	r.batchStoredAt = append(r.batchStoredAt, now)
	r.inputTokens += progress.InputTokens
	return true, nil
}

// slowClassifier blocks until the request context is done, then returns
// answer and err.
type slowClassifier struct {
	answer classification.Classification
	err    error
}

func (c slowClassifier) Classify(ctx context.Context, _ classification.Content, _ []classification.Question) (classification.Classification, error) {
	<-ctx.Done()
	if c.err != nil {
		return classification.Classification{}, c.err
	}
	return c.answer, nil
}

func newDeadlineWorker(repo classification.Repository, classifier classification.Classifier) *classification.Worker {
	return newClockedWorker(repo, classifier, time.Now)
}

func newClockedWorker(repo classification.Repository, classifier classification.Classifier, now func() time.Time, opts ...classification.WorkerOption) *classification.Worker {
	return newConfiguredWorker(repo, classifier, now, func(*classification.WorkerConfig) {}, opts...)
}

// newConfiguredWorker returns a test worker whose config configure adjusts.
func newConfiguredWorker(repo classification.Repository, classifier classification.Classifier, now func() time.Time, configure func(*classification.WorkerConfig), opts ...classification.WorkerOption) *classification.Worker {
	cfg := classification.WorkerConfig{
		Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
		Concurrency:     1,
		BatchSize:       1,
		TextCharLimit:   8000,
		AttemptLimit:    3,
		RetryBaseDelay:  time.Minute,
		PendingDeferral: time.Minute,
		DailyRequestCap: 100,
	}
	configure(&cfg)
	return classification.NewWorker(repo, classifier, classification.Policy{Version: "v1"}, cfg, now, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
}

// A request cut off by the cycle deadline was already counted against the
// daily cap, so its failure must still be recorded: otherwise attempt_count
// never rises, the attempt limit is never reached, and the job is re-sent
// after every lease (R2).
func TestWorkerRunOnce_CycleDeadlineDuringRequestStillRecordsFailure(t *testing.T) {
	repo := &deadlineRepository{job: classification.ClaimedJob{
		ID: 1, SubjectURI: "at://did:plc:author/social.coves.community.post/1",
		SubjectKind: classification.SubjectKindPost, RecordCID: "bafy1", AttemptCount: 1,
	}}
	worker := newDeadlineWorker(repo, slowClassifier{err: context.DeadlineExceeded})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := worker.RunOnce(ctx)

	require.NoError(t, err, "recording the failure must not fail on the expired cycle context")
	require.Len(t, repo.failures, 1, "the cut-off request's failure was not recorded")
	assert.Equal(t, classification.ErrorCodeTransport, repo.failures[0].ErrorCode)
	assert.False(t, repo.failures[0].Final)
}

// An answer that arrives as the cycle deadline passes must still be stored
// and its tokens counted.
func TestWorkerRunOnce_AnswerAtCycleDeadlineIsStillStored(t *testing.T) {
	repo := &deadlineRepository{job: classification.ClaimedJob{
		ID: 2, SubjectURI: "at://did:plc:author/social.coves.community.post/2",
		SubjectKind: classification.SubjectKindPost, RecordCID: "bafy2",
	}}
	worker := newDeadlineWorker(repo, slowClassifier{answer: classification.Classification{
		ModelID: "text-model", Probabilities: map[string]float64{"spam": 0.1}, InputTokens: 42,
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := worker.RunOnce(ctx)

	require.NoError(t, err, "storing the answer must not fail on the expired cycle context")
	require.Len(t, repo.results, 1, "the answer was not stored")
	assert.Equal(t, 42, repo.inputTokens)
}

// Claimed jobs the run did not reach before its deadline are given back at
// once instead of sitting leased until ClaimLease.
func TestWorkerRunOnce_CycleDeadlineReleasesUnreachedClaims(t *testing.T) {
	repo := &deadlineRepository{
		job: classification.ClaimedJob{
			ID: 3, SubjectURI: "at://did:plc:author/social.coves.community.post/3",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy3",
		},
		extraJobs: []classification.ClaimedJob{{
			ID: 4, SubjectURI: "at://did:plc:author/social.coves.community.post/4",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy4",
		}},
	}
	worker := newDeadlineWorker(repo, slowClassifier{err: context.DeadlineExceeded})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := worker.RunOnce(ctx)

	require.NoError(t, err)
	require.Len(t, repo.failures, 1, "the cut-off request's failure was not recorded")
	assert.Equal(t, []int64{4}, repo.released, "the unreached claim was not released")
}

// testClock is a manually advanced clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// advancingClassifier moves the clock forward, as a slow request does, then
// fails.
type advancingClassifier struct {
	clock *testClock
	delay time.Duration
}

func (c advancingClassifier) Classify(_ context.Context, _ classification.Content, _ []classification.Question) (classification.Classification, error) {
	c.clock.Advance(c.delay)
	return classification.Classification{}, context.DeadlineExceeded
}

// A job that fails late in a long run must still wait out its backoff from
// the moment it failed. Backing off from the run's start would put RetryAt in
// the past and re-send the job on the next cycle (R2).
func TestWorkerRunOnce_RetryBackoffStartsAtFailureTime(t *testing.T) {
	runStart := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: runStart}
	repo := &deadlineRepository{job: classification.ClaimedJob{
		ID: 5, SubjectURI: "at://did:plc:author/social.coves.community.post/5",
		SubjectKind: classification.SubjectKindPost, RecordCID: "bafy5", AttemptCount: 1,
	}}
	worker := newClockedWorker(repo, advancingClassifier{clock: clock, delay: 2 * time.Minute}, clock.Now)

	require.NoError(t, worker.RunOnce(context.Background()))

	require.Len(t, repo.failures, 1)
	failedAt := runStart.Add(2 * time.Minute)
	assert.Equal(t, failedAt.Add(2*time.Minute), repo.failures[0].RetryAt,
		"the second attempt's backoff (2 x RetryBaseDelay) must start when the request failed")
	assert.True(t, repo.failures[0].RetryAt.After(clock.Now()), "RetryAt is already in the past")
}

// Each request is counted on the UTC day it is sent, not the day its run
// started.
func TestWorkerRunOnce_RequestCountedOnTheDayItIsSent(t *testing.T) {
	runStart := time.Date(2026, 11, 12, 23, 59, 0, 0, time.UTC)
	clock := &testClock{now: runStart}
	repo := &deadlineRepository{
		job: classification.ClaimedJob{
			ID: 6, SubjectURI: "at://did:plc:author/social.coves.community.post/6",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy6",
		},
		extraJobs: []classification.ClaimedJob{{
			ID: 7, SubjectURI: "at://did:plc:author/social.coves.community.post/6",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy6",
		}},
	}
	worker := newClockedWorker(repo, advancingClassifier{clock: clock, delay: 2 * time.Minute}, clock.Now)

	require.NoError(t, worker.RunOnce(context.Background()))

	require.Len(t, repo.reservedAt, 2)
	assert.Equal(t, runStart, repo.reservedAt[0])
	assert.Equal(t, runStart.Add(2*time.Minute), repo.reservedAt[1],
		"the second request was sent after UTC midnight and must count on the new day")
}

// cidImageFetcher serves every image as a JPEG whose bytes are its CID, so a
// request's images name the refs they came from.
type cidImageFetcher struct{}

func (cidImageFetcher) FetchImage(_ context.Context, ref classification.ImageRef) (classification.Image, error) {
	return classification.Image{ContentType: "image/jpeg", Data: []byte(ref.CID)}, nil
}

// recordedRequest is what one Classify call was asked: the text and the
// images, by their bytes.
type recordedRequest struct {
	text   string
	images []string
}

// recordingClassifier records every Classify call and answers it with the
// image model; onCall, when set, runs during the call with its 1-based number.
type recordingClassifier struct {
	mu       sync.Mutex
	requests []recordedRequest
	onCall   func(call int)
}

func (c *recordingClassifier) Classify(_ context.Context, content classification.Content, _ []classification.Question) (classification.Classification, error) {
	images := make([]string, 0, len(content.Images))
	for _, image := range content.Images {
		images = append(images, string(image.Data))
	}
	c.mu.Lock()
	c.requests = append(c.requests, recordedRequest{text: content.Text, images: images})
	call := len(c.requests)
	c.mu.Unlock()
	if c.onCall != nil {
		c.onCall(call)
	}
	return classification.Classification{
		ModelID: "image-model", Probabilities: map[string]float64{"spam": 0.1}, InputTokens: 10,
	}, nil
}

// imageRefs returns refs for the literal CIDs, owned by did:plc:author.
func imageRefs(cids ...string) []classification.ImageRef {
	refs := make([]classification.ImageRef, 0, len(cids))
	for _, cid := range cids {
		refs = append(refs, classification.ImageRef{OwnerDID: "did:plc:author", CID: cid})
	}
	return refs
}

var nineImageCIDs = []string{
	"bafkreiam6rl6essht4bp2tjukqbyt5za6cah3t7zfj2weeebmwzgg7vif4",
	"bafkreic2a4l4wzmwi2hkdx72qyar7gypjfzurwaeegbvwulzt6nowrkwii",
	"bafkreigz6mj254wzpzmn54crd7oboujnkptlgdkxrbqk4bfvfcggui4qca",
	"bafkreignszboyqeqxveflglf6fn2zsitwvbaqggttugmpynvt5fjev6jea",
	"bafkreibs3bxpxtsqtdekej5qlaibnecpyuem2l4hodzg25imgwxlkpki4q",
	"bafkreiflrixmd5zsynidmid5hbbvqpp7dbutahsxjqk3vxjd7agnawwrti",
	"bafkreiefw6crhntrbnyin6yj7soyffwuelh3mufzfn5fkxxvoakezu2ob4",
	"bafkreid3xxdiwiwfpcnu2qzfjmnvt7ati2wgi2m5ybqzi7qfceyzlwm4jm",
	"bafkreiaci2qzz32n3acyamowlft67prnxk7jlnsresykbrpxx2s3qywhom",
}

// A subject's images go out at most four to a request, in order, and every
// request carries the subject's whole text (R4).
func TestWorkerRunOnce_SplitsImagesIntoBatchesThatEachCarryTheText(t *testing.T) {
	t.Run("batches", func(t *testing.T) {
		for _, test := range []struct {
			name string
			refs []classification.ImageRef
			want [][]classification.ImageRef
		}{
			{
				name: "nine refs make batches of 4, 4 and 1",
				refs: imageRefs(nineImageCIDs...),
				want: [][]classification.ImageRef{
					imageRefs(nineImageCIDs[0], nineImageCIDs[1], nineImageCIDs[2], nineImageCIDs[3]),
					imageRefs(nineImageCIDs[4], nineImageCIDs[5], nineImageCIDs[6], nineImageCIDs[7]),
					imageRefs(nineImageCIDs[8]),
				},
			},
			{
				name: "four refs make one batch",
				refs: imageRefs(nineImageCIDs[0], nineImageCIDs[1], nineImageCIDs[2], nineImageCIDs[3]),
				want: [][]classification.ImageRef{
					imageRefs(nineImageCIDs[0], nineImageCIDs[1], nineImageCIDs[2], nineImageCIDs[3]),
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert.Equal(t, test.want, classification.ImageBatches(test.refs))
			})
		}
		t.Run("no refs make one empty, text-only batch", func(t *testing.T) {
			batches := classification.ImageBatches(nil)
			require.Len(t, batches, 1)
			assert.Empty(t, batches[0])
		})
	})

	t.Run("worker requests", func(t *testing.T) {
		repo := &deadlineRepository{
			job: classification.ClaimedJob{
				ID: 8, SubjectURI: "at://did:plc:author/social.coves.community.post/8",
				SubjectKind: classification.SubjectKindPost, RecordCID: "bafy8",
			},
			postImages: imageRefs(nineImageCIDs...),
		}
		classifier := &recordingClassifier{}
		worker := newClockedWorker(repo, classifier, time.Now, classification.WithImageFetcher(cidImageFetcher{}))

		require.NoError(t, worker.RunOnce(context.Background()))

		assert.Equal(t, []recordedRequest{
			{text: "Harbor seals", images: nineImageCIDs[0:4]},
			{text: "Harbor seals", images: nineImageCIDs[4:8]},
			{text: "Harbor seals", images: nineImageCIDs[8:9]},
		}, classifier.requests)
	})
}

// Each image batch is a request of its own and counts on the UTC day it is
// sent: a subject whose first batch crosses UTC midnight counts its second
// batch on the new day, and each batch's stored usage keeps its own send date
// (R2, R3).
func TestWorkerRunOnce_EachImageBatchIsCountedOnTheDayItIsSent(t *testing.T) {
	pacific := time.FixedZone("UTC-8", -8*60*60)
	// 2026-11-12 23:59 UTC.
	clock := &testClock{now: time.Date(2026, 11, 12, 15, 59, 0, 0, pacific)}
	repo := &deadlineRepository{
		job: classification.ClaimedJob{
			ID: 9, SubjectURI: "at://did:plc:author/social.coves.community.post/9",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy9",
		},
		postImages: imageRefs(nineImageCIDs[0:5]...),
	}
	classifier := &recordingClassifier{onCall: func(call int) {
		if call == 1 {
			clock.Advance(2 * time.Minute)
		}
	}}
	worker := newClockedWorker(repo, classifier, clock.Now, classification.WithImageFetcher(cidImageFetcher{}))

	require.NoError(t, worker.RunOnce(context.Background()))

	utcDates := func(instants []time.Time) []string {
		dates := make([]string, 0, len(instants))
		for _, instant := range instants {
			dates = append(dates, instant.UTC().Format(time.DateOnly))
		}
		return dates
	}
	assert.Equal(t, []string{"2026-11-12", "2026-11-13"}, utcDates(repo.reservedAt),
		"batch 2 was sent after UTC midnight and must be reserved on the new day")
	assert.Equal(t, []string{"2026-11-12", "2026-11-13"}, utcDates(repo.batchStoredAt),
		"each batch's usage must be stored on the day that batch was sent")
}

// PendingDeferralDelay doubles the configured deferral with each deferral
// in a row and stops growing at six hours (R2).
func TestPendingDeferralDelay(t *testing.T) {
	for _, test := range []struct {
		name      string
		base      time.Duration
		deferrals int
		want      time.Duration
	}{
		{name: "first deferral is the base", base: 10 * time.Minute, deferrals: 1, want: 10 * time.Minute},
		{name: "second doubles", base: 10 * time.Minute, deferrals: 2, want: 20 * time.Minute},
		{name: "third doubles again", base: 10 * time.Minute, deferrals: 3, want: 40 * time.Minute},
		{name: "sixth is under the cap", base: 10 * time.Minute, deferrals: 6, want: 320 * time.Minute},
		{name: "seventh is capped", base: 10 * time.Minute, deferrals: 7, want: 6 * time.Hour},
		{name: "a huge count is capped without overflow", base: 10 * time.Minute, deferrals: 1 << 30, want: 6 * time.Hour},
		{name: "zero counts as the first", base: 10 * time.Minute, deferrals: 0, want: 10 * time.Minute},
		{name: "a base above the cap is kept", base: 12 * time.Hour, deferrals: 4, want: 12 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, classification.PendingDeferralDelay(test.base, test.deferrals))
		})
	}
}

// A job awaiting admission is deferred by the backoff for its next deferral:
// one already deferred twice waits four times the configured deferral (R2).
func TestWorkerRunOnce_DeferralBacksOffByDeferralCount(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	repo := &deadlineRepository{
		job: classification.ClaimedJob{
			ID: 21, SubjectURI: "at://did:plc:author/social.coves.community.post/21",
			SubjectKind: classification.SubjectKindPost, RecordCID: "bafy21", DeferralCount: 2,
		},
		postVisibility: classification.VisibilityAwaitingAdmission,
	}
	worker := newClockedWorker(repo, &recordingClassifier{}, clock.Now)

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.Equal(t, []time.Time{time.Date(2026, 11, 12, 12, 4, 0, 0, time.UTC)}, repo.deferredUntil,
		"the third deferral waits 4 x PendingDeferral")
}

// requestFailureRecord returns the "classification request failed" record.
func requestFailureRecord(t *testing.T, records []slog.Record) slog.Record {
	t.Helper()
	for _, record := range records {
		if record.Message == "classification request failed" {
			return record
		}
	}
	t.Fatal("no classification request failed record")
	return slog.Record{}
}

// A failed request logs the provider's HTTP status, so a provider_error can
// be told apart; the failure that ends the job is an Error, a retry a Warn.
// A final failure with stored batches stores them as a partial result, the
// images never sent counted skipped, and discards nothing; one with no stored
// batch stores nothing.
func TestWorkerRunOnce_RequestFailureLogsHTTPStatusAndFinalAtError(t *testing.T) {
	for _, test := range []struct {
		name          string
		attemptCount  int
		storedBatches int
		wantLevel     slog.Level
		wantPartial   bool
	}{
		{name: "retry", attemptCount: 0, storedBatches: 1, wantLevel: slog.LevelWarn},
		{name: "final", attemptCount: 2, storedBatches: 1, wantLevel: slog.LevelError, wantPartial: true},
		{name: "final with no stored batch", attemptCount: 2, wantLevel: slog.LevelError},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &deadlineRepository{
				job: classification.ClaimedJob{
					ID: 22, SubjectURI: "at://did:plc:author/social.coves.community.post/22",
					SubjectKind: classification.SubjectKindPost, RecordCID: "bafy22", AttemptCount: test.attemptCount,
				},
				postImages: imageRefs(nineImageCIDs[0:5]...),
			}
			for range test.storedBatches {
				repo.batches = append(repo.batches, classification.BatchProgress{
					Index: 0, ModelID: "image-model", Probabilities: map[string]float64{"spam": 0.1}, ImagesSent: 4,
				})
			}
			handler := &recordingHandler{}
			worker := classification.NewWorker(repo,
				sentinelClassifier{err: &classification.ProviderError{Code: classification.ErrorCodeProviderError, HTTPStatus: 503}},
				classification.Policy{Version: "v1"}, classification.WorkerConfig{
					Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
					Concurrency:     1,
					BatchSize:       1,
					TextCharLimit:   8000,
					AttemptLimit:    3,
					RetryBaseDelay:  time.Minute,
					PendingDeferral: time.Minute,
					DailyRequestCap: 100,
				}, time.Now, slog.New(handler), classification.WithImageFetcher(cidImageFetcher{}))

			require.NoError(t, worker.RunOnce(context.Background()))

			record := requestFailureRecord(t, handler.records)
			assert.Equal(t, test.wantLevel, record.Level)
			assert.Equal(t, "503", attributes(record)["http_status"])
			assert.Equal(t, classification.ErrorCodeProviderError, attributes(record)["error_code"])
			var discards []slog.Record
			for _, logged := range handler.records {
				if attributes(logged)["code"] == "batches_discarded" {
					discards = append(discards, logged)
				}
			}
			assert.Empty(t, discards, "no discard is logged")
			assert.Empty(t, repo.results, "a failed job stores no complete result")
			if !test.wantPartial {
				assert.Empty(t, repo.partialResults, "no partial result is stored")
				return
			}
			require.Len(t, repo.partialResults, 1, "the stored batch becomes a partial result")
			assert.Equal(t, classification.Result{
				ModelID:       "image-model",
				Probabilities: map[string]float64{"spam": 0.1},
				ImagesSent:    4,
				ImagesSkipped: 1,
				Partial:       true,
			}, repo.partialResults[0], "the unsent fifth image counts skipped")
			assert.Equal(t, []classification.Failure{repo.partialFailures[0]}, repo.partialFailures)
			assert.True(t, repo.partialFailures[0].Final)
			assert.Equal(t, classification.ErrorCodeProviderError, repo.partialFailures[0].ErrorCode)
			assert.Empty(t, repo.failures, "the failure is recorded with the partial result")
			assert.Empty(t, repo.batches, "storing the partial result discarded the stored batches")
			var stored []slog.Record
			for _, logged := range handler.records {
				if attributes(logged)["code"] == "partial_result_stored" {
					stored = append(stored, logged)
				}
			}
			assert.Len(t, stored, 1, "the partial result is logged once with its code")
		})
	}
}
