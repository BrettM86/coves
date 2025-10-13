package middleware

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter implements a simple in-memory rate limiter
// For production, consider using Redis or a distributed rate limiter
type RateLimiter struct {
	clients  map[string]*clientLimit
	requests int
	window   time.Duration
	mu       sync.Mutex
}

type clientLimit struct {
	resetTime time.Time
	count     int
}

// NewRateLimiter creates a new rate limiter
// requests: maximum number of requests allowed per window
// window: time window duration (e.g., 1 minute)
func NewRateLimiter(requests int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		clients:  make(map[string]*clientLimit),
		requests: requests,
		window:   window,
	}

	// Cleanup old entries every window duration
	go rl.cleanup()

	return rl
}

// Middleware returns a rate limiting middleware
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use IP address as client identifier
		// In production, consider using authenticated user ID if available
		clientID := getClientIP(r)

		if !rl.allow(clientID) {
			http.Error(w, "Rate limit exceeded. Please try again later.", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// allow checks if a client is allowed to make a request
func (rl *RateLimiter) allow(clientID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now().UTC()

	// Get or create client limit
	client, exists := rl.clients[clientID]
	if !exists {
		rl.clients[clientID] = &clientLimit{
			count:     1,
			resetTime: now.Add(rl.window),
		}
		return true
	}

	// Check if window has expired
	if now.After(client.resetTime) {
		client.count = 1
		client.resetTime = now.Add(rl.window)
		return true
	}

	// Check if under limit
	if client.count < rl.requests {
		client.count++
		return true
	}

	// Rate limit exceeded
	return false
}

// cleanup removes expired client entries periodically
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		now := time.Now().UTC()
		for clientID, client := range rl.clients {
			if now.After(client.resetTime) {
				delete(rl.clients, clientID)
			}
		}
		rl.mu.Unlock()
	}
}

// getClientIP extracts the client IP from the request
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (if behind proxy)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		return forwarded
	}

	// Check X-Real-IP header
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to RemoteAddr
	return r.RemoteAddr
}
