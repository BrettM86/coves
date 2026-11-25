package classification_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/internal/core/imageproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Subject identity sentinels: none of them may reach a log record or a
// returned Sweep error.
const (
	sentinelAuthorDID    = "did:plc:sentinelauthor"
	sentinelCommunityDID = "did:plc:sentinelcommunity"
	sentinelHandle       = "sentinel.handle.test"
	sentinelContent      = "sentinel content"
	sentinelSubjectURI   = "at://did:plc:sentinelauthor/social.coves.community.post/sentinel"
	sentinelProbability  = 0.8765
	// sentinelCID prefixes the job's record CID, the subject's changed CID
	// and its image blob CID.
	sentinelCID      = "bafysentinel"
	sentinelImageCID = sentinelCID + "image"
)

// sentinelErrorText is the text of every injected error, as a sender or
// repository error that echoes its input would carry.
const sentinelErrorText = "request for did:plc:sentinelauthor (sentinel.handle.test) about \"sentinel content\" in did:plc:sentinelcommunity failed"

var sentinels = []string{sentinelAuthorDID, sentinelCommunityDID, sentinelHandle, sentinelContent, sentinelCID, "0.8765"}

// fixedErrorCode is the shape of a fixed log error code.
var fixedErrorCode = regexp.MustCompile(`^[a-z][a-z_]*$`)

// assertNoSentinel fails when any record's message or attribute value holds a
// sentinel.
func assertNoSentinel(t *testing.T, records []slog.Record) {
	t.Helper()
	for _, record := range records {
		for _, sentinel := range sentinels {
			assert.NotContains(t, record.Message, sentinel, "log message %q holds a sentinel", record.Message)
			for key, value := range attributes(record) {
				assert.NotContains(t, value, sentinel, "log %q attribute %q holds a sentinel", record.Message, key)
			}
		}
	}
}

// errorCodes returns the error_code attribute of every record that has one.
func errorCodes(records []slog.Record) []string {
	var codes []string
	for _, record := range records {
		if code, ok := attributes(record)["error_code"]; ok {
			codes = append(codes, code)
		}
	}
	return codes
}

// assertNoSentinelInError fails when err's text holds a sentinel.
func assertNoSentinelInError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, sentinel := range sentinels {
		assert.NotContains(t, err.Error(), sentinel, "returned error holds a sentinel")
	}
}

// auditRepository is a deadlineRepository whose post shows the sentinel
// content, with switches for the daily cap, a discarded result and a failing
// result store.
type auditRepository struct {
	*deadlineRepository
	capReached     bool
	discardResult  bool
	storeResultErr error
	loadSubjectErr error
	claimErr       error
}

func (r *auditRepository) ClaimDue(ctx context.Context, evaluation classification.Evaluation, now time.Time, limit int, lease time.Duration) ([]classification.ClaimedJob, error) {
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	return r.deadlineRepository.ClaimDue(ctx, evaluation, now, limit, lease)
}

func (r *auditRepository) LoadPostSubject(ctx context.Context, subjectURI string) (classification.PostSubject, error) {
	if r.loadSubjectErr != nil {
		return classification.PostSubject{}, r.loadSubjectErr
	}
	post, err := r.deadlineRepository.LoadPostSubject(ctx, subjectURI)
	if err != nil {
		return post, err
	}
	post.Fields = classification.PostFields{Title: sentinelContent, Body: sentinelContent + " by " + sentinelHandle}
	return post, nil
}

func (r *auditRepository) ReserveRequest(ctx context.Context, modelID string, now time.Time, dailyCap int) (bool, error) {
	if r.capReached {
		return false, nil
	}
	return r.deadlineRepository.ReserveRequest(ctx, modelID, now, dailyCap)
}

func (r *auditRepository) StoreResult(ctx context.Context, job classification.ClaimedJob, result classification.Result) (bool, error) {
	if r.storeResultErr != nil {
		return false, r.storeResultErr
	}
	if r.discardResult {
		return false, nil
	}
	return r.deadlineRepository.StoreResult(ctx, job, result)
}

// sentinelClassifier answers with the sentinel probability, or fails with
// err.
type sentinelClassifier struct{ err error }

func (c sentinelClassifier) Classify(context.Context, classification.Content, []classification.Question) (classification.Classification, error) {
	if c.err != nil {
		return classification.Classification{}, c.err
	}
	return classification.Classification{
		ModelID:       "text-model",
		Probabilities: map[string]float64{"sexual_minors": sentinelProbability},
		InputTokens:   12,
	}, nil
}

// discardingNotifier accepts every after-commit notification.
type discardingNotifier struct{}

func (discardingNotifier) DeliverPending(context.Context, int64) {}

