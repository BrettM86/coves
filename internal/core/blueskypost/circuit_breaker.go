package blueskypost

import (
	"fmt"
	"log"
	"sync"
	"time"
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
	// First check under read lock if we need to transition
	cb.mu.RLock()
	state := cb.getState(provider)
	lastFail := cb.lastFailure[provider]
	needsTransition := state == stateOpen && time.Since(lastFail) > cb.openDuration
	cb.mu.RUnlock()

	// If we need to transition, acquire write lock and re-check
	if needsTransition {
		cb.mu.Lock()
		// Re-check state in case another goroutine already transitioned
		state = cb.getState(provider)
		lastFail = cb.lastFailure[provider]
		if state == stateOpen && time.Since(lastFail) > cb.openDuration {
			cb.state[provider] = stateHalfOpen
			cb.logStateChange(provider, stateHalfOpen)
		}
		state = cb.state[provider]
		cb.mu.Unlock()
		// Return based on new state
		if state == stateHalfOpen {
			return true, nil
		}
	}

	// Now check state under read lock
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	state = cb.getState(provider)

	switch state {
	case stateClosed:
		return true, nil
	case stateOpen:
		// Still in open period
		failCount := cb.failures[provider]
		nextRetry := cb.lastFailure[provider].Add(cb.openDuration)
		return false, fmt.Errorf(
			"%w for provider '%s' (failures: %d, next retry: %s)",
			ErrCircuitOpen,
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

// recordSuccess records a successful fetch, resetting failure count
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

// recordFailure records a failed fetch attempt
func (cb *circuitBreaker) recordFailure(provider string, err error) {
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
				"[BLUESKY-CIRCUIT] Opening circuit for provider '%s' after %d consecutive failures. Last error: %v",
				provider,
				failCount,
				err,
			)
			cb.lastStateLog[provider] = time.Now()
		}
	} else {
		log.Printf(
			"[BLUESKY-CIRCUIT] Failure %d/%d for provider '%s': %v",
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

	log.Printf("[BLUESKY-CIRCUIT] Circuit for provider '%s' is now %s", provider, stateStr)
	cb.lastStateLog[provider] = time.Now()
}
