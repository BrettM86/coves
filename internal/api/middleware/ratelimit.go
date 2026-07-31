package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a simple in-memory token-bucket-style limiter keyed by client IP.
// For multi-replica deploys, swap in a Redis-backed implementation.
type RateLimiter struct {
	clients map[string]*clientLimit
	stop    chan struct{}
	// now reads the wall clock. Production always gets time.Now; the seam
	// exists because a rate-limit window is a deadline, and a test that waits
	// out a real one is slow at best and flaky at worst. See
	// newRateLimiterWithClock.
	now      func() time.Time
	requests int
	window   time.Duration
	name     string // used in security-event logs to distinguish global vs per-route
	mu       sync.Mutex
	stopOnce sync.Once
}

type clientLimit struct {
	resetTime time.Time
	count     int
}

// NewRateLimiter creates a new rate limiter with an unnamed identity. Prefer
// NewNamedRateLimiter so 429 logs are attributable to a specific route.
func NewRateLimiter(requests int, window time.Duration) *RateLimiter {
	return NewNamedRateLimiter("default", requests, window)
}

// NewNamedRateLimiter creates a rate limiter that tags its 429 logs with name.
// Pass something route-distinct like "signupToken" or "global".
func NewNamedRateLimiter(name string, requests int, window time.Duration) *RateLimiter {
	return newRateLimiterWithClock(name, requests, window, func() time.Time {
		return time.Now().UTC()
	})
}

// newRateLimiterWithClock is NewNamedRateLimiter with the clock supplied.
// Test-only: the exported constructors are the production entry points and
// always pass time.Now.
//
// now must be safe for concurrent use — allow() calls it on the request path
// and the cleanup goroutine calls it on its own schedule.
func newRateLimiterWithClock(name string, requests int, window time.Duration, now func() time.Time) *RateLimiter {
	rl := &RateLimiter{
		clients:  make(map[string]*clientLimit),
		stop:     make(chan struct{}),
		now:      now,
		requests: requests,
		window:   window,
		name:     name,
	}
	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine. Safe to call multiple times.
// Production limiters live for process lifetime and never need Stop; tests must
// call it (typically via t.Cleanup) to avoid leaking goroutines.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() {
		close(rl.stop)
	})
}

// Middleware returns a rate limiting middleware.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID := GetClientIP(r)

		if !rl.allow(clientID) {
			// Security event: log every 429 so abuse is observable.
			// Includes path so an operator can distinguish bot probes from a hot user.
			slog.Warn("rate limit exceeded",
				slog.String("limiter", rl.name),
				slog.String("client_ip", clientID),
				slog.String("path", r.URL.Path),
				slog.Int("limit", rl.requests),
				slog.Duration("window", rl.window),
			)
			http.Error(w, "Rate limit exceeded. Please try again later.", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// allow returns true if the client is under the request budget for the current
// window, and false otherwise. Exported on the limiter for unit testing.
func (rl *RateLimiter) allow(clientID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	client, exists := rl.clients[clientID]
	if !exists {
		rl.clients[clientID] = &clientLimit{
			count:     1,
			resetTime: now.Add(rl.window),
		}
		return true
	}

	if now.After(client.resetTime) {
		client.count = 1
		client.resetTime = now.Add(rl.window)
		return true
	}

	if client.count < rl.requests {
		client.count++
		return true
	}

	return false
}

// clientCount returns the number of tracked client entries. Test-only helper:
// lets us assert that the cleanup goroutine actually evicts expired buckets.
func (rl *RateLimiter) clientCount() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.clients)
}

// cleanup removes expired client entries periodically. Exits when Stop is called.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := rl.now()
			for clientID, client := range rl.clients {
				if now.After(client.resetTime) {
					delete(rl.clients, clientID)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// GetClientIP extracts the client IP to use as a rate-limit key.
//
// SECURITY NOTE: header selection matters here — a naive implementation lets an
// attacker spawn a fresh rate-limit bucket per request by rotating spoofed
// X-Forwarded-For values. Caddy *appends* to XFF rather than replacing, so any
// client-supplied entries survive the proxy hop on the left side of the list.
//
// Strategy:
//  1. Prefer X-Real-IP — Caddy sets this to the direct upstream client (the most
//     trustworthy hop we see) and a client can't override it.
//  2. Fall back to the *rightmost* X-Forwarded-For entry. The rightmost hop is
//     the one our proxy itself appended, so it's the least spoofable.
//  3. Final fallback: r.RemoteAddr with the ephemeral port stripped — otherwise
//     each new TCP connection opens a new bucket.
//
// Behind our Caddy reverse proxy these headers are always set, so direct
// inspection of RemoteAddr only fires for misconfigurations or local tests.
func GetClientIP(r *http.Request) string {
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		rightmost := strings.TrimSpace(parts[len(parts)-1])
		if rightmost != "" {
			return rightmost
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr without a port is unusual but possible (e.g. unix sockets,
		// some test setups). Use it verbatim rather than dropping the request.
		return r.RemoteAddr
	}
	return host
}
