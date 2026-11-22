package communitysignals

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"Coves/internal/core/adminreports"
)

// claimLease is how long a claimed event stays out of other claims while its
// send runs. Events are claimed one at a time, so a lease covers one send and
// is well above a Telegram send's timeout. A lease expires when the claiming
// process died or its send outlived the lease; the event is then retried, and
// the outcome of the expired claim is not recorded (ErrLeaseLost).
const claimLease = 5 * time.Minute

// stateWriteTimeout bounds each outcome write that must happen even when the
// run's context ended during the send: an event's delivery or failure, and
// the digest marker.
const stateWriteTimeout = 10 * time.Second

// codeContextDone is deliverDue's code for a run whose context ended. It is
// not a failure: Sweep returns the context's error and DeliverPending logs
// nothing.
const codeContextDone = "context_done"

// Service delivers flag change alerts and the weekly digest.
type Service struct {
	repo   Repository
	sender adminreports.MessageSender
	cfg    Config
	now    func() time.Time
	logger *slog.Logger
}

// NewService builds a Service. A nil sender means Telegram is unconfigured:
// like cfg.AlertsEnabled false, nothing is sent. A nil now uses time.Now and a
// nil logger uses slog.Default.
func NewService(repo Repository, sender adminreports.MessageSender, cfg Config, now func() time.Time, logger *slog.Logger) *Service {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, sender: sender, cfg: cfg, now: now, logger: logger}
}

func (s *Service) sendingEnabled() bool {
	return s.sender != nil && s.cfg.AlertsEnabled
}

// DeliverPending sends the due alerts for one community. The indexer calls it
// after the community write commits. Failures are recorded on the event for
// the sweep to retry and logged under a fixed error code, never returned.
func (s *Service) DeliverPending(ctx context.Context, communityDID string) {
	if !s.sendingEnabled() {
		return
	}
	if code := s.deliverDue(ctx, communityDID); code != "" && code != codeContextDone {
		s.logger.Error("community signal: delivering due flag events failed", slog.String("error_code", code))
	}
}

// Sweep sends every due alert. It returns the context's error when ctx ends
// and a fixed error when the repository fails; a failed send is recorded on
// the event with its next attempt time.
func (s *Service) Sweep(ctx context.Context) error {
	if !s.sendingEnabled() {
		return nil
	}
	switch s.deliverDue(ctx, "") {
	case "":
		return nil
	case codeContextDone:
		return ctx.Err()
	case errorCodeClaimFailed:
		return errClaimFailed
	default:
		return errRecordFailed
	}
}

// deliverDue claims and delivers due events one at a time until none is due;
// communityDID "" means every community. Each claim's lease starts just
// before its own send. A delivered or failed event is not due again at once,
// so the loop ends. It returns codeContextDone once ctx ends, the error code
// of a repository failure, or "".
func (s *Service) deliverDue(ctx context.Context, communityDID string) string {
	for {
		if ctx.Err() != nil {
			return codeContextDone
		}
		now := s.now()
		event, ok, err := s.repo.ClaimDueEvent(ctx, communityDID, now, now.Add(claimLease), s.cfg.AttemptLimit)
		if err != nil {
			if ctx.Err() != nil {
				return codeContextDone
			}
			return errorCodeClaimFailed
		}
		if !ok {
			return ""
		}
		if code := s.deliver(ctx, event); code != "" {
			return code
		}
	}
}

// deliver sends one claimed event and records the outcome under its lease. It
// returns the error code of a repository failure, or ""; a send failure or a
// lost lease is logged and not returned. The outcome is recorded even when ctx
// ends during the send, so a delivered event is not resent and a failed send
// is counted.
func (s *Service) deliver(ctx context.Context, event Event) string {
	sendErr := s.sender.SendMessage(ctx, RenderAlert(event))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	var markErr error
	if sendErr != nil {
		attempts := event.Attempts + 1
		next := s.now().Add(retryDelay(s.cfg.RetryBaseDelay, attempts))
		s.logger.Warn("community signal: flag alert send failed",
			slog.Int64("event_id", event.ID), slog.Int("attempts", attempts),
			slog.String("error_code", ErrorCodeSendFailed))
		markErr = s.repo.MarkEventFailed(writeCtx, event.ID, event.NextAttemptAt, ErrorCodeSendFailed, next)
	} else {
		markErr = s.repo.MarkEventDelivered(writeCtx, event.ID, event.NextAttemptAt, s.now())
	}
	if errors.Is(markErr, ErrLeaseLost) {
		s.logger.Warn("community signal: flag event lease expired before its outcome was recorded",
			slog.Int64("event_id", event.ID), slog.Bool("send_failed", sendErr != nil),
			slog.String("error_code", errorCodeLeaseLost))
		return ""
	}
	if markErr != nil {
		return errorCodeRecordFailed
	}
	return ""
}

// RunWeeklyDigest sends the weekly digest once per Monday 09:00 UTC slot. It
// does nothing until DigestSlot(now) is later than the persisted marker. The
// digest covers the week before the slot; the marker is persisted after the
// send, or when no community qualifies. A failed send leaves the marker so
// the next run retries; a sent digest's marker is persisted even when ctx
// ends during the send, so it is not sent twice. It returns the context's
// error when ctx ends before the send, and otherwise fixed errors, never the
// sender or repository text.
func (s *Service) RunWeeklyDigest(ctx context.Context) error {
	if !s.sendingEnabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	slot := DigestSlot(s.now())
	last, ok, err := s.repo.DigestLastSent(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errDigestMarkerReadFailed
	}
	if ok && !slot.After(last) {
		return nil
	}
	rows, err := s.repo.DigestRows(ctx, slot, s.cfg.DigestMinPosts, s.cfg.DigestRiseThreshold)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errDigestRowsFailed
	}
	if len(rows) > 0 {
		if err := s.sender.SendMessage(ctx, RenderDigest(rows)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errDigestSendFailed
		}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	if err := s.repo.SetDigestLastSent(writeCtx, slot); err != nil {
		return errDigestMarkerWriteFailed
	}
	return nil
}
