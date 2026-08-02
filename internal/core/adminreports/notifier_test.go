package adminreports

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// stubSender records what a notifier hands to its transport.
type stubSender struct {
	sendFunc func(ctx context.Context, text string) error
	messages []string
}

func (s *stubSender) SendMessage(ctx context.Context, text string) error {
	s.messages = append(s.messages, text)
	if s.sendFunc != nil {
		return s.sendFunc(ctx, text)
	}
	return nil
}

// testReport builds a report with every field populated, so a test asserting
// that something is *absent* from an alert cannot pass by accident.
func testReport(reason Reason) *Report {
	return &Report{
		ID:          47,
		ReporterDID: "did:plc:reporter7890",
		TargetURI:   "at://did:plc:author123/social.coves.post/abc123",
		TargetType:  TargetTypePost,
		Reason:      reason,
		Explanation: "the explanation text quoting the reported content",
		Status:      StatusOpen,
		CreatedAt:   time.Date(2026, 8, 1, 14, 22, 31, 0, time.UTC),
	}
}

func TestNotifyNewReport_SendsAlert(t *testing.T) {
	sender := &stubSender{}
	notifier := NewMessageNotifier(sender, nil)
	report := testReport(ReasonSpam)

	if err := notifier.NotifyNewReport(context.Background(), report); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(sender.messages) != 1 {
		t.Fatalf("expected exactly 1 message, got %d", len(sender.messages))
	}

	// Assert on what actually reaches the transport, not just that something
	// did. The privacy guarantee belongs to the notifier: checking only
	// FormatReportAlert leaves `SendMessage(ctx, alert + report.Explanation)`
	// passing every test in this file.
	if got := sender.messages[0]; got != FormatReportAlert(report) {
		t.Errorf("notifier sent something other than the formatted alert:\n%s", got)
	}
}

// The boundary version of TestFormatReportAlert_WithholdsReporterAndExplanation:
// what the transport receives is what leaves our infrastructure.
func TestNotifyNewReport_SentMessageWithholdsReporterAndExplanation(t *testing.T) {
	for _, reason := range ValidReasons() {
		t.Run(string(reason), func(t *testing.T) {
			sender := &stubSender{}
			report := testReport(reason)

			if err := NewMessageNotifier(sender, nil).NotifyNewReport(context.Background(), report); err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if len(sender.messages) != 1 {
				t.Fatalf("expected exactly 1 message, got %d", len(sender.messages))
			}

			sent := sender.messages[0]
			if strings.Contains(sent, report.ReporterDID) {
				t.Errorf("message sent to the transport leaked the reporter DID:\n%s", sent)
			}
			if strings.Contains(sent, report.Explanation) {
				t.Errorf("message sent to the transport leaked the explanation:\n%s", sent)
			}
		})
	}
}

