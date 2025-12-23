package blueskypost

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMapAPIPostToResult_BasicPost(t *testing.T) {
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:alice123/app.bsky.feed.post/abc123",
		CID: "cid123",
		Author: blueskyAPIAuthor{
			DID:         "did:plc:alice123",
			Handle:      "alice.bsky.social",
			DisplayName: "Alice",
			Avatar:      "https://example.com/avatar.jpg",
		},
		Record: blueskyAPIRecord{
			Text:      "Hello world!",
			CreatedAt: "2025-12-21T10:30:00Z",
		},
		ReplyCount:  5,
		RepostCount: 10,
		LikeCount:   20,
	}

	result := mapAPIPostToResult(apiPost)

	// Verify basic fields
	if result.URI != apiPost.URI {
		t.Errorf("Expected URI %q, got %q", apiPost.URI, result.URI)
	}
	if result.CID != apiPost.CID {
		t.Errorf("Expected CID %q, got %q", apiPost.CID, result.CID)
	}
	if result.Text != "Hello world!" {
		t.Errorf("Expected text %q, got %q", "Hello world!", result.Text)
	}
	if result.ReplyCount != 5 {
		t.Errorf("Expected reply count 5, got %d", result.ReplyCount)
	}
	if result.RepostCount != 10 {
		t.Errorf("Expected repost count 10, got %d", result.RepostCount)
	}
	if result.LikeCount != 20 {
		t.Errorf("Expected like count 20, got %d", result.LikeCount)
	}

	// Verify author
	if result.Author == nil {
		t.Fatal("Author should not be nil")
	}
	if result.Author.DID != "did:plc:alice123" {
		t.Errorf("Expected DID %q, got %q", "did:plc:alice123", result.Author.DID)
	}
	if result.Author.Handle != "alice.bsky.social" {
		t.Errorf("Expected handle %q, got %q", "alice.bsky.social", result.Author.Handle)
	}
	if result.Author.DisplayName != "Alice" {
		t.Errorf("Expected display name %q, got %q", "Alice", result.Author.DisplayName)
	}
	if result.Author.Avatar != "https://example.com/avatar.jpg" {
		t.Errorf("Expected avatar %q, got %q", "https://example.com/avatar.jpg", result.Author.Avatar)
	}
}

func TestMapAPIPostToResult_TimestampParsing(t *testing.T) {
	tests := []struct {
		name        string
		createdAt   string
		expectError bool
	}{
		{
			name:        "valid RFC3339 timestamp",
			createdAt:   "2025-12-21T10:30:00Z",
			expectError: false,
		},
		{
			name:        "valid RFC3339 with timezone",
			createdAt:   "2025-12-21T10:30:00-05:00",
			expectError: false,
		},
		{
			name:        "valid RFC3339 with milliseconds",
			createdAt:   "2025-12-21T10:30:00.123Z",
			expectError: false,
		},
		{
			name:        "invalid timestamp",
			createdAt:   "not-a-timestamp",
			expectError: true,
		},
		{
			name:        "empty timestamp",
			createdAt:   "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiPost := &blueskyAPIPost{
				URI: "at://did:plc:test/app.bsky.feed.post/test",
				CID: "cid",
				Author: blueskyAPIAuthor{
					DID:    "did:plc:test",
					Handle: "test.bsky.social",
				},
				Record: blueskyAPIRecord{
					Text:      "Test",
					CreatedAt: tt.createdAt,
				},
			}

			result := mapAPIPostToResult(apiPost)

			if tt.expectError {
				if !result.CreatedAt.IsZero() {
					t.Errorf("Expected zero time for invalid timestamp, got %v", result.CreatedAt)
				}
			} else {
				if result.CreatedAt.IsZero() {
					t.Error("Expected valid timestamp, got zero time")
				}

				// Verify the timestamp is reasonable
				now := time.Now()
				if result.CreatedAt.After(now.Add(24 * time.Hour)) {
					t.Error("Timestamp is unreasonably far in the future")
				}
			}
		})
	}
}

