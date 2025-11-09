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

func TestFetchKagiKite_Success(t *testing.T) {
	// Mock Kagi HTML response
	mockHTML := `<!DOCTYPE html>
<html>
<head>
	<title>FAA orders 10% flight cuts at 40 airports - Kagi News</title>
	<meta property="og:title" content="FAA orders 10% flight cuts" />
	<meta property="og:description" content="Flight restrictions announced" />
</head>
<body>
	<img src="https://kagiproxy.com/img/DHdCvN_NqVDWU3UyoNZSv86b" alt="Airport runway" />
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	require.NoError(t, err)
	assert.Equal(t, "article", result.Type)
	assert.Equal(t, "FAA orders 10% flight cuts", result.Title)
	assert.Equal(t, "Flight restrictions announced", result.Description)
	assert.Contains(t, result.ThumbnailURL, "kagiproxy.com")
	assert.Equal(t, "kagi", result.Provider)
	assert.Equal(t, "kite.kagi.com", result.Domain)
}

func TestFetchKagiKite_NoImage(t *testing.T) {
	mockHTML := `<!DOCTYPE html>
<html>
<head><title>Test Story</title></head>
<body><p>No images here</p></body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "no image found")
}

func TestFetchKagiKite_FallbackToTitle(t *testing.T) {
	mockHTML := `<!DOCTYPE html>
<html>
<head><title>Fallback Title</title></head>
<body>
	<img src="https://kagiproxy.com/img/test123" />
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	require.NoError(t, err)
	assert.Equal(t, "Fallback Title", result.Title)
	assert.Contains(t, result.ThumbnailURL, "kagiproxy.com")
}

func TestFetchKagiKite_ImageWithAltText(t *testing.T) {
	mockHTML := `<!DOCTYPE html>
<html>
<head><title>News Story</title></head>
<body>
	<img src="https://kagiproxy.com/img/xyz789" alt="This is the alt text description" />
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	require.NoError(t, err)
	assert.Equal(t, "News Story", result.Title)
	assert.Equal(t, "This is the alt text description", result.Description)
	assert.Contains(t, result.ThumbnailURL, "kagiproxy.com")
}

func TestFetchKagiKite_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "HTTP 404")
}

func TestFetchKagiKite_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 100*time.Millisecond, "TestBot/1.0")

	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestFetchKagiKite_MultipleImages_PicksSecond(t *testing.T) {
	mockHTML := `<!DOCTYPE html>
<html>
<head><title>Story with multiple images</title></head>
<body>
	<img src="https://kagiproxy.com/img/first123" alt="First image (header/logo)" />
	<img src="https://kagiproxy.com/img/second456" alt="Second image" />
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	require.NoError(t, err)
	// We skip the first image (often a header/logo) and use the second
	assert.Contains(t, result.ThumbnailURL, "second456")
	assert.Equal(t, "Second image", result.Description)
}

func TestFetchKagiKite_OnlyNonKagiImages_NoMatch(t *testing.T) {
	mockHTML := `<!DOCTYPE html>
<html>
<head><title>Story with non-Kagi images</title></head>
<body>
	<img src="https://example.com/img/test.jpg" alt="External image" />
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockHTML))
	}))
	defer server.Close()

	ctx := context.Background()

	result, err := fetchKagiKite(ctx, server.URL, 5*time.Second, "TestBot/1.0")

	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "no image found")
}
