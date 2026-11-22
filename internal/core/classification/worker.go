package classification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/imageproxy"
)

// ClaimLease is how long a claimed job stays unclaimable while the worker
// processes it. It must outlast a whole run, whose deadline the caller sets, so
// a job is never claimed twice while the run that claimed it still holds it; a
// worker that dies holding a claim gives the job back when the lease runs out.
const ClaimLease = 5 * time.Minute

// stateWriteTimeout bounds each write of a job's state that must happen even
// when the run's context is done: a request's outcome, which the daily cap
// already counted, and the release of a claim the run will not process.
const stateWriteTimeout = 10 * time.Second

// codeDailyCapReached is the log code of a run stopped by the daily request
// cap.
const codeDailyCapReached = "daily_cap_reached"

// codeBatchesDiscarded is the log code of a final failure that discarded the
// job's stored image batches.
const codeBatchesDiscarded = "batches_discarded"

// codePartialResultStored is the log code of a final failure whose answered
// batches were stored as a partial result.
const codePartialResultStored = "partial_result_stored"

// maxPendingDeferral is the longest the deferral backoff grows to, unless
// the configured PendingDeferral is itself longer.
const maxPendingDeferral = 6 * time.Hour

// PendingDeferralDelay is the wait of a job's deferrals-th deferral in a row
// awaiting admission: base doubled per earlier deferral, at most six hours
// or base when base is longer. A count below 1 is the first deferral.
func PendingDeferralDelay(base time.Duration, deferrals int) time.Duration {
	limit := max(base, maxPendingDeferral)
	delay := base
	for i := 1; i < deferrals && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}

// codeImageFetchFailed is the log code of an image that could not be fetched
// for a reason other than an expected refusal.
const codeImageFetchFailed = "classification_image_fetch_failed"

// Log codes of a failed run or job, by the step that failed.
const (
	codeClaimFailed         = "claim_failed"
	codeLoadSubjectFailed   = "load_subject_failed"
	codeLoadProgressFailed  = "load_progress_failed"
	codeSettleFailed        = "settle_failed"
	codeReserveFailed       = "reserve_failed"
	codeStoreBatchFailed    = "store_batch_failed"
	codeStoreFailed         = "store_failed"
	codeReleaseFailed       = "release_failed"
	codeRecordFailureFailed = "record_failure_failed"
	codeTimeout             = "timeout"
	codeCanceled            = "canceled"
	codeJobFailed           = "job_failed"
)

// stepError marks an error with the fixed log code of the step that failed.
type stepError struct {
	code string
	err  error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// stepFailed marks err, when not nil, with code.
func stepFailed(code string, err error) error {
	if err == nil {
		return nil
	}
	return &stepError{code: code, err: err}
}

// failureCode is the fixed log code of a run or job error: a context
// timeout or cancellation, else the code of the step that failed.
func failureCode(err error) string {
	var step *stepError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return codeTimeout
	case errors.Is(err, context.Canceled):
		return codeCanceled
	case errors.As(err, &step):
		return step.code
	default:
		return codeJobFailed
	}
}

// failureLevel is the log level of a failure code: a run cancelled by
// shutdown is a Warn, every other failure an Error.
func failureLevel(code string) slog.Level {
	if code == codeCanceled {
		return slog.LevelWarn
	}
	return slog.LevelError
}

// WorkerConfig configures the classification worker.
type WorkerConfig struct {
	Evaluation      Evaluation
	Concurrency     int
	BatchSize       int
	TextCharLimit   int
	AttemptLimit    int
	RetryBaseDelay  time.Duration
	PendingDeferral time.Duration
	DailyRequestCap int
	// RequestTimeout is the longest one classifier request may take. A request
	// is not started when less than this remains before the run's deadline, so
	// the deadline does not cut it off and count it as a failed attempt. Zero
	// starts every request whatever the deadline.
	RequestTimeout time.Duration
}

// Worker classifies due jobs from the queue.
type Worker struct {
	repo       Repository
	classifier Classifier
	policy     Policy
	cfg        WorkerConfig
	now        func() time.Time
	logger     *slog.Logger
	// imageFetcher reads subject images; nil when the image proxy is off.
	imageFetcher ImageFetcher
	// scoreAlertThresholds are the thresholds a stored result raises score
	// alerts for, and scoreAlertNotifier is told after such a result commits.
	scoreAlertThresholds []ScoreAlertThreshold
	scoreAlertNotifier   ScoreAlertNotifier
	// authPause stops claiming while the provider refuses the API token.
	authPause *ProviderAuthPause

	// capLoggedDate is the UTC date the daily cap was last logged, so the cap
	// is logged once per day however many runs it stops.
	capLogMu      sync.Mutex
	capLoggedDate string
}

