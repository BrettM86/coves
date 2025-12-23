package blueskypost

import (
	"Coves/internal/atproto/oauth"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// blueskyAPIBaseURL is the public Bluesky API endpoint
const blueskyAPIBaseURL = "https://public.api.bsky.app"

// blueskyAPIResponse represents the response from app.bsky.feed.getPosts
type blueskyAPIResponse struct {
	Posts []blueskyAPIPost `json:"posts"`
}

// blueskyAPIPost represents a post in the Bluesky API response
type blueskyAPIPost struct {
	Author      blueskyAPIAuthor `json:"author"`
	Record      blueskyAPIRecord `json:"record"`
	Embed       *blueskyAPIEmbed `json:"embed,omitempty"`
	URI         string           `json:"uri"`
	CID         string           `json:"cid"`
	IndexedAt   string           `json:"indexedAt"`
	ReplyCount  int              `json:"replyCount"`
	RepostCount int              `json:"repostCount"`
	LikeCount   int              `json:"likeCount"`
}

// blueskyAPIAuthor represents the author in the Bluesky API response
type blueskyAPIAuthor struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
}

// blueskyAPIRecord represents the post record in the Bluesky API response
type blueskyAPIRecord struct {
	Embed     *recordEmbed `json:"embed,omitempty"`
	Text      string       `json:"text"`
	CreatedAt string       `json:"createdAt"`
}

// recordEmbed represents embedded content in the post record
type recordEmbed struct {
	Video  json.RawMessage    `json:"video,omitempty"`
	Record *recordEmbedRecord `json:"record,omitempty"`
	Type   string             `json:"$type"`
	Images []json.RawMessage  `json:"images,omitempty"`
}

// recordEmbedRecord represents a quoted post in the embed
type recordEmbedRecord struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// blueskyAPIEmbed represents resolved embed data in the API response
type blueskyAPIEmbed struct {
	Video  json.RawMessage        `json:"video,omitempty"`
	Record *blueskyAPIEmbedRecord `json:"record,omitempty"`
	Media  *blueskyAPIEmbedMedia  `json:"media,omitempty"`
	Type   string                 `json:"$type"`
	Images []json.RawMessage      `json:"images,omitempty"`
}

// blueskyAPIEmbedMedia represents media in a recordWithMedia embed
type blueskyAPIEmbedMedia struct {
	Type   string            `json:"$type"`
	Images []json.RawMessage `json:"images,omitempty"`
	Video  json.RawMessage   `json:"video,omitempty"`
}

// blueskyAPIEmbedRecord represents a quoted post embed in the API response
// For record#view: this directly contains the viewRecord fields
// For recordWithMedia#view: this contains a nested "record" field with viewRecord
type blueskyAPIEmbedRecord struct {
	// For recordWithMedia#view - nested structure
	Record *blueskyAPIViewRecord `json:"record,omitempty"`

	// For record#view - direct viewRecord fields
	URI       string                   `json:"uri,omitempty"`
	CID       string                   `json:"cid,omitempty"`
	Author    *blueskyAPIAuthor        `json:"author,omitempty"`
	Value     *blueskyAPIRecordValue   `json:"value,omitempty"`
	LikeCount int                      `json:"likeCount,omitempty"`
	ReplyCount int                     `json:"replyCount,omitempty"`
	RepostCount int                    `json:"repostCount,omitempty"`
	IndexedAt string                   `json:"indexedAt,omitempty"`
	Embeds    []json.RawMessage        `json:"embeds,omitempty"`
}

// blueskyAPIViewRecord represents the viewRecord structure for quoted posts
type blueskyAPIViewRecord struct {
	URI         string                 `json:"uri"`
	CID         string                 `json:"cid"`
	Author      blueskyAPIAuthor       `json:"author"`
	Value       *blueskyAPIRecordValue `json:"value,omitempty"`
	LikeCount   int                    `json:"likeCount"`
	ReplyCount  int                    `json:"replyCount"`
	RepostCount int                    `json:"repostCount"`
	IndexedAt   string                 `json:"indexedAt"`
	Embeds      []json.RawMessage      `json:"embeds,omitempty"`
}

