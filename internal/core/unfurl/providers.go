package unfurl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// maxUnfurlBodyBytes is how much of a remote response this site is willing to
// hold in memory: 10 MB, the limit both HTML reads below have always enforced
// through an io.LimitReader.
//
// IT IS ALSO WHAT THE TRANSPORT IS TOLD, PLUS ONE, in NewService. The shared
// SSRF-safe client defaults to oauth.DefaultMaxResponseBytes (32 MiB), which is
// larger, so adopting it without passing this value would triple what a pasted
// link can make this process allocate — silently, since every existing fixture
// still passes under the looser bound. Both layers read the same constant so the
// two cannot drift, and the +1 is the room readCappedBody needs to PROBE past
// this limit rather than truncate at it. See readCappedBody for why that byte
// decides between a refusal and a half-page served into a post.
const maxUnfurlBodyBytes = 10 * 1024 * 1024

// readCappedBody reads a remote page and REFUSES one that runs past this site's
// limit, instead of handing back the part that fitted.
//
// # WHY THE LIMIT READER PROBES ONE BYTE PAST THE CAP
//
// io.ReadAll over an io.LimitReader cannot tell "the body ended" from "the limit
// was reached" — both arrive as EOF and a nil error. Reading maxUnfurlBodyBytes
// exactly is therefore a silent truncation: a page one byte too long comes back
// clipped, and because parseOpenGraph and html.Parse are both error-tolerant by
// design, the clipped bytes still yield an og:title and og:description. The
// caller gets an UnfurlResult that reads as a complete page, caches it for 24
// hours and serves it into a post. The extra byte's EXISTENCE is the signal, so
// it has to be asked for.
//
// It follows that the transport must be told maxUnfurlBodyBytes+1 (NewService
// does): a transport cap set to exactly the limit clips the probing byte and
// restores the truncation this function exists to prevent. imageproxy/fetcher.go
// and posts' newGuardedRematerializeBlobClient are the same pairing, and the
// only reason they were not the same defect.
func readCappedBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxUnfurlBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(data) > maxUnfurlBodyBytes {
		return nil, fmt.Errorf("%w: read more than %d bytes", ErrPageTooLarge, maxUnfurlBodyBytes)
	}
	return data, nil
}

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

// isSupported checks if this is a valid HTTP/HTTPS URL we can meaningfully unfurl.
//
// kite.kagi.com is explicitly excluded: it's a client-rendered SPA whose
// server-rendered <title>, og:title, og:description, and og:image are all
// the same default fallback for every URL (the top story, randomly localized
// by path). Unfurling those would attach the same wrong title/description/image
// to every kite story. The kagi-news trusted aggregator already supplies
// authoritative metadata from the Kagi JSON feed, so the unfurl path for
// kite URLs has no value to recover.
func isSupported(urlStr string) bool {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if extractDomain(urlStr) == "kite.kagi.com" {
		return false
	}
	return true
}

// isOEmbedProvider checks if we have an oEmbed endpoint for this URL
func isOEmbedProvider(urlStr string) bool {
	domain := extractDomain(urlStr)
	_, exists := oEmbedEndpoints[domain]
	return exists
}

// fetchOEmbed fetches oEmbed data from the provider
func fetchOEmbed(ctx context.Context, urlStr string, client *http.Client, userAgent string) (*oEmbedResponse, error) {
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

// normalizeURL converts protocol-relative URLs to HTTPS
// Examples:
//
//	"//example.com/image.jpg" -> "https://example.com/image.jpg"
//	"https://example.com/image.jpg" -> "https://example.com/image.jpg" (unchanged)
func normalizeURL(urlStr string) string {
	if strings.HasPrefix(urlStr, "//") {
		return "https:" + urlStr
	}
	return urlStr
}

// mapOEmbedToResult converts oEmbed response to UnfurlResult
func mapOEmbedToResult(oembed *oEmbedResponse, originalURL string) *UnfurlResult {
	result := &UnfurlResult{
		URI:          originalURL,
		Title:        oembed.Title,
		Description:  oembed.Description,
		ThumbnailURL: normalizeURL(oembed.ThumbnailURL),
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
func fetchOpenGraph(ctx context.Context, urlStr string, client *http.Client, userAgent string) (*UnfurlResult, error) {
	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP request returned status %d", resp.StatusCode)
	}

	body, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, err
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
		ThumbnailURL: normalizeURL(og.Image),
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
func fetchKagiKite(ctx context.Context, urlStr string, client *http.Client, userAgent string) (*UnfurlResult, error) {
	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Read before parsing, rather than handing html.Parse the capped reader
	// directly, because the overrun has to be decided on the WHOLE body: a
	// streaming parse would consume the page and only then meet the extra byte,
	// by which point a document already exists to return. Reading first costs
	// nothing — io.ReadAll's buffer is the same 10 MB the parse was going to
	// allocate anyway.
	body, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse HTML
	doc, err := html.Parse(bytes.NewReader(body))
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
