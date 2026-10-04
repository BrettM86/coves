package imageproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cdnTestToken = "cf-purge-token-SENTINEL-7f3a"
	cdnTestBody  = "BODY-SENTINEL-cf-purge-token-SENTINEL-7f3a"
)

var cdnTestBlob = BlockedBlob{OwnerDID: moderationTestOwner, CID: moderationTestCID}

var cdnTestPresets = []string{"avatar", "avatar_small", "banner", "content_preview", "content_full", "embed_thumbnail"}

type cdnRequest struct {
	method, path, authorization string
	files                       []string
	decodeError                 error
}

type cdnEndpoint struct {
	mu       sync.Mutex
	requests []cdnRequest
	respond  func([]string) (int, string)
	wait     func(*http.Request)
}

func (endpoint *cdnEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files []string `json:"files"`
	}
	decodeError := json.NewDecoder(r.Body).Decode(&body)
	request := cdnRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), body.Files, decodeError}
	endpoint.mu.Lock()
	endpoint.requests = append(endpoint.requests, request)
	endpoint.mu.Unlock()
	if endpoint.wait != nil {
		endpoint.wait(r)
	}
	status, response := http.StatusOK, `{"success":true}`
	if endpoint.respond != nil {
		status, response = endpoint.respond(body.Files)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(response))
}

func (endpoint *cdnEndpoint) snapshot() []cdnRequest {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	return append([]cdnRequest(nil), endpoint.requests...)
}

func newCDNTestPurger(t *testing.T, server *httptest.Server, bases []string, timeout time.Duration) *CloudflarePurger {
	t.Helper()
	purger, err := NewCloudflarePurger(CloudflarePurgerConfig{APIBase: server.URL, ZoneID: "zone-abc", APIToken: cdnTestToken, BaseURLs: bases, Timeout: timeout})
	require.NoError(t, err)
	require.NotNil(t, purger)
	return purger
}

func cdnExpectedURLs(blob BlockedBlob, bases []string) []string {
	var urls []string
	for _, base := range bases {
		for _, preset := range cdnTestPresets {
			urls = append(urls, blobs.HydrateImageURL(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: base}, "", blob.OwnerDID, blob.CID, preset))
		}
	}
	return urls
}

func TestCloudflarePurgerOneBlobURLsAndRequest(t *testing.T) {
	endpoint := &cdnEndpoint{}
	server := httptest.NewServer(endpoint)
	defer server.Close()
	result := newCDNTestPurger(t, server, []string{"https://img.example.test"}, 5*time.Second).PurgeBlobs(t.Context(), []BlockedBlob{cdnTestBlob})
	requests := endpoint.snapshot()
	require.Len(t, requests, 1)
	assert.NoError(t, requests[0].decodeError)
	assert.Equal(t, "POST", requests[0].method)
	assert.Equal(t, "/zones/zone-abc/purge_cache", requests[0].path)
	assert.Equal(t, "Bearer "+cdnTestToken, requests[0].authorization)
	assert.ElementsMatch(t, []string{
		"https://img.example.test/img/avatar/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		"https://img.example.test/img/avatar_small/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		"https://img.example.test/img/banner/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		"https://img.example.test/img/content_preview/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		"https://img.example.test/img/content_full/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
		"https://img.example.test/img/embed_thumbnail/plain/did:plc:moderationimageowner/bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi",
	}, requests[0].files)
	assert.ElementsMatch(t, cdnExpectedURLs(cdnTestBlob, []string{"https://img.example.test"}), requests[0].files)
	assert.Equal(t, []BlockedBlob{cdnTestBlob}, result.Acknowledged)
	assert.Empty(t, result.Failed)
}

