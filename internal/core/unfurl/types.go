package unfurl

import "time"

// UnfurlResult represents the result of unfurling a URL
type UnfurlResult struct {
	Type         string `json:"type"`         // "video", "article", "image", "website"
	URI          string `json:"uri"`          // Original URL
	Title        string `json:"title"`        // Page/video title
	Description  string `json:"description"`  // Page/video description
	ThumbnailURL string `json:"thumbnailUrl"` // Preview image URL
	Provider     string `json:"provider"`     // "streamable", "youtube", "reddit"
	Domain       string `json:"domain"`       // Domain of the URL
	Width        int    `json:"width"`        // Media width (if applicable)
	Height       int    `json:"height"`       // Media height (if applicable)
}

// CacheEntry represents a cached unfurl result with metadata
type CacheEntry struct {
	FetchedAt    time.Time    `db:"fetched_at"`
	ExpiresAt    time.Time    `db:"expires_at"`
	CreatedAt    time.Time    `db:"created_at"`
	ThumbnailURL *string      `db:"thumbnail_url"`
	URL          string       `db:"url"`
	Provider     string       `db:"provider"`
	Metadata     UnfurlResult `db:"metadata"`
}