// NewWorker returns a classification worker.
func NewWorker(repo Repository, classifier Classifier, policy Policy, cfg WorkerConfig, now func() time.Time, logger *slog.Logger, opts ...WorkerOption) *Worker {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 1
	}
	worker := &Worker{
		repo:       repo,
		classifier: classifier,
		policy:     policy,
		cfg:        cfg,
		now:        now,
		logger:     logger,
	}
	for _, option := range opts {
		option(worker)
	}
	if worker.authPause == nil {
		worker.authPause = NewProviderAuthPause()
	}
	return worker
}

// RunOnce claims up to BatchSize due jobs and processes them, Concurrency at
// a time. Each job's subject is checked before each of its requests: a changed CID
// supersedes the job, a missing or hidden subject closes it, and one awaiting
// admission is deferred. When the daily request cap is reached the run stops
// and every claimed job not yet processed is given back unchanged; so it is
// when the provider refuses the API token, which also pauses claiming
// (ProviderAuthPause). A paused worker claims nothing. A failure of one job
// does not stop the others; RunOnce returns them joined.
func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.now()
	if _, paused := w.authPause.PausedUntil(now); paused {
		return nil
	}
	jobs, err := w.repo.ClaimDue(ctx, w.cfg.Evaluation, now, w.cfg.BatchSize, ClaimLease)
	if err != nil {
		code := failureCode(stepFailed(codeClaimFailed, err))
		w.logger.Log(ctx, failureLevel(code), "classification claim failed", slog.String("error_code", code))
		return fmt.Errorf("claiming classification jobs: %w", err)
	}

	var (
		mu       sync.Mutex
		next     int
		stopped  bool
		failures []error
		wg       sync.WaitGroup
	)
	// take hands out the next claimed job, and whether the daily cap or a
	// provider auth pause stopped the run.
	take := func() (ClaimedJob, bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(jobs) {
			return ClaimedJob{}, false, false
		}
		job := jobs[next]
		next++
		return job, stopped, true
	}
	record := func(job ClaimedJob, err error) {
		mu.Lock()
		defer mu.Unlock()
		if errors.Is(err, errDailyCapReached) || errors.Is(err, errProviderAuthPaused) {
			stopped = true
			if err == errDailyCapReached || err == errProviderAuthPaused {
				return
			}
			// The stop came with another failure, such as a claim that
			// could not be given back, which is logged and returned.
		}
		// The error can wrap subject detail, so only its fixed code is logged.
		code := failureCode(err)
		w.logger.Log(ctx, failureLevel(code), "classification job failed",
			slog.Int64("job_id", job.ID),
			slog.String("error_code", code))
		failures = append(failures, fmt.Errorf("classification job %d: %w", job.ID, err))
	}

	workers := min(w.cfg.Concurrency, len(jobs))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, stopped, ok := take()
				if !ok {
					return
				}
				// A job claimed but not reached before the run's context ended
				// is given back now rather than left leased until ClaimLease.
				if stopped || ctx.Err() != nil {
					if err := w.releaseClaim(ctx, job); err != nil {
						record(job, err)
					}
					continue
				}
				if err := w.process(ctx, job); err != nil {
					record(job, err)
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(failures...)
}

// errDailyCapReached stops a run: the job was released, and so must be every
// job after it.
var errDailyCapReached = errors.New("classification daily request cap reached")

// errProviderAuthPaused stops a run like errDailyCapReached: the provider
// refused the API token, or the worker is paused because it did.
var errProviderAuthPaused = errors.New("classification paused: provider refused the API token")

// requestFailedError is a classifier request to modelID that failed and has
// not been recorded yet; process records it with the job's progress.
type requestFailedError struct {
	err     error
	modelID string
}

func (e *requestFailedError) Error() string { return e.err.Error() }
func (e *requestFailedError) Unwrap() error { return e.err }

