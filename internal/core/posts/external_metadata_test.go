package posts

import (
	"strings"
	"testing"

	"Coves/internal/core/unfurl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// familyEmoji is one grapheme cluster of 25 bytes: four 4-byte code points
// joined by three 3-byte ZERO WIDTH JOINERs.
const familyEmoji = "\U0001F468\u200d\U0001F469\u200d\U0001F467\u200d\U0001F466"

// Unfurled og:title / og:description / oEmbed provider_name come from an
// arbitrary third-party page. Whatever they hold, the external embed this
// AppView signs must fit social.coves.embed.external#external:
// title 500 graphemes / 5000 bytes, description 1000 graphemes / 10000 bytes,
// provider 100 bytes. Truncation must never split a grapheme cluster.
func TestMergeUnfurlMetadata_TruncatesToLexiconLimits(t *testing.T) {
	require.Equal(t, 25, len(familyEmoji), "fixture: family emoji must be 25 bytes")

	tests := []struct {
		name     string
		external map[string]interface{}
		result   unfurl.UnfurlResult
		want     map[string]interface{}
	}{
		{
			name:     "50,000-character ASCII description is cut to 1000 characters",
			external: map[string]interface{}{"uri": "https://example.com/a"},
			result: unfurl.UnfurlResult{
				Type: "article", Title: "Short title", Description: strings.Repeat("a", 50000),
				Provider: "example", Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       "Short title",
				"description": strings.Repeat("a", 1000),
				"embedType":   "article",
				"provider":    "example",
				"domain":      "example.com",
			},
		},
		{
			name:     "over-long ASCII title is cut to 500 characters",
			external: map[string]interface{}{"uri": "https://example.com/a"},
			result: unfurl.UnfurlResult{
				Type: "article", Title: strings.Repeat("t", 2000), Description: "d",
				Provider: "example", Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       strings.Repeat("t", 500),
				"description": "d",
				"embedType":   "article",
				"provider":    "example",
				"domain":      "example.com",
			},
		},
		{
			name:     "300-byte provider is cut to its first 100 bytes",
			external: map[string]interface{}{"uri": "https://example.com/a"},
			result: unfurl.UnfurlResult{
				Type: "video", Title: "t", Description: "d",
				Provider: strings.Repeat("p", 100) + strings.Repeat("q", 200), Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       "t",
				"description": "d",
				"embedType":   "video",
				"provider":    strings.Repeat("p", 100),
				"domain":      "example.com",
			},
		},
		{
			name:     "multibyte description hits the 10000-byte cap before the grapheme cap, nothing split",
			external: map[string]interface{}{"uri": "https://example.com/a"},
			result: unfurl.UnfurlResult{
				Type: "article", Title: "t", Description: strings.Repeat(familyEmoji, 1000),
				Provider: "example", Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       "t",
				"description": strings.Repeat(familyEmoji, 400),
				"embedType":   "article",
				"provider":    "example",
				"domain":      "example.com",
			},
		},
		{
			name:     "multibyte title hits the 5000-byte cap before the grapheme cap, nothing split",
			external: map[string]interface{}{"uri": "https://example.com/a"},
			result: unfurl.UnfurlResult{
				Type: "article", Title: strings.Repeat(familyEmoji, 300), Description: "d",
				Provider: "example", Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       strings.Repeat(familyEmoji, 200),
				"description": "d",
				"embedType":   "article",
				"provider":    "example",
				"domain":      "example.com",
			},
		},
		{
			name: "client-supplied title is preserved, not overwritten by the unfurl",
			external: map[string]interface{}{
				"uri":   "https://example.com/a",
				"title": "Client title",
			},
			result: unfurl.UnfurlResult{
				Type: "article", Title: strings.Repeat("u", 2000), Description: "d",
				Provider: "example", Domain: "example.com",
			},
			want: map[string]interface{}{
				"uri":         "https://example.com/a",
				"title":       "Client title",
				"description": "d",
				"embedType":   "article",
				"provider":    "example",
				"domain":      "example.com",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.result
			mergeUnfurlMetadata(tt.external, &result)
			assert.Equal(t, tt.want, tt.external)
		})
	}
}

// A client may supply embed.external.title / description itself. The shared
// content gate must refuse values the lexicon forbids rather than sign them.
func TestNormalizeAndValidatePostContent_RejectsOverLimitExternalText(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]interface{}
		wantErr bool
	}{
		{
			name:    "title of 501 ASCII graphemes",
			extra:   map[string]interface{}{"title": strings.Repeat("a", 501)},
			wantErr: true,
		},
		{
			name:    "title of exactly 500 ASCII graphemes",
			extra:   map[string]interface{}{"title": strings.Repeat("a", 500)},
			wantErr: false,
		},
		{
			name:    "description of 1001 ASCII graphemes",
			extra:   map[string]interface{}{"description": strings.Repeat("a", 1001)},
			wantErr: true,
		},
		{
			name:    "title of 400 CJK graphemes (1200 bytes) is within both limits",
			extra:   map[string]interface{}{"title": strings.Repeat("日", 400)},
			wantErr: false,
		},
		{
			name:    "numeric title is not a string",
			extra:   map[string]interface{}{"title": float64(42)},
			wantErr: true,
		},
		{
			name:    "array description is not a string",
			extra:   map[string]interface{}{"description": []interface{}{strings.Repeat("a", 5000)}},
			wantErr: true,
		},
		{
			name:    "null title is left for the unfurl to fill",
			extra:   map[string]interface{}{"title": nil},
			wantErr: false,
		},
		{
			name:    "title of 250 graphemes but 6250 bytes exceeds the 5000-byte cap",
			extra:   map[string]interface{}{"title": strings.Repeat(familyEmoji, 250)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalizeAndValidatePostContent(postContent{
				Embed: embedExternal("https://example.com/a", tt.extra),
			})
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, IsValidationError(err), "want a ValidationError, got %T: %v", err, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
