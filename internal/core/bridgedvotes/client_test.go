package bridgedvotes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const clientVoteAggregatesPath = "/xrpc/social.coves.bridge.getVoteAggregates"

func TestClientGetVoteAggregatesRequestAndHappyPath(t *testing.T) {
	t.Parallel()

	inputURIs := []string{"at://a", "at://b"}
	updatedAt := "2026-08-31T02:04:01.080Z"
	expectedAsOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)

	type recordedRequest struct {
		method string
		path   string
		uris   []string
	}
	var (
		mu       sync.Mutex
		recorded []recordedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			uris:   decodeClientURIs(r.URL.Query()["uris"]),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{{
				"uri": "at://a", "upvotes": 2, "downvotes": 1, "updatedAt": updatedAt,
			}},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(context.Background(), trustedHost(t, server.URL), inputURIs)
	require.NoError(t, err)

	mu.Lock()
	requests := append([]recordedRequest(nil), recorded...)
	mu.Unlock()
	require.Len(t, requests, 1, "the client must issue one XRPC request")
	require.Equal(t, http.MethodGet, requests[0].method)
	require.Equal(t, clientVoteAggregatesPath, requests[0].path)
	require.ElementsMatch(t, inputURIs, requests[0].uris)
	require.Equal(t, []bridgedvotes.Aggregate{{
		URI: "at://a", Upvotes: 2, Downvotes: 1, AsOf: expectedAsOf,
	}}, got, "well-formed but omitted URIs are absent without making the request fail")
}

func TestClientGetVoteAggregatesDropsInvalidCounts(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	expectedAsOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{
				{"uri": "at://valid", "upvotes": 2, "downvotes": 1, "updatedAt": updatedAt},
				{"uri": "at://negative-up", "upvotes": -1, "downvotes": 0, "updatedAt": updatedAt},
				{"uri": "at://negative-down", "upvotes": 0, "downvotes": -1, "updatedAt": updatedAt},
				{"uri": "at://excess-up", "upvotes": 1_000_001, "downvotes": 0, "updatedAt": updatedAt},
				{"uri": "at://excess-down", "upvotes": 0, "downvotes": 1_000_001, "updatedAt": updatedAt},
			},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL),
		[]string{"at://valid", "at://negative-up", "at://negative-down", "at://excess-up", "at://excess-down"},
	)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Aggregate{{
		URI: "at://valid", Upvotes: 2, Downvotes: 1, AsOf: expectedAsOf,
	}}, got)
}

func TestClientGetVoteAggregatesDropsInvalidAsOf(t *testing.T) {
	t.Parallel()

	const validUpdatedAt = "2026-08-31T02:04:01.080Z"
	expectedAsOf, err := time.Parse(time.RFC3339Nano, validUpdatedAt)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{
				{"uri": "at://invalid-as-of", "upvotes": 4, "downvotes": 1, "updatedAt": "not-a-time"},
				{"uri": "at://valid", "upvotes": 2, "downvotes": 1, "updatedAt": validUpdatedAt},
			},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://invalid-as-of", "at://valid"},
	)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Aggregate{{
		URI: "at://valid", Upvotes: 2, Downvotes: 1, AsOf: expectedAsOf,
	}}, got)
}

func TestClientGetVoteAggregatesBatchCap(t *testing.T) {
	t.Parallel()

	t.Run("101 URIs rejected without request", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			t.Error("batch larger than 100 must be rejected before an HTTP request")
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
			context.Background(), trustedHost(t, server.URL), clientURIs(101),
		)
		require.Error(t, err)
		require.Zero(t, requests.Load())
	})

	t.Run("exactly 100 URIs allowed", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"aggregates": []any{}}); err != nil {
				t.Errorf("encode empty aggregate response: %v", err)
			}
		}))
		t.Cleanup(server.Close)

		got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
			context.Background(), trustedHost(t, server.URL), clientURIs(100),
		)
		require.NoError(t, err)
		require.Empty(t, got)
		require.EqualValues(t, 1, requests.Load())
	})
}

func TestClientGetVoteAggregatesErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantTransient: true},
		{name: "service unavailable", status: http.StatusServiceUnavailable, wantTransient: true},
		{name: "request timeout", status: http.StatusRequestTimeout, wantTransient: true},
		{name: "too early", status: http.StatusTooEarly, wantTransient: true},
		{name: "bad request", status: http.StatusBadRequest, wantTransient: false},
		{name: "not found", status: http.StatusNotFound, wantTransient: false},
		{name: "forbidden", status: http.StatusForbidden, wantTransient: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(test.status), test.status)
			}))
			t.Cleanup(server.Close)

			_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
				context.Background(), trustedHost(t, server.URL), []string{"at://a"},
			)
			require.Error(t, err)
			require.Equal(t, test.wantTransient, bridgedvotes.IsTransient(err))
		})
	}

	t.Run("transport failure", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := bridgedvotes.NewClient(server.Client())
		host := trustedHost(t, server.URL)
		server.Close()

		_, err := client.GetVoteAggregates(context.Background(), host, []string{"at://a"})
		require.Error(t, err)
		require.True(t, bridgedvotes.IsTransient(err))
	})
}

func TestClientGetVoteAggregatesEmptyURIsDoesNotRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		t.Error("empty URI input must not make an HTTP request")
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(context.Background(), trustedHost(t, server.URL), nil)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Zero(t, requests.Load())
}

func TestClientGetVoteAggregatesExcludesUnrequestedResponseEntries(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	asOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{
				{"uri": "at://requested-a", "upvotes": 2, "downvotes": 1, "updatedAt": updatedAt},
				{"uri": "at://unrequested", "upvotes": 99, "downvotes": 41, "updatedAt": updatedAt},
				{"uri": "at://requested-b", "upvotes": 4, "downvotes": 0, "updatedAt": updatedAt},
			},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested-a", "at://requested-b"},
	)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Aggregate{
		{URI: "at://requested-a", Upvotes: 2, Downvotes: 1, AsOf: asOf},
		{URI: "at://requested-b", Upvotes: 4, Downvotes: 0, AsOf: asOf},
	}, got, "the bridge may answer only for subjects named in this request")
}

func TestClientGetVoteAggregatesRejectsResponseLargerThanOneMiB(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aggregates":[],"padding":"` + strings.Repeat("x", 1<<20) + `"}`))
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.Error(t, err, "responses larger than 1 MiB must be rejected")
	require.False(t, bridgedvotes.IsTransient(err), "a response-size contract violation is permanent")
}

func TestClientGetVoteAggregatesDeduplicatesResponseURIsFirstWins(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	asOf, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{
				{"uri": "at://requested", "upvotes": 2, "downvotes": 1, "updatedAt": updatedAt},
				{"uri": "at://requested", "upvotes": 9, "downvotes": 4, "updatedAt": updatedAt},
				{"uri": "at://requested", "upvotes": 100, "downvotes": 50, "updatedAt": updatedAt},
			},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.NoError(t, err)
	require.Equal(t, []bridgedvotes.Aggregate{{
		URI: "at://requested", Upvotes: 2, Downvotes: 1, AsOf: asOf,
	}}, got, "one requested URI may cause at most one DB write, with the first response occurrence winning")
}

func TestClientGetVoteAggregatesDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var redirectedRequests atomic.Int64
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aggregates":[]}`))
	}))
	t.Cleanup(redirectTarget.Close)
	redirectingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+clientVoteAggregatesPath, http.StatusFound)
	}))
	t.Cleanup(redirectingServer.Close)

	_, err := bridgedvotes.NewClient(redirectingServer.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, redirectingServer.URL), []string{"at://requested"},
	)
	assert.Error(t, err, "redirect responses are permanent contract violations")
	if err != nil {
		assert.False(t, bridgedvotes.IsTransient(err))
	}
	assert.Zero(t, redirectedRequests.Load(), "the client must not send trusted-host requests to a redirect target")
}

func TestClientGetVoteAggregatesClassifiesTruncatedBodyAsTransient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write([]byte(`{"aggregates":[{"uri":"at://requested"`))
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.Error(t, err)
	require.True(t, bridgedvotes.IsTransient(err),
		"a response truncated mid-body is a retryable transport fault")
}

func decodeClientURIs(values []string) []string {
	var uris []string
	for _, value := range values {
		for _, uri := range strings.Split(value, ",") {
			if uri = strings.TrimSpace(uri); uri != "" {
				uris = append(uris, uri)
			}
		}
	}
	return uris
}

func clientURIs(count int) []string {
	uris := make([]string, count)
	for i := range uris {
		uris[i] = fmt.Sprintf("at://subject-%03d", i)
	}
	return uris
}