// subject is a job's loaded subject: its visibility, current CID, text
// content and images.
type subject struct {
	visibility    SubjectVisibility
	cid           string
	content       Content
	imageRefs     []ImageRef
	imagesSkipped int
}

// process takes one claimed job to its next state. The subject's images go
// out in batches of at most MaxImagesPerRequest, one request each, every one
// carrying the subject's text; a subject without images, or whose images
// cannot be fetched because the worker has no image fetcher, is one
// text-only request. A later batch none of whose images could be fetched
// sends no request, since its text is already answered; its images count
// skipped. Completed batches, skipped-only ones included, are stored as they
// finish, so a run stopped by the cap, a failed request or the cycle deadline
// continues at the first unfinished batch. The subject is checked before the
// first batch and again after each batch's image fetches; after the
// last batch the result is the per-question maximum across them.
func (w *Worker) process(ctx context.Context, job ClaimedJob) error {
	current, err := w.loadSubject(ctx, &job)
	if err != nil {
		return err
	}
	if settled, err := w.settleUnclassifiable(ctx, job, current); settled {
		return err
	}

	progress, err := w.repo.LoadBatchProgress(ctx, job.ID)
	if err != nil {
		return stepFailed(codeLoadProgressFailed, err)
	}
	finished := make(map[int]bool, len(progress))
	answered := false
	for _, batch := range progress {
		finished[batch.Index] = true
		answered = true
	}

	batches := ImageBatches(current.imageRefs)
	unsendable := 0
	if w.imageFetcher == nil {
		// No image can be sent: every unfinished batch would be the same
		// text-only request, so the subject is one text batch.
		for index, refs := range batches {
			if !finished[index] {
				unsendable += len(refs)
			}
		}
		batches = ImageBatches(nil)
		if answered {
			// An earlier run's batch already answered the text.
			batches = nil
		}
	}

	for index, refs := range batches {
		if finished[index] {
			continue
		}
		batch, stored, err := w.classifyBatch(ctx, job, index, refs, current.content, answered)
		var failed *requestFailedError
		if errors.As(err, &failed) {
			return w.handleRequestFailure(ctx, job, current, progress, failed.modelID, failed.err)
		}
		if err != nil || !stored {
			return err
		}
		progress = append(progress, batch)
		answered = true
	}
	if unsendable > 0 {
		progress = append(progress, BatchProgress{ImagesSkipped: unsendable})
	}
	return w.storeResult(ctx, job, current, progress)
}

// settleUnclassifiable ends or defers a job whose subject may not be
// classified now, reporting whether it did: a missing or hidden subject
// closes the job, a changed CID supersedes it, and one awaiting admission is
// deferred. Closing a job discards its stored batches.
func (w *Worker) settleUnclassifiable(ctx context.Context, job ClaimedJob, current subject) (bool, error) {
	switch {
	case current.visibility == VisibilityMissing:
		return true, stepFailed(codeSettleFailed, w.repo.Close(ctx, job.ID, CloseReasonNotVisible))
	case current.cid != job.RecordCID:
		return true, stepFailed(codeSettleFailed, w.repo.Close(ctx, job.ID, CloseReasonSuperseded))
	case current.visibility == VisibilityAwaitingAdmission:
		delay := PendingDeferralDelay(w.cfg.PendingDeferral, job.DeferralCount+1)
		return true, stepFailed(codeSettleFailed, w.repo.Defer(ctx, job.ID, w.now().Add(delay)))
	case current.visibility != VisibilityVisible:
		return true, stepFailed(codeSettleFailed, w.repo.Close(ctx, job.ID, CloseReasonNotVisible))
	}
	return false, nil
}

