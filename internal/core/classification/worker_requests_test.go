package classification_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/internal/core/imageproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingImageFetcher refuses the CIDs in failures with their error and
// serves every other image as cidImageFetcher does.
type failingImageFetcher struct {
	failures map[string]error
}

func (f failingImageFetcher) FetchImage(ctx context.Context, ref classification.ImageRef) (classification.Image, error) {
	if err, ok := f.failures[ref.CID]; ok {
		return classification.Image{}, err
	}
	return cidImageFetcher{}.FetchImage(ctx, ref)
}

// hookImageFetcher runs onFetch before serving each image as cidImageFetcher
// does.
type hookImageFetcher struct {
	onFetch func()
}

func (f hookImageFetcher) FetchImage(ctx context.Context, ref classification.ImageRef) (classification.Image, error) {
	f.onFetch()
	return cidImageFetcher{}.FetchImage(ctx, ref)
}

func imagePostJob(id int64) classification.ClaimedJob {
	return classification.ClaimedJob{
		ID: id, SubjectURI: fmt.Sprintf("at://did:plc:author/social.coves.community.post/%d", id),
		SubjectKind: classification.SubjectKindPost, RecordCID: fmt.Sprintf("bafy%d", id),
	}
}

// Without an image fetcher no image can be sent, so a subject with images is
// one text-only request, not one identical request per image batch, and
// every image is counted skipped (R4, R5).
func TestWorkerRunOnce_WithoutImageFetcherSendsOneTextRequest(t *testing.T) {
	repo := &deadlineRepository{
		job:               imagePostJob(20),
		postImages:        imageRefs(nineImageCIDs...),
		postImagesSkipped: 1,
	}
	classifier := &recordingClassifier{}
	worker := newClockedWorker(repo, classifier, time.Now)

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.Equal(t, []recordedRequest{{text: "Harbor seals", images: []string{}}}, classifier.requests,
		"a subject whose images cannot be sent is one text-only request")
	assert.Len(t, repo.reservedAt, 1, "only the one request may use the daily cap")
	require.Len(t, repo.results, 1)
	assert.Equal(t, 10, repo.results[0].ImagesSkipped, "the nine images and the subject's own skipped slot")
	assert.Equal(t, 0, repo.results[0].ImagesSent)
	assert.Equal(t, "text-model", repo.results[0].ModelID)
	assert.Equal(t, map[string]float64{"spam": 0.1}, repo.results[0].Probabilities)
}

// A later batch none of whose images could be fetched would only repeat the
// text already answered, so it sends no request; its images still count
// skipped (R4, R5).
func TestWorkerRunOnce_LaterBatchWithNoFetchedImageSendsNoRequest(t *testing.T) {
	t.Run("images refused in this run", func(t *testing.T) {
		repo := &deadlineRepository{job: imagePostJob(21), postImages: imageRefs(nineImageCIDs[0:6]...)}
		classifier := &recordingClassifier{}
		worker := newClockedWorker(repo, classifier, time.Now, classification.WithImageFetcher(failingImageFetcher{failures: map[string]error{
			nineImageCIDs[4]: imageproxy.ErrBlobBlocked,
			nineImageCIDs[5]: errors.New("connection refused"),
		}}))

		require.NoError(t, worker.RunOnce(context.Background()))

		assert.Equal(t, []recordedRequest{{text: "Harbor seals", images: nineImageCIDs[0:4]}}, classifier.requests)
		assert.Len(t, repo.reservedAt, 1, "the empty batch may not use the daily cap")
		require.Len(t, repo.results, 1)
		assert.Equal(t, 4, repo.results[0].ImagesSent)
		assert.Equal(t, 2, repo.results[0].ImagesSkipped)
		assert.Equal(t, "image-model", repo.results[0].ModelID)
	})

	t.Run("first batch stored by an earlier run, fetcher now off", func(t *testing.T) {
		repo := &deadlineRepository{
			job:        imagePostJob(22),
			postImages: imageRefs(nineImageCIDs...),
			batches: []classification.BatchProgress{{
				Index: 0, ModelID: "image-model", Probabilities: map[string]float64{"spam": 0.7},
				ImagesSent: 4, InputTokens: 10,
			}},
		}
		classifier := &recordingClassifier{}
		worker := newClockedWorker(repo, classifier, time.Now)

		require.NoError(t, worker.RunOnce(context.Background()))

		assert.Empty(t, classifier.requests, "the stored batch already answered the text")
		assert.Empty(t, repo.reservedAt)
		require.Len(t, repo.results, 1)
		assert.Equal(t, 4, repo.results[0].ImagesSent)
		assert.Equal(t, 5, repo.results[0].ImagesSkipped)
		assert.Equal(t, "image-model", repo.results[0].ModelID)
		assert.Equal(t, map[string]float64{"spam": 0.7}, repo.results[0].Probabilities)
	})
}

