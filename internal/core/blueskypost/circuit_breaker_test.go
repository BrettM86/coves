package blueskypost

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := newCircuitBreaker()

	// Circuit should start in closed state
	canAttempt, err := cb.canAttempt("test-provider")
	if !canAttempt {
		t.Error("Circuit breaker should start in closed state")
	}
	if err != nil {
		t.Errorf("canAttempt() should not return error for closed circuit, got: %v", err)
	}
}

func TestCircuitBreaker_OpensAfterThresholdFailures(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "test-provider"
	testErr := errors.New("test error")

	// Record failures up to threshold (default is 3)
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Circuit should now be open
	canAttempt, err := cb.canAttempt(provider)
	if canAttempt {
		t.Error("Circuit breaker should be open after threshold failures")
	}
	if err == nil {
		t.Error("canAttempt() should return error when circuit is open")
	}
}

func TestCircuitBreaker_StaysClosedBelowThreshold(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "test-provider"
	testErr := errors.New("test error")

	// Record failures below threshold
	for i := 0; i < cb.failureThreshold-1; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Circuit should still be closed
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Error("Circuit breaker should remain closed below threshold")
	}
	if err != nil {
		t.Errorf("canAttempt() should not return error below threshold, got: %v", err)
	}
}

func TestCircuitBreaker_TransitionsToHalfOpenAfterTimeout(t *testing.T) {
	cb := newCircuitBreaker()
	// Set a very short open duration for testing
	cb.openDuration = 10 * time.Millisecond
	provider := "test-provider"
	testErr := errors.New("test error")

	// Open the circuit
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Verify circuit is open
	canAttempt, err := cb.canAttempt(provider)
	if canAttempt || err == nil {
		t.Fatal("Circuit should be open after threshold failures")
	}

	// Wait for the open duration to pass
	time.Sleep(cb.openDuration + 5*time.Millisecond)

	// Circuit should transition to half-open and allow attempt
	canAttempt, err = cb.canAttempt(provider)
	if !canAttempt {
		t.Error("Circuit breaker should transition to half-open after timeout")
	}
	if err != nil {
		t.Errorf("canAttempt() should not return error in half-open state, got: %v", err)
	}
}

func TestCircuitBreaker_ClosesOnSuccessAfterHalfOpen(t *testing.T) {
	cb := newCircuitBreaker()
	cb.openDuration = 10 * time.Millisecond
	provider := "test-provider"
	testErr := errors.New("test error")

	// Open the circuit
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Wait for half-open
	time.Sleep(cb.openDuration + 5*time.Millisecond)

	// Verify we can attempt
	canAttempt, _ := cb.canAttempt(provider)
	if !canAttempt {
		t.Fatal("Circuit should be half-open")
	}

	// Record a success
	cb.recordSuccess(provider)

	// Circuit should now be closed
	cb.mu.RLock()
	state := cb.getState(provider)
	cb.mu.RUnlock()

	if state != stateClosed {
		t.Errorf("Circuit should be closed after success in half-open state, got state: %v", state)
	}

	// Should allow attempts without error
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Error("Circuit should be closed and allow attempts")
	}
	if err != nil {
		t.Errorf("canAttempt() should not return error when closed, got: %v", err)
	}
}

func TestCircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "test-provider"
	testErr := errors.New("test error")

	// Record some failures (but below threshold)
	for i := 0; i < cb.failureThreshold-1; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Verify failure count
	cb.mu.RLock()
	failCount := cb.failures[provider]
	cb.mu.RUnlock()
	if failCount != cb.failureThreshold-1 {
		t.Errorf("Expected %d failures, got %d", cb.failureThreshold-1, failCount)
	}

	// Record a success
	cb.recordSuccess(provider)

	// Failure count should be reset
	cb.mu.RLock()
	failCount = cb.failures[provider]
	cb.mu.RUnlock()
	if failCount != 0 {
		t.Errorf("Expected 0 failures after success, got %d", failCount)
	}
}

func TestCircuitBreaker_IndependentProviders(t *testing.T) {
	cb := newCircuitBreaker()
	provider1 := "provider-1"
	provider2 := "provider-2"
	testErr := errors.New("test error")

	// Open circuit for provider1
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider1, testErr)
	}

	// Provider1 should be open
	canAttempt1, err1 := cb.canAttempt(provider1)
	if canAttempt1 || err1 == nil {
		t.Error("Provider1 circuit should be open")
	}

	// Provider2 should still be closed
	canAttempt2, err2 := cb.canAttempt(provider2)
	if !canAttempt2 {
		t.Error("Provider2 circuit should be closed")
	}
	if err2 != nil {
		t.Errorf("Provider2 canAttempt() should not return error, got: %v", err2)
	}
}

func TestCircuitBreaker_ConcurrentAccess(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "test-provider"
	testErr := errors.New("test error")

	// Number of concurrent goroutines
	numGoroutines := 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Concurrently record failures and check state
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			// Mix of operations
			if idx%3 == 0 {
				cb.recordFailure(provider, testErr)
			} else if idx%3 == 1 {
				cb.recordSuccess(provider)
			} else {
				_, _ = cb.canAttempt(provider)
			}
		}(i)
	}

	wg.Wait()

	// No panic or race conditions should occur
	// Final state check - just ensure we can call canAttempt
	_, _ = cb.canAttempt(provider)
}