// classifyBatch fetches one batch's images, then sends its request and
// stores its progress, reporting whether the batch is done. Images that
// cannot be fetched are counted skipped; a batch with no fetched image is a
// text-only request to the text model, or, when answered reports the text
// already answered, no request at all: the batch is stored with only its
// skipped count, no model and no answer, so a later run does not fetch it
// again. A batch with image refs reloads and checks the subject after the
// fetches, before its request or its skipped-only store. No
// request is started with less than RequestTimeout left before the run's
// deadline, and the fetches are bounded so that much is left (fetchContext). The clock is read for this batch, so its request is counted on
// the UTC day it is sent. A false report with a nil error means the job is
// done with for this run: given back by the cycle deadline, settled by the
// subject check, failed and recorded, or ended while the request was in
// flight.
func (w *Worker) classifyBatch(ctx context.Context, job ClaimedJob, index int, refs []ImageRef, content Content, answered bool) (BatchProgress, bool, error) {
	if ctx.Err() != nil {
		// The previous batch's request may have outlived the run; the claim
		// is given back with the stored batches kept.
		return BatchProgress{}, false, w.releaseClaim(ctx, job)
	}
	fetchCtx, cancelFetch, fits := w.fetchContext(ctx)
	if !fits {
		// No fetch could finish in time for the request to fit.
		return BatchProgress{}, false, w.releaseClaim(ctx, job)
	}
	images, skipped := w.fetchImages(fetchCtx, job, refs)
	cancelFetch()
	if ctx.Err() != nil {
		// Fetches cut off by the run's own deadline are not skipped images.
		return BatchProgress{}, false, w.releaseClaim(ctx, job)
	}
	if len(refs) > 0 {
		// The fetches took time in which the subject may have changed.
		current, err := w.loadSubject(ctx, &job)
		if err != nil {
			return BatchProgress{}, false, err
		}
		if settled, err := w.settleUnclassifiable(ctx, job, current); settled {
			return BatchProgress{}, false, err
		}
		content = current.content
	}
	if len(images) == 0 && answered {
		// The text is already answered, so the batch sends no request; it is
		// stored so a run cut off at a later batch does not fetch it again.
		batch := BatchProgress{Index: index, Probabilities: map[string]float64{}, ImagesSkipped: skipped}
		stored, err := w.repo.StoreBatchProgress(ctx, job.ID, batch, w.now())
		if err != nil {
			return BatchProgress{}, false, stepFailed(codeStoreBatchFailed, err)
		}
		if !stored {
			w.logJobEnded(job)
		}
		return batch, stored, nil
	}
	if w.cfg.RequestTimeout > 0 {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < w.cfg.RequestTimeout {
			// The deadline would cut the request off and count it a failure.
			return BatchProgress{}, false, w.releaseClaim(ctx, job)
		}
	}

	now := w.now()
	if _, paused := w.authPause.PausedUntil(now); paused {
		// Another request of this run had the token refused.
		if err := w.releaseClaim(ctx, job); err != nil {
			return BatchProgress{}, false, err
		}
		return BatchProgress{}, false, errProviderAuthPaused
	}
	modelID := w.cfg.Evaluation.TextModelID
	if len(images) > 0 {
		modelID = w.cfg.Evaluation.ImageModelID
	}
	reserved, err := w.repo.ReserveRequest(ctx, modelID, now, w.cfg.DailyRequestCap)
	if err != nil {
		return BatchProgress{}, false, stepFailed(codeReserveFailed, err)
	}
	if !reserved {
		if err := w.releaseClaim(ctx, job); err != nil {
			return BatchProgress{}, false, err
		}
		w.logDailyCapReached(now, modelID)
		return BatchProgress{}, false, errDailyCapReached
	}

	content.Images = images
	answer, err := w.classifier.Classify(ctx, content, w.policy.Questions)
	if err != nil {
		// process records the failure with the job's progress.
		return BatchProgress{}, false, &requestFailedError{err: err, modelID: modelID}
	}
	w.authPause.reset(modelID)

	// The request is counted against the cap, so its answer is written even
	// when the run's context ended during it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()

	batch := BatchProgress{
		Index:         index,
		ModelID:       modelID,
		Probabilities: answer.Probabilities,
		ImagesSent:    len(images),
		ImagesSkipped: skipped,
		InputTokens:   answer.InputTokens,
	}
	stored, err := w.repo.StoreBatchProgress(writeCtx, job.ID, batch, now)
	if err != nil {
		return BatchProgress{}, false, stepFailed(codeStoreBatchFailed, err)
	}
	if !stored {
		w.logJobEnded(job)
	}
	return batch, stored, nil
}

// fetchContext bounds a batch's image fetches so its request still fits
// before the run's deadline: the fetches end RequestTimeout, plus a tenth of
// it for the subject reload and the reservation, before that deadline.
// Images the bound cuts off count skipped, so a hanging PDS cannot use up
// the run and leave the batch, and its text, unsent every run. It reports
// false when that bound has already passed.
func (w *Worker) fetchContext(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	deadline, ok := ctx.Deadline()
	if !ok || w.cfg.RequestTimeout <= 0 {
		return ctx, func() {}, true
	}
	fetchDeadline := deadline.Add(-(w.cfg.RequestTimeout + w.cfg.RequestTimeout/10))
	if !time.Now().Before(fetchDeadline) {
		return nil, nil, false
	}
	fetchCtx, cancel := context.WithDeadline(ctx, fetchDeadline)
	return fetchCtx, cancel, true
}

