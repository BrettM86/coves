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
	Type   string                 `json:"$type"`
	Images []json.RawMessage      `json:"images,omitempty"`
}

// blueskyAPIEmbedRecord represents a quoted post embed in the API response
type blueskyAPIEmbedRecord struct {
	Record blueskyAPIPost `json:"record"`
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
		if post.Embed.Record != nil &&
			(post.Embed.Type == "app.bsky.embed.record#view" ||
				post.Embed.Type == "app.bsky.embed.recordWithMedia#view") {
			quotedPost := mapAPIPostToResult(&post.Embed.Record.Record)
			// Don't recurse deeper than 1 level
			quotedPost.QuotedPost = nil
			result.QuotedPost = quotedPost
		}
	}

	return result
}
