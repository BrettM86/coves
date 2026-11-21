package jetstream

import (
	"errors"
	"strings"
	"testing"

	"Coves/internal/core/posts"
)

// TestParseCommunityDecision_RemovalReasonAndCodeBounds pins the lexicon bounds
// of social.coves.community.removal on the consumer: code maxLength 64 bytes,
// reason maxGraphemes 1000 and maxLength 10000 bytes. A record outside them can
// never become valid, so it is a permanent refusal; one at the bounds keeps its
// reason verbatim.
func TestParseCommunityDecision_RemovalReasonAndCodeBounds(t *testing.T) {
	t.Parallel()

	// 1000 graphemes, each "a" plus five U+0301 combining marks: 11 bytes per
	// grapheme, 11000 bytes in all.
	combiningHeavy := strings.Repeat("á́́́́", 1000)

	for _, tc := range []struct {
		name       string
		code       string
		reason     string
		wantRefuse bool
	}{
		{name: "1000 decomposed graphemes (2000 runes, 3000 bytes) are accepted", code: "spam", reason: strings.Repeat("e\u0301", 1000)},
		{name: "a code of exactly 64 bytes is accepted", code: strings.Repeat("c", 64), reason: "Posting the same link in every thread"},
		{name: "1001 graphemes are refused", code: "spam", reason: strings.Repeat("a", 1001), wantRefuse: true},
		{name: "over 10000 bytes within 1000 graphemes is refused", code: "spam", reason: combiningHeavy, wantRefuse: true},
		{name: "a code of 65 bytes is refused", code: strings.Repeat("c", 65), reason: "Posting the same link in every thread", wantRefuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := map[string]interface{}{
				"$type": posts.RemovalCollection,
				"subject": map[string]interface{}{
					"uri": "at://did:plc:postauthor/social.coves.community.postv2/3kabc",
					"cid": "bafyreiremovalsubjectaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				"code":      tc.code,
				"reason":    tc.reason,
				"createdAt": "2026-11-14T12:00:00.000Z",
			}

			parsed, err := parseCommunityDecision(record, posts.RemovalCollection)
			if tc.wantRefuse {
				if !errors.Is(err, ErrPermanentEvent) {
					t.Fatalf("expected ErrPermanentEvent, got err=%v (parsed=%t)", err, parsed != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the record to be accepted, got %v", err)
			}
			if parsed.Reason != tc.reason {
				t.Fatalf("reason was not kept verbatim: got %d bytes, want %d bytes", len(parsed.Reason), len(tc.reason))
			}
		})
	}
}

// TestParseCommunityDecision_RemovalReasonStripsNUL pins that a lexicon-valid
// reason containing U+0000 still yields a removal: Postgres TEXT cannot hold the
// NUL byte, so the consumer drops it from the stored reason rather than let the
// write fail on every retry and leave the post visible. A reason of only NUL
// bytes becomes empty, which is stored as no reason.
func TestParseCommunityDecision_RemovalReasonStripsNUL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		reason     string
		wantReason string
	}{
		{name: "NUL bytes inside a reason are removed", reason: "Spam\x00 link\x00", wantReason: "Spam link"},
		{name: "a reason of only NUL bytes becomes empty", reason: "\x00\x00", wantReason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := map[string]interface{}{
				"$type": posts.RemovalCollection,
				"subject": map[string]interface{}{
					"uri": "at://did:plc:postauthor/social.coves.community.postv2/3kabc",
					"cid": "bafyreiremovalsubjectaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				"code":      "spam",
				"reason":    tc.reason,
				"createdAt": "2026-11-14T12:00:00.000Z",
			}

			parsed, err := parseCommunityDecision(record, posts.RemovalCollection)
			if err != nil {
				t.Fatalf("expected the record to be accepted, got %v", err)
			}
			if parsed.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", parsed.Reason, tc.wantReason)
			}
		})
	}
}
