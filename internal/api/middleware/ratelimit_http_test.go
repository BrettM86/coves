package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// These tests drive the limiter through Middleware and a real ServeHTTP call,
// where ratelimit_test.go pins allow() directly. The HTTP layer is where the
// client-identity extraction, the 429 body and the method-independence live, so
// it is worth exercising separately — with the budgets production actually
// configures in cmd/server/routes.go and internal/api/routes/.

// newHTTPTestLimiter builds a limiter whose background cleanup goroutine is
// reclaimed when the test ends. NewRateLimiter starts that goroutine and only
// Stop ends it, so every limiter here goes through this helper.
func newHTTPTestLimiter(t *testing.T, requests int, window time.Duration) *RateLimiter {
	t.Helper()
	rl := NewRateLimiter(requests, window)
	t.Cleanup(rl.Stop)
	return rl
}

// newHTTPTestLimiterWithClock is newHTTPTestLimiter for the window-expiry
// tests, which drive time themselves rather than waiting for it. testClock is
// defined in ratelimit_test.go.
func newHTTPTestLimiterWithClock(t *testing.T, requests int, window time.Duration, clock *testClock) *RateLimiter {
	t.Helper()
	rl := newRateLimiterWithClock("default", requests, window, clock.Now)
	t.Cleanup(rl.Stop)
	return rl
}

// okHandler is the terminal handler for these tests: it proves the request
// reached past the limiter.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestRateLimiter_HTTP_GeneralEndpoints covers the global limiter's budget
// (100 requests per minute per client) as wired in cmd/server/routes.go.
func TestRateLimiter_HTTP_GeneralEndpoints(t *testing.T) {
	t.Run("allows requests under limit", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 100, 1*time.Minute).Middleware(okHandler())

		for i := 0; i < 50; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "192.168.1.100:12345"
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}
	})

	t.Run("blocks requests at limit", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 10, 1*time.Minute).Middleware(okHandler())
		clientIP := "192.168.1.101:12345"

		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request 11 should be rate limited")
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded", "Should have rate limit error message")
	})

	t.Run("returns proper 429 status code", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 1, 1*time.Minute).Middleware(okHandler())
		clientIP := "192.168.1.102:12345"

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should return 429 Too Many Requests")
		assert.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
	})

	t.Run("rate limits are per-client", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 2, 1*time.Minute).Middleware(okHandler())

		client1IP := "192.168.1.103:12345"
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = client1IP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = client1IP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Client 1 should be rate limited")

		client2IP := "192.168.1.104:12345"
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = client2IP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Client 2 should not be affected by Client 1's rate limit")
	})

	t.Run("buckets by X-Forwarded-For", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 1, 1*time.Minute).Middleware(okHandler())

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		req = httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should rate limit based on X-Forwarded-For")
	})

	t.Run("buckets by X-Real-IP", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 1, 1*time.Minute).Middleware(okHandler())

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Real-IP", "203.0.113.2")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		req = httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Real-IP", "203.0.113.2")
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should rate limit based on X-Real-IP")
	})
}

// TestRateLimiter_HTTP_CommentEndpoints covers the stricter per-route budget
// (20 requests per minute) that cmd/server/routes.go puts in front of the
// nested comment queries.
func TestRateLimiter_HTTP_CommentEndpoints(t *testing.T) {
	const commentsPath = "/xrpc/social.coves.community.comment.getComments?post=at://test"

	// A comment handler writes JSON, so this also proves the limiter passes a
	// non-trivial response body through untouched.
	commentHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"comments": []map[string]interface{}{},
		})
	})
	handler := newHTTPTestLimiter(t, 20, 1*time.Minute).Middleware(commentHandler)

	t.Run("allows requests under comment limit", func(t *testing.T) {
		clientIP := "192.168.1.110:12345"

		for i := 0; i < 15; i++ {
			req := httptest.NewRequest("GET", commentsPath, nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
			assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
		}
	})

	t.Run("blocks requests at comment limit", func(t *testing.T) {
		clientIP := "192.168.1.111:12345"

		for i := 0; i < 20; i++ {
			req := httptest.NewRequest("GET", commentsPath, nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		req := httptest.NewRequest("GET", commentsPath, nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request 21 should be rate limited")
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded")
	})
}

// TestRateLimiter_HTTP_NoRateLimitHeaders pins the current response shape: the
// limiter reports nothing about the remaining budget. Clients therefore cannot
// back off proactively, only react to a 429. If X-RateLimit-* or Retry-After
// are ever added, this test should be rewritten to assert their values rather
// than deleted.
func TestRateLimiter_HTTP_NoRateLimitHeaders(t *testing.T) {
	t.Run("successful response carries no budget headers", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 5, 1*time.Minute).Middleware(okHandler())

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "192.168.1.120:12345"
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Remaining"))
		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Reset"))
		assert.Equal(t, "", rr.Header().Get("Retry-After"))
	})

	t.Run("429 response explains itself in the body", func(t *testing.T) {
		handler := newHTTPTestLimiter(t, 1, 1*time.Minute).Middleware(okHandler())
		clientIP := "192.168.1.121:12345"

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code)
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded")
		assert.Contains(t, rr.Body.String(), "Please try again later")
		assert.Equal(t, "", rr.Header().Get("Retry-After"), "no Retry-After to tell the client when to come back")
	})
}

