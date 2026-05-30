package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allow() is the load-bearing primitive — every per-route rate limit and the
// global limit go through it. These tests pin the contract directly so we don't
// rely on flaky end-to-end timing.

func TestRateLimiter_Allow_UnderLimit(t *testing.T) {
	rl := NewNamedRateLimiter("test", 3, time.Minute)
	t.Cleanup(rl.Stop)

	for i := 0; i < 3; i++ {
		assert.True(t, rl.allow("ip-a"), "request %d should be allowed (limit 3)", i+1)
	}
}

func TestRateLimiter_Allow_RejectsOverLimit(t *testing.T) {
	rl := NewNamedRateLimiter("test", 3, time.Minute)
	t.Cleanup(rl.Stop)

	for i := 0; i < 3; i++ {
		require.True(t, rl.allow("ip-a"))
	}
	assert.False(t, rl.allow("ip-a"), "4th request must be rejected")
	assert.False(t, rl.allow("ip-a"), "subsequent requests stay rejected within window")
}

func TestRateLimiter_Allow_PerClientIsolation(t *testing.T) {
	// Two distinct clients must have independent budgets.
	rl := NewNamedRateLimiter("test", 2, time.Minute)
	t.Cleanup(rl.Stop)

	require.True(t, rl.allow("ip-a"))
	require.True(t, rl.allow("ip-a"))
	assert.False(t, rl.allow("ip-a"), "ip-a exhausted")

	assert.True(t, rl.allow("ip-b"), "ip-b must not be affected by ip-a")
	assert.True(t, rl.allow("ip-b"))
	assert.False(t, rl.allow("ip-b"))
}

func TestRateLimiter_Allow_WindowResets(t *testing.T) {
	// Tiny window so the test runs fast.
	rl := NewNamedRateLimiter("test", 1, 50*time.Millisecond)
	t.Cleanup(rl.Stop)

	require.True(t, rl.allow("ip-a"))
	require.False(t, rl.allow("ip-a"))

	time.Sleep(80 * time.Millisecond)

	assert.True(t, rl.allow("ip-a"), "window expired — budget should reset")
}

func TestRateLimiter_Middleware_Returns429AfterLimit(t *testing.T) {
	rl := NewNamedRateLimiter("test", 2, time.Minute)
	t.Cleanup(rl.Stop)
	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Real-IP", "10.0.0.1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "request %d should pass", i+1)
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Real-IP", "10.0.0.1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "3rd request must return 429")
}

func TestRateLimiter_Middleware_DoesNotCallNextOnReject(t *testing.T) {
	rl := NewNamedRateLimiter("test", 1, time.Minute)
	t.Cleanup(rl.Stop)
	calls := 0
	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))

	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.Header.Set("X-Real-IP", "10.0.0.2")
	handler.ServeHTTP(httptest.NewRecorder(), req1)

	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("X-Real-IP", "10.0.0.2")
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	assert.Equal(t, 1, calls, "rejected request must NOT invoke the inner handler")
}

func TestGetClientIP_PrefersXRealIPOverXForwardedFor(t *testing.T) {
	// X-Real-IP is set by our proxy to the direct upstream and can't be spoofed
	// by the client; XFF is appended-to and the leftmost entries are attacker-
	// controlled. So X-Real-IP must win when both are present.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	req.RemoteAddr = "9.9.9.9:1234"

	assert.Equal(t, "5.6.7.8", GetClientIP(req))
}

func TestGetClientIP_FallsBackToXRealIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "5.6.7.8")
	req.RemoteAddr = "9.9.9.9:1234"

	assert.Equal(t, "5.6.7.8", GetClientIP(req))
}

func TestGetClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"

	assert.Equal(t, "9.9.9.9", GetClientIP(req))
}

func TestGetClientIP_XForwardedForParsesRightmostHop(t *testing.T) {
	// The rightmost XFF entry is the one our own proxy appended, so it's the
	// most trustworthy hop in the list.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3")
	req.RemoteAddr = "9.9.9.9:1234"

	assert.Equal(t, "3.3.3.3", GetClientIP(req))
}

func TestGetClientIP_RemoteAddrWithoutPort(t *testing.T) {
	// Unusual (unix socket, odd test harness) but SplitHostPort errors should
	// not drop the request — fall back to RemoteAddr verbatim.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1"

	assert.Equal(t, "10.0.0.1", GetClientIP(req))
}

func TestRateLimiter_TreatsClientWithVaryingProxyChainAsSameBucket(t *testing.T) {
	// Security property: an attacker rotating the leftmost (spoofable) XFF
	// entries cannot escape their bucket, because we key on the rightmost hop.
	rl := NewNamedRateLimiter("test", 1, time.Minute)
	t.Cleanup(rl.Stop)
	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.Header.Set("X-Forwarded-For", "1.1.1.1, 9.9.9.9")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusOK, w1.Code, "first request consumes the budget")

	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("X-Forwarded-For", "2.2.2.2, 9.9.9.9")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code,
		"rotating the spoofable leftmost XFF entry must NOT mint a new bucket")
}

func TestRateLimiter_Cleanup_RemovesExpiredEntries(t *testing.T) {
	// Tiny window: cleanup ticker also fires every `window`, so sleeping past
	// 2× the window guarantees at least one tick has cleared expired entries.
	rl := NewNamedRateLimiter("test", 1, 50*time.Millisecond)
	t.Cleanup(rl.Stop)

	require.True(t, rl.allow("ip-a"))
	require.Equal(t, 1, rl.clientCount(), "entry should be tracked immediately")

	time.Sleep(150 * time.Millisecond)

	assert.Equal(t, 0, rl.clientCount(), "cleanup goroutine must evict expired entries")
}

func TestRateLimiter_AllowIsConcurrencySafe(t *testing.T) {
	// 100 racing goroutines, limit 50: exactly 50 must win. Run under -race.
	const goroutines = 100
	const limit = 50

	rl := NewNamedRateLimiter("test", limit, time.Minute)
	t.Cleanup(rl.Stop)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if rl.allow("ip-x") {
				allowed.Add(1)
			}
		}()
	}

	close(start)
	wg.Wait()

	assert.Equal(t, int64(limit), allowed.Load(),
		"under contention exactly `limit` calls must return true")
}

func TestRateLimiter_PerRouteFiresBeforeGlobal(t *testing.T) {
	// Two stacked limiters: a generous global and a tight per-route. The
	// per-route limiter is the inner middleware and should reject the 6th
	// request before the global one even sees it (well, the global sees it
	// and approves — but the inner handler must only fire 5 times).
	global := NewNamedRateLimiter("global", 100, time.Minute)
	t.Cleanup(global.Stop)
	perRoute := NewNamedRateLimiter("per-route", 5, time.Minute)
	t.Cleanup(perRoute.Stop)

	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	// global wraps per-route wraps inner: a request hits global first, then
	// per-route, then inner. Per-route is the *tighter* limit so it's the one
	// that should reject the 6th call.
	handler := global.Middleware(perRoute.Middleware(inner))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Real-IP", "10.0.0.99")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "request %d should pass", i+1)
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Real-IP", "10.0.0.99")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "6th request must return 429")
	assert.Equal(t, int64(5), calls.Load(),
		"inner handler must have fired exactly 5 times (per-route limit), proving the per-route limiter rejected the 6th request rather than the global one")
}
