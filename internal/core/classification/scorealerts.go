package classification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ScoreAlertThreshold is one CLASSIFIER_SCORE_ALERTS entry: an alert is
// raised when QuestionKey's probability is at least Threshold.
type ScoreAlertThreshold struct {
	QuestionKey string
	Threshold   float64
}

// ParseScoreAlerts parses CLASSIFIER_SCORE_ALERTS, comma-separated
// key:threshold pairs, against the policy. Every key must be one of the
// policy's question keys, appear once, and carry a threshold in (0, 1].
// Errors name the setting and the offending key as given.
func ParseScoreAlerts(raw string, policy Policy) ([]ScoreAlertThreshold, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("CLASSIFIER_SCORE_ALERTS must list at least one key:threshold pair")
	}
	questionKeys := make(map[string]bool, len(policy.Questions))
	for _, question := range policy.Questions {
		questionKeys[question.Key] = true
	}
	seen := make(map[string]bool)
	var thresholds []ScoreAlertThreshold
	for _, entry := range strings.Split(raw, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(entry), ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return nil, fmt.Errorf("CLASSIFIER_SCORE_ALERTS: entry %q is not key:threshold", strings.TrimSpace(entry))
		}
		if !questionKeys[key] {
			return nil, fmt.Errorf("CLASSIFIER_SCORE_ALERTS: %q is not a question key of policy %s", key, policy.Version)
		}
		if seen[key] {
			return nil, fmt.Errorf("CLASSIFIER_SCORE_ALERTS: %q is listed more than once", key)
		}
		threshold, err := strconv.ParseFloat(value, 64)
		if err != nil || !(threshold > 0 && threshold <= 1) {
			return nil, fmt.Errorf("CLASSIFIER_SCORE_ALERTS: %q threshold %q must be a number greater than 0 and at most 1", key, value)
		}
		seen[key] = true
		thresholds = append(thresholds, ScoreAlertThreshold{QuestionKey: key, Threshold: threshold})
	}
	return thresholds, nil
}

// MetScoreAlerts returns the question keys whose probability meets their
// threshold, in threshold order.
// A question with no probability never meets its threshold.
func MetScoreAlerts(probabilities map[string]float64, thresholds []ScoreAlertThreshold) []string {
	var met []string
	for _, threshold := range thresholds {
		if probability, ok := probabilities[threshold.QuestionKey]; ok && probability >= threshold.Threshold {
			met = append(met, threshold.QuestionKey)
		}
	}
	return met
}

// ScoreAlert is one stored score alert to deliver.
type ScoreAlert struct {
	ID          int64
	ResultID    int64
	JobID       int64
	QuestionKey string
	SubjectKind SubjectKind
	Attempts    int
	// SubjectURI is the result's subject: an AT-URI for a post or comment,
	// the community DID for a community.
	SubjectURI   string
	CommunityDID string
	// RootURI is a comment's root post AT-URI; empty for other kinds or when
	// the comment row is gone.
	RootURI string
}

// RenderScoreAlert renders the operator message for an alert. The first line
// carries the result ID, question key and subject kind. A URI line follows
// with the subject identifier verbatim when there is one, and an Open line
// with the subject's web link under webBaseURL when one can be built. The
// message names the subject, so it is sent to operators and never logged;
// the Telegram client disables link previews, so Telegram never fetches the
// linked page.
func RenderScoreAlert(alert ScoreAlert, webBaseURL string) string {
	lines := []string{fmt.Sprintf("Classifier score alert: result %d, question %s, subject %s",
		alert.ResultID, alert.QuestionKey, alert.SubjectKind)}
	if alert.SubjectURI != "" {
		lines = append(lines, "URI: "+alert.SubjectURI)
		if link, ok := scoreAlertLink(alert, webBaseURL); ok {
			lines = append(lines, "Open: "+link)
		}
	}
	return strings.Join(lines, "\n")
}