func TestMapAPIPostToResult_MediaInRecordEmbed(t *testing.T) {
	tests := []struct {
		recordEmbed   *recordEmbed
		name          string
		expectedCount int
		expectedMedia bool
	}{
		{
			name:          "no embed",
			recordEmbed:   nil,
			expectedMedia: false,
			expectedCount: 0,
		},
		{
			name: "single image",
			recordEmbed: &recordEmbed{
				Type:   "app.bsky.embed.images",
				Images: []json.RawMessage{json.RawMessage(`{"alt":"test"}`)},
			},
			expectedMedia: true,
			expectedCount: 1,
		},
		{
			name: "multiple images",
			recordEmbed: &recordEmbed{
				Type: "app.bsky.embed.images",
				Images: []json.RawMessage{
					json.RawMessage(`{"alt":"test1"}`),
					json.RawMessage(`{"alt":"test2"}`),
					json.RawMessage(`{"alt":"test3"}`),
				},
			},
			expectedMedia: true,
			expectedCount: 3,
		},
		{
			name: "video",
			recordEmbed: &recordEmbed{
				Type:  "app.bsky.embed.video",
				Video: json.RawMessage(`{"cid":"video123"}`),
			},
			expectedMedia: true,
			expectedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiPost := &blueskyAPIPost{
				URI: "at://did:plc:test/app.bsky.feed.post/test",
				CID: "cid",
				Author: blueskyAPIAuthor{
					DID:    "did:plc:test",
					Handle: "test.bsky.social",
				},
				Record: blueskyAPIRecord{
					Text:      "Test",
					CreatedAt: "2025-12-21T10:30:00Z",
					Embed:     tt.recordEmbed,
				},
			}

			result := mapAPIPostToResult(apiPost)

			if result.HasMedia != tt.expectedMedia {
				t.Errorf("Expected HasMedia %v, got %v", tt.expectedMedia, result.HasMedia)
			}
			if result.MediaCount != tt.expectedCount {
				t.Errorf("Expected MediaCount %d, got %d", tt.expectedCount, result.MediaCount)
			}
		})
	}
}

func TestMapAPIPostToResult_MediaInAPIEmbed(t *testing.T) {
	tests := []struct {
		apiEmbed      *blueskyAPIEmbed
		name          string
		expectedCount int
		expectedMedia bool
	}{
		{
			name:          "no embed",
			apiEmbed:      nil,
			expectedMedia: false,
			expectedCount: 0,
		},
		{
			name: "images in API embed",
			apiEmbed: &blueskyAPIEmbed{
				Type: "app.bsky.embed.images#view",
				Images: []json.RawMessage{
					json.RawMessage(`{"thumb":"url1"}`),
					json.RawMessage(`{"thumb":"url2"}`),
				},
			},
			expectedMedia: true,
			expectedCount: 2,
		},
		{
			name: "video in API embed",
			apiEmbed: &blueskyAPIEmbed{
				Type:  "app.bsky.embed.video#view",
				Video: json.RawMessage(`{"playlist":"url"}`),
			},
			expectedMedia: true,
			expectedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiPost := &blueskyAPIPost{
				URI: "at://did:plc:test/app.bsky.feed.post/test",
				CID: "cid",
				Author: blueskyAPIAuthor{
					DID:    "did:plc:test",
					Handle: "test.bsky.social",
				},
				Record: blueskyAPIRecord{
					Text:      "Test",
					CreatedAt: "2025-12-21T10:30:00Z",
				},
				Embed: tt.apiEmbed,
			}

			result := mapAPIPostToResult(apiPost)

			if result.HasMedia != tt.expectedMedia {
				t.Errorf("Expected HasMedia %v, got %v", tt.expectedMedia, result.HasMedia)
			}
			if result.MediaCount != tt.expectedCount {
				t.Errorf("Expected MediaCount %d, got %d", tt.expectedCount, result.MediaCount)
			}
		})
	}
}

