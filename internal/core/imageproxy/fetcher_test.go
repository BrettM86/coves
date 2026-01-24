package imageproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPDSFetcher_Fetch_Success(t *testing.T) {
	// Setup test server that returns blob data
	expectedData := []byte("test image data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request path and query parameters
		if r.URL.Path != "/xrpc/com.atproto.sync.getBlob" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("did") != "did:plc:test123" {
			t.Errorf("unexpected did: %s", r.URL.Query().Get("did"))
		}
		if r.URL.Query().Get("cid") != "bafyreicid123" {
			t.Errorf("unexpected cid: %s", r.URL.Query().Get("cid"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write(expectedData)
	}))
	defer server.Close()

	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx := context.Background()

	data, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if string(data) != string(expectedData) {
		t.Errorf("expected data %q, got %q", expectedData, data)
	}
}

func TestPDSFetcher_Fetch_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrPDSNotFound) {
		t.Errorf("expected ErrPDSNotFound, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the timeout
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Use a very short timeout
	fetcher := NewPDSFetcher(50 * time.Millisecond, 10)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrPDSTimeout) {
		t.Errorf("expected ErrPDSTimeout, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_NetworkError(t *testing.T) {
	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx := context.Background()

	// Use an invalid URL that will cause a network error
	_, err := fetcher.Fetch(ctx, "http://localhost:99999", "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrPDSFetchFailed) {
		t.Errorf("expected ErrPDSFetchFailed, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep to allow context cancellation
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context immediately
	cancel()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if err == nil {
		t.Error("expected error due to context cancellation")
	}
	// Context cancellation should return ErrPDSTimeout
	if !errors.Is(err, ErrPDSTimeout) {
		t.Errorf("expected ErrPDSTimeout for context cancellation, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrPDSFetchFailed) {
		t.Errorf("expected ErrPDSFetchFailed, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_URLConstruction(t *testing.T) {
	var capturedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	}))
	defer server.Close()

	fetcher := NewPDSFetcher(5 * time.Second, 10)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:abc123", "bafyreicid456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedPath := "/xrpc/com.atproto.sync.getBlob?cid=bafyreicid456&did=did%3Aplc%3Aabc123"
	if capturedURL != expectedPath {
		t.Errorf("expected URL %q, got %q", expectedPath, capturedURL)
	}
}

func TestPDSFetcher_Fetch_ImageTooLarge_ContentLength(t *testing.T) {
	// Server returns Content-Length header indicating size exceeds limit
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set Content-Length larger than the max (1MB)
		w.Header().Set("Content-Length", "2097152") // 2MB
		w.WriteHeader(http.StatusOK)
		// Don't actually write 2MB of data
	}))
	defer server.Close()

	// Use 1MB max size
	fetcher := NewPDSFetcher(5*time.Second, 1)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("expected ErrImageTooLarge, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_ImageTooLarge_StreamingBody(t *testing.T) {
	// Server doesn't send Content-Length but streams more data than allowed
	largeData := make([]byte, 2*1024*1024) // 2MB of zeros
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(largeData)
	}))
	defer server.Close()

	// Use 1MB max size
	fetcher := NewPDSFetcher(5*time.Second, 1)
	ctx := context.Background()

	_, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("expected ErrImageTooLarge, got: %v", err)
	}
}

func TestPDSFetcher_Fetch_SizeWithinLimit(t *testing.T) {
	// Server returns data within the limit
	testData := make([]byte, 512*1024) // 512KB
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(testData)
	}))
	defer server.Close()

	// Use 1MB max size
	fetcher := NewPDSFetcher(5*time.Second, 1)
	ctx := context.Background()

	data, err := fetcher.Fetch(ctx, server.URL, "did:plc:test123", "bafyreicid123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(data) != len(testData) {
		t.Errorf("expected %d bytes, got %d", len(testData), len(data))
	}
}

func TestPDSFetcher_Fetch_DefaultMaxSize(t *testing.T) {
	// Test that 0 for maxSizeMB uses the default
	fetcher := NewPDSFetcher(5*time.Second, 0)
	expectedDefault := int64(DefaultMaxSourceSizeMB) * 1024 * 1024

	if fetcher.maxSizeBytes != expectedDefault {
		t.Errorf("expected default maxSizeBytes %d, got %d", expectedDefault, fetcher.maxSizeBytes)
	}
}

func TestPDSFetcher_Fetch_NegativeMaxSize(t *testing.T) {
	// Test that negative maxSizeMB uses the default
	fetcher := NewPDSFetcher(5*time.Second, -5)
	expectedDefault := int64(DefaultMaxSourceSizeMB) * 1024 * 1024

	if fetcher.maxSizeBytes != expectedDefault {
		t.Errorf("expected default maxSizeBytes %d, got %d", expectedDefault, fetcher.maxSizeBytes)
	}
}