// blueskyAPIRecordValue represents the actual post content in a viewRecord
type blueskyAPIRecordValue struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

// fetchBlueskyPost fetches a Bluesky post from the public API
func fetchBlueskyPost(ctx context.Context, atURI string, timeout time.Duration) (*BlueskyPostResult, error) {
	// Create SSRF-safe HTTP client
	client := oauth.NewSSRFSafeHTTPClient(false) // Don't allow private IPs
	client.Timeout = timeout

	// Construct API URL
	apiURL := fmt.Sprintf("%s/xrpc/app.bsky.feed.getPosts?uris=%s", blueskyAPIBaseURL, url.QueryEscape(atURI))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set User-Agent header
	req.Header.Set("User-Agent", "CovesBot/1.0 (+https://coves.social)")

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle 404 - post is deleted or doesn't exist
	if resp.StatusCode == http.StatusNotFound {
		return &BlueskyPostResult{
			URI:         atURI,
			Unavailable: true,
			Message:     "This Bluesky post is unavailable",
		}, nil
	}

	// Handle other non-200 responses
	if resp.StatusCode != http.StatusOK {
		// Limit error body to 1KB to prevent unbounded reads
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var apiResp blueskyAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Validate we got a post
	if len(apiResp.Posts) == 0 {
		return &BlueskyPostResult{
			URI:         atURI,
			Unavailable: true,
			Message:     "This Bluesky post is unavailable",
		}, nil
	}

	// Convert API response to BlueskyPostResult
	post := apiResp.Posts[0]
	result := mapAPIPostToResult(&post)

	return result, nil
}

// mapAPIPostToResult converts a Bluesky API post to BlueskyPostResult
func mapAPIPostToResult(post *blueskyAPIPost) *BlueskyPostResult {
	result := &BlueskyPostResult{
		URI:         post.URI,
		CID:         post.CID,
		Text:        post.Record.Text,
		ReplyCount:  post.ReplyCount,
		RepostCount: post.RepostCount,
		LikeCount:   post.LikeCount,
		Author: &Author{
			DID:         post.Author.DID,
			Handle:      post.Author.Handle,
			DisplayName: post.Author.DisplayName,
			Avatar:      post.Author.Avatar,
		},
	}

	// Parse CreatedAt timestamp
	if post.Record.CreatedAt != "" {
		createdAt, err := time.Parse(time.RFC3339, post.Record.CreatedAt)
		if err == nil {
			result.CreatedAt = createdAt
		} else {
			log.Printf("[BLUESKY] Warning: Failed to parse CreatedAt timestamp %q for post %s: %v", post.Record.CreatedAt, post.URI, err)
		}
	}

	// Check for media in the record embed (Phase 1: indicator only)
	if post.Record.Embed != nil {
		if len(post.Record.Embed.Images) > 0 {
			result.HasMedia = true
			result.MediaCount = len(post.Record.Embed.Images)
		}
		if len(post.Record.Embed.Video) > 0 {
			result.HasMedia = true
			result.MediaCount = 1
		}
	}

	// Check for media in the resolved embed (may have additional info)
	if post.Embed != nil {
		if len(post.Embed.Images) > 0 {
			result.HasMedia = true
			if result.MediaCount == 0 {
				result.MediaCount = len(post.Embed.Images)
			}
		}
		if len(post.Embed.Video) > 0 {
			result.HasMedia = true
			if result.MediaCount == 0 {
				result.MediaCount = 1
			}
		}

		// Handle quoted post (1 level deep only)
		// Support both pure record embeds and recordWithMedia embeds
		if post.Embed.Record != nil {
			var quotedPost *BlueskyPostResult

			switch post.Embed.Type {
			case "app.bsky.embed.record#view":
				// For record#view: viewRecord fields are directly on embed.record
				quotedPost = mapViewRecordToResult(post.Embed.Record)

			case "app.bsky.embed.recordWithMedia#view":
				// For recordWithMedia#view: viewRecord is nested in embed.record.record
				if post.Embed.Record.Record != nil {
					quotedPost = mapNestedViewRecordToResult(post.Embed.Record.Record)
				}

				// Also check for media in the recordWithMedia embed
				if post.Embed.Media != nil {
					if len(post.Embed.Media.Images) > 0 {
						result.HasMedia = true
						if result.MediaCount == 0 {
							result.MediaCount = len(post.Embed.Media.Images)
						}
					}
					if len(post.Embed.Media.Video) > 0 {
						result.HasMedia = true
						if result.MediaCount == 0 {
							result.MediaCount = 1
						}
					}
				}
			}

			if quotedPost != nil {
				// Don't recurse deeper than 1 level
				quotedPost.QuotedPost = nil
				result.QuotedPost = quotedPost
			}
		}
	}

	return result
}

// mapViewRecordToResult maps a blueskyAPIEmbedRecord (with direct viewRecord fields) to BlueskyPostResult
// This is used for app.bsky.embed.record#view where the viewRecord fields are at the top level
func mapViewRecordToResult(embedRecord *blueskyAPIEmbedRecord) *BlueskyPostResult {
	if embedRecord == nil {
		return nil
	}

	result := &BlueskyPostResult{
		URI:         embedRecord.URI,
		CID:         embedRecord.CID,
		ReplyCount:  embedRecord.ReplyCount,
		RepostCount: embedRecord.RepostCount,
		LikeCount:   embedRecord.LikeCount,
	}

	// Map author if present
	if embedRecord.Author != nil {
		result.Author = &Author{
			DID:         embedRecord.Author.DID,
			Handle:      embedRecord.Author.Handle,
			DisplayName: embedRecord.Author.DisplayName,
			Avatar:      embedRecord.Author.Avatar,
		}
	}

	// Map value (actual post content) if present
	if embedRecord.Value != nil {
		result.Text = embedRecord.Value.Text
		if embedRecord.Value.CreatedAt != "" {
			createdAt, err := time.Parse(time.RFC3339, embedRecord.Value.CreatedAt)
			if err == nil {
				result.CreatedAt = createdAt
			}
		}
	}

	// Check for media in embeds array
	if len(embedRecord.Embeds) > 0 {
		result.HasMedia = true
		result.MediaCount = len(embedRecord.Embeds)
	}

	return result
}

// mapNestedViewRecordToResult maps a blueskyAPIViewRecord to BlueskyPostResult
// This is used for app.bsky.embed.recordWithMedia#view where the viewRecord is nested
func mapNestedViewRecordToResult(viewRecord *blueskyAPIViewRecord) *BlueskyPostResult {
	if viewRecord == nil {
		return nil
	}

	result := &BlueskyPostResult{
		URI:         viewRecord.URI,
		CID:         viewRecord.CID,
		ReplyCount:  viewRecord.ReplyCount,
		RepostCount: viewRecord.RepostCount,
		LikeCount:   viewRecord.LikeCount,
		Author: &Author{
			DID:         viewRecord.Author.DID,
			Handle:      viewRecord.Author.Handle,
			DisplayName: viewRecord.Author.DisplayName,
			Avatar:      viewRecord.Author.Avatar,
		},
	}

	// Map value (actual post content) if present
	if viewRecord.Value != nil {
		result.Text = viewRecord.Value.Text
		if viewRecord.Value.CreatedAt != "" {
			createdAt, err := time.Parse(time.RFC3339, viewRecord.Value.CreatedAt)
			if err == nil {
				result.CreatedAt = createdAt
			}
		}
	}

	// Check for media in embeds array
	if len(viewRecord.Embeds) > 0 {
		result.HasMedia = true
		result.MediaCount = len(viewRecord.Embeds)
	}

	return result
}