func TestMapAPIPostToResult_QuotedPost(t *testing.T) {
	// Create a quoted post structure
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:alice123/app.bsky.feed.post/abc123",
		CID: "cid123",
		Author: blueskyAPIAuthor{
			DID:    "did:plc:alice123",
			Handle: "alice.bsky.social",
		},
		Record: blueskyAPIRecord{
			Text:      "Check out this post!",
			CreatedAt: "2025-12-21T10:30:00Z",
		},
		Embed: &blueskyAPIEmbed{
			Type: "app.bsky.embed.record#view",
			Record: &blueskyAPIEmbedRecord{
				Record: blueskyAPIPost{
					URI: "at://did:plc:bob456/app.bsky.feed.post/xyz789",
					CID: "cid789",
					Author: blueskyAPIAuthor{
						DID:    "did:plc:bob456",
						Handle: "bob.bsky.social",
					},
					Record: blueskyAPIRecord{
						Text:      "Original post",
						CreatedAt: "2025-12-20T08:00:00Z",
					},
					LikeCount: 100,
				},
			},
		},
	}

	result := mapAPIPostToResult(apiPost)

	// Verify main post
	if result.Text != "Check out this post!" {
		t.Errorf("Expected main post text %q, got %q", "Check out this post!", result.Text)
	}

	// Verify quoted post exists
	if result.QuotedPost == nil {
		t.Fatal("Expected quoted post, got nil")
	}

	// Verify quoted post content
	if result.QuotedPost.Text != "Original post" {
		t.Errorf("Expected quoted post text %q, got %q", "Original post", result.QuotedPost.Text)
	}
	if result.QuotedPost.Author.Handle != "bob.bsky.social" {
		t.Errorf("Expected quoted post handle %q, got %q", "bob.bsky.social", result.QuotedPost.Author.Handle)
	}
	if result.QuotedPost.LikeCount != 100 {
		t.Errorf("Expected quoted post like count 100, got %d", result.QuotedPost.LikeCount)
	}

	// Verify no nested quoted posts (1 level deep only)
	if result.QuotedPost.QuotedPost != nil {
		t.Error("Quoted posts should not be nested more than 1 level deep")
	}
}

func TestMapAPIPostToResult_QuotedPostNonRecordEmbed(t *testing.T) {
	// Test that non-record embeds don't create quoted posts
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:test/app.bsky.feed.post/test",
		CID: "cid",
		Author: blueskyAPIAuthor{
			DID:    "did:plc:test",
			Handle: "test.bsky.social",
		},
		Record: blueskyAPIRecord{
			Text:      "Test",
			CreatedAt: "2025-12-21T10:30:00Z",
		},
		Embed: &blueskyAPIEmbed{
			Type: "app.bsky.embed.images#view",
			Images: []json.RawMessage{
				json.RawMessage(`{"thumb":"url"}`),
			},
		},
	}

	result := mapAPIPostToResult(apiPost)

	if result.QuotedPost != nil {
		t.Error("Non-record embeds should not create quoted posts")
	}
}

func TestMapAPIPostToResult_EmptyOptionalFields(t *testing.T) {
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:test/app.bsky.feed.post/test",
		CID: "cid",
		Author: blueskyAPIAuthor{
			DID:    "did:plc:test",
			Handle: "test.bsky.social",
			// DisplayName and Avatar omitted
		},
		Record: blueskyAPIRecord{
			Text: "Test",
			// CreatedAt omitted
		},
		// Counts default to 0
	}

	result := mapAPIPostToResult(apiPost)

	if result.Author.DisplayName != "" {
		t.Errorf("Expected empty display name, got %q", result.Author.DisplayName)
	}
	if result.Author.Avatar != "" {
		t.Errorf("Expected empty avatar, got %q", result.Author.Avatar)
	}
	if !result.CreatedAt.IsZero() {
		t.Error("Expected zero time for missing CreatedAt")
	}
	if result.ReplyCount != 0 || result.RepostCount != 0 || result.LikeCount != 0 {
		t.Error("Expected zero counts for missing engagement metrics")
	}
}

