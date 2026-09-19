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

// blueskyAPI says where the fetcher sends requests. Production always uses the
// public AppView with the SSRF guard on; the field exists so this package's own
// tests can point at an httptest server, which necessarily listens on loopback
// and would otherwise be rejected by that guard. Before this existed, three
// "unit" tests reached public.api.bsky.app for real and passed only while
// Bluesky was up and reachable.
type blueskyAPI struct {
	baseURL string
	// allowPrivateHost disables the SSRF protection that blocks private and
	// loopback addresses. Never set outside tests.
	allowPrivateHost bool
}

func defaultBlueskyAPI() blueskyAPI {
	return blueskyAPI{baseURL: blueskyAPIBaseURL}
}

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

// recordEmbed is retained only for record metadata and media indicators. Raw
// record blobs are never used to construct resolved image URLs.
type recordEmbed struct {
	Video  json.RawMessage    `json:"video,omitempty"`
	Record *recordEmbedRecord `json:"record,omitempty"`
	Media  *recordEmbedMedia  `json:"media,omitempty"`
	Type   string             `json:"$type"`
	Images []json.RawMessage  `json:"images,omitempty"`
}

type recordEmbedMedia struct {
	Type   string            `json:"$type"`
	Images []json.RawMessage `json:"images,omitempty"`
	Video  json.RawMessage   `json:"video,omitempty"`
}

type recordEmbedRecord struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// blueskyAPIEmbed represents each resolved embed view union member. For
// recordWithMedia, Media is the media view and Record is the record#view
// wrapper whose Record field contains the quoted-record union.
type blueskyAPIEmbed struct {
	Video    json.RawMessage        `json:"video,omitempty"`
	Record   *blueskyAPIEmbedRecord `json:"record,omitempty"`
	Media    *blueskyAPIEmbed       `json:"media,omitempty"`
	External *blueskyAPIExternal    `json:"external,omitempty"`
	Type     string                 `json:"$type"`
	Images   []json.RawMessage      `json:"images,omitempty"`
}

