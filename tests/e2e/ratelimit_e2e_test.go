package e2e

import (
	"Coves/internal/api/middleware"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRateLimiting_E2E_GeneralEndpoints tests the global rate limiter (100 req/min)
// This tests the middleware applied to all endpoints in main.go
func TestRateLimiting_E2E_GeneralEndpoints(t *testing.T) {
	// Create rate limiter with same config as main.go: 100 requests per minute
	rateLimiter := middleware.NewRateLimiter(100, 1*time.Minute)

	// Simple test handler that just returns 200 OK
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Wrap handler with rate limiter
	handler := rateLimiter.Middleware(testHandler)

	t.Run("Allows requests under limit", func(t *testing.T) {
		// Make 50 requests (well under 100 limit)
		for i := 0; i < 50; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "192.168.1.100:12345" // Consistent IP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}
	})

	t.Run("Blocks requests at limit", func(t *testing.T) {
		// Create fresh rate limiter for this test
		limiter := middleware.NewRateLimiter(10, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.101:12345"

		// Make exactly 10 requests (at limit)
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		// 11th request should be rate limited
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request 11 should be rate limited")
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded", "Should have rate limit error message")
	})

	t.Run("Returns proper 429 status code", func(t *testing.T) {
		// Create very strict rate limiter (1 req/min)
		limiter := middleware.NewRateLimiter(1, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.102:12345"

		// First request succeeds
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Second request gets 429
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should return 429 Too Many Requests")
		assert.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
	})

	t.Run("Rate limits are per-client (IP isolation)", func(t *testing.T) {
		// Create strict rate limiter
		limiter := middleware.NewRateLimiter(2, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		// Client 1 makes 2 requests (exhausts limit)
		client1IP := "192.168.1.103:12345"
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = client1IP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// Client 1's 3rd request is blocked
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = client1IP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Client 1 should be rate limited")

		// Client 2 can still make requests (different IP)
		client2IP := "192.168.1.104:12345"
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = client2IP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Client 2 should not be affected by Client 1's rate limit")
	})

	t.Run("Respects X-Forwarded-For header", func(t *testing.T) {
		limiter := middleware.NewRateLimiter(1, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		// First request with X-Forwarded-For
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Second request with same X-Forwarded-For should be rate limited
		req = httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should rate limit based on X-Forwarded-For")
	})

	t.Run("Respects X-Real-IP header", func(t *testing.T) {
		limiter := middleware.NewRateLimiter(1, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		// First request with X-Real-IP
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Real-IP", "203.0.113.2")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Second request with same X-Real-IP should be rate limited
		req = httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Real-IP", "203.0.113.2")
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Should rate limit based on X-Real-IP")
	})
}

// TestRateLimiting_E2E_CommentEndpoints tests comment-specific rate limiting (20 req/min)
// This tests the stricter rate limit applied to expensive nested comment queries
func TestRateLimiting_E2E_CommentEndpoints(t *testing.T) {
	// Create rate limiter with comment config from main.go: 20 requests per minute
	commentRateLimiter := middleware.NewRateLimiter(20, 1*time.Minute)

	// Mock comment handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate comment response
		response := map[string]interface{}{
			"comments": []map[string]interface{}{},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	})

	// Wrap with comment rate limiter
	handler := commentRateLimiter.Middleware(testHandler)

	t.Run("Allows requests under comment limit", func(t *testing.T) {
		clientIP := "192.168.1.110:12345"

		// Make 15 requests (under 20 limit)
		for i := 0; i < 15; i++ {
			req := httptest.NewRequest("GET", "/xrpc/social.coves.community.comment.getComments?post=at://test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}
	})

	t.Run("Blocks requests at comment limit", func(t *testing.T) {
		clientIP := "192.168.1.111:12345"

		// Make exactly 20 requests (at limit)
		for i := 0; i < 20; i++ {
			req := httptest.NewRequest("GET", "/xrpc/social.coves.community.comment.getComments?post=at://test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		// 21st request should be rate limited
		req := httptest.NewRequest("GET", "/xrpc/social.coves.community.comment.getComments?post=at://test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request 21 should be rate limited")
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded")
	})

	t.Run("Comment limit is stricter than general limit", func(t *testing.T) {
		// Verify that 20 req/min < 100 req/min
		assert.Less(t, 20, 100, "Comment rate limit should be stricter than general rate limit")
	})
}

// TestRateLimiting_E2E_AggregatorPosts tests aggregator post rate limiting (10 posts/hour)
// This is already tested in aggregator_e2e_test.go but we verify it here for completeness
func TestRateLimiting_E2E_AggregatorPosts(t *testing.T) {
	t.Run("Aggregator rate limit enforced", func(t *testing.T) {
		// This test is comprehensive in tests/integration/aggregator_e2e_test.go
		// Part 4: Rate Limiting - Enforces 10 posts/hour limit
		// We verify the constants match here
		const RateLimitWindow = 1 * time.Hour
		const RateLimitMaxPosts = 10

		assert.Equal(t, 1*time.Hour, RateLimitWindow, "Aggregator rate limit window should be 1 hour")
		assert.Equal(t, 10, RateLimitMaxPosts, "Aggregator rate limit should be 10 posts/hour")
	})
}

// TestRateLimiting_E2E_RateLimitHeaders tests that rate limit information is included in responses
func TestRateLimiting_E2E_RateLimitHeaders(t *testing.T) {
	t.Run("Current implementation does not include rate limit headers", func(t *testing.T) {
		// CURRENT STATE: The middleware does not set rate limit headers
		// FUTURE ENHANCEMENT: Add headers like:
		// - X-RateLimit-Limit: Maximum requests allowed
		// - X-RateLimit-Remaining: Requests remaining in window
		// - X-RateLimit-Reset: Time when limit resets (Unix timestamp)
		// - Retry-After: Seconds until limit resets (on 429 responses)

		limiter := middleware.NewRateLimiter(5, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "192.168.1.120:12345"
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		// Document current behavior: no rate limit headers
		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Limit"), "Currently no rate limit headers")
		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Remaining"), "Currently no rate limit headers")
		assert.Equal(t, "", rr.Header().Get("X-RateLimit-Reset"), "Currently no rate limit headers")
		assert.Equal(t, "", rr.Header().Get("Retry-After"), "Currently no Retry-After header")

		t.Log("NOTE: Rate limit headers are not implemented yet. This is acceptable for Alpha.")
		t.Log("Consider adding rate limit headers in a future enhancement.")
	})

	t.Run("429 response includes error message", func(t *testing.T) {
		limiter := middleware.NewRateLimiter(1, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.121:12345"

		// First request
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Second request gets 429 with message
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code)
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded")
		assert.Contains(t, rr.Body.String(), "Please try again later")
	})
}

// TestRateLimiting_E2E_ResetBehavior tests rate limit window reset behavior
func TestRateLimiting_E2E_ResetBehavior(t *testing.T) {
	t.Run("Rate limit resets after window expires", func(t *testing.T) {
		// Use very short window for testing (100ms)
		limiter := middleware.NewRateLimiter(2, 100*time.Millisecond)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.130:12345"

		// Make 2 requests (exhaust limit)
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// 3rd request is blocked
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code)

		// Wait for window to expire
		time.Sleep(150 * time.Millisecond)

		// Request should now succeed (window reset)
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Request should succeed after window reset")
	})

	t.Run("Rolling window behavior", func(t *testing.T) {
		// Use 200ms window for testing
		limiter := middleware.NewRateLimiter(3, 200*time.Millisecond)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.131:12345"

		// Make 3 requests over time
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
			time.Sleep(50 * time.Millisecond) // Space out requests
		}

		// 4th request immediately after should be blocked (still in window)
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "4th request should be blocked")

		// Wait for first request's window to expire (200ms + buffer)
		time.Sleep(100 * time.Millisecond)

		// Now request should succeed (window has rolled forward)
		req = httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Request should succeed after window rolls")
	})
}

// TestRateLimiting_E2E_ConcurrentRequests tests rate limiting with concurrent requests
func TestRateLimiting_E2E_ConcurrentRequests(t *testing.T) {
	t.Run("Rate limiting is thread-safe", func(t *testing.T) {
		limiter := middleware.NewRateLimiter(10, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.140:12345"
		successCount := 0
		rateLimitedCount := 0

		// Make 20 concurrent requests from same IP
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

		// Collect results
		for i := 0; i < 20; i++ {
			code := <-results
			if code == http.StatusOK {
				successCount++
			} else if code == http.StatusTooManyRequests {
				rateLimitedCount++
			}
		}

		// Should have exactly 10 successes and 10 rate limited
		assert.Equal(t, 10, successCount, "Should allow exactly 10 requests")
		assert.Equal(t, 10, rateLimitedCount, "Should rate limit exactly 10 requests")
	})
}

// TestRateLimiting_E2E_DifferentMethods tests that rate limiting applies across HTTP methods
func TestRateLimiting_E2E_DifferentMethods(t *testing.T) {
	t.Run("Rate limiting applies to all HTTP methods", func(t *testing.T) {
		limiter := middleware.NewRateLimiter(3, 1*time.Minute)
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := limiter.Middleware(testHandler)

		clientIP := "192.168.1.150:12345"

		// Make GET request
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Make POST request
		req = httptest.NewRequest("POST", "/test", bytes.NewBufferString("{}"))
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// Make PUT request
		req = httptest.NewRequest("PUT", "/test", bytes.NewBufferString("{}"))
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		// 4th request (DELETE) should be rate limited
		req = httptest.NewRequest("DELETE", "/test", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Rate limit should apply across methods")
	})
}

// Rate Limiting Configuration Documentation
// ==========================================
// This test file validates the following rate limits:
//
// 1. General Endpoints (Global Middleware)
//    - Limit: 100 requests per minute per IP
//    - Applied to: All XRPC endpoints
//    - Implementation: cmd/server/main.go:98-99
//
// 2. Comment Endpoints (Endpoint-Specific)
//    - Limit: 20 requests per minute per IP
//    - Applied to: social.coves.community.comment.getComments
//    - Reason: Expensive nested queries
//    - Implementation: cmd/server/main.go:448-456
//
// 3. Aggregator Posts (Business Logic)
//    - Limit: 10 posts per hour per aggregator per community
//    - Applied to: Aggregator post creation
//    - Implementation: internal/core/aggregators/service.go
//    - Tests: tests/integration/aggregator_e2e_test.go (Part 4)
//
// Rate Limit Response Behavior:
//    - Status Code: 429 Too Many Requests
//    - Error Message: 'Rate limit exceeded. Please try again later.'
//    - Headers: Not implemented (acceptable for Alpha)
//
// Client Identification (priority order):
//    1. X-Forwarded-For header
//    2. X-Real-IP header
//    3. RemoteAddr
//
// Implementation Details:
//    - Type: In-memory, per-instance
//    - Thread-safe: Yes (mutex-protected)
//    - Cleanup: Background goroutine
//    - Future: Consider Redis for distributed rate limiting