// fetchImages reads a batch's images, counting the ones that cannot be read
// as skipped. Without an image fetcher every image is skipped.
func (w *Worker) fetchImages(ctx context.Context, job ClaimedJob, refs []ImageRef) ([]Image, int) {
	if w.imageFetcher == nil {
		return nil, len(refs)
	}
	images := make([]Image, 0, len(refs))
	skipped := 0
	for _, ref := range refs {
		image, err := w.imageFetcher.FetchImage(ctx, ref)
		if err != nil {
			skipped++
			w.logImageSkipped(ctx, job, ref, err)
			continue
		}
		images = append(images, image)
	}
	return images, skipped
}

// logImageSkipped logs an image that could not be fetched by the class of its
// error, never the error text, which can name the image owner: a refusal by
// moderation or the blob index, or a fetch cut off by the run's end or by
// the fetch bound, is expected and logged at Info; anything else is a Warn
// with a code.
func (w *Worker) logImageSkipped(ctx context.Context, job ClaimedJob, ref ImageRef, err error) {
	class := imageFetchErrorClass(err)
	if class == "other" && ctx.Err() == nil {
		w.logger.Warn("classification image skipped: fetch failed",
			slog.String("code", codeImageFetchFailed),
			slog.String("error_class", class),
			slog.Int64("job_id", job.ID))
		return
	}
	if class == "other" {
		class = "cut_off"
	}
	w.logger.Info("classification image skipped: not fetchable",
		slog.String("error_class", class),
		slog.Int64("job_id", job.ID))
}

// imageFetchErrorClass names an image fetch error's expected refusal, or
// "other".
func imageFetchErrorClass(err error) string {
	switch {
	case errors.Is(err, imageproxy.ErrBlobBlocked):
		return "blob_blocked"
	case errors.Is(err, imageproxy.ErrBlobNotIndexed):
		return "blob_not_indexed"
	case errors.Is(err, imageproxy.ErrCIDMismatch):
		return "cid_mismatch"
	default:
		return "other"
	}
}

// storeResult stores the job's result from its batches: the per-question
// maximum, the images sent and skipped (the subject's own skipped slots
// included), and the image model when any batch sent images, else the text
// model. The last batch's request was counted, so the result is written even
// when the run's context is done.
func (w *Worker) storeResult(ctx context.Context, job ClaimedJob, current subject, progress []BatchProgress) error {
	result := w.buildResult(current, progress)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	stored, err := w.repo.StoreResult(writeCtx, job, result)
	if err != nil {
		return stepFailed(codeStoreFailed, err)
	}
	w.resultStored(ctx, job, result, stored)
	return nil
}

// buildResult is the result of the job's batches: the per-question maximum,
// the images sent and skipped (the subject's own skipped slots included), the
// score alerts it meets, and the image model when any batch sent images, else
// the text model.
func (w *Worker) buildResult(current subject, progress []BatchProgress) Result {
	result := Result{
		ModelID:       w.cfg.Evaluation.TextModelID,
		ImagesSkipped: current.imagesSkipped,
		TextTruncated: current.content.TextTruncated,
	}
	answers := make([]map[string]float64, 0, len(progress))
	for _, batch := range progress {
		answers = append(answers, batch.Probabilities)
		result.ImagesSent += batch.ImagesSent
		result.ImagesSkipped += batch.ImagesSkipped
	}
	result.Probabilities = MaxProbabilities(answers)
	if result.ImagesSent > 0 {
		result.ModelID = w.cfg.Evaluation.ImageModelID
	}
	result.AlertQuestions = MetScoreAlerts(result.Probabilities, w.scoreAlertThresholds)
	return result
}

// resultStored follows a result write: a result with nowhere to go is
// logged, and a stored one with met score alerts has them delivered.
func (w *Worker) resultStored(ctx context.Context, job ClaimedJob, result Result, stored bool) {
	if !stored {
		w.logJobEnded(job)
		return
	}
	if len(result.AlertQuestions) > 0 && w.scoreAlertNotifier != nil {
		w.scoreAlertNotifier.DeliverPending(ctx, job.ID)
	}
}

