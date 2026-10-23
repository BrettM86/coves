package jetstream

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

// TestValidateCommentEvent_ContentLimitsMatchLexicon pins the comment content
// bounds to the social.coves.community.comment lexicon (maxGraphemes 10000,
// maxLength 100000 bytes). Only the content differs between rows; the reply
// refs and repo DID are valid, so content alone decides the outcome.
func TestValidateCommentEvent_ContentLimitsMatchLexicon(t *testing.T) {
	// "e" plus three combining marks: 1 grapheme, 7 bytes.
	const sevenByteGrapheme = "é̂̃"
	// "e" plus fifty combining acute accents: 1 grapheme, 101 bytes.
	longCombiningGrapheme := "e" + strings.Repeat("́", 50)

	tests := []struct {
		name          string
		content       string
		wantBytes     int
		wantGraphemes int
		wantPermanent bool
	}{
		{
			name:          "9999 multi-byte graphemes over 30000 bytes is accepted",
			content:       strings.Repeat(sevenByteGrapheme, 9999),
			wantBytes:     69993,
			wantGraphemes: 9999,
			wantPermanent: false,
		},
		{
			name:          "exactly 10000 graphemes is accepted",
			content:       strings.Repeat("a", 10000),
			wantBytes:     10000,
			wantGraphemes: 10000,
			wantPermanent: false,
		},
		{
			name:          "exactly 100000 bytes within 10000 graphemes is accepted",
			content:       strings.Repeat(longCombiningGrapheme, 990) + strings.Repeat("a", 10),
			wantBytes:     100000,
			wantGraphemes: 1000,
			wantPermanent: false,
		},
		{
			name:          "100001 bytes within 10000 graphemes is rejected",
			content:       strings.Repeat(longCombiningGrapheme, 990) + strings.Repeat("a", 11),
			wantBytes:     100001,
			wantGraphemes: 1001,
			wantPermanent: true,
		},
		{
			name:          "10001 graphemes within 100000 bytes is rejected",
			content:       strings.Repeat("a", 10001),
			wantBytes:     10001,
			wantGraphemes: 10001,
			wantPermanent: true,
		},
		{
			name:          "over 100000 bytes within 10000 graphemes is rejected",
			content:       strings.Repeat(longCombiningGrapheme, 1000),
			wantBytes:     101000,
			wantGraphemes: 1000,
			wantPermanent: true,
		},
	}

	consumer := &CommentEventConsumer{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(tt.content); got != tt.wantBytes {
				t.Fatalf("fixture precondition: content is %d bytes, want %d", got, tt.wantBytes)
			}
			if got := uniseg.GraphemeClusterCount(tt.content); got != tt.wantGraphemes {
				t.Fatalf("fixture precondition: content is %d graphemes, want %d", got, tt.wantGraphemes)
			}

			comment := &CommentRecordFromJetstream{
				Type:      "social.coves.community.comment",
				Content:   tt.content,
				CreatedAt: "2026-10-22T00:00:00Z",
				Reply: ReplyRefFromJetstream{
					Root: StrongRefFromJetstream{
						URI: "at://did:plc:community123/social.coves.community.post/3kroot",
						CID: "bafyreiroot",
					},
					Parent: StrongRefFromJetstream{
						URI: "at://did:plc:community123/social.coves.community.post/3kroot",
						CID: "bafyreiroot",
					},
				},
			}

			err := consumer.validateCommentEvent(context.Background(), "did:plc:commenter123", comment)

			if tt.wantPermanent {
				if !errors.Is(err, ErrPermanentEvent) {
					t.Fatalf("validateCommentEvent() error = %v, want ErrPermanentEvent", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCommentEvent() error = %v, want nil", err)
			}
		})
	}
}