// TestWorkerLogsCarryNoSubjectIdentity runs the worker over each logged path
// with a subject, content, probability and injected errors that hold the
// sentinels: no record names the subject, and each failure record carries its
// fixed error_code (B10). A job failure is an Error record carrying the
// job's ID; a claim failure has no job.
func TestWorkerLogsCarryNoSubjectIdentity(t *testing.T) {
	sentinelErr := errors.New(sentinelErrorText)
	sentinelImages := func(r *auditRepository) {
		r.postImages = []classification.ImageRef{{OwnerDID: sentinelAuthorDID, CID: sentinelImageCID}}
	}
	for _, test := range []struct {
		name         string
		attemptCount int
		classifier   sentinelClassifier
		imageFetcher classification.ImageFetcher
		configure    func(*auditRepository)
		wantCode     string
		// wantJobError expects wantCode on an Error record; wantJobID is the
		// job_id it carries, empty for none.
		wantJobError bool
		wantJobID    string
		// wantNoError expects no Error record: a cancelled run is a Warn.
		wantNoError bool
	}{
		{name: "success with an alert"},
		{name: "transport failure", classifier: sentinelClassifier{err: sentinelErr}, wantCode: classification.ErrorCodeTransport},
		{
			name:         "final failure",
			attemptCount: 2,
			classifier: sentinelClassifier{err: fmt.Errorf("%s: %w", sentinelErrorText,
				&classification.ProviderError{Code: classification.ErrorCodeRateLimited, HTTPStatus: 429})},
			wantCode: classification.ErrorCodeRateLimited,
		},
		{name: "daily cap reached", configure: func(r *auditRepository) { r.capReached = true }},
		{name: "superseded close", configure: func(r *auditRepository) { r.postCID = "bafysentinelnew" }},
		{name: "not visible close", configure: func(r *auditRepository) { r.postVisibility = classification.VisibilityMissing }},
		{name: "discarded result", configure: func(r *auditRepository) { r.discardResult = true }},
		{
			name: "store result failure", configure: func(r *auditRepository) { r.storeResultErr = sentinelErr },
			wantCode: "store_failed", wantJobError: true, wantJobID: "41",
		},
		{
			name: "load subject failure", configure: func(r *auditRepository) { r.loadSubjectErr = sentinelErr },
			wantCode: "load_subject_failed", wantJobError: true, wantJobID: "41",
		},
		{
			name: "store result timeout", configure: func(r *auditRepository) {
				r.storeResultErr = fmt.Errorf("%s: %w", sentinelErrorText, context.DeadlineExceeded)
			},
			wantCode: "timeout", wantJobError: true, wantJobID: "41",
		},
		{
			name: "store result canceled", configure: func(r *auditRepository) {
				r.storeResultErr = fmt.Errorf("%s: %w", sentinelErrorText, context.Canceled)
			},
			wantCode: "canceled", wantNoError: true,
		},
		{
			name: "claim failure", configure: func(r *auditRepository) { r.claimErr = sentinelErr },
			wantCode: "claim_failed", wantJobError: true,
		},
		{
			name: "image fetch failure", configure: sentinelImages,
			imageFetcher: failingImageFetcher{failures: map[string]error{sentinelImageCID: sentinelErr}},
		},
		{
			name: "image refused", configure: sentinelImages,
			imageFetcher: failingImageFetcher{failures: map[string]error{
				sentinelImageCID: fmt.Errorf("%s: %w", sentinelErrorText, imageproxy.ErrBlobBlocked),
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &auditRepository{deadlineRepository: &deadlineRepository{job: classification.ClaimedJob{
				ID:           41,
				SubjectURI:   sentinelSubjectURI,
				SubjectKind:  classification.SubjectKindPost,
				RecordCID:    "bafysentinel",
				AuthorDID:    sentinelAuthorDID,
				CommunityDID: sentinelCommunityDID,
				AttemptCount: test.attemptCount,
			}}}
			if test.configure != nil {
				test.configure(repo)
			}
			handler := &recordingHandler{}
			options := []classification.WorkerOption{classification.WithScoreAlerts(
				[]classification.ScoreAlertThreshold{{QuestionKey: "sexual_minors", Threshold: 0.3}}, discardingNotifier{})}
			if test.imageFetcher != nil {
				options = append(options, classification.WithImageFetcher(test.imageFetcher))
			}
			worker := classification.NewWorker(repo, test.classifier, classification.Policy{Version: "v1"}, classification.WorkerConfig{
				Evaluation:      classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-model", ImageModelID: "image-model"},
				TextCharLimit:   8000,
				AttemptLimit:    3,
				RetryBaseDelay:  time.Minute,
				PendingDeferral: time.Minute,
				DailyRequestCap: 100,
			}, time.Now, slog.New(handler), options...)

			_ = worker.RunOnce(context.Background())

			assertNoSentinel(t, handler.records)
			if test.wantCode != "" {
				assert.Contains(t, errorCodes(handler.records), test.wantCode)
			}
			if test.wantJobError {
				assertJobErrorLogged(t, handler.records, test.wantCode, test.wantJobID)
			}
			if test.wantNoError {
				for _, record := range handler.records {
					assert.NotEqual(t, slog.LevelError, record.Level, "%q is logged as an error", record.Message)
				}
			}
		})
	}
}

// assertJobErrorLogged fails unless an Error record carries code as its
// error_code and jobID as its job_id, or no job_id when jobID is empty.
func assertJobErrorLogged(t *testing.T, records []slog.Record, code, jobID string) {
	t.Helper()
	for _, record := range records {
		attrs := attributes(record)
		if record.Level == slog.LevelError && attrs["error_code"] == code && attrs["job_id"] == jobID {
			return
		}
	}
	t.Errorf("no Error record with error_code %q and job_id %q", code, jobID)
}

// auditAlertRepository hands out alerts one claim at a time and answers each
// method with its configured error.
type auditAlertRepository struct {
	alerts       []classification.ScoreAlert
	claimErr     error
	deliveredErr error
	failedErr    error
}

func (r *auditAlertRepository) ClaimDueAlert(_ context.Context, _ int64, _, _ time.Time, _ int) (classification.ScoreAlert, bool, error) {
	if r.claimErr != nil {
		return classification.ScoreAlert{}, false, r.claimErr
	}
	if len(r.alerts) == 0 {
		return classification.ScoreAlert{}, false, nil
	}
	alert := r.alerts[0]
	r.alerts = r.alerts[1:]
	return alert, true, nil
}

func (r *auditAlertRepository) MarkAlertDelivered(context.Context, int64, time.Time, time.Time) error {
	return r.deliveredErr
}

func (r *auditAlertRepository) MarkAlertFailed(context.Context, int64, time.Time, string, time.Time) error {
	return r.failedErr
}

// auditSender answers every send with err.
type auditSender struct{ err error }

func (s auditSender) SendMessage(context.Context, string) error { return s.err }

// TestScoreAlertServiceLogsCarryNoSubjectIdentity runs the score alert
// service's after-commit delivery and its sweep over each failure, with
// injected errors that hold the sentinels: no record or Sweep error holds a
// sentinel, each failure is logged under a fixed error_code, and a
// repository failure makes Sweep return an error (B10).
func TestScoreAlertServiceLogsCarryNoSubjectIdentity(t *testing.T) {
	sentinelErr := errors.New(sentinelErrorText)
	for _, test := range []struct {
		name         string
		senderErr    error
		repo         func() *auditAlertRepository
		wantCode     string
		wantSweepErr bool
	}{
		{
			name:      "send failure",
			senderErr: sentinelErr,
			repo: func() *auditAlertRepository {
				return &auditAlertRepository{alerts: []classification.ScoreAlert{sentinelAlert()}}
			},
			wantCode: "send_failed",
		},
		{
			name: "lease lost",
			repo: func() *auditAlertRepository {
				return &auditAlertRepository{
					alerts:       []classification.ScoreAlert{sentinelAlert()},
					deliveredErr: fmt.Errorf("%s: %w", sentinelErrorText, classification.ErrScoreAlertLeaseLost),
				}
			},
		},
		{
			name:         "claim error",
			repo:         func() *auditAlertRepository { return &auditAlertRepository{claimErr: sentinelErr} },
			wantSweepErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingHandler{}
			newService := func() *classification.ScoreAlertService {
				return classification.NewScoreAlertService(test.repo(), auditSender{err: test.senderErr},
					classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute, WebBaseURL: "https://coves.sentinel.test"},
					func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }, slog.New(handler))
			}

			newService().DeliverPending(context.Background(), 41)
			deliverRecords := len(handler.records)
			sweepErr := newService().Sweep(context.Background())

			assertNoSentinel(t, handler.records)
			assertNoSentinelInError(t, sweepErr)
			if test.wantSweepErr {
				assert.Error(t, sweepErr, "a repository failure makes Sweep return an error")
			}
			require.NotZero(t, deliverRecords, "the after-commit delivery logs the failure")
			codes := errorCodes(handler.records[:deliverRecords])
			require.NotEmpty(t, codes, "the after-commit failure record carries an error_code")
			for _, code := range codes {
				assert.Regexp(t, fixedErrorCode, code)
			}
			if test.wantCode != "" {
				assert.Contains(t, codes, test.wantCode)
			}
		})
	}
}

func sentinelAlert() classification.ScoreAlert {
	return classification.ScoreAlert{
		ID: 5, ResultID: 4217, JobID: 41, QuestionKey: "sexual_minors", SubjectKind: classification.SubjectKindPost,
		SubjectURI: sentinelSubjectURI, CommunityDID: sentinelCommunityDID, RootURI: sentinelSubjectURI,
	}
}
