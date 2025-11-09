package unfurl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenGraph_ValidTags(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head>
    <meta property="og:title" content="Test Article Title" />
    <meta property="og:description" content="This is a test description" />
    <meta property="og:image" content="https://example.com/image.jpg" />
    <meta property="og:url" content="https://example.com/canonical" />
</head>
<body>
    <p>Some content</p>
</body>
</html>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	assert.Equal(t, "Test Article Title", og.Title)
	assert.Equal(t, "This is a test description", og.Description)
	assert.Equal(t, "https://example.com/image.jpg", og.Image)
	assert.Equal(t, "https://example.com/canonical", og.URL)
}

func TestParseOpenGraph_MissingImage(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head>
    <meta property="og:title" content="Article Without Image" />
    <meta property="og:description" content="No image tag" />
</head>
<body></body>
</html>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	assert.Equal(t, "Article Without Image", og.Title)
	assert.Equal(t, "No image tag", og.Description)
	assert.Empty(t, og.Image, "Image should be empty when not provided")
}

func TestParseOpenGraph_FallbackToTitle(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head>
    <title>Page Title Fallback</title>
    <meta name="description" content="Meta description fallback" />
</head>
<body></body>
</html>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	assert.Equal(t, "Page Title Fallback", og.Title, "Should fall back to <title>")
	assert.Equal(t, "Meta description fallback", og.Description, "Should fall back to meta description")
}

func TestParseOpenGraph_PreferOpenGraphOverFallback(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head>
    <title>Page Title</title>
    <meta name="description" content="Meta description" />
    <meta property="og:title" content="OpenGraph Title" />
    <meta property="og:description" content="OpenGraph Description" />
</head>
<body></body>
</html>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	assert.Equal(t, "OpenGraph Title", og.Title, "Should prefer og:title")
	assert.Equal(t, "OpenGraph Description", og.Description, "Should prefer og:description")
}

func TestParseOpenGraph_MalformedHTML(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head>
    <meta property="og:title" content="Still Works" />
    <meta property="og:description" content="Even with broken tags
</head>
<body>
    <p>Unclosed paragraph
</body>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	// Best-effort parsing should still extract what it can
	assert.NotEmpty(t, og.Title, "Should extract title despite malformed HTML")
}

func TestParseOpenGraph_Empty(t *testing.T) {
	html := `
<!DOCTYPE html>
<html>
<head></head>
<body></body>
</html>
`

	og, err := parseOpenGraph(html)
	require.NoError(t, err)

	assert.Empty(t, og.Title)
	assert.Empty(t, og.Description)
	assert.Empty(t, og.Image)
}

func TestFetchOpenGraph_Success(t *testing.T) {
	// Create test server with OpenGraph metadata
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("User-Agent"), "CovesBot")

		html := `
<!DOCTYPE html>
<html>
<head>
    <meta property="og:title" content="Test News Article" />
    <meta property="og:description" content="Breaking news story" />
    <meta property="og:image" content="https://example.com/news.jpg" />
    <meta property="og:url" content="https://example.com/article/123" />
</head>
<body><p>Article content</p></body>
</html>
`
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	ctx := context.Background()
	result, err := fetchOpenGraph(ctx, server.URL, 10*time.Second, "CovesBot/1.0")
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "Test News Article", result.Title)
	assert.Equal(t, "Breaking news story", result.Description)
	assert.Equal(t, "https://example.com/news.jpg", result.ThumbnailURL)
	assert.Equal(t, "article", result.Type)
	assert.Equal(t, "opengraph", result.Provider)
}

func TestFetchOpenGraph_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ctx := context.Background()
	result, err := fetchOpenGraph(ctx, server.URL, 10*time.Second, "CovesBot/1.0")
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "404")
}

func TestFetchOpenGraph_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()
	result, err := fetchOpenGraph(ctx, server.URL, 100*time.Millisecond, "CovesBot/1.0")
	require.Error(t, err)
	assert.Nil(t, result)
}

func TestFetchOpenGraph_NoMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html := `<html><head></head><body><p>No metadata</p></body></html>`
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	ctx := context.Background()
	result, err := fetchOpenGraph(ctx, server.URL, 10*time.Second, "CovesBot/1.0")
	require.NoError(t, err)
	require.NotNil(t, result)

	// Should still return a result with domain
	assert.Equal(t, "article", result.Type)
	assert.Equal(t, "opengraph", result.Provider)
	assert.NotEmpty(t, result.Domain)
}

func TestIsOEmbedProvider(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"https://streamable.com/abc123", true},
		{"https://www.youtube.com/watch?v=test", true},
		{"https://youtu.be/test", true},
		{"https://reddit.com/r/test/comments/123", true},
		{"https://www.reddit.com/r/test/comments/123", true},
		{"https://example.com/article", false},
		{"https://news.ycombinator.com/item?id=123", false},
		{"https://kite.kagi.com/search?q=test", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			result := isOEmbedProvider(tt.url)
			assert.Equal(t, tt.expected, result, "URL: %s", tt.url)
		})
	}
}

func TestIsSupported(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"https://example.com", true},
		{"http://example.com", true},
		{"https://news.site.com/article", true},
		{"ftp://example.com", false},
		{"not-a-url", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			result := isSupported(tt.url)
			assert.Equal(t, tt.expected, result, "URL: %s", tt.url)
		})
	}
}

func TestGetAttr(t *testing.T) {
	html := `<meta property="og:title" content="Test Title" name="test" />`
	doc, err := parseOpenGraph(html)
	require.NoError(t, err)

	// This is a simple test to verify the helper function works
	// The actual usage is tested in the parseOpenGraph tests
	assert.NotNil(t, doc)
}