func TestCloudflarePurgerTwoBasesOneRequest(t *testing.T) {
	endpoint := &cdnEndpoint{}
	server := httptest.NewServer(endpoint)
	defer server.Close()
	result := newCDNTestPurger(t, server, []string{"https://img.example.test", "https://cdn.example.test"}, 5*time.Second).PurgeBlobs(t.Context(), []BlockedBlob{cdnTestBlob})
	requests := endpoint.snapshot()
	require.Len(t, requests, 1)
	assert.NoError(t, requests[0].decodeError)
	assert.Len(t, requests[0].files, 12)
	assert.ElementsMatch(t, cdnExpectedURLs(cdnTestBlob, []string{"https://img.example.test", "https://cdn.example.test"}), requests[0].files)
	for _, preset := range cdnTestPresets {
		assert.Contains(t, requests[0].files, blobs.HydrateImageURL(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test", CDNURL: "https://cdn.example.test"}, "", cdnTestBlob.OwnerDID, cdnTestBlob.CID, preset))
	}
	assert.Equal(t, []BlockedBlob{cdnTestBlob}, result.Acknowledged)
	assert.Empty(t, result.Failed)
}

func cdnTwentyBlobs() []BlockedBlob {
	var pairs []BlockedBlob
	for index := range 20 {
		pairs = append(pairs, BlockedBlob{OwnerDID: fmt.Sprintf("did:plc:cdnowner%02d", index), CID: moderationTestCID})
	}
	return pairs
}

func TestCloudflarePurgerBatchesWholePairsAndEmptyInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		bases []string
	}{
		{"one base", []string{"https://img.example.test"}},
		{"two bases", []string{"https://img.example.test", "https://cdn.example.test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := &cdnEndpoint{}
			server := httptest.NewServer(endpoint)
			defer server.Close()
			purger := newCDNTestPurger(t, server, test.bases, 5*time.Second)
			empty := purger.PurgeBlobs(t.Context(), nil)
			assert.Empty(t, endpoint.snapshot())
			assert.Empty(t, empty.Acknowledged)
			assert.Empty(t, empty.Failed)
			pairs := cdnTwentyBlobs()
			result := purger.PurgeBlobs(t.Context(), pairs)
			requests := endpoint.snapshot()
			require.Greater(t, len(requests), 1)
			var allFiles []string
			for _, request := range requests {
				assert.NoError(t, request.decodeError)
				assert.LessOrEqual(t, len(request.files), 100)
				allFiles = append(allFiles, request.files...)
			}
			var expected []string
			for _, pair := range pairs {
				urls := cdnExpectedURLs(pair, test.bases)
				expected = append(expected, urls...)
				containing := 0
				for _, request := range requests {
					if slices.Contains(request.files, urls[0]) {
						containing++
						for _, url := range urls {
							assert.Contains(t, request.files, url)
						}
					}
				}
				assert.Equal(t, 1, containing, "pair %v", pair)
			}
			assert.ElementsMatch(t, expected, allFiles)
			assert.ElementsMatch(t, pairs, result.Acknowledged)
			assert.Empty(t, result.Failed)
		})
	}
}

func TestCloudflarePurgerFailureOnlyMarksItsBatch(t *testing.T) {
	bases := []string{"https://img.example.test", "https://cdn.example.test"}
	pairs := cdnTwentyBlobs()
	firstURL := cdnExpectedURLs(pairs[0], bases)[0]
	endpoint := &cdnEndpoint{respond: func(files []string) (int, string) {
		if slices.Contains(files, firstURL) {
			return 500, `{"success":false}`
		}
		return 200, `{"success":true}`
	}}
	server := httptest.NewServer(endpoint)
	defer server.Close()
	result := newCDNTestPurger(t, server, bases, 5*time.Second).PurgeBlobs(t.Context(), pairs)
	requests := endpoint.snapshot()
	require.Greater(t, len(requests), 1)
	var failed, acknowledged []BlockedBlob
	failedIndex := -1
	for index, request := range requests {
		if slices.Contains(request.files, firstURL) {
			failedIndex = index
		}
	}
	require.GreaterOrEqual(t, failedIndex, 0)
	assert.Less(t, failedIndex, len(requests)-1, "failure must precede a later successful batch")
	for _, pair := range pairs {
		inFailedRequest := slices.Contains(requests[failedIndex].files, cdnExpectedURLs(pair, bases)[0])
		if inFailedRequest {
			failed = append(failed, pair)
		} else {
			acknowledged = append(acknowledged, pair)
		}
	}
	assert.ElementsMatch(t, acknowledged, result.Acknowledged)
	assert.Len(t, result.Failed, len(failed))
	var failedBlobs []BlockedBlob
	for _, failure := range result.Failed {
		assert.Equal(t, "http_500", failure.Code)
		failedBlobs = append(failedBlobs, failure.Blob)
		assert.NotContains(t, result.Acknowledged, failure.Blob)
	}
	assert.ElementsMatch(t, failed, failedBlobs)
}

func TestCloudflarePurgerResponseFailuresAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name            string
		status          int
		body, code      string
		closed, timeout bool
	}{
		{"unsuccessful", 200, `{"success":false,"detail":"` + cdnTestBody + `"}`, "api_unsuccessful", false, false},
		{"invalid json", 200, `not json ` + cdnTestBody, "api_unsuccessful", false, false},
		{"empty object", 200, `{}`, "api_unsuccessful", false, false},
		{"missing success", 200, `{"detail":"` + cdnTestBody + `"}`, "api_unsuccessful", false, false},
		{"accepted", 202, `{"success":true,"detail":"` + cdnTestBody + `"}`, "", false, false},
		{"rate limited", 429, cdnTestBody, "http_429", false, false},
		{"server error", 500, cdnTestBody, "http_500", false, false},
		{"closed server", 200, cdnTestBody, "transport", true, false},
		{"timeout", 200, cdnTestBody, "transport", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			endpoint := &cdnEndpoint{respond: func([]string) (int, string) { return test.status, test.body }}
			if test.timeout {
				endpoint.wait = func(r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}
			server := httptest.NewServer(endpoint)
			if test.closed {
				server.Close()
			} else {
				t.Cleanup(server.Close)
			}
			if test.timeout {
				t.Cleanup(func() { close(release) })
			}
			timeout := 5 * time.Second
			if test.timeout {
				timeout = 50 * time.Millisecond
			}
			purger := newCDNTestPurger(t, server, []string{"https://img.example.test"}, timeout)
			var result CDNPurgeResult
			if test.timeout {
				done := make(chan CDNPurgeResult, 1)
				go func() { done <- purger.PurgeBlobs(t.Context(), []BlockedBlob{cdnTestBlob}) }()
				select {
				case result = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("purge did not respect timeout")
				}
			} else {
				result = purger.PurgeBlobs(t.Context(), []BlockedBlob{cdnTestBlob})
			}
			if test.code == "" {
				assert.Equal(t, []BlockedBlob{cdnTestBlob}, result.Acknowledged)
				assert.Empty(t, result.Failed)
			} else {
				assert.Empty(t, result.Acknowledged)
				assert.Equal(t, []CDNPurgeFailure{{Blob: cdnTestBlob, Code: test.code}}, result.Failed)
			}
			assert.NotContains(t, fmt.Sprintf("%+v", result), cdnTestToken)
			assert.NotContains(t, fmt.Sprintf("%+v", result), cdnTestBody)
		})
	}
}

func TestNewCloudflarePurgerRequiresConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*CloudflarePurgerConfig)
	}{
		{"APIBase", func(c *CloudflarePurgerConfig) { c.APIBase = "" }},
		{"ZoneID", func(c *CloudflarePurgerConfig) { c.ZoneID = "" }},
		{"APIToken", func(c *CloudflarePurgerConfig) { c.APIToken = "" }},
		{"BaseURLs", func(c *CloudflarePurgerConfig) { c.BaseURLs = nil }},
		{"Timeout", func(c *CloudflarePurgerConfig) { c.Timeout = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer server.Close()
			config := CloudflarePurgerConfig{APIBase: server.URL, ZoneID: "zone-abc", APIToken: cdnTestToken, BaseURLs: []string{"https://img.example.test"}, Timeout: 5 * time.Second}
			test.change(&config)
			_, err := NewCloudflarePurger(config)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cdnTestToken)
		})
	}
}