func TestMapAPIPostToResult_MediaFromBothEmbeds(t *testing.T) {
	// Test that media is detected from either record embed or API embed
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:test/app.bsky.feed.post/test",
		CID: "cid",
		Author: blueskyAPIAuthor{
			DID:    "did:plc:test",
			Handle: "test.bsky.social",
		},
		Record: blueskyAPIRecord{
			Text:      "Test",
			CreatedAt: "2025-12-21T10:30:00Z",
			Embed: &recordEmbed{
				Type:   "app.bsky.embed.images",
				Images: []json.RawMessage{json.RawMessage(`"img1"`), json.RawMessage(`"img2"`)},
			},
		},
		Embed: &blueskyAPIEmbed{
			Type:   "app.bsky.embed.images#view",
			Images: []json.RawMessage{json.RawMessage(`"img1"`), json.RawMessage(`"img2"`)},
		},
	}

	result := mapAPIPostToResult(apiPost)

	if !result.HasMedia {
		t.Error("Expected HasMedia to be true")
	}
	// MediaCount should be set from the first source (record embed)
	if result.MediaCount != 2 {
		t.Errorf("Expected MediaCount 2, got %d", result.MediaCount)
	}
}

func TestMapAPIPostToResult_ComplexPost(t *testing.T) {
	// Test a post with media (images)
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:alice123/app.bsky.feed.post/complex",
		CID: "cid123",
		Author: blueskyAPIAuthor{
			DID:         "did:plc:alice123",
			Handle:      "alice.bsky.social",
			DisplayName: "Alice",
			Avatar:      "https://example.com/alice.jpg",
		},
		Record: blueskyAPIRecord{
			Text:      "Complex post with media",
			CreatedAt: "2025-12-21T10:30:00Z",
			Embed: &recordEmbed{
				Type:   "app.bsky.embed.images",
				Images: []json.RawMessage{json.RawMessage(`"img1"`)},
			},
		},
		Embed: &blueskyAPIEmbed{
			Type:   "app.bsky.embed.images#view",
			Images: []json.RawMessage{json.RawMessage(`"img1"`)},
		},
		ReplyCount:  10,
		RepostCount: 20,
		LikeCount:   30,
	}

	result := mapAPIPostToResult(apiPost)

	// Verify main post
	if result.Text != "Complex post with media" {
		t.Errorf("Unexpected text: %q", result.Text)
	}
	if !result.HasMedia {
		t.Error("Expected HasMedia to be true")
	}
	if result.MediaCount != 1 {
		t.Errorf("Expected MediaCount 1, got %d", result.MediaCount)
	}

	// Verify engagement
	if result.ReplyCount != 10 || result.RepostCount != 20 || result.LikeCount != 30 {
		t.Error("Engagement counts don't match")
	}
}

func TestMapAPIPostToResult_NilSafety(t *testing.T) {
	// Ensure the function handles nil embeds gracefully
	apiPost := &blueskyAPIPost{
		URI: "at://did:plc:test/app.bsky.feed.post/test",
		CID: "cid",
		Author: blueskyAPIAuthor{
			DID:    "did:plc:test",
			Handle: "test.bsky.social",
		},
		Record: blueskyAPIRecord{
			Text:      "Test",
			CreatedAt: "2025-12-21T10:30:00Z",
			Embed:     nil,
		},
		Embed: nil,
	}

	// Should not panic
	result := mapAPIPostToResult(apiPost)

	if result.HasMedia {
		t.Error("Expected HasMedia to be false with nil embeds")
	}
	if result.MediaCount != 0 {
		t.Errorf("Expected MediaCount 0 with nil embeds, got %d", result.MediaCount)
	}
	if result.QuotedPost != nil {
		t.Error("Expected no quoted post with nil embeds")
	}
}