// scoreAlertLink builds the subject's web link, escaping each path segment.
// ok is false when the base URL is empty, the community DID is missing, or a
// needed AT-URI (the subject, or a comment's root post) does not parse to a
// repository and record key.
func scoreAlertLink(alert ScoreAlert, webBaseURL string) (string, bool) {
	base := strings.TrimSuffix(webBaseURL, "/")
	if base == "" || alert.CommunityDID == "" {
		return "", false
	}
	community := base + "/c/" + url.PathEscape(alert.CommunityDID)
	switch alert.SubjectKind {
	case SubjectKindCommunity:
		return community, true
	case SubjectKindPost:
		author, rkey, ok := scoreAlertRecord(alert.SubjectURI)
		if !ok {
			return "", false
		}
		return community + "/post/" + author + "/" + rkey, true
	case SubjectKindComment:
		rootAuthor, rootKey, ok := scoreAlertRecord(alert.RootURI)
		if !ok {
			return "", false
		}
		commenter, commentKey, ok := scoreAlertRecord(alert.SubjectURI)
		if !ok {
			return "", false
		}
		return community + "/post/" + rootAuthor + "/" + rootKey + "/comment/" + commenter + "/" + commentKey, true
	default:
		return "", false
	}
}

// scoreAlertRecord returns the escaped repository and record key of a record
// AT-URI; ok is false when uri does not parse or has no record key.
func scoreAlertRecord(uri string) (repository, recordKey string, ok bool) {
	parsed, err := syntax.ParseATURI(uri)
	if err != nil || parsed.RecordKey() == "" {
		return "", "", false
	}
	return url.PathEscape(parsed.Authority().String()), url.PathEscape(parsed.RecordKey().String()), true
}

// MessageSender delivers one plain-text message to operators.
type MessageSender interface {
	SendMessage(ctx context.Context, text string) error
}

// ErrScoreAlertLeaseLost means an alert's claim lease expired, and another
// claim took it or it was delivered, before the claim's outcome was recorded.
// The outcome is not recorded.
var ErrScoreAlertLeaseLost = errors.New("classification score alert lease lost")

// Score alert error codes. ScoreAlertErrorCodeSendFailed is the only one
// stored (last_error_code); the others are logged.
const (
	// ScoreAlertErrorCodeSendFailed: the sender failed. The Telegram adapter
	// scrubs its errors, so no finer class is reliable.
	ScoreAlertErrorCodeSendFailed = "send_failed"
	// ScoreAlertErrorCodeLeaseLost: the claim's lease expired before its
	// outcome was recorded.
	ScoreAlertErrorCodeLeaseLost = "lease_lost"
	// ScoreAlertErrorCodeClaimFailed: claiming a due alert failed.
	ScoreAlertErrorCodeClaimFailed = "claim_failed"
	// ScoreAlertErrorCodeRecordFailed: recording a claimed alert's outcome
	// failed.
	ScoreAlertErrorCodeRecordFailed = "record_failed"
	// ScoreAlertErrorCodeAttemptsExhausted: an alert's last allowed send
	// failed, so it is left undelivered.
	ScoreAlertErrorCodeAttemptsExhausted = "attempts_exhausted"
)

// scoreAlertCodeContextDone is deliverDue's code for a run whose context
// ended. It is not a failure: Sweep returns the context's error and
// DeliverPending logs nothing.
const scoreAlertCodeContextDone = "context_done"

// The errors Sweep returns. They are fixed, so a returned error never carries
// repository text that could name a subject.
var (
	errScoreAlertClaimFailed  = errors.New("classification score alert: claiming a due alert failed")
	errScoreAlertRecordFailed = errors.New("classification score alert: recording an alert outcome failed")
)