// The subject is checked after a batch's images are fetched, immediately
// before its request: a subject hidden or edited while its images were being
// read is not sent (R2).
func TestWorkerRunOnce_SubjectChangedDuringImageFetchSendsNoRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*deadlineRepository)
		reason string
	}{
		{
			name:   "hidden",
			change: func(r *deadlineRepository) { r.postVisibility = classification.VisibilityHidden },
			reason: classification.CloseReasonNotVisible,
		},
		{
			name:   "edited",
			change: func(r *deadlineRepository) { r.postCID = "bafyedited" },
			reason: classification.CloseReasonSuperseded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &deadlineRepository{job: imagePostJob(23), postImages: imageRefs(nineImageCIDs[0:2]...)}
			classifier := &recordingClassifier{}
			fetcher := hookImageFetcher{onFetch: func() {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				test.change(repo)
			}}
			worker := newClockedWorker(repo, classifier, time.Now, classification.WithImageFetcher(fetcher))

			require.NoError(t, worker.RunOnce(context.Background()))

			assert.Empty(t, classifier.requests, "the changed subject was sent")
			assert.Empty(t, repo.reservedAt)
			assert.Empty(t, repo.results)
			assert.Empty(t, repo.batches)
			assert.Equal(t, []string{test.reason}, repo.closeReasons)
		})
	}
}

