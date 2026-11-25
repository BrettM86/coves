package classification_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deliveryAlertRepository hands out alerts one claim at a time, fails a claim
// on a done context as a database would, and keeps the context error each
// outcome write saw.
type deliveryAlertRepository struct {
	alerts          []classification.ScoreAlert
	deliveredCtxErr []error
	failedCtxErr    []error
}

func (r *deliveryAlertRepository) ClaimDueAlert(ctx context.Context, _ int64, _, _ time.Time, _ int) (classification.ScoreAlert, bool, error) {
	if err := ctx.Err(); err != nil {
		return classification.ScoreAlert{}, false, err
	}
	if len(r.alerts) == 0 {
		return classification.ScoreAlert{}, false, nil
	}
	alert := r.alerts[0]
	r.alerts = r.alerts[1:]
	return alert, true, nil
}

func (r *deliveryAlertRepository) MarkAlertDelivered(ctx context.Context, _ int64, _, _ time.Time) error {
	r.deliveredCtxErr = append(r.deliveredCtxErr, ctx.Err())
	return ctx.Err()
}

func (r *deliveryAlertRepository) MarkAlertFailed(ctx context.Context, _ int64, _ time.Time, _ string, _ time.Time) error {
	r.failedCtxErr = append(r.failedCtxErr, ctx.Err())
	return ctx.Err()
}

// cancellingSender ends the run's context during the send, then answers err.
type cancellingSender struct {
	cancel context.CancelFunc
	err    error
}

func (s cancellingSender) SendMessage(context.Context, string) error {
	s.cancel()
	return s.err
}

func newDeliveryService(repo classification.ScoreAlertRepository, sender classification.MessageSender, handler *recordingHandler) *classification.ScoreAlertService {
	return classification.NewScoreAlertService(repo, sender,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 3, RetryBaseDelay: time.Minute},
		func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }, slog.New(handler))
}

// A send whose context ends mid-send still has its outcome recorded, on a
// context that is not done, so a delivered alert is not resent and a failed
// one is counted.
func TestScoreAlertService_RecordsOutcomeWhenContextEndsMidSend(t *testing.T) {
	for _, test := range []struct {
		name    string
		sendErr error
	}{
		{name: "delivered"},
		{name: "failed", sendErr: errors.New("send failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &deliveryAlertRepository{alerts: []classification.ScoreAlert{{ID: 4, ResultID: 9}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := newDeliveryService(repo, cancellingSender{cancel: cancel, err: test.sendErr}, &recordingHandler{})

			err := service.Sweep(ctx)

			assert.ErrorIs(t, err, context.Canceled)
			if test.sendErr == nil {
				require.Len(t, repo.deliveredCtxErr, 1)
				assert.NoError(t, repo.deliveredCtxErr[0], "the delivery is recorded on a context that is not done")
				assert.Empty(t, repo.failedCtxErr)
			} else {
				require.Len(t, repo.failedCtxErr, 1)
				assert.NoError(t, repo.failedCtxErr[0], "the failure is recorded on a context that is not done")
				assert.Empty(t, repo.deliveredCtxErr)
			}
		})
	}
}

// A sweep or after-commit delivery on a done context returns the context's
// error, which the caller recognises as cancellation, and logs no repository
// failure.
func TestScoreAlertService_DoneContextIsNotARepositoryFailure(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "cancelled", ctx: cancelled, want: context.Canceled},
		{name: "deadline exceeded", ctx: expired, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingHandler{}
			repo := &deliveryAlertRepository{alerts: []classification.ScoreAlert{{ID: 4, ResultID: 9}}}
			service := newDeliveryService(repo, auditSender{}, handler)

			assert.ErrorIs(t, service.Sweep(test.ctx), test.want)
			service.DeliverPending(test.ctx, 1)

			for _, record := range handler.records {
				assert.Less(t, record.Level, slog.LevelError, "a done context is not logged as an error: %q", record.Message)
				code := attributes(record)["error_code"]
				assert.NotEqual(t, classification.ScoreAlertErrorCodeClaimFailed, code)
				assert.NotEqual(t, classification.ScoreAlertErrorCodeRecordFailed, code)
			}
		})
	}
}

// The failed send that uses up an alert's last attempt is logged at Error with
// the alert ID and attempts_exhausted; an earlier failed send is not.
func TestScoreAlertService_LogsExhaustedAttempts(t *testing.T) {
	for _, test := range []struct {
		name          string
		priorAttempts int
		wantExhausted bool
	}{
		{name: "earlier attempt", priorAttempts: 0},
		{name: "final attempt", priorAttempts: 2, wantExhausted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingHandler{}
			repo := &deliveryAlertRepository{alerts: []classification.ScoreAlert{{ID: 4, ResultID: 9, Attempts: test.priorAttempts}}}
			service := newDeliveryService(repo, auditSender{err: errors.New("send failed")}, handler)

			require.NoError(t, service.Sweep(context.Background()))

			var exhausted []slog.Record
			for _, record := range handler.records {
				if attributes(record)["error_code"] == "attempts_exhausted" {
					exhausted = append(exhausted, record)
				}
			}
			if !test.wantExhausted {
				assert.Empty(t, exhausted)
				for _, record := range handler.records {
					assert.Less(t, record.Level, slog.LevelError, "an earlier failed send is not an error: %q", record.Message)
				}
				return
			}
			require.Len(t, exhausted, 1)
			assert.Equal(t, slog.LevelError, exhausted[0].Level)
			assert.Equal(t, "4", attributes(exhausted[0])["alert_id"])
		})
	}
}

// textSender records the text of every send and answers nil.
type textSender struct{ texts []string }

func (s *textSender) SendMessage(_ context.Context, text string) error {
	s.texts = append(s.texts, text)
	return nil
}

// The delivered message carries the subject's web link built from the
// service's configured WebBaseURL.
func TestScoreAlertService_DeliversTheLinkFromTheConfiguredBaseURL(t *testing.T) {
	repo := &deliveryAlertRepository{alerts: []classification.ScoreAlert{{
		ID: 4, ResultID: 9, JobID: 1, QuestionKey: "spam_scam", SubjectKind: classification.SubjectKindComment,
		SubjectURI:   "at://did:plc:deliverycommenter/social.coves.community.comment/3kdelivercmt",
		CommunityDID: "did:plc:deliverycommunity",
		RootURI:      "at://did:plc:deliveryauthor/social.coves.community.postv2/3kdeliverpost",
	}}}
	sender := &textSender{}
	service := classification.NewScoreAlertService(repo, sender,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 3, RetryBaseDelay: time.Minute, WebBaseURL: "https://coves.example"},
		func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }, slog.New(&recordingHandler{}))

	service.DeliverPending(context.Background(), 1)

	assert.Equal(t, []string{
		"Classifier score alert: result 9, question spam_scam, subject comment\n" +
			"URI: at://did:plc:deliverycommenter/social.coves.community.comment/3kdelivercmt\n" +
			"Open: https://coves.example/c/did:plc:deliverycommunity/post/did:plc:deliveryauthor/3kdeliverpost/comment/did:plc:deliverycommenter/3kdelivercmt",
	}, sender.texts)
}