type blueskyAPIExternal struct {
	URI         string `json:"uri"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Thumb       string `json:"thumb,omitempty"`
}

// blueskyAPIEmbedRecord represents both a record union member and the
// record#view wrapper used by recordWithMedia.
type blueskyAPIEmbedRecord struct {
	Type string `json:"$type,omitempty"`

	Blocked  bool `json:"blocked,omitempty"`
	NotFound bool `json:"notFound,omitempty"`
	Detached bool `json:"detached,omitempty"`

	Record *blueskyAPIEmbedRecord `json:"record,omitempty"`

	URI         string                 `json:"uri,omitempty"`
	CID         string                 `json:"cid,omitempty"`
	Author      *blueskyAPIAuthor      `json:"author,omitempty"`
	Value       *blueskyAPIRecordValue `json:"value,omitempty"`
	LikeCount   int                    `json:"likeCount,omitempty"`
	ReplyCount  int                    `json:"replyCount,omitempty"`
	RepostCount int                    `json:"repostCount,omitempty"`
	IndexedAt   string                 `json:"indexedAt,omitempty"`
	Embeds      []json.RawMessage      `json:"embeds,omitempty"`
}

type blueskyAPIRecordValue struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type blueskyAPIResolvedImage struct {
	Thumb       string       `json:"thumb"`
	Fullsize    string       `json:"fullsize"`
	Alt         string       `json:"alt"`
	AspectRatio *AspectRatio `json:"aspectRatio,omitempty"`
}

// fetchBlueskyPost fetches a Bluesky post from the public API
func fetchBlueskyPost(ctx context.Context, atURI string, timeout time.Duration, api blueskyAPI) (*BlueskyPostResult, error) {
	// Create SSRF-safe HTTP client
	client := oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(api.allowPrivateHost)...)
	client.Timeout = timeout

	// Construct API URL
	apiURL := fmt.Sprintf("%s/xrpc/app.bsky.feed.getPosts?uris=%s", api.baseURL, url.QueryEscape(atURI))

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
	var apiResponse blueskyAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	// Validate we got a post
	if len(apiResponse.Posts) == 0 {
		return &BlueskyPostResult{
			URI:         atURI,
			Unavailable: true,
			Message:     "This Bluesky post is unavailable",
		}, nil
	}

	// Convert API response to BlueskyPostResult
	return mapAPIPostToResult(&apiResponse.Posts[0]), nil
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
		Author:      mapAuthor(&post.Author),
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

	if post.Embed == nil {
		// Keep the legacy indicator for raw video records when the AppView did
		// not supply a resolved view. Raw blobs are never exposed as media URLs.
		if post.Record.Embed != nil && len(post.Record.Embed.Video) > 0 {
			result.HasMedia = true
			result.MediaCount = 1
		}
		return result
	}

	switch post.Embed.Type {
	case "app.bsky.embed.images#view", "app.bsky.embed.video#view", "app.bsky.embed.external#view":
		applyResolvedMedia(result, post.Embed)
	case "app.bsky.embed.record#view":
		result.QuotedPost = mapViewRecordToResult(post.Embed.Record)
	case "app.bsky.embed.recordWithMedia#view":
		applyResolvedMedia(result, post.Embed.Media)
		if post.Embed.Record != nil {
			result.QuotedPost = mapViewRecordToResult(post.Embed.Record.Record)
		}
	}

	if result.QuotedPost != nil {
		result.QuotedPost.QuotedPost = nil
	}
	return result
}

func mapAuthor(author *blueskyAPIAuthor) *Author {
	if author == nil {
		return nil
	}
	mapped := &Author{
		DID:         author.DID,
		Handle:      author.Handle,
		DisplayName: author.DisplayName,
	}
	if isValidBlueskyCDNURL(author.Avatar) {
		mapped.Avatar = author.Avatar
	}
	return mapped
}

func applyResolvedMedia(result *BlueskyPostResult, embed *blueskyAPIEmbed) {
	if embed == nil {
		return
	}
	switch embed.Type {
	case "app.bsky.embed.images#view":
		result.Images = append(result.Images, mapResolvedImages(embed.Images)...)
		result.MediaCount = len(result.Images)
		result.HasMedia = result.MediaCount > 0
	case "app.bsky.embed.video#view":
		result.HasMedia = true
		result.MediaCount = 1
	case "app.bsky.embed.external#view":
		result.Embed = mapExternalEmbed(embed.External)
	case "app.bsky.embed.recordWithMedia#view":
		applyResolvedMedia(result, embed.Media)
	}
}

func mapResolvedImages(rawImages []json.RawMessage) []BlueskyImage {
	images := make([]BlueskyImage, 0, len(rawImages))
	for _, rawImage := range rawImages {
		var image blueskyAPIResolvedImage
		if err := json.Unmarshal(rawImage, &image); err != nil {
			continue
		}
		if !isValidBlueskyCDNURL(image.Thumb) || !isValidBlueskyCDNURL(image.Fullsize) {
			continue
		}
		mapped := BlueskyImage{
			Thumb:    image.Thumb,
			Fullsize: image.Fullsize,
			Alt:      image.Alt,
		}
		if image.AspectRatio != nil && image.AspectRatio.Width > 0 && image.AspectRatio.Height > 0 {
			aspectRatio := *image.AspectRatio
			mapped.AspectRatio = &aspectRatio
		}
		images = append(images, mapped)
	}
	return images
}

func mapExternalEmbed(external *blueskyAPIExternal) *ExternalEmbed {
	if external == nil || external.URI == "" {
		return nil
	}
	embed := &ExternalEmbed{
		URI:         external.URI,
		Title:       external.Title,
		Description: external.Description,
	}
	if isValidBlueskyCDNURL(external.Thumb) {
		embed.Thumb = external.Thumb
	}
	return embed
}

func mapUnavailableEmbedRecord(embedRecord *blueskyAPIEmbedRecord) *BlueskyPostResult {
	if embedRecord == nil {
		return nil
	}
	if embedRecord.Blocked || embedRecord.Type == "app.bsky.embed.record#viewBlocked" {
		return &BlueskyPostResult{
			URI:         embedRecord.URI,
			Author:      mapAuthor(embedRecord.Author),
			Unavailable: true,
			Message:     "This post is from a blocked account",
		}
	}
	if embedRecord.NotFound || embedRecord.Type == "app.bsky.embed.record#viewNotFound" {
		return &BlueskyPostResult{
			URI:         embedRecord.URI,
			Unavailable: true,
			Message:     "This post has been deleted",
		}
	}
	if embedRecord.Detached || embedRecord.Type == "app.bsky.embed.record#viewDetached" {
		return &BlueskyPostResult{
			URI:         embedRecord.URI,
			Unavailable: true,
			Message:     "This post is unavailable",
		}
	}
	return nil
}

// mapViewRecordToResult maps the quoted-record union. Unknown union members
// are ignored rather than producing a zero-valued quote.
func mapViewRecordToResult(embedRecord *blueskyAPIEmbedRecord) *BlueskyPostResult {
	if embedRecord == nil {
		return nil
	}
	if unavailable := mapUnavailableEmbedRecord(embedRecord); unavailable != nil {
		return unavailable
	}
	if embedRecord.Type != "" && embedRecord.Type != "app.bsky.embed.record#viewRecord" {
		return nil
	}

	result := &BlueskyPostResult{
		URI:         embedRecord.URI,
		CID:         embedRecord.CID,
		ReplyCount:  embedRecord.ReplyCount,
		RepostCount: embedRecord.RepostCount,
		LikeCount:   embedRecord.LikeCount,
		Author:      mapAuthor(embedRecord.Author),
	}
	if embedRecord.Value != nil {
		result.Text = embedRecord.Value.Text
		if embedRecord.Value.CreatedAt != "" {
			createdAt, err := time.Parse(time.RFC3339, embedRecord.Value.CreatedAt)
			if err == nil {
				result.CreatedAt = createdAt
			} else {
				log.Printf("[BLUESKY] Warning: Failed to parse CreatedAt timestamp %q for quoted post %s: %v",
					embedRecord.Value.CreatedAt, embedRecord.URI, err)
			}
		}
	}

	for _, rawEmbed := range embedRecord.Embeds {
		var embed blueskyAPIEmbed
		if err := json.Unmarshal(rawEmbed, &embed); err != nil {
			log.Printf("[BLUESKY] Warning: Failed to unmarshal embed in quoted post: %v", err)
			continue
		}
		applyResolvedMedia(result, &embed)
	}
	return result
}