func TestClientGetVoteAggregatesMalformedJSONWithOKStatusIsPermanent(t *testing.T) {
	t.Parallel()

	// A proxy answering an XRPC path with an HTML error page and HTTP 200 is the
	// exact failure that wedged the Tidepool side; the poller's poison-batch
	// path depends on this classification.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>Bad gateway</body></html>"))
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.Error(t, err)
	require.False(t, bridgedvotes.IsTransient(err), "a body that is not the contract is a permanent failure")
}

func TestClientGetVoteAggregatesMissingAggregatesKeyIsPermanent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.Error(t, err, "a renamed envelope must not decode as an empty success that marks every subject polled")
	require.False(t, bridgedvotes.IsTransient(err))
}

func TestClientGetVoteAggregatesEveryEntryRejectedIsPermanent(t *testing.T) {
	t.Parallel()

	const updatedAt = "2026-08-31T02:04:01.080Z"
	tests := []struct {
		name    string
		entries []map[string]any
	}{
		{name: "only unrequested subjects", entries: []map[string]any{
			{"uri": "at://other", "upvotes": 1, "downvotes": 0, "updatedAt": updatedAt},
		}},
		{name: "unparseable updatedAt format", entries: []map[string]any{
			{"uri": "at://requested", "upvotes": 1, "downvotes": 0, "updatedAt": "1756605841"},
		}},
		{name: "counts out of range", entries: []map[string]any{
			{"uri": "at://requested", "upvotes": -1, "downvotes": 0, "updatedAt": updatedAt},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"aggregates": test.entries}); err != nil {
					t.Errorf("encode aggregate response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
				context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
			)
			require.Error(t, err, "a response that gets every entry wrong is a contract break, not a partial success")
			require.False(t, bridgedvotes.IsTransient(err))
		})
	}
}

func TestClientGetVoteAggregatesDropsFutureAndZeroAsOf(t *testing.T) {
	t.Parallel()

	const validUpdatedAt = "2026-08-31T02:04:01.080Z"
	expectedAsOf, err := time.Parse(time.RFC3339Nano, validUpdatedAt)
	require.NoError(t, err)
	farFuture := time.Now().Add(bridgedvotes.MaxAsOfSkew + time.Hour).UTC().Format(time.RFC3339)
	withinSkew := time.Now().Add(bridgedvotes.MaxAsOfSkew / 2).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"aggregates": []map[string]any{
				{"uri": "at://future", "upvotes": 4, "downvotes": 1, "updatedAt": farFuture},
				{"uri": "at://zero", "upvotes": 4, "downvotes": 1, "updatedAt": "0001-01-01T00:00:00Z"},
				{"uri": "at://skew", "upvotes": 3, "downvotes": 0, "updatedAt": withinSkew},
				{"uri": "at://valid", "upvotes": 2, "downvotes": 1, "updatedAt": validUpdatedAt},
			},
		}); err != nil {
			t.Errorf("encode aggregate response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	got, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://future", "at://zero", "at://skew", "at://valid"},
	)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "at://skew", got[0].URI, "a stamp inside the skew allowance is ordinary clock drift")
	require.Equal(t, bridgedvotes.Aggregate{
		URI: "at://valid", Upvotes: 2, Downvotes: 1, AsOf: expectedAsOf,
	}, got[1], "a far-future stamp would win the >= guard once and then reject every honest aggregate")
}

func TestClientGetVoteAggregatesRejectsZeroTrustedHost(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(server.Client()).GetVoteAggregates(
		context.Background(), bridgedvotes.TrustedHost{}, []string{"at://requested"},
	)
	require.Error(t, err)
	require.Zero(t, requests.Load())
}

func TestNewClientNilDefaultIsSSRFGuarded(t *testing.T) {
	t.Parallel()

	// The default client must be the guarded one used by every other outbound
	// fetch: a loopback bridge is exactly what the guard refuses without the
	// dev-only private-host hatch, and the refusal must never reach the server.
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aggregates":[]}`))
	}))
	t.Cleanup(server.Close)

	_, err := bridgedvotes.NewClient(nil).GetVoteAggregates(
		context.Background(), trustedHost(t, server.URL), []string{"at://requested"},
	)
	require.Error(t, err, "the nil-client default must refuse a private address")
	require.Zero(t, requests.Load(), "the guard must refuse before dialing")
}

func trustedHost(t *testing.T, raw string) bridgedvotes.TrustedHost {
	t.Helper()
	host, err := bridgedvotes.ParseTrustedHost(raw)
	require.NoError(t, err)
	return host
}