// TestRateLimiter_HTTP_WindowReset covers budget recovery once a window
// expires. The limiter's clock is injected here, so the window boundary is
// crossed by advancing time rather than by waiting for it: no wall clock is
// spent and there is no margin to tune.
func TestRateLimiter_HTTP_WindowReset(t *testing.T) {
	t.Run("budget returns after the window expires", func(t *testing.T) {
		clock := newTestClock()
		handler := newHTTPTestLimiterWithClock(t, 2, 1*time.Minute, clock).Middleware(okHandler())
		clientIP := "192.168.1.130:12345"

		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code)

		clock.Advance(1*time.Minute + time.Second)

		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Request should succeed after window reset")
	})

	t.Run("window is anchored to the first request, not the last", func(t *testing.T) {
		// Spacing requests inside the window must not extend it: the reset time
		// is set when the bucket opens and is never pushed back by later
		// requests. Three requests spread over 100ms of a 200ms window still
		// exhaust the budget, and the budget returns on the original schedule.
		clock := newTestClock()
		handler := newHTTPTestLimiterWithClock(t, 3, 200*time.Millisecond, clock).Middleware(okHandler())
		clientIP := "192.168.1.131:12345"

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
			clock.Advance(50 * time.Millisecond)
		}

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "4th request should be blocked")

		// 150ms into the window when the 4th request was rejected; another
		// 100ms puts the clock at 250ms, past the 200ms reset the FIRST request
		// set. If later requests had pushed the reset back, this would still be
		// inside the window and still be a 429.
		clock.Advance(100 * time.Millisecond)

		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Request should succeed once the original window elapses")
	})
}

// TestRateLimiter_HTTP_ConcurrentRequests proves the budget is exact under
// concurrency: 20 simultaneous requests against a budget of 10 must split
// 10/10, never 11/9. Run with -race, this also covers the map guard.
func TestRateLimiter_HTTP_ConcurrentRequests(t *testing.T) {
	handler := newHTTPTestLimiter(t, 10, 1*time.Minute).Middleware(okHandler())

	clientIP := "192.168.1.140:12345"
	results := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			results <- rr.Code
		}()
	}

	var successCount, rateLimitedCount int
	for i := 0; i < 20; i++ {
		switch code := <-results; code {
		case http.StatusOK:
			successCount++
		case http.StatusTooManyRequests:
			rateLimitedCount++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}

	assert.Equal(t, 10, successCount, "Should allow exactly 10 requests")
	assert.Equal(t, 10, rateLimitedCount, "Should rate limit exactly 10 requests")
}

// TestRateLimiter_HTTP_AcrossHTTPMethods proves the budget is per client, not
// per method — otherwise an abuser could quadruple their budget by rotating
// verbs against the same route.
func TestRateLimiter_HTTP_AcrossHTTPMethods(t *testing.T) {
	handler := newHTTPTestLimiter(t, 3, 1*time.Minute).Middleware(okHandler())
	clientIP := "192.168.1.150:12345"

	for _, tc := range []struct{ method, body string }{
		{method: "GET"},
		{method: "POST", body: "{}"},
		{method: "PUT", body: "{}"},
	} {
		var body io.Reader
		if tc.body != "" {
			body = bytes.NewBufferString(tc.body)
		}
		req := httptest.NewRequest(tc.method, "/test", body)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "%s should consume budget, not bypass it", tc.method)
	}

	req := httptest.NewRequest("DELETE", "/test", nil)
	req.RemoteAddr = clientIP
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Rate limit should apply across methods")
}
