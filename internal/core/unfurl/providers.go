package unfurl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Provider configuration
var oEmbedEndpoints = map[string]string{
	"streamable.com": "https://api.streamable.com/oembed",
	"youtube.com":    "https://www.youtube.com/oembed",
	"youtu.be":       "https://www.youtube.com/oembed",
	"reddit.com":     "https://www.reddit.com/oembed",
}

// oEmbedResponse represents a standard oEmbed response
type oEmbedResponse struct {
	ThumbnailURL    string `json:"thumbnail_url"`
	Version         string `json:"version"`
	Title           string `json:"title"`
	AuthorName      string `json:"author_name"`
	ProviderName    string `json:"provider_name"`
	ProviderURL     string `json:"provider_url"`
	Type            string `json:"type"`
	HTML            string `json:"html"`
	Description     string `json:"description"`
	ThumbnailWidth  int    `json:"thumbnail_width"`
	ThumbnailHeight int    `json:"thumbnail_height"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
}

// extractDomain extracts the domain from a URL
func extractDomain(urlStr string) string {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return ""
	}
	// Remove www. prefix
	domain := strings.TrimPrefix(parsed.Host, "www.")
	return domain
}

// isSupported checks if this is a valid HTTP/HTTPS URL
func isSupported(urlStr string) bool {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

// isOEmbedProvider checks if we have an oEmbed endpoint for this URL
func isOEmbedProvider(urlStr string) bool {
	domain := extractDomain(urlStr)
	_, exists := oEmbedEndpoints[domain]
	return exists
}

// fetchOEmbed fetches oEmbed data from the provider
func fetchOEmbed(ctx context.Context, urlStr string, timeout time.Duration, userAgent string) (*oEmbedResponse, error) {
	domain := extractDomain(urlStr)
	endpoint, exists := oEmbedEndpoints[domain]
	if !exists {
		return nil, fmt.Errorf("no oEmbed endpoint for domain: %s", domain)
	}

	// Build oEmbed request URL
	oembedURL := fmt.Sprintf("%s?url=%s&format=json", endpoint, url.QueryEscape(urlStr))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", oembedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create oEmbed request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	// Create HTTP client with timeout
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch oEmbed data: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oEmbed endpoint returned status %d", resp.StatusCode)
	}

	// Parse JSON response
	var oembed oEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		return nil, fmt.Errorf("failed to parse oEmbed response: %w", err)
	}

	return &oembed, nil
}

// mapOEmbedToResult converts oEmbed response to UnfurlResult
func mapOEmbedToResult(oembed *oEmbedResponse, originalURL string) *UnfurlResult {
	result := &UnfurlResult{
		URI:          originalURL,
		Title:        oembed.Title,
		Description:  oembed.Description,
		ThumbnailURL: oembed.ThumbnailURL,
		Provider:     strings.ToLower(oembed.ProviderName),
		Domain:       extractDomain(originalURL),
		Width:        oembed.Width,
		Height:       oembed.Height,
	}

	// Map oEmbed type to our embedType
	switch oembed.Type {
	case "video":
		result.Type = "video"
	case "photo":
		result.Type = "image"
	default:
		result.Type = "article"
	}

	// If no description but we have author name, use that
	if result.Description == "" && oembed.AuthorName != "" {
		result.Description = fmt.Sprintf("By %s", oembed.AuthorName)
	}

	return result
}

// openGraphData represents OpenGraph metadata extracted from HTML
type openGraphData struct {
	Title       string
	Description string
	Image       string
	URL         string
}

// fetchOpenGraph fetches OpenGraph metadata from a URL
func fetchOpenGraph(ctx context.Context, urlStr string, timeout time.Duration, userAgent string) (*UnfurlResult, error) {
	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	// Create HTTP client with timeout
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP request returned status %d", resp.StatusCode)
	}

	// Read response body (limit to 10MB to prevent abuse)
	limitedReader := io.LimitReader(resp.Body, 10*1024*1024)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse OpenGraph metadata
	og, err := parseOpenGraph(string(body))
	if err != nil {
		return nil, fmt.Errorf("failed to parse OpenGraph metadata: %w", err)
	}

	// Build UnfurlResult
	result := &UnfurlResult{
		Type:         "article", // Default type for OpenGraph
		URI:          urlStr,
		Title:        og.Title,
		Description:  og.Description,
		ThumbnailURL: og.Image,
		Provider:     "opengraph",
		Domain:       extractDomain(urlStr),
	}

	// Use og:url if available and valid
	if og.URL != "" {
		result.URI = og.URL
	}

	return result, nil
}

// parseOpenGraph extracts OpenGraph metadata from HTML
func parseOpenGraph(htmlContent string) (*openGraphData, error) {
	og := &openGraphData{}
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		// Try best-effort parsing even with invalid HTML
		return og, nil
	}

	// Extract OpenGraph tags and fallbacks
	var pageTitle string
	var metaDescription string

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				property := getAttr(n, "property")
				name := getAttr(n, "name")
				content := getAttr(n, "content")

				// OpenGraph tags
				if strings.HasPrefix(property, "og:") {
					switch property {
					case "og:title":
						if og.Title == "" {
							og.Title = content
						}
					case "og:description":
						if og.Description == "" {
							og.Description = content
						}
					case "og:image":
						if og.Image == "" {
							og.Image = content
						}
					case "og:url":
						if og.URL == "" {
							og.URL = content
						}
					}
				}

				// Fallback meta tags
				if name == "description" && metaDescription == "" {
					metaDescription = content
				}

			case "title":
				if pageTitle == "" && n.FirstChild != nil {
					pageTitle = n.FirstChild.Data
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}

	traverse(doc)

	// Apply fallbacks
	if og.Title == "" {
		og.Title = pageTitle
	}
	if og.Description == "" {
		og.Description = metaDescription
	}

	return og, nil
}

// getAttr gets an attribute value from an HTML node
func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// fetchKagiKite handles special unfurling for Kagi Kite news pages
// Kagi Kite pages use client-side rendering, so og:image tags aren't available at SSR time
// Instead, we parse the HTML to extract the story image from the page content
func fetchKagiKite(ctx context.Context, urlStr string, timeout time.Duration, userAgent string) (*UnfurlResult, error) {
	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	// Create HTTP client with timeout
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Limit response size to 10MB
	limitedReader := io.LimitReader(resp.Body, 10*1024*1024)

	// Parse HTML
	doc, err := html.Parse(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}

	result := &UnfurlResult{
		Type:     "article",
		URI:      urlStr,
		Domain:   "kite.kagi.com",
		Provider: "kagi",
	}

	// First try OpenGraph tags (in case they get added in the future)
	var findOG func(*html.Node)
	findOG = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			var property, content string
			for _, attr := range n.Attr {
				if attr.Key == "property" {
					property = attr.Val
				} else if attr.Key == "content" {
					content = attr.Val
				}
			}

			switch property {
			case "og:title":
				if result.Title == "" {
					result.Title = content
				}
			case "og:description":
				if result.Description == "" {
					result.Description = content
				}
			case "og:image":
				if result.ThumbnailURL == "" {
					result.ThumbnailURL = content
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findOG(c)
		}
	}
	findOG(doc)

	// Fallback: Extract from page content
	// Look for images with kagiproxy.com URLs (Kagi's image proxy)
	// Note: Skip the first image as it's often a shared header/logo
	if result.ThumbnailURL == "" {
		var images []struct {
			url string
			alt string
		}

		var findImg func(*html.Node)
		findImg = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "img" {
				for _, attr := range n.Attr {
					if attr.Key == "src" && strings.Contains(attr.Val, "kagiproxy.com") {
						// Get alt text if available
						var altText string
						for _, a := range n.Attr {
							if a.Key == "alt" {
								altText = a.Val
								break
							}
						}
						images = append(images, struct {
							url string
							alt string
						}{url: attr.Val, alt: altText})
						break
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				findImg(c)
			}
		}
		findImg(doc)

		// Skip first image (often shared header/logo), use second if available
		if len(images) > 1 {
			result.ThumbnailURL = images[1].url
			if result.Description == "" && images[1].alt != "" {
				result.Description = images[1].alt
			}
		} else if len(images) == 1 {
			// Only one image found, use it
			result.ThumbnailURL = images[0].url
			if result.Description == "" && images[0].alt != "" {
				result.Description = images[0].alt
			}
		}
	}

	// Fallback to <title> tag if og:title not found
	if result.Title == "" {
		var findTitle func(*html.Node) string
		findTitle = func(n *html.Node) string {
			if n.Type == html.ElementNode && n.Data == "title" {
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					return n.FirstChild.Data
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if title := findTitle(c); title != "" {
					return title
				}
			}
			return ""
		}
		result.Title = findTitle(doc)
	}

	// If still no image, return error
	if result.ThumbnailURL == "" {
		return nil, fmt.Errorf("no image found in Kagi page")
	}

	return result, nil
}
