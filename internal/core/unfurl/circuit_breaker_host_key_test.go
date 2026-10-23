package unfurl

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The OpenGraph breaker must be keyed per host, and the breaker's per-key maps
// must stay bounded.
//
// UnfurlURL used the constant key "opengraph" for every non-oEmbed host, so
// three consecutive failures fetching ANY one host opened the circuit for every
// OpenGraph unfurl on the instance for openDuration. One dead site disabled
// link previews for every other site.

// hostRoutedTransport answers by request host without opening a socket, so the
// SSRF guard's dialer is never reached: alpha.example always fails with a 500,
// beta.example serves a page with an og:title.
type hostRoutedTransport struct{}

func (hostRoutedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Hostname() {
	case "alpha.example":
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("down")),
			Request:    req,
		}, nil
	case "beta.example":
		page := `<html><head><meta property="og:title" content="Beta Title"></head><body></body></html>`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(page)),
			Request:    req,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected host in test transport: %s", req.URL.Hostname())
	}
}

func TestUnfurlService_FailuresOnOneHostDoNotDisableAnotherHost(t *testing.T) {
	t.Parallel()

	// Neither host may route to the oEmbed or Kagi paths, or this test would not
	// be exercising the OpenGraph breaker key at all.
	for _, link := range []string{"https://alpha.example/a", "https://beta.example/b"} {
		require.True(t, isSupported(link), "%s must be supported", link)
		require.False(t, isOEmbedProvider(link), "%s must not route to oEmbed", link)
		require.NotEqual(t, "kite.kagi.com", extractDomain(link))
	}

	svc := NewService(alwaysMissRepo{}, PrivateHostOptions(false)...).(*service)
	svc.httpClient.Transport = hostRoutedTransport{}
	ctx := context.Background()

	threshold := svc.circuitBreaker.failureThreshold
	for i := 0; i <= threshold; i++ {
		_, err := svc.UnfurlURL(ctx, fmt.Sprintf("https://alpha.example/page-%d", i))
		require.Error(t, err, "alpha.example request %d must fail", i)
	}

	tests := []struct {
		name          string
		link          string
		wantTitle     string
		wantErrSubstr string
	}{
		{
			name:      "a healthy host still unfurls after another host tripped its breaker",
			link:      "https://beta.example/story",
			wantTitle: "Beta Title",
		},
		{
			name:          "the failing host's own breaker is open",
			link:          "https://alpha.example/after-open",
			wantErrSubstr: "circuit breaker open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := svc.UnfurlURL(ctx, tt.link)
			if tt.wantErrSubstr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSubstr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, tt.wantTitle, result.Title)
		})
	}
}

// TestCircuitBreaker_TrackedKeysAreBounded: once keys are per host, an attacker
// can mint unlimited keys by posting links to distinct hosts. Every map the
// breaker keeps per key must have a cap.
//
// Not parallel: it silences the global logger for 50000 failure log lines, and
// the serial phase finishes before parallel tests resume. No test in this
// package asserts on log output.
func TestCircuitBreaker_TrackedKeysAreBounded(t *testing.T) {
	previous := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(previous) })

	cb := newCircuitBreaker()
	// Each key fails failureThreshold times so its circuit opens and every
	// per-key map (failures, lastFailure, state, lastStateLog) is written.
	for i := 0; i < 50000; i++ {
		key := fmt.Sprintf("host-%d.example", i)
		for j := 0; j < cb.failureThreshold; j++ {
			cb.recordFailure(key, fmt.Errorf("HTTP request returned status 500"))
		}
	}

	cb.mu.RLock()
	defer cb.mu.RUnlock()
	assert.LessOrEqual(t, len(cb.failures), 10000, "failures map")
	assert.LessOrEqual(t, len(cb.lastFailure), 10000, "lastFailure map")
	assert.LessOrEqual(t, len(cb.state), 10000, "state map")
	assert.LessOrEqual(t, len(cb.lastStateLog), 10000, "lastStateLog map")
}