// A request the cycle deadline would cut off is not started: the claim is
// given back with no reservation and no failure, so attempt_count is not
// spent on the run's own deadline (R2).
func TestWorkerRunOnce_TooLittleCycleLeftForARequestReleasesTheClaim(t *testing.T) {
	repo := &deadlineRepository{job: imagePostJob(24)}
	classifier := &recordingClassifier{}
	worker := newConfiguredWorker(repo, classifier, time.Now, func(cfg *classification.WorkerConfig) {
		cfg.RequestTimeout = 2 * time.Hour
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	require.NoError(t, worker.RunOnce(ctx))

	assert.Empty(t, classifier.requests)
	assert.Empty(t, repo.reservedAt)
	assert.Empty(t, repo.failures)
	assert.Empty(t, repo.results)
	assert.Equal(t, []int64{24}, repo.released)
}

// With enough of the cycle left the request goes out as usual.
func TestWorkerRunOnce_EnoughCycleLeftForARequestSendsIt(t *testing.T) {
	repo := &deadlineRepository{job: imagePostJob(25)}
	classifier := &recordingClassifier{}
	worker := newConfiguredWorker(repo, classifier, time.Now, func(cfg *classification.WorkerConfig) {
		cfg.RequestTimeout = time.Minute
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	require.NoError(t, worker.RunOnce(ctx))

	assert.Len(t, classifier.requests, 1)
	assert.Len(t, repo.results, 1)
	assert.Empty(t, repo.released)
}

// recordingHandler is a slog.Handler that keeps every record.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// attributes returns a record's attributes as strings by key.
func attributes(record slog.Record) map[string]string {
	values := map[string]string{}
	record.Attrs(func(attribute slog.Attr) bool {
		values[attribute.Key] = attribute.Value.String()
		return true
	})
	return values
}

// An image refused by moderation or the blob index is expected and logged at
// Info; any other fetch failure is a Warn with a code, so a broken image path
// is visible. Neither log names the image owner or the raw error.
func TestWorkerRunOnce_LogsImageFetchFailuresByClass(t *testing.T) {
	repo := &deadlineRepository{job: imagePostJob(26), postImages: imageRefs(nineImageCIDs[0:2]...)}
	handler := &recordingHandler{}
	worker := classification.NewWorker(repo, &recordingClassifier{}, classification.Policy{Version: "v1"}, classification.WorkerConfig{
		Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
		TextCharLimit:   8000,
		AttemptLimit:    3,
		DailyRequestCap: 100,
	}, time.Now, slog.New(handler), classification.WithImageFetcher(failingImageFetcher{failures: map[string]error{
		nineImageCIDs[0]: fmt.Errorf("fetching image through the image proxy: %w", imageproxy.ErrBlobBlocked),
		nineImageCIDs[1]: errors.New("resolving image owner did:plc:author: no such host"),
	}}))

	require.NoError(t, worker.RunOnce(context.Background()))

	var skipLogs []slog.Record
	for _, record := range handler.records {
		if attributes(record)["error_class"] != "" {
			skipLogs = append(skipLogs, record)
		}
	}
	require.Len(t, skipLogs, 2)
	assert.Equal(t, slog.LevelInfo, skipLogs[0].Level)
	assert.Equal(t, "blob_blocked", attributes(skipLogs[0])["error_class"])
	assert.Equal(t, slog.LevelWarn, skipLogs[1].Level)
	assert.Equal(t, "classification_image_fetch_failed", attributes(skipLogs[1])["code"])
	assert.Equal(t, "other", attributes(skipLogs[1])["error_class"])
	for _, record := range skipLogs {
		for key, value := range attributes(record) {
			assert.NotContains(t, value, "did:plc:author", "log attribute %q names the image owner", key)
		}
		assert.NotContains(t, record.Message, "did:plc:author")
	}
}

// recordingImageFetcher records every CID it is asked for, then refuses the
// CIDs in failures as failingImageFetcher does and serves the rest.
type recordingImageFetcher struct {
	mu       sync.Mutex
	fetched  []string
	failures map[string]error
}

func (f *recordingImageFetcher) FetchImage(ctx context.Context, ref classification.ImageRef) (classification.Image, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, ref.CID)
	f.mu.Unlock()
	return failingImageFetcher{failures: f.failures}.FetchImage(ctx, ref)
}

// failingCallClassifier answers as recordingClassifier does, except that its
// failCall-th call (1-based) fails.
type failingCallClassifier struct {
	recordingClassifier
	failCall int
}

func (c *failingCallClassifier) Classify(ctx context.Context, content classification.Content, questions []classification.Question) (classification.Classification, error) {
	answer, err := c.recordingClassifier.Classify(ctx, content, questions)
	c.mu.Lock()
	call := len(c.requests)
	c.mu.Unlock()
	if call == c.failCall {
		return classification.Classification{}, errors.New("provider unavailable")
	}
	return answer, err
}

// A middle batch none of whose images could be fetched is stored with its
// skipped count and no request, so a run cut off at a later batch does not
// fetch it again: an unreachable PDS would otherwise spend the run's fetch
// time on it every lease (R4, R5).
func TestWorkerRunOnce_SkippedOnlyMiddleBatchIsStoredAndNotFetchedAgain(t *testing.T) {
	unreachable := map[string]error{}
	for _, cid := range nineImageCIDs[4:8] {
		unreachable[cid] = errors.New("connection refused")
	}
	repo := &deadlineRepository{job: imagePostJob(30), postImages: imageRefs(nineImageCIDs...)}
	classifier := &failingCallClassifier{failCall: 2}
	firstFetcher := &recordingImageFetcher{failures: unreachable}
	worker := newClockedWorker(repo, classifier, time.Now, classification.WithImageFetcher(firstFetcher))

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.Equal(t, []recordedRequest{
		{text: "Harbor seals", images: nineImageCIDs[0:4]},
		{text: "Harbor seals", images: nineImageCIDs[8:9]},
	}, classifier.requests, "the skipped-only batch sends no request")
	require.Len(t, repo.failures, 1, "the final batch's request failed")
	assert.Len(t, repo.reservedAt, 2, "the skipped-only batch may not use the daily cap")
	require.Len(t, repo.batches, 2, "the answered batch and the skipped-only batch are stored")
	assert.Equal(t, 1, repo.batches[1].Index)
	assert.Equal(t, 0, repo.batches[1].ImagesSent)
	assert.Equal(t, 4, repo.batches[1].ImagesSkipped)
	assert.Empty(t, repo.batches[1].Probabilities)
	assert.Equal(t, 0, repo.batches[1].InputTokens)
	assert.Empty(t, repo.results)

	retryClassifier := &recordingClassifier{}
	retryFetcher := &recordingImageFetcher{failures: unreachable}
	retryWorker := newClockedWorker(repo, retryClassifier, time.Now, classification.WithImageFetcher(retryFetcher))

	require.NoError(t, retryWorker.RunOnce(context.Background()))

	assert.Equal(t, nineImageCIDs[8:9], retryFetcher.fetched, "the next run starts at the unfinished final batch")
	assert.Equal(t, []recordedRequest{{text: "Harbor seals", images: nineImageCIDs[8:9]}}, retryClassifier.requests)
	require.Len(t, repo.results, 1)
	assert.Equal(t, 5, repo.results[0].ImagesSent)
	assert.Equal(t, 4, repo.results[0].ImagesSkipped)
	assert.Equal(t, "image-model", repo.results[0].ModelID)
	assert.Equal(t, map[string]float64{"spam": 0.1}, repo.results[0].Probabilities)
}

// A final batch that fetches no image still rechecks the subject: one hidden
// or edited during the earlier request or the final batch's fetches is
// closed, with no result and its stored batches discarded (R2, R4).
func TestWorkerRunOnce_SubjectChangedBeforeSkippedOnlyBatchClosesTheJob(t *testing.T) {
	blocked := map[string]error{nineImageCIDs[4]: imageproxy.ErrBlobBlocked}
	for _, test := range []struct {
		name   string
		change func(*deadlineRepository)
		reason string
		// during names when the subject changes: in the first request, or
		// in the final batch's fetch.
		during string
	}{
		{
			name:   "hidden during the first request",
			change: func(r *deadlineRepository) { r.postVisibility = classification.VisibilityHidden },
			reason: classification.CloseReasonNotVisible,
			during: "request",
		},
		{
			name:   "edited during the first request",
			change: func(r *deadlineRepository) { r.postCID = "bafyedited" },
			reason: classification.CloseReasonSuperseded,
			during: "request",
		},
		{
			name:   "hidden during the final batch's fetch",
			change: func(r *deadlineRepository) { r.postVisibility = classification.VisibilityHidden },
			reason: classification.CloseReasonNotVisible,
			during: "fetch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &deadlineRepository{job: imagePostJob(31), postImages: imageRefs(nineImageCIDs[0:5]...)}
			change := func() {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				test.change(repo)
			}
			classifier := &recordingClassifier{}
			var fetcher classification.ImageFetcher = failingImageFetcher{failures: blocked}
			if test.during == "request" {
				classifier.onCall = func(call int) {
					if call == 1 {
						change()
					}
				}
			} else {
				fetcher = changingImageFetcher{changeOn: nineImageCIDs[4], change: change, failures: blocked}
			}
			worker := newClockedWorker(repo, classifier, time.Now, classification.WithImageFetcher(fetcher))

			require.NoError(t, worker.RunOnce(context.Background()))

			assert.Equal(t, []recordedRequest{{text: "Harbor seals", images: nineImageCIDs[0:4]}}, classifier.requests)
			assert.Equal(t, []string{test.reason}, repo.closeReasons)
			assert.Empty(t, repo.results, "a closed subject stores no result")
			assert.Empty(t, repo.batches, "closing the job discards its batches")
		})
	}
}

// changingImageFetcher runs change when asked for changeOn, then serves or
// refuses images as failingImageFetcher does.
type changingImageFetcher struct {
	changeOn string
	change   func()
	failures map[string]error
}

func (f changingImageFetcher) FetchImage(ctx context.Context, ref classification.ImageRef) (classification.Image, error) {
	if ref.CID == f.changeOn {
		f.change()
	}
	return failingImageFetcher{failures: f.failures}.FetchImage(ctx, ref)
}

// blockingImageFetcher holds every fetch until its context ends, like a PDS
// that hangs instead of refusing.
type blockingImageFetcher struct{}

func (blockingImageFetcher) FetchImage(ctx context.Context, _ classification.ImageRef) (classification.Image, error) {
	<-ctx.Done()
	return classification.Image{}, ctx.Err()
}

// The first batch's fetches are bounded so its request still fits in the
// cycle: four images on a hanging PDS, fetched one after another, would
// otherwise use up the cycle, release the claim with nothing stored, and
// repeat every run without the text ever being classified. Images cut off by
// that bound count skipped and the text goes out alone (R4, R5).
func TestWorkerRunOnce_HangingFirstBatchFetchesLeaveTimeForTheTextRequest(t *testing.T) {
	const requestTimeout = 500 * time.Millisecond
	// The production cycle: four requests plus a margin of one request.
	const cycleTimeout = 4*requestTimeout + requestTimeout
	repo := &deadlineRepository{job: imagePostJob(40), postImages: imageRefs(nineImageCIDs[0:4]...)}
	classifier := &recordingClassifier{}
	worker := newConfiguredWorker(repo, classifier, time.Now, func(cfg *classification.WorkerConfig) {
		cfg.RequestTimeout = requestTimeout
	}, classification.WithImageFetcher(blockingImageFetcher{}))

	ctx, cancel := context.WithTimeout(context.Background(), cycleTimeout)
	defer cancel()
	require.NoError(t, worker.RunOnce(ctx))

	assert.Empty(t, repo.released, "the claim was given back with nothing stored")
	assert.Equal(t, []recordedRequest{{text: "Harbor seals", images: []string{}}}, classifier.requests)
	require.Len(t, repo.results, 1)
	assert.Equal(t, 0, repo.results[0].ImagesSent)
	assert.Equal(t, 4, repo.results[0].ImagesSkipped)
	assert.Equal(t, "text-model", repo.results[0].ModelID)
}
