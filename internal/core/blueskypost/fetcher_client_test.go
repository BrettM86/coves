package blueskypost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewBlueskyAPI_ClientHasNoTimeoutOfItsOwn pins that the per-fetch context
// deadline is the only bound. The SSRF-safe client ships with a 15s Timeout;
// left in place it would silently cap any configured fetch timeout above it.
func TestNewBlueskyAPI_ClientHasNoTimeoutOfItsOwn(t *testing.T) {
	api := newBlueskyAPI(blueskyAPIBaseURL, false)

	if api.client == nil {
		t.Fatal("newBlueskyAPI must build the shared client")
	}
	if api.client.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 so the fetch context is the single bound", api.client.Timeout)
	}
	if api.client.CheckRedirect == nil {
		t.Error("client must keep the SSRF-safe client's redirect policy")
	}
}

// TestFetchBlueskyPost_GuardedClientRefusesLoopback proves the production
// client keeps its dial-time SSRF check: a loopback server is refused and never
// receives the request.
func TestFetchBlueskyPost_GuardedClientRefusesLoopback(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	result, err := fetchBlueskyPost(context.Background(), testPostURI, time.Second, newBlueskyAPI(server.URL, false))

	if err == nil {
		t.Fatalf("fetch from a loopback host succeeded with result %+v; the SSRF guard must refuse it", result)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("loopback server received %d requests, want 0", got)
	}
}

// TestFetchBlueskyPost_StalledBodyBoundedByTimeout proves the configured
// timeout covers reading the body, not just receiving headers: the server sends
// headers and part of the JSON, then stalls until the test ends.
func TestFetchBlueskyPost_StalledBodyBoundedByTimeout(t *testing.T) {
	release := make(chan struct{})
	bodyStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"posts":[`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(bodyStarted)
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	// Cleanups run last-in first-out: release the handler before Close waits
	// for it.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	const fetchTimeout = 100 * time.Millisecond
	type fetchOutcome struct {
		result *BlueskyPostResult
		err    error
	}
	outcome := make(chan fetchOutcome, 1)
	go func() {
		result, err := fetchBlueskyPost(context.Background(), testPostURI, fetchTimeout, newBlueskyAPI(server.URL, true))
		outcome <- fetchOutcome{result: result, err: err}
	}()

	// If the timeout is not enforced the fetch never returns; fail here rather
	// than hang until the suite's deadline.
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	var got fetchOutcome
	select {
	case got = <-outcome:
	case <-watchdog.C:
		t.Fatalf("fetch still running 5s after a %v timeout; the stalled body was not bounded", fetchTimeout)
	}

	// The deadline must have fired while reading the body, not before the
	// headers arrived.
	select {
	case <-bodyStarted:
	default:
		t.Fatal("the server never flushed the partial body; the timeout fired before body reading began")
	}
	if got.err == nil {
		t.Fatalf("fetch of a stalled body succeeded with result %+v; want a timeout error", got.result)
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", got.err)
	}
}

// TestService_WithTimeout_NonPositiveKeepsDefault guards against a zero or
// negative timeout expiring every fetch before it starts.
func TestService_WithTimeout_NonPositiveKeepsDefault(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		svc := NewService(newMockRepository(), &mockIdentityResolver{}, WithTimeout(timeout)).(*service)
		if svc.timeout != defaultFetchTimeout {
			t.Errorf("WithTimeout(%v): timeout = %v, want default %v", timeout, svc.timeout, defaultFetchTimeout)
		}
	}
}
