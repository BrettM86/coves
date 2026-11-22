package communitysignals_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"Coves/internal/core/adminreports"
	"Coves/internal/core/communitysignals"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deliveryRepository hands out events one claim at a time, fails a read on a
// done context as a database would, and keeps the context error each outcome
// write saw.
type deliveryRepository struct {
	events          []communitysignals.Event
	digestRows      []communitysignals.DigestRow
	deliveredCtxErr []error
	failedCtxErr    []error
	markerCtxErr    []error
}

func (r *deliveryRepository) ClaimDueEvent(ctx context.Context, _ string, _, _ time.Time, _ int) (communitysignals.Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return communitysignals.Event{}, false, err
	}
	if len(r.events) == 0 {
		return communitysignals.Event{}, false, nil
	}
	event := r.events[0]
	r.events = r.events[1:]
	return event, true, nil
}

func (r *deliveryRepository) MarkEventDelivered(ctx context.Context, _ int64, _, _ time.Time) error {
	r.deliveredCtxErr = append(r.deliveredCtxErr, ctx.Err())
	return ctx.Err()
}

func (r *deliveryRepository) MarkEventFailed(ctx context.Context, _ int64, _ time.Time, _ string, _ time.Time) error {
	r.failedCtxErr = append(r.failedCtxErr, ctx.Err())
	return ctx.Err()
}

func (r *deliveryRepository) DigestRows(ctx context.Context, _ time.Time, _ int, _ float64) ([]communitysignals.DigestRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.digestRows, nil
}

func (r *deliveryRepository) DigestLastSent(ctx context.Context) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	return time.Time{}, false, nil
}

func (r *deliveryRepository) SetDigestLastSent(ctx context.Context, _ time.Time) error {
	r.markerCtxErr = append(r.markerCtxErr, ctx.Err())
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

func newDeliveryService(repo communitysignals.Repository, sender adminreports.MessageSender, handler *recordingHandler) *communitysignals.Service {
	return communitysignals.NewService(repo, sender,
		communitysignals.Config{AlertsEnabled: true, AttemptLimit: 3, RetryBaseDelay: time.Minute},
		func() time.Time { return time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC) }, slog.New(handler))
}

// A send whose context ends mid-send still has its outcome recorded, on a
// context that is not done, so a delivered event is not resent and a failed
// one is counted.
func TestService_RecordsOutcomeWhenContextEndsMidSend(t *testing.T) {
	for _, test := range []struct {
		name    string
		sendErr error
	}{
		{name: "delivered"},
		{name: "failed", sendErr: errors.New("send failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &deliveryRepository{events: []communitysignals.Event{{ID: 4, NewNSFW: true}}}
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

// A digest sent while the run's context ends still persists its marker, so
// the digest is not sent twice.
func TestService_RunWeeklyDigestPersistsMarkerWhenContextEndsMidSend(t *testing.T) {
	repo := &deliveryRepository{digestRows: []communitysignals.DigestRow{{CommunityHandle: "c.test", RecentPosts: 20, RecentShare: 0.5}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := newDeliveryService(repo, cancellingSender{cancel: cancel}, &recordingHandler{})

	require.NoError(t, service.RunWeeklyDigest(ctx))

	require.Len(t, repo.markerCtxErr, 1)
	assert.NoError(t, repo.markerCtxErr[0], "the marker is persisted on a context that is not done")
}

// A sweep, after-commit delivery or digest run on a done context returns the
// context's error, which the caller recognises as cancellation, and logs no
// repository failure.
func TestService_DoneContextIsNotARepositoryFailure(t *testing.T) {
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
			repo := &deliveryRepository{events: []communitysignals.Event{{ID: 4, NewNSFW: true}}}
			service := newDeliveryService(repo, auditSender{}, handler)

			assert.ErrorIs(t, service.Sweep(test.ctx), test.want)
			assert.ErrorIs(t, service.RunWeeklyDigest(test.ctx), test.want)
			service.DeliverPending(test.ctx, "did:plc:community")

			for _, record := range handler.records {
				assert.Less(t, record.Level, slog.LevelError, "a done context is not logged as an error: %q", record.Message)
				code := attributes(record)["error_code"]
				assert.NotEqual(t, "claim_failed", code)
				assert.NotEqual(t, "record_failed", code)
			}
		})
	}
}
