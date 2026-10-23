package utils

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// TestParseCreatedAt_ClampsFutureTimestamp pins that a record asserting a
// createdAt in the future is clamped to the current time, so a hostile repo
// cannot pin itself to the top of subscribed_at/blocked_at DESC orderings.
func TestParseCreatedAt_ClampsFutureTimestamp(t *testing.T) {
	tests := []struct {
		name      string
		createdAt func() string
		// expected, when non-zero, is the exact time the result must equal.
		// When zero, the result must fall within the wall-clock bounds of the call.
		expected time.Time
	}{
		{
			name: "future createdAt is clamped to now",
			createdAt: func() string {
				return time.Now().Add(48 * time.Hour).Format(time.RFC3339)
			},
		},
		{
			name:      "past createdAt is preserved",
			createdAt: func() string { return "2024-01-15T10:30:00Z" },
			expected:  time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := map[string]interface{}{"createdAt": tt.createdAt()}

			before := time.Now()
			got := ParseCreatedAt(record, "at://did:plc:test/social.coves.community.block/abc")
			after := time.Now()

			if !tt.expected.IsZero() {
				if !got.Equal(tt.expected) {
					t.Fatalf("ParseCreatedAt() = %v, want %v", got, tt.expected)
				}
				return
			}

			if got.After(after) {
				t.Fatalf("ParseCreatedAt() = %v, want clamped to now (not after %v)", got, after)
			}
			if got.Before(before.Add(-time.Second)) {
				t.Fatalf("ParseCreatedAt() = %v, want clamped to now (not before %v)", got, before.Add(-time.Second))
			}
		})
	}
}

// TestParseCreatedAt_WarningNamesRecordURI pins that the clamp warning names
// the record it came from, so an operator can find the offending record.
func TestParseCreatedAt_WarningNamesRecordURI(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	const uri = "at://did:plc:blocker/social.coves.actor.block/3kabc"
	record := map[string]interface{}{"createdAt": time.Now().Add(48 * time.Hour).Format(time.RFC3339)}
	ParseCreatedAt(record, uri)

	if !strings.Contains(buf.String(), uri) {
		t.Fatalf("clamp warning %q does not name record %s", buf.String(), uri)
	}
}
