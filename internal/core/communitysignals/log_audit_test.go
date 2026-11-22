package communitysignals_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communitysignals"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community identity sentinels: none of them may reach a log record or a
// returned Sweep error.
const (
	sentinelCommunityDID = "did:plc:sentinelcommunity"
	sentinelAuthorDID    = "did:plc:sentinelauthor"
	sentinelHandle       = "sentinel.handle.test"
	sentinelContent      = "sentinel content"
)

// sentinelErrorText is the text of every injected error, as a sender or
// repository error that echoes its input would carry.
const sentinelErrorText = "request for did:plc:sentinelauthor (sentinel.handle.test) about \"sentinel content\" in did:plc:sentinelcommunity failed"

var sentinels = []string{sentinelCommunityDID, sentinelAuthorDID, sentinelHandle, sentinelContent}

// fixedErrorCode is the shape of a fixed log error code.
var fixedErrorCode = regexp.MustCompile(`^[a-z][a-z_]*$`)

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

// auditRepository hands out events one claim at a time and answers each
// method with its configured error.
type auditRepository struct {
	events       []communitysignals.Event
	claimErr     error
	deliveredErr error
	failedErr    error
}

func (r *auditRepository) ClaimDueEvent(_ context.Context, _ string, _, _ time.Time, _ int) (communitysignals.Event, bool, error) {
	if r.claimErr != nil {
		return communitysignals.Event{}, false, r.claimErr
	}
	if len(r.events) == 0 {
		return communitysignals.Event{}, false, nil
	}
	event := r.events[0]
	r.events = r.events[1:]
	return event, true, nil
}

func (r *auditRepository) MarkEventDelivered(context.Context, int64, time.Time, time.Time) error {
	return r.deliveredErr
}

func (r *auditRepository) MarkEventFailed(context.Context, int64, time.Time, string, time.Time) error {
	return r.failedErr
}

func (r *auditRepository) DigestRows(context.Context, time.Time, int, float64) ([]communitysignals.DigestRow, error) {
	return nil, nil
}

func (r *auditRepository) DigestLastSent(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (r *auditRepository) SetDigestLastSent(context.Context, time.Time) error { return nil }

// auditSender answers every send with err.
type auditSender struct{ err error }

func (s auditSender) SendMessage(context.Context, string) error { return s.err }

func sentinelEvent() communitysignals.Event {
	return communitysignals.Event{
		ID:              7,
		CommunityDID:    sentinelCommunityDID,
		CommunityHandle: sentinelHandle,
		NewNSFW:         true,
		OccurredAt:      time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC),
		NextAttemptAt:   time.Date(2026, 11, 12, 12, 5, 0, 0, time.UTC),
	}
}

// TestServiceLogsCarryNoCommunityIdentity runs the after-commit delivery and
// the sweep over each failure, with an event and injected errors that hold
// the sentinels: no record or Sweep error holds a sentinel, each failure is
// logged under a fixed error_code, and a repository failure makes Sweep
// return an error (B10).
func TestServiceLogsCarryNoCommunityIdentity(t *testing.T) {
	sentinelErr := errors.New(sentinelErrorText)
	for _, test := range []struct {
		name         string
		senderErr    error
		repo         func() *auditRepository
		wantCode     string
		wantSweepErr bool
	}{
		{
			name:      "send failure",
			senderErr: sentinelErr,
			repo:      func() *auditRepository { return &auditRepository{events: []communitysignals.Event{sentinelEvent()}} },
			wantCode:  communitysignals.ErrorCodeSendFailed,
		},
		{
			name: "lease lost",
			repo: func() *auditRepository {
				return &auditRepository{
					events:       []communitysignals.Event{sentinelEvent()},
					deliveredErr: fmt.Errorf("%s: %w", sentinelErrorText, communitysignals.ErrLeaseLost),
				}
			},
		},
		{
			name:         "claim error",
			repo:         func() *auditRepository { return &auditRepository{claimErr: sentinelErr} },
			wantSweepErr: true,
		},
		{
			name: "outcome write error",
			repo: func() *auditRepository {
				return &auditRepository{events: []communitysignals.Event{sentinelEvent()}, deliveredErr: sentinelErr}
			},
			wantSweepErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingHandler{}
			newService := func() *communitysignals.Service {
				return communitysignals.NewService(test.repo(), auditSender{err: test.senderErr},
					communitysignals.Config{AlertsEnabled: true, AttemptLimit: 8, RetryBaseDelay: time.Minute},
					func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }, slog.New(handler))
			}

			newService().DeliverPending(context.Background(), sentinelCommunityDID)
			deliverRecords := len(handler.records)
			sweepErr := newService().Sweep(context.Background())

			for _, record := range handler.records {
				for _, sentinel := range sentinels {
					assert.NotContains(t, record.Message, sentinel, "log message %q holds a sentinel", record.Message)
					for key, value := range attributes(record) {
						assert.NotContains(t, value, sentinel, "log %q attribute %q holds a sentinel", record.Message, key)
					}
				}
			}
			if sweepErr != nil {
				for _, sentinel := range sentinels {
					assert.NotContains(t, sweepErr.Error(), sentinel, "Sweep's error holds a sentinel")
				}
			}
			if test.wantSweepErr {
				assert.Error(t, sweepErr, "a repository failure makes Sweep return an error")
			}
			require.NotZero(t, deliverRecords, "the after-commit delivery logs the failure")
			var codes []string
			for _, record := range handler.records[:deliverRecords] {
				if code, ok := attributes(record)["error_code"]; ok {
					codes = append(codes, code)
				}
			}
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