// logJobEnded logs an answer with nowhere to go: the job was erased with its
// author, or ended, while the request was in flight.
func (w *Worker) logJobEnded(job ClaimedJob) {
	w.logger.Info("classification result discarded: job no longer pending",
		slog.Int64("job_id", job.ID))
}

// loadSubject reads the job's subject by kind. A comment job takes the
// community its subject resolves to, which StoreResult writes to the result
// and the job.
func (w *Worker) loadSubject(ctx context.Context, job *ClaimedJob) (subject, error) {
	current, err := w.loadSubjectByKind(ctx, job)
	return current, stepFailed(codeLoadSubjectFailed, err)
}

// loadSubjectByKind reads the job's subject from the repository for its kind.
func (w *Worker) loadSubjectByKind(ctx context.Context, job *ClaimedJob) (subject, error) {
	switch job.SubjectKind {
	case SubjectKindPost:
		post, err := w.repo.LoadPostSubject(ctx, job.SubjectURI)
		if err != nil {
			return subject{}, err
		}
		return subject{
			visibility:    post.Visibility,
			cid:           post.CID,
			content:       NewPostContent(post.Fields, w.cfg.TextCharLimit),
			imageRefs:     post.ImageRefs,
			imagesSkipped: post.ImagesSkipped,
		}, nil
	case SubjectKindComment:
		comment, err := w.repo.LoadCommentSubject(ctx, job.SubjectURI)
		if err != nil {
			return subject{}, err
		}
		job.CommunityDID = comment.CommunityDID
		return subject{
			visibility:    comment.Visibility,
			cid:           comment.CID,
			content:       NewCommentContent(comment.Body, w.cfg.TextCharLimit),
			imageRefs:     comment.ImageRefs,
			imagesSkipped: comment.ImagesSkipped,
		}, nil
	case SubjectKindCommunity:
		community, err := w.repo.LoadCommunitySubject(ctx, job.SubjectURI)
		if err != nil {
			return subject{}, err
		}
		return subject{
			visibility:    community.Visibility,
			cid:           community.CID,
			content:       NewCommunityContent(community.Fields, w.cfg.TextCharLimit),
			imageRefs:     community.ImageRefs,
			imagesSkipped: community.ImagesSkipped,
		}, nil
	default:
		return subject{}, fmt.Errorf("unsupported subject kind %q", job.SubjectKind)
	}
}

// releaseClaim gives a claimed job back, even when the run's context is done.
func (w *Worker) releaseClaim(ctx context.Context, job ClaimedJob) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	return stepFailed(codeReleaseFailed, w.repo.ReleaseClaim(writeCtx, job))
}

// handleRequestFailure settles a failed request. A refused API token
// (ErrorCodeProviderAuth) is not the job's failure: claiming is paused, the
// claim is given back with no attempt spent, and the run stops. The pause
// starts before the release, so a failed or slow release neither skips nor
// delays it; a failed release is returned with the stop. Any other
// failure is recorded on the job (recordFailure). The request was counted
// against the cap, so the outcome is written even when the run's context
// ended during it: a lost failure would leave attempt_count unchanged and
// the job re-sent after every lease.
func (w *Worker) handleRequestFailure(ctx context.Context, job ClaimedJob, current subject, progress []BatchProgress, modelID string, requestErr error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	if ErrorCodeOf(requestErr) == ErrorCodeProviderAuth {
		w.pauseOnProviderAuth(writeCtx, modelID, requestErr)
		if err := w.releaseClaim(writeCtx, job); err != nil {
			return errors.Join(err, errProviderAuthPaused)
		}
		return errProviderAuthPaused
	}
	return w.recordFailure(writeCtx, job, current, progress, requestErr)
}

// pauseOnProviderAuth pauses claiming after the provider refused the API
// token for modelID, logging the pause once at Error with the HTTP status and
// never the token. A refusal while already paused, from a concurrent request of the
// same run, is not logged again.
func (w *Worker) pauseOnProviderAuth(ctx context.Context, modelID string, requestErr error) {
	until, length, started := w.authPause.start(w.now(), modelID)
	if !started {
		return
	}
	w.logger.LogAttrs(ctx, slog.LevelError, "classification paused: provider refused the API token",
		slog.String("code", ErrorCodeProviderAuth),
		slog.Int("http_status", providerHTTPStatus(requestErr)),
		slog.Duration("paused_for", length),
		slog.Time("paused_until", until))
}

