package classification_test

import (
	"testing"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
)

// TestNewPostContent pins how a post's classified fields become the text sent
// to the classifier, and the rune-based truncation (B2).
func TestNewPostContent(t *testing.T) {
	for _, test := range []struct {
		name          string
		fields        classification.PostFields
		limit         int
		wantText      string
		wantTruncated bool
	}{
		{
			name: "all fields joined in order",
			fields: classification.PostFields{
				Title:         "Harbor seals",
				Body:          "Spotted three on the rocks.",
				ExternalURI:   "https://example.com/seals",
				ExternalTitle: "Seal survey",
			},
			limit:    8000,
			wantText: "Harbor seals\n\nSpotted three on the rocks.\n\nSeal survey\n\nhttps://example.com/seals",
		},
		{
			name: "empty parts are skipped",
			fields: classification.PostFields{
				Title:       "Harbor seals",
				ExternalURI: "https://example.com/seals",
			},
			limit:    8000,
			wantText: "Harbor seals\n\nhttps://example.com/seals",
		},
		{
			name:          "longer than the limit keeps the first limit runes",
			fields:        classification.PostFields{Title: "abcdefghij"},
			limit:         4,
			wantText:      "abcd",
			wantTruncated: true,
		},
		{
			name:          "multi-byte runes count as one each",
			fields:        classification.PostFields{Title: "日本語テキスト"},
			limit:         3,
			wantText:      "日本語",
			wantTruncated: true,
		},
		{
			name:          "truncation applies to the joined text",
			fields:        classification.PostFields{Title: "héllo", Body: "wörld"},
			limit:         8,
			wantText:      "héllo\n\nw",
			wantTruncated: true,
		},
		{
			name:     "exactly the limit is not truncated",
			fields:   classification.PostFields{Title: "日本語テ"},
			limit:    4,
			wantText: "日本語テ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := classification.NewPostContent(test.fields, test.limit)
			assert.Equal(t, test.wantText, content.Text)
			assert.Equal(t, test.wantTruncated, content.TextTruncated)
		})
	}
}

// TestPostFieldsFromRecord pins which record fields are classified: title,
// body and an external embed's link; an images embed gives no link (B2).
func TestPostFieldsFromRecord(t *testing.T) {
	title := "Harbor seals"
	body := "Spotted three on the rocks."

	t.Run("external embed gives the link", func(t *testing.T) {
		embed := `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/seals","title":"Seal survey","description":"Annual count"}}`
		assert.Equal(t, classification.PostFields{
			Title:         "Harbor seals",
			Body:          "Spotted three on the rocks.",
			ExternalURI:   "https://example.com/seals",
			ExternalTitle: "Seal survey",
		}, classification.PostFieldsFromRecord(&title, &body, embed))
	})

	t.Run("images embed gives no link", func(t *testing.T) {
		embed := `{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"bafkreib2ikx3q5kawl3cqhvbvquyqthfsc3iapmcvjtpn4tbsdkdx5wexi"},"mimeType":"image/jpeg","size":1234},"alt":"https://example.com/not-a-link"}]}`
		assert.Equal(t, classification.PostFields{
			Title: "Harbor seals",
			Body:  "Spotted three on the rocks.",
		}, classification.PostFieldsFromRecord(&title, &body, embed))
	})

	t.Run("nil title and no embed", func(t *testing.T) {
		assert.Equal(t, classification.PostFields{
			Body: "Spotted three on the rocks.",
		}, classification.PostFieldsFromRecord(nil, &body, ""))
	})
}

// TestNewCommentContent pins that a comment's classifier text is exactly its
// body, cut to the char limit in runes (U1).
func TestNewCommentContent(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          string
		limit         int
		wantText      string
		wantTruncated bool
	}{
		{
			name:     "body only",
			body:     "Saw two otters at the marina this morning.",
			limit:    8000,
			wantText: "Saw two otters at the marina this morning.",
		},
		{
			name:     "exactly the limit is not truncated",
			body:     "日本語テ",
			limit:    4,
			wantText: "日本語テ",
		},
		{
			name:          "longer than the limit keeps the first limit runes",
			body:          "héllo wörld",
			limit:         7,
			wantText:      "héllo w",
			wantTruncated: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := classification.NewCommentContent(test.body, test.limit)
			assert.Equal(t, test.wantText, content.Text)
			assert.Equal(t, test.wantTruncated, content.TextTruncated)
		})
	}
}

// TestNewCommunityContent pins that a community's classifier text is its
// non-empty name, display name and description joined by blank lines, cut
// to the char limit in runes (U1).
func TestNewCommunityContent(t *testing.T) {
	for _, test := range []struct {
		name          string
		fields        classification.CommunityFields
		limit         int
		wantText      string
		wantTruncated bool
	}{
		{
			name: "all fields joined in order",
			fields: classification.CommunityFields{
				Name:        "tidepools",
				DisplayName: "Tide Pools",
				Description: "Rock pools and the creatures in them.",
			},
			limit:    8000,
			wantText: "tidepools\n\nTide Pools\n\nRock pools and the creatures in them.",
		},
		{
			name: "empty display name is skipped",
			fields: classification.CommunityFields{
				Name:        "tidepools",
				Description: "Rock pools and the creatures in them.",
			},
			limit:    8000,
			wantText: "tidepools\n\nRock pools and the creatures in them.",
		},
		{
			name:     "name only",
			fields:   classification.CommunityFields{Name: "tidepools"},
			limit:    8000,
			wantText: "tidepools",
		},
		{
			name: "truncation applies to the joined text",
			fields: classification.CommunityFields{
				Name:        "kelp",
				DisplayName: "Kélp",
			},
			limit:         8,
			wantText:      "kelp\n\nKé",
			wantTruncated: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := classification.NewCommunityContent(test.fields, test.limit)
			assert.Equal(t, test.wantText, content.Text)
			assert.Equal(t, test.wantTruncated, content.TextTruncated)
		})
	}
}
