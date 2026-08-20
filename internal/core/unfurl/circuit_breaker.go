package unfurl

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
)

// circuitState represents the state of a circuit breaker
type circuitState int

const (
	stateClosed   circuitState = iota // Normal operation
	stateOpen                         // Circuit is open (provider failing)
	stateHalfOpen                     // Testing if provider recovered
)

// circuitBreaker tracks failures per provider and stops trying failing providers
type circuitBreaker struct {
	failures         map[string]int
	lastFailure      map[string]time.Time
	state            map[string]circuitState
	lastStateLog     map[string]time.Time
	failureThreshold int
	openDuration     time.Duration
	mu               sync.RWMutex
}

// newCircuitBreaker creates a circuit breaker with default settings
func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		failureThreshold: 3,               // Open after 3 consecutive failures
		openDuration:     5 * time.Minute, // Keep open for 5 minutes
		failures:         make(map[string]int),
		lastFailure:      make(map[string]time.Time),
		state:            make(map[string]circuitState),
		lastStateLog:     make(map[string]time.Time),
	}
}

// canAttempt checks if we should attempt to call this provider
// Returns true if circuit is closed or half-open (ready to retry)
func (cb *circuitBreaker) canAttempt(provider string) (bool, error) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	state := cb.getState(provider)

	switch state {
	case stateClosed:
		return true, nil
	case stateOpen:
		// Check if we should transition to half-open
		lastFail := cb.lastFailure[provider]
		if time.Since(lastFail) > cb.openDuration {
			// Transition to half-open (allow one retry)
			cb.mu.RUnlock()
			cb.mu.Lock()
			cb.state[provider] = stateHalfOpen
			cb.logStateChange(provider, stateHalfOpen)
			cb.mu.Unlock()
			cb.mu.RLock()
			return true, nil
		}
		// Still in open period
		failCount := cb.failures[provider]
		nextRetry := lastFail.Add(cb.openDuration)
		return false, fmt.Errorf(
			"circuit breaker open for provider '%s' (failures: %d, next retry: %s)",
			provider,
			failCount,
			nextRetry.Format("15:04:05"),
		)
	case stateHalfOpen:
		return true, nil
	default:
		return true, nil
	}
}

// recordSuccess records a successful unfurl, resetting failure count
func (cb *circuitBreaker) recordSuccess(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	oldState := cb.getState(provider)

	// Reset failure tracking
	delete(cb.failures, provider)
	delete(cb.lastFailure, provider)
	cb.state[provider] = stateClosed

	// Log recovery if we were in a failure state
	if oldState != stateClosed {
		cb.logStateChange(provider, stateClosed)
	}
}

// recordFailure records a failed unfurl attempt.
//
// # A GUARD REFUSAL IS NOT A FAILURE, AND COUNTING IT IS A DENIAL OF SERVICE
//
// The SSRF guard refuses a private, loopback or link-local destination before a
// packet leaves the process, so a refusal carries no information about whether
// the provider is up — nobody spoke to it. Counting one is wrong twice over.
//
// The first cost is availability, and it is an attack. The breaker's bucket for
// the OpenGraph path is the constant string "opengraph": ONE bucket for the
// whole instance, not one per host. Three pasted `http://127.0.0.1/` links
// therefore disabled link previews for every user of this instance for
// openDuration, at the cost of three posts, repeatable on expiry. The guard did
// its job on each request and the aggregate was the outage — a denial of service
// delivered THROUGH the security control.
//
// The second is diagnostic: the log line below names a healthy provider as the
// failing party and quotes an address error as the evidence, which is exactly
// the wrong place to send an operator looking.
//
// THE CHECK LIVES HERE, not at the three call sites in UnfurlURL, because those
// three sit a few lines apart and each looks like the one above it. That is the
// shape the three-unguarded-clients defect in providers.go took; one check at
// the single point where a failure is COUNTED cannot be forgotten by the fourth
// path someone adds. errors.Is and not a comparison, because what arrives here
// has been through http.Client.Do's *url.Error and one fmt.Errorf of UnfurlURL's
// own.
func (cb *circuitBreaker) recordFailure(provider string, err error) {
	if errors.Is(err, covesoauth.ErrBlockedAddress) {
		log.Printf(
			"[UNFURL-CIRCUIT] Not counting a refused address against provider '%s': %v",
			provider,
			err,
		)
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Increment failure count
	cb.failures[provider]++
	cb.lastFailure[provider] = time.Now()

	failCount := cb.failures[provider]

	// Check if we should open the circuit
	if failCount >= cb.failureThreshold {
		oldState := cb.getState(provider)
		cb.state[provider] = stateOpen
		if oldState != stateOpen {
			log.Printf(
				"[UNFURL-CIRCUIT] Opening circuit for provider '%s' after %d consecutive failures. Last error: %v",
				provider,
				failCount,
				err,
			)
			cb.lastStateLog[provider] = time.Now()
		}
	} else {
		log.Printf(
			"[UNFURL-CIRCUIT] Failure %d/%d for provider '%s': %v",
			failCount,
			cb.failureThreshold,
			provider,
			err,
		)
	}
}

// getState returns the current state (must be called with lock held)
func (cb *circuitBreaker) getState(provider string) circuitState {
	if state, exists := cb.state[provider]; exists {
		return state
	}
	return stateClosed
}

// logStateChange logs state transitions (must be called with lock held)
// Debounced to avoid log spam (max once per minute per provider)
func (cb *circuitBreaker) logStateChange(provider string, newState circuitState) {
	lastLog, exists := cb.lastStateLog[provider]
	if exists && time.Since(lastLog) < time.Minute {
		return // Don't spam logs
	}

	var stateStr string
	switch newState {
	case stateClosed:
		stateStr = "CLOSED (recovered)"
	case stateOpen:
		stateStr = "OPEN (failing)"
	case stateHalfOpen:
		stateStr = "HALF-OPEN (testing)"
	}

	log.Printf("[UNFURL-CIRCUIT] Circuit for provider '%s' is now %s", provider, stateStr)
	cb.lastStateLog[provider] = time.Now()
}

// getStats returns current circuit breaker stats (for debugging/monitoring)
func (cb *circuitBreaker) getStats() map[string]interface{} {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	stats := make(map[string]interface{})

	// Collect all providers with any activity (state, failures, or both)
	providers := make(map[string]bool)
	for provider := range cb.state {
		providers[provider] = true
	}
	for provider := range cb.failures {
		providers[provider] = true
	}

	for provider := range providers {
		state := cb.getState(provider)
		var stateStr string
		switch state {
		case stateClosed:
			stateStr = "closed"
		case stateOpen:
			stateStr = "open"
		case stateHalfOpen:
			stateStr = "half-open"
		}

		stats[provider] = map[string]interface{}{
			"state":        stateStr,
			"failures":     cb.failures[provider],
			"last_failure": cb.lastFailure[provider],
		}
	}
	return stats
}