// A reporter must not be able to suppress the alert about their own report by
// submitting an oversized (but pattern-valid) target URI that pushes the
// message past the delivery channel's size limit.
func TestFormatReportAlert_BoundsOversizedTargetURI(t *testing.T) {
	report := testReport(ReasonCSAM)
	report.TargetURI = "at://did:plc:" + strings.Repeat("a", 9000) + "/social.coves.post/abc123"

	alert := FormatReportAlert(report)

	if runes := len([]rune(alert)); runes > maxAlertLength {
		t.Errorf("alert is %d runes, want <= %d", runes, maxAlertLength)
	}
	if !strings.Contains(alert, truncationMarker) {
		t.Errorf("an oversized URI must be marked as truncated:\n%s", alert)
	}
	// Truncation must not cost the operator the report ID — it is the only
	// handle they have for finding the full record.
	if !strings.Contains(alert, "#47") {
		t.Errorf("truncation dropped the report ID:\n%s", alert)
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{"under the limit is untouched", "short", 10, "short"},
		{"at the limit is untouched", "exactly10!", 10, "exactly10!"},
		{"over the limit is marked", "abcdefghijk", 10, "abcdefghij" + truncationMarker},
		{"empty stays empty", "", 10, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateRunes(tt.input, tt.limit); got != tt.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.want)
			}
		})
	}

	// Cutting mid-sequence would emit invalid UTF-8, which channels reject —
	// turning a too-long alert into a rejected one, the failure this prevents.
	t.Run("cuts on rune boundaries", func(t *testing.T) {
		got := truncateRunes(strings.Repeat("é", 20), 5)
		if !utf8.ValidString(got) {
			t.Errorf("truncation produced invalid UTF-8: %q", got)
		}
		if want := strings.Repeat("é", 5) + truncationMarker; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// The alert must never carry the reporter's identity or the explanation text:
// both travel through third-party infrastructure, and the explanation quotes
// the very content being reported.
func TestFormatReportAlert_WithholdsReporterAndExplanation(t *testing.T) {
	report := testReport(ReasonCSAM)
	alert := FormatReportAlert(report)

	if strings.Contains(alert, report.ReporterDID) {
		t.Errorf("alert leaked the reporter DID:\n%s", alert)
	}
	if strings.Contains(alert, report.Explanation) {
		t.Errorf("alert leaked the explanation text:\n%s", alert)
	}
}

func TestFormatReportAlert_CarriesActionableFields(t *testing.T) {
	report := testReport(ReasonDoxing)
	alert := FormatReportAlert(report)

	for _, want := range []string{
		"#47",
		string(ReasonDoxing),
		string(TargetTypePost),
		report.TargetURI,
		"2026-08-01T14:22:31Z",
	} {
		if !strings.Contains(alert, want) {
			t.Errorf("alert missing %q:\n%s", want, alert)
		}
	}
}

// Urgency has to be visible in the notification preview on a phone, or the
// distinction between a spam report and a CSAM report is lost at the moment it
// matters most.
func TestFormatReportAlert_FlagsUrgentReasons(t *testing.T) {
	tests := []struct {
		reason     Reason
		wantUrgent bool
	}{
		{ReasonCSAM, true},
		{ReasonDoxing, true},
		{ReasonIllegal, true},
		{ReasonSpam, false},
		{ReasonHarassment, false},
		{ReasonOther, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			alert := FormatReportAlert(testReport(tt.reason))
			gotUrgent := strings.Contains(alert, "URGENT")

			if gotUrgent != tt.wantUrgent {
				t.Errorf("reason %q: urgent flag = %v, want %v:\n%s",
					tt.reason, gotUrgent, tt.wantUrgent, alert)
			}
		})
	}
}

func TestNotifyNewReport_FiltersByReason(t *testing.T) {
	sender := &stubSender{}
	notifier := NewMessageNotifier(sender, []Reason{ReasonCSAM, ReasonIllegal})

	if err := notifier.NotifyNewReport(context.Background(), testReport(ReasonSpam)); err != nil {
		t.Fatalf("a filtered reason must not error, got: %v", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("expected spam to be filtered out, got %d message(s)", len(sender.messages))
	}

	if err := notifier.NotifyNewReport(context.Background(), testReport(ReasonCSAM)); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("expected csam to pass the filter, got %d message(s)", len(sender.messages))
	}
}

// An empty filter must mean "everything". Reading it as "nothing" would leave
// the default configuration silently delivering no alerts at all.
func TestNotifyNewReport_EmptyFilterAlertsOnEveryReason(t *testing.T) {
	sender := &stubSender{}
	notifier := NewMessageNotifier(sender, nil)

	for _, reason := range ValidReasons() {
		if err := notifier.NotifyNewReport(context.Background(), testReport(reason)); err != nil {
			t.Fatalf("reason %q: expected no error, got: %v", reason, err)
		}
	}

	if len(sender.messages) != len(ValidReasons()) {
		t.Errorf("expected %d alerts, got %d", len(ValidReasons()), len(sender.messages))
	}
}

func TestNotifyNewReport_PropagatesSenderError(t *testing.T) {
	sendErr := errors.New("transport unavailable")
	sender := &stubSender{
		sendFunc: func(ctx context.Context, text string) error { return sendErr },
	}
	notifier := NewMessageNotifier(sender, nil)

	err := notifier.NotifyNewReport(context.Background(), testReport(ReasonCSAM))
	if !errors.Is(err, sendErr) {
		t.Fatalf("expected the sender's error to surface, got: %v", err)
	}
}

func TestNotifyNewReport_RejectsNilInputs(t *testing.T) {
	t.Run("nil report", func(t *testing.T) {
		notifier := NewMessageNotifier(&stubSender{}, nil)
		if err := notifier.NotifyNewReport(context.Background(), nil); !errors.Is(err, ErrNilReport) {
			t.Fatalf("expected ErrNilReport, got: %v", err)
		}
	})

	t.Run("nil sender", func(t *testing.T) {
		notifier := NewMessageNotifier(nil, nil)
		err := notifier.NotifyNewReport(context.Background(), testReport(ReasonCSAM))
		if !errors.Is(err, ErrNoMessageSender) {
			t.Fatalf("expected ErrNoMessageSender, got: %v", err)
		}
	})
}

func TestParseAlertReasons(t *testing.T) {
	t.Run("valid names", func(t *testing.T) {
		reasons, err := ParseAlertReasons([]string{"csam", " doxing ", "illegal"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		want := []Reason{ReasonCSAM, ReasonDoxing, ReasonIllegal}
		if len(reasons) != len(want) {
			t.Fatalf("expected %d reasons, got %d: %v", len(want), len(reasons), reasons)
		}
		for i, reason := range reasons {
			if reason != want[i] {
				t.Errorf("reason %d = %q, want %q", i, reason, want[i])
			}
		}
	})

	t.Run("empty input means every reason", func(t *testing.T) {
		reasons, err := ParseAlertReasons(nil)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if reasons != nil {
			t.Errorf("expected nil (alert on everything), got: %v", reasons)
		}
	})

	// A typo must fail startup. Silently dropping it would narrow the alert set
	// without saying so — restoring the silence this feature exists to remove.
	t.Run("unknown name is rejected", func(t *testing.T) {
		_, err := ParseAlertReasons([]string{"csam", "urgent"})
		if !errors.Is(err, ErrUnknownAlertReason) {
			t.Fatalf("expected ErrUnknownAlertReason, got: %v", err)
		}
		if !strings.Contains(err.Error(), "urgent") {
			t.Errorf("error should name the offending value, got: %v", err)
		}
	})
}
