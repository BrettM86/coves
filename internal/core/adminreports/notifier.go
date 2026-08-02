package adminreports

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Notifier raises an out-of-band alert when a report is filed.
//
// Reports land in a Postgres table that nobody is watching. Without an alert a
// CSAM report can sit unread indefinitely while the response clock runs, which
// is the failure this port exists to close. It is deliberately
// channel-agnostic — Telegram today, email or push later — and lives in the
// domain rather than in an adapter because *what an alert says* about a report
// is a domain decision, not a transport one.
type Notifier interface {
	// NotifyNewReport alerts an operator that report has been filed.
	//
	// Delivery is best-effort by contract: the report row is already committed
	// by the time this runs, so an error here is a failed alert, never a
	// failed report.
	NotifyNewReport(ctx context.Context, report *Report) error
}

// MessageSender delivers a single plain-text message to an operator over some
// channel. Keeping the transport this narrow means the domain never learns
// what Telegram is, and any future channel satisfies it without touching this
// package.
type MessageSender interface {
	SendMessage(ctx context.Context, text string) error
}

// Notifier construction and dispatch errors.
var (
	// ErrNilReport indicates NotifyNewReport was handed no report.
	ErrNilReport = errors.New("cannot raise an alert for a nil report")

	// ErrNoMessageSender indicates the notifier was built without a transport.
	ErrNoMessageSender = errors.New("notifier has no message sender")

	// ErrUnknownAlertReason indicates a configured alert reason is not one of
	// the valid report categories.
	ErrUnknownAlertReason = errors.New("unknown alert reason")
)

const (
	// maxAlertTargetURI bounds how much of the reported URI reaches the alert.
	//
	// TargetURI is caller-supplied and effectively unbounded: atURIPattern uses
	// unbounded quantifiers and SubmitReportRequest.Validate caps only the
	// explanation, so a pattern-valid multi-kilobyte URI passes validation and
	// stores fine. Sent verbatim it would push the message past the delivery
	// channel's size limit and the alert would be rejected outright — handing a
	// reporter the ability to suppress the alert about their own report. The
	// full URI is in the row; the alert only needs enough to recognise it.
	maxAlertTargetURI = 300

	// maxAlertLength clamps the rendered alert. 4096 characters is the smallest
	// per-message ceiling among the channels this text is delivered over
	// (Telegram's sendMessage limit); the margin absorbs the truncation marker.
	// Clamping in the domain rather than in a transport means every present and
	// future channel inherits the guarantee.
	maxAlertLength = 4000

	// truncationMarker is appended wherever text was cut, so a reader never
	// mistakes a shortened value for the whole one.
	truncationMarker = "…(truncated)"
)

// urgentReasons are the categories that carry legal exposure and a response
// clock. They are flagged in the alert text so a genuine emergency is visually
// distinct from a spam report in a phone notification.
var urgentReasons = map[Reason]bool{
	ReasonCSAM:    true,
	ReasonDoxing:  true,
	ReasonIllegal: true,
}

// ParseAlertReasons converts operator-supplied reason names into the domain
// vocabulary, rejecting anything that is not a valid category.
//
// An unrecognised entry is an error rather than a silently dropped filter: a
// typo like "csam " or "CSAM_URGENT" that quietly narrowed the alert set to
// nothing would restore exactly the silence this feature removes.
//
// An empty input returns nil, which NewMessageNotifier reads as "alert on
// every reason".
func ParseAlertReasons(names []string) ([]Reason, error) {
	var reasons []Reason
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if !IsValidReason(trimmed) {
			return nil, fmt.Errorf("%w: %q is not one of csam, doxing, harassment, spam, illegal, other",
				ErrUnknownAlertReason, trimmed)
		}
		reasons = append(reasons, Reason(trimmed))
	}
	return reasons, nil
}

// messageNotifier formats reports as plain text and hands them to a sender.
type messageNotifier struct {
	sender MessageSender

	// reasons restricts which categories raise an alert. Nil or empty means
	// every reason alerts.
	reasons map[Reason]bool
}

// NewMessageNotifier builds a Notifier that renders reports as plain text and
// delivers them through sender.
//
// reasons filters which categories alert; passing nil alerts on every reason.
// The filter exists because alert fatigue is the standard way systems like
// this die — an operator who has learned to swipe away spam reports will swipe
// away a CSAM report too — but it defaults to open on purpose: there is no
// admin read endpoint yet, so a filtered-out report is one nobody ever sees.
func NewMessageNotifier(sender MessageSender, reasons []Reason) Notifier {
	notifier := &messageNotifier{sender: sender}
	if len(reasons) > 0 {
		notifier.reasons = make(map[Reason]bool, len(reasons))
		for _, reason := range reasons {
			notifier.reasons[reason] = true
		}
	}
	return notifier
}

// NotifyNewReport sends the alert for report, unless its reason is filtered out.
func (n *messageNotifier) NotifyNewReport(ctx context.Context, report *Report) error {
	if report == nil {
		return ErrNilReport
	}
	if n.sender == nil {
		return ErrNoMessageSender
	}
	if len(n.reasons) > 0 && !n.reasons[report.Reason] {
		return nil
	}
	return n.sender.SendMessage(ctx, FormatReportAlert(report))
}

// FormatReportAlert renders a report as the plain-text body of an operator
// alert.
//
// The reporter's DID and the explanation text are deliberately withheld.
// Alerts travel through third-party infrastructure (Telegram's servers, and
// whatever mail provider is added later), and two things must not go there:
// the explanation, which for a CSAM or doxing report quotes the very content
// being reported, and the reporter's identity, which links a real person to an
// accusation. Both are one indexed lookup away for an operator who needs them,
// which is the point of leading with the report ID.
func FormatReportAlert(report *Report) string {
	var text strings.Builder

	if urgentReasons[report.Reason] {
		text.WriteString("\U0001F6A8 URGENT — new Coves report\n\n")
	} else {
		text.WriteString("New Coves report\n\n")
	}

	fmt.Fprintf(&text, "Report:  #%d\n", report.ID)
	fmt.Fprintf(&text, "Reason:  %s\n", report.Reason)
	fmt.Fprintf(&text, "Target:  %s\n", report.TargetType)
	fmt.Fprintf(&text, "URI:     %s\n", truncateRunes(report.TargetURI, maxAlertTargetURI))
	fmt.Fprintf(&text, "Filed:   %s\n", report.CreatedAt.UTC().Format(time.RFC3339))

	// TODO(admin-read-endpoints): until listReports/resolveReport exist, this
	// query *is* the runbook — an alert an operator cannot act on is only
	// marginally better than no alert. Replace it with a deep link once they
	// land. Note SELECT * returns the two fields this alert withholds, so it is
	// an escape hatch for someone at a psql prompt, not for the chat.
	text.WriteString("\nReporter and explanation withheld from this alert.\n")
	fmt.Fprintf(&text, "Read them with:\n  SELECT * FROM admin_reports WHERE id = %d;", report.ID)

	// Clamp last: every field above is bounded, but the ceiling is a property of
	// the whole message, and a future field must not be able to breach it.
	return truncateRunes(text.String(), maxAlertLength)
}

// truncateRunes shortens s to at most limit runes, marking that it was cut.
//
// Runes rather than bytes: slicing a multi-byte sequence mid-character yields
// invalid UTF-8, which delivery channels reject — turning a too-long alert into
// a rejected one, which is the failure this is here to prevent.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + truncationMarker
}