func TestCircuitBreaker_MultipleProvidersThreadSafety(t *testing.T) {
	cb := newCircuitBreaker()
	testErr := errors.New("test error")
	numProviders := 10
	numOpsPerProvider := 100

	var wg sync.WaitGroup
	wg.Add(numProviders)

	// Concurrent operations on different providers
	for i := 0; i < numProviders; i++ {
		go func(providerID int) {
			defer wg.Done()
			provider := "provider-" + string(rune('0'+providerID))

			for j := 0; j < numOpsPerProvider; j++ {
				switch j % 4 {
				case 0:
					cb.recordFailure(provider, testErr)
				case 1:
					cb.recordSuccess(provider)
				case 2:
					_, _ = cb.canAttempt(provider)
				case 3:
					// Read state
					cb.mu.RLock()
					_ = cb.getState(provider)
					cb.mu.RUnlock()
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify all providers are accessible without panic
	for i := 0; i < numProviders; i++ {
		provider := "provider-" + string(rune('0'+i))
		_, _ = cb.canAttempt(provider)
	}
}

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	cb := newCircuitBreaker()
	cb.openDuration = 10 * time.Millisecond
	provider := "test-provider"
	testErr := errors.New("test error")

	// Initial state: closed
	cb.mu.RLock()
	if cb.getState(provider) != stateClosed {
		t.Error("Initial state should be closed")
	}
	cb.mu.RUnlock()

	// Transition to open
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	cb.mu.RLock()
	if cb.getState(provider) != stateOpen {
		t.Error("State should be open after threshold failures")
	}
	cb.mu.RUnlock()

	// Wait for half-open transition
	time.Sleep(cb.openDuration + 5*time.Millisecond)
	_, _ = cb.canAttempt(provider) // Trigger state check

	cb.mu.RLock()
	state := cb.getState(provider)
	cb.mu.RUnlock()
	if state != stateHalfOpen {
		t.Errorf("State should be half-open after timeout, got: %v", state)
	}

	// Transition back to closed
	cb.recordSuccess(provider)

	cb.mu.RLock()
	if cb.getState(provider) != stateClosed {
		t.Error("State should be closed after success in half-open")
	}
	cb.mu.RUnlock()
}

func TestCircuitBreaker_ErrorMessage(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "test-provider"
	testErr := errors.New("test error")

	// Open the circuit
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Check error message contains useful information
	_, err := cb.canAttempt(provider)
	if err == nil {
		t.Fatal("Expected error when circuit is open")
	}

	errMsg := err.Error()
	if !contains(errMsg, "circuit breaker open") {
		t.Errorf("Error message should mention circuit breaker, got: %s", errMsg)
	}
	if !contains(errMsg, provider) {
		t.Errorf("Error message should contain provider name, got: %s", errMsg)
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb := newCircuitBreaker()
	cb.openDuration = 10 * time.Millisecond
	provider := "test-provider"
	testErr := errors.New("test error")

	// Open the circuit
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, testErr)
	}

	// Wait for half-open
	time.Sleep(cb.openDuration + 5*time.Millisecond)
	_, _ = cb.canAttempt(provider)

	// Record another failure in half-open state
	cb.recordFailure(provider, testErr)

	// Circuit should be open again (failure count incremented)
	cb.mu.RLock()
	failCount := cb.failures[provider]
	cb.mu.RUnlock()

	if failCount < cb.failureThreshold {
		t.Errorf("Expected failure count >= %d after half-open failure, got: %d", cb.failureThreshold, failCount)
	}
}

func TestCircuitBreaker_CustomThresholdAndDuration(t *testing.T) {
	cb := &circuitBreaker{
		failureThreshold: 5,
		openDuration:     20 * time.Millisecond,
		failures:         make(map[string]int),
		lastFailure:      make(map[string]time.Time),
		state:            make(map[string]circuitState),
		lastStateLog:     make(map[string]time.Time),
	}

	provider := "test-provider"
	testErr := errors.New("test error")

	// Should not open until 5 failures
	for i := 0; i < 4; i++ {
		cb.recordFailure(provider, testErr)
	}

	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt || err != nil {
		t.Error("Circuit should remain closed before threshold")
	}

	// 5th failure should open it
	cb.recordFailure(provider, testErr)

	canAttempt, err = cb.canAttempt(provider)
	if canAttempt || err == nil {
		t.Error("Circuit should be open after 5 failures")
	}

	// Should not transition to half-open before 20ms
	time.Sleep(10 * time.Millisecond)
	canAttempt, _ = cb.canAttempt(provider)
	if canAttempt {
		t.Error("Circuit should still be open before timeout")
	}

	// Should transition after 20ms
	time.Sleep(15 * time.Millisecond)
	canAttempt, _ = cb.canAttempt(provider)
	if !canAttempt {
		t.Error("Circuit should be half-open after timeout")
	}
}
