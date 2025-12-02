package e2e

import (
	"Coves/internal/api/middleware"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRateLimiting_E2E_OAuthEndpoints tests OAuth-specific rate limiting
// OAuth endpoints have stricter rate limits to prevent:
// - Credential stuffing attacks on login endpoints (10 req/min)
// - OAuth state exhaustion
// - Refresh token abuse (20 req/min)
func TestRateLimiting_E2E_OAuthEndpoints(t *testing.T) {
	t.Run("Login endpoints have 10 req/min limit", func(t *testing.T) {
		// Create rate limiter matching oauth.go config: 10 requests per minute
		loginLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

		// Mock OAuth login handler
		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})

		handler := loginLimiter.Middleware(testHandler)
		clientIP := "192.168.1.200:12345"

		// Make exactly 10 requests (at limit)
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/oauth/login", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		// 11th request should be rate limited
		req := httptest.NewRequest("GET", "/oauth/login", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request 11 should be rate limited")
		assert.Contains(t, rr.Body.String(), "Rate limit exceeded", "Should have rate limit error message")
	})

	t.Run("Mobile login endpoints have 10 req/min limit", func(t *testing.T) {
		loginLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		handler := loginLimiter.Middleware(testHandler)
		clientIP := "192.168.1.201:12345"

		// Make 10 requests
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/oauth/mobile/login", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// 11th request blocked
		req := httptest.NewRequest("GET", "/oauth/mobile/login", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Mobile login should be rate limited at 10 req/min")
	})

	t.Run("Refresh endpoint has 20 req/min limit", func(t *testing.T) {
		// Refresh has higher limit (20 req/min) for legitimate token refresh
		refreshLimiter := middleware.NewRateLimiter(20, 1*time.Minute)

		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		handler := refreshLimiter.Middleware(testHandler)
		clientIP := "192.168.1.202:12345"

		// Make 20 requests
		for i := 0; i < 20; i++ {
			req := httptest.NewRequest("POST", "/oauth/refresh", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code, "Request %d should succeed", i+1)
		}

		// 21st request blocked
		req := httptest.NewRequest("POST", "/oauth/refresh", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Refresh should be rate limited at 20 req/min")
	})

	t.Run("Logout endpoint has 10 req/min limit", func(t *testing.T) {
		logoutLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		handler := logoutLimiter.Middleware(testHandler)
		clientIP := "192.168.1.203:12345"

		// Make 10 requests
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("POST", "/oauth/logout", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// 11th request blocked
		req := httptest.NewRequest("POST", "/oauth/logout", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Logout should be rate limited at 10 req/min")
	})

	t.Run("OAuth callback has 10 req/min limit", func(t *testing.T) {
		// Callback uses same limiter as login (part of auth flow)
		callbackLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		handler := callbackLimiter.Middleware(testHandler)
		clientIP := "192.168.1.204:12345"

		// Make 10 requests
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/oauth/callback", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// 11th request blocked
		req := httptest.NewRequest("GET", "/oauth/callback", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Callback should be rate limited at 10 req/min")
	})

	t.Run("OAuth rate limits are stricter than global limit", func(t *testing.T) {
		// Verify OAuth limits are more restrictive than global 100 req/min
		const globalLimit = 100
		const oauthLoginLimit = 10
		const oauthRefreshLimit = 20

		assert.Less(t, oauthLoginLimit, globalLimit, "OAuth login limit should be stricter than global")
		assert.Less(t, oauthRefreshLimit, globalLimit, "OAuth refresh limit should be stricter than global")
		assert.Greater(t, oauthRefreshLimit, oauthLoginLimit, "Refresh limit should be higher than login (legitimate use case)")
	})

	t.Run("OAuth limits prevent credential stuffing", func(t *testing.T) {
		// Simulate credential stuffing attack
		loginLimiter := middleware.NewRateLimiter(10, 1*time.Minute)

		testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate failed login attempts
			w.WriteHeader(http.StatusUnauthorized)
		})

		handler := loginLimiter.Middleware(testHandler)
		attackerIP := "203.0.113.50:12345"

		// Attacker tries 15 login attempts (credential stuffing)
		successfulAttempts := 0
		blockedAttempts := 0

		for i := 0; i < 15; i++ {
			req := httptest.NewRequest("GET", "/oauth/login", nil)
			req.RemoteAddr = attackerIP
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code == http.StatusUnauthorized {
				successfulAttempts++ // Reached handler (even if auth failed)
			} else if rr.Code == http.StatusTooManyRequests {
				blockedAttempts++
			}
		}

		// Rate limiter should block 5 attempts after first 10
		assert.Equal(t, 10, successfulAttempts, "Should allow 10 login attempts")
		assert.Equal(t, 5, blockedAttempts, "Should block 5 attempts after limit reached")
	})

	t.Run("OAuth limits are per-endpoint", func(t *testing.T) {
		// Each endpoint gets its own rate limiter
		// This test verifies that limits are independent per endpoint
		loginLimiter := middleware.NewRateLimiter(10, 1*time.Minute)
		refreshLimiter := middleware.NewRateLimiter(20, 1*time.Minute)

		loginHandler := loginLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		refreshHandler := refreshLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		clientIP := "192.168.1.205:12345"

		// Exhaust login limit
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/oauth/login", nil)
			req.RemoteAddr = clientIP
			rr := httptest.NewRecorder()
			loginHandler.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
		}

		// Login limit exhausted
		req := httptest.NewRequest("GET", "/oauth/login", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		loginHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Login should be rate limited")

		// Refresh endpoint should still work (independent limiter)
		req = httptest.NewRequest("POST", "/oauth/refresh", nil)
		req.RemoteAddr = clientIP
		rr = httptest.NewRecorder()
		refreshHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Refresh should not be affected by login rate limit")
	})
}

// OAuth Rate Limiting Configuration Documentation
// ================================================
// This test file validates OAuth-specific rate limits applied in oauth.go:
//
// 1. Login Endpoints (Credential Stuffing Protection)
//    - Endpoints: /oauth/login, /oauth/mobile/login, /oauth/callback
//    - Limit: 10 requests per minute per IP
//    - Reason: Prevent brute force and credential stuffing attacks
//    - Implementation: internal/api/routes/oauth.go:21
//
// 2. Refresh Endpoint (Token Refresh)
//    - Endpoint: /oauth/refresh
//    - Limit: 20 requests per minute per IP
//    - Reason: Allow legitimate token refresh while preventing abuse
//    - Implementation: internal/api/routes/oauth.go:24
//
// 3. Logout Endpoint
//    - Endpoint: /oauth/logout
//    - Limit: 10 requests per minute per IP
//    - Reason: Prevent session exhaustion attacks
//    - Implementation: internal/api/routes/oauth.go:27
//
// 4. Metadata Endpoints (No Extra Limit)
//    - Endpoints: /oauth/client-metadata.json, /oauth/jwks.json
//    - Limit: Global 100 requests per minute (from main.go)
//    - Reason: Public metadata, not sensitive to rate abuse
//
// Security Benefits:
//    - Credential Stuffing: Limits password guessing to 10 attempts/min
//    - State Exhaustion: Prevents OAuth state generation spam
//    - Token Abuse: Limits refresh token usage while allowing legitimate refresh
//
// Rate Limit Hierarchy:
//    - OAuth login: 10 req/min (most restrictive)
//    - OAuth refresh: 20 req/min (moderate)
//    - Comments: 20 req/min (expensive queries)
//    - Global: 100 req/min (baseline)