// ScoreAlertRepository stores score alerts and their delivery state.
type ScoreAlertRepository interface {
	// ClaimDueAlert claims the undelivered alert below attemptLimit that is
	// due at now (never attempted, or its next attempt at or before now),
	// oldest due first; jobID 0 means any job. The claimed alert's next
	// attempt moves to leaseUntil, so no other claim returns it until then;
	// an alert another claim is taking at the same moment is skipped. The
	// returned alert carries its attempts before the claim. ok is false when
	// nothing is due.
	ClaimDueAlert(ctx context.Context, jobID int64, now, leaseUntil time.Time, attemptLimit int) (ScoreAlert, bool, error)
	// MarkAlertDelivered records the delivery of a claimed alert. It returns
	// ErrScoreAlertLeaseLost when the alert no longer holds lease.
	MarkAlertDelivered(ctx context.Context, id int64, lease, at time.Time) error
	// MarkAlertFailed increments the attempt count and records the failure of
	// a claimed alert. It returns ErrScoreAlertLeaseLost when the alert no
	// longer holds lease.
	MarkAlertFailed(ctx context.Context, id int64, lease time.Time, errorCode string, nextAttemptAt time.Time) error
}

// ScoreAlertDeliveryConfig configures score alert delivery.
type ScoreAlertDeliveryConfig struct {
	// AttemptLimit is how many failed sends an alert gets before it is left
	// undelivered.
	AttemptLimit int
	// RetryBaseDelay is the wait after the first failed send; each further
	// failure doubles it, up to scoreAlertMaxRetryDelay.
	RetryBaseDelay time.Duration
	// WebBaseURL is the web app's base URL (APPVIEW_PUBLIC_URL) that alert
	// links are built from. Empty means alerts carry no link.
	WebBaseURL string
}

// scoreAlertClaimLease is how long a claimed alert stays out of other claims
// while its send runs. Alerts are claimed one at a time, so a lease covers one
// send and is well above a Telegram send's timeout. A lease expires when the
// claiming process died or its send outlived the lease; the alert is then
// retried, and the expired claim's outcome is not recorded.
const scoreAlertClaimLease = 5 * time.Minute

// scoreAlertMaxRetryDelay caps the retry backoff.
const scoreAlertMaxRetryDelay = 6 * time.Hour

// scoreAlertRetryDelay is the wait after the attempts-th failed send:
// base*2^(attempts-1), capped at scoreAlertMaxRetryDelay.
func scoreAlertRetryDelay(base time.Duration, attempts int) time.Duration {
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= scoreAlertMaxRetryDelay/2 {
			return scoreAlertMaxRetryDelay
		}
		delay *= 2
	}
	return min(delay, scoreAlertMaxRetryDelay)
}

// ScoreAlertService delivers score alerts to operators.
type ScoreAlertService struct {
	repo   ScoreAlertRepository
	sender MessageSender
	cfg    ScoreAlertDeliveryConfig
	now    func() time.Time
	logger *slog.Logger
}

// NewScoreAlertService builds a ScoreAlertService. A nil sender means
// Telegram is unconfigured: alerts are recorded by the worker and never sent.
// A nil now uses time.Now and a nil logger uses slog.Default.
func NewScoreAlertService(repo ScoreAlertRepository, sender MessageSender, cfg ScoreAlertDeliveryConfig, now func() time.Time, logger *slog.Logger) *ScoreAlertService {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ScoreAlertService{repo: repo, sender: sender, cfg: cfg, now: now, logger: logger}
}

// DeliverPending sends the due alerts of one job. The worker calls it after
// the job's result commits. Failures are recorded on the alert for the sweep
// to retry and logged, never returned.
func (s *ScoreAlertService) DeliverPending(ctx context.Context, jobID int64) {
	if s.sender == nil {
		return
	}
	if code := s.deliverDue(ctx, jobID); code != "" && code != scoreAlertCodeContextDone {
		s.logger.Error("classification score alert: delivering due alerts failed",
			slog.Int64("job_id", jobID), slog.String("error_code", code))
	}
}

// Sweep sends every due alert. It returns the context's error when ctx ends
// and a fixed error when the repository fails; a failed send is recorded on
// the alert with its next attempt time.
func (s *ScoreAlertService) Sweep(ctx context.Context) error {
	if s.sender == nil {
		return nil
	}
	switch s.deliverDue(ctx, 0) {
	case "":
		return nil
	case scoreAlertCodeContextDone:
		return ctx.Err()
	case ScoreAlertErrorCodeClaimFailed:
		return errScoreAlertClaimFailed
	default:
		return errScoreAlertRecordFailed
	}
}