// providerHTTPStatus is the provider's HTTP status carried by err, 0 when
// there was no response.
func providerHTTPStatus(err error) int {
	var providerError *ProviderError
	if errors.As(err, &providerError) {
		return providerError.HTTPStatus
	}
	return 0
}

// recordFailure counts a failed request on the job: retrying after an
// exponential backoff that starts when the request failed, or failed at the
// attempt limit. The failure is logged with the provider's HTTP status (0
// when there was no response), at Error when it ends the job. A final failure
// of a job with answered batches stores them as a partial result
// (storePartialResult); one without discards any stored batches, and the
// discard is logged.
func (w *Worker) recordFailure(ctx context.Context, job ClaimedJob, current subject, progress []BatchProgress, requestErr error) error {
	code := ErrorCodeOf(requestErr)
	if code == "" {
		code = ErrorCodeTransport
	}
	attempts := job.AttemptCount + 1
	failure := Failure{
		ErrorCode: code,
		Final:     attempts >= w.cfg.AttemptLimit,
		RetryAt:   w.now().Add(w.cfg.RetryBaseDelay * time.Duration(1<<min(attempts-1, 30))),
	}
	level := slog.LevelWarn
	if failure.Final {
		level = slog.LevelError
	}
	w.logger.LogAttrs(ctx, level, "classification request failed",
		slog.Int64("job_id", job.ID),
		slog.String("error_code", code),
		slog.Int("http_status", providerHTTPStatus(requestErr)),
		slog.Int("attempt_count", attempts),
		slog.Bool("final", failure.Final))
	if failure.Final && hasAnswer(progress) {
		return w.storePartialResult(ctx, job, current, progress, failure)
	}
	discarded, err := w.repo.RecordFailure(ctx, job.ID, failure)
	if err != nil {
		return stepFailed(codeRecordFailureFailed, err)
	}
	if discarded > 0 {
		w.logger.LogAttrs(ctx, slog.LevelWarn, "classification job batches discarded",
			slog.String("code", codeBatchesDiscarded),
			slog.Int("batches_discarded", discarded))
	}
	return nil
}

// hasAnswer reports whether any stored batch carries the provider's answer;
// a skipped-only batch carries none.
func hasAnswer(progress []BatchProgress) bool {
	for _, batch := range progress {
		if batch.ModelID != "" {
			return true
		}
	}
	return false
}

// storePartialResult ends the job failed with the result of its answered
// batches, marked partial: the images in batches never answered count as
// skipped, and its met score alerts are raised as storeResult raises them.
// The subject was checked before the failed request, as it is before the
// request whose answer storeResult stores.
func (w *Worker) storePartialResult(ctx context.Context, job ClaimedJob, current subject, progress []BatchProgress, failure Failure) error {
	result := w.buildResult(current, progress)
	result.Partial = true
	unsent := len(current.imageRefs) - result.ImagesSent - (result.ImagesSkipped - current.imagesSkipped)
	result.ImagesSkipped += max(unsent, 0)
	stored, err := w.repo.StorePartialResult(ctx, job, result, failure)
	if err != nil {
		return stepFailed(codeStoreFailed, err)
	}
	if stored {
		w.logger.LogAttrs(ctx, slog.LevelWarn, "classification partial result stored",
			slog.String("code", codePartialResultStored),
			slog.Int64("job_id", job.ID),
			slog.Int("batches_stored", len(progress)))
	}
	w.resultStored(ctx, job, result, stored)
	return nil
}

// logDailyCapReached logs the cap once per UTC day.
func (w *Worker) logDailyCapReached(now time.Time, modelID string) {
	date := now.UTC().Format(time.DateOnly)
	w.capLogMu.Lock()
	defer w.capLogMu.Unlock()
	if w.capLoggedDate == date {
		return
	}
	w.capLoggedDate = date
	w.logger.Warn("classification daily request cap reached",
		slog.String("code", codeDailyCapReached),
		slog.String("model_id", modelID),
		slog.Int("daily_request_cap", w.cfg.DailyRequestCap))
}