// deliverDue claims and delivers due alerts one at a time until none is due;
// jobID 0 means every job. Each claim's lease starts from the clock just
// before its own send. A delivered or failed alert is not due again at once,
// so the loop ends. It returns scoreAlertCodeContextDone once ctx ends, the
// error code of a repository failure, or "".
func (s *ScoreAlertService) deliverDue(ctx context.Context, jobID int64) string {
	for {
		if ctx.Err() != nil {
			return scoreAlertCodeContextDone
		}
		now := s.now()
		// The lease is matched by equality against the stored timestamp, so
		// it is cut to the database's microsecond precision.
		lease := now.Add(scoreAlertClaimLease).Truncate(time.Microsecond)
		alert, ok, err := s.repo.ClaimDueAlert(ctx, jobID, now, lease, s.cfg.AttemptLimit)
		if err != nil {
			if ctx.Err() != nil {
				return scoreAlertCodeContextDone
			}
			return ScoreAlertErrorCodeClaimFailed
		}
		if !ok {
			return ""
		}
		if code := s.deliver(ctx, alert, lease); code != "" {
			return code
		}
	}
}

// deliver sends one claimed alert and records the outcome under its lease. It
// returns the error code of a repository failure, or ""; a send failure or a
// lost lease is logged and not returned. The outcome is recorded even when
// ctx ends during the send, so a delivered alert is not resent and a failed
// send is counted.
func (s *ScoreAlertService) deliver(ctx context.Context, alert ScoreAlert, lease time.Time) string {
	sendErr := s.sender.SendMessage(ctx, RenderScoreAlert(alert, s.cfg.WebBaseURL))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()
	attempts := alert.Attempts + 1
	var markErr error
	if sendErr != nil {
		next := s.now().Add(scoreAlertRetryDelay(s.cfg.RetryBaseDelay, attempts))
		markErr = s.repo.MarkAlertFailed(writeCtx, alert.ID, lease, ScoreAlertErrorCodeSendFailed, next)
		if markErr == nil && attempts >= s.cfg.AttemptLimit {
			s.logger.Error("classification score alert: last attempt failed, alert left undelivered",
				slog.Int64("alert_id", alert.ID), slog.Int64("result_id", alert.ResultID),
				slog.Int("attempts", attempts), slog.String("error_code", ScoreAlertErrorCodeAttemptsExhausted))
		} else {
			s.logger.Warn("classification score alert: send failed",
				slog.Int64("alert_id", alert.ID), slog.Int64("result_id", alert.ResultID),
				slog.Int("attempts", attempts), slog.String("error_code", ScoreAlertErrorCodeSendFailed))
		}
	} else {
		markErr = s.repo.MarkAlertDelivered(writeCtx, alert.ID, lease, s.now())
	}
	if errors.Is(markErr, ErrScoreAlertLeaseLost) {
		s.logger.Warn("classification score alert: lease expired before its outcome was recorded",
			slog.Int64("alert_id", alert.ID), slog.Int64("result_id", alert.ResultID),
			slog.Bool("send_failed", sendErr != nil), slog.String("error_code", ScoreAlertErrorCodeLeaseLost))
		return ""
	}
	if markErr != nil {
		return ScoreAlertErrorCodeRecordFailed
	}
	return ""
}

// ScoreAlertNotifier is told after a stored result with alerts commits.
type ScoreAlertNotifier interface {
	DeliverPending(ctx context.Context, jobID int64)
}

// WithScoreAlerts makes the worker record a score alert with each stored
// result for every threshold the result meets, and tell notifier after the
// result commits. A nil notifier records alerts for the sweep only.
func WithScoreAlerts(thresholds []ScoreAlertThreshold, notifier ScoreAlertNotifier) WorkerOption {
	return func(w *Worker) {
		w.scoreAlertThresholds = thresholds
		w.scoreAlertNotifier = notifier
	}
}
