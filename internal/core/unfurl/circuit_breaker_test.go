package unfurl

import (
	"fmt"
	"testing"
	"time"
)

func TestCircuitBreaker_Basic(t *testing.T) {
	cb := newCircuitBreaker()

	provider := "test-provider"

	// Should start closed (allow attempts)
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Errorf("Expected circuit to be closed initially, but got error: %v", err)
	}

	// Record success
	cb.recordSuccess(provider)
	canAttempt, _ = cb.canAttempt(provider)
	if !canAttempt {
		t.Error("Expected circuit to remain closed after success")
	}
}

func TestCircuitBreaker_OpensAfterFailures(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "failing-provider"

	// Record failures up to threshold
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, fmt.Errorf("test error %d", i))
	}

	// Circuit should now be open
	canAttempt, err := cb.canAttempt(provider)
	if canAttempt {
		t.Error("Expected circuit to be open after threshold failures")
	}
	if err == nil {
		t.Error("Expected error when circuit is open")
	}
}

func TestCircuitBreaker_RecoveryAfterSuccess(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "recovery-provider"

	// Record some failures
	cb.recordFailure(provider, fmt.Errorf("error 1"))
	cb.recordFailure(provider, fmt.Errorf("error 2"))

	// Record success - should reset failure count
	cb.recordSuccess(provider)

	// Should be able to attempt again
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Errorf("Expected circuit to be closed after success, but got error: %v", err)
	}

	// Failure count should be reset
	if count := cb.failures[provider]; count != 0 {
		t.Errorf("Expected failure count to be reset to 0, got %d", count)
	}
}

func TestCircuitBreaker_HalfOpenTransition(t *testing.T) {
	cb := newCircuitBreaker()
	cb.openDuration = 100 * time.Millisecond // Short duration for testing
	provider := "half-open-provider"

	// Open the circuit
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure(provider, fmt.Errorf("error %d", i))
	}

	// Should be open
	canAttempt, _ := cb.canAttempt(provider)
	if canAttempt {
		t.Error("Expected circuit to be open")
	}

	// Wait for open duration
	time.Sleep(150 * time.Millisecond)

	// Should transition to half-open and allow one attempt
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Errorf("Expected circuit to transition to half-open after duration, but got error: %v", err)
	}

	// State should be half-open
	cb.mu.RLock()
	state := cb.state[provider]
	cb.mu.RUnlock()

	if state != stateHalfOpen {
		t.Errorf("Expected state to be half-open, got %v", state)
	}
}

func TestCircuitBreaker_MultipleProviders(t *testing.T) {
	cb := newCircuitBreaker()

	// Open circuit for provider A
	for i := 0; i < cb.failureThreshold; i++ {
		cb.recordFailure("providerA", fmt.Errorf("error"))
	}

	// Provider A should be blocked
	canAttemptA, _ := cb.canAttempt("providerA")
	if canAttemptA {
		t.Error("Expected providerA circuit to be open")
	}

	// Provider B should still be open (independent circuits)
	canAttemptB, err := cb.canAttempt("providerB")
	if !canAttemptB {
		t.Errorf("Expected providerB circuit to be closed, but got error: %v", err)
	}
}

func TestCircuitBreaker_GetStats(t *testing.T) {
	cb := newCircuitBreaker()

	// Record some activity
	cb.recordFailure("provider1", fmt.Errorf("error 1"))
	cb.recordFailure("provider1", fmt.Errorf("error 2"))

	stats := cb.getStats()

	// Should have stats for providers with failures
	if providerStats, ok := stats["provider1"]; !ok {
		t.Error("Expected stats for provider1")
	} else {
		// Check that failure count is tracked
		statsMap := providerStats.(map[string]interface{})
		if failures, ok := statsMap["failures"].(int); !ok || failures != 2 {
			t.Errorf("Expected 2 failures for provider1, got %v", statsMap["failures"])
		}
	}

	// Provider that succeeds is cleaned up from state
	cb.recordSuccess("provider2")
	_ = cb.getStats()
	// Provider2 should not be in stats (or have state "closed" with 0 failures)
}

func TestCircuitBreaker_FailureThresholdExact(t *testing.T) {
	cb := newCircuitBreaker()
	provider := "exact-threshold-provider"

	// Record failures just below threshold
	for i := 0; i < cb.failureThreshold-1; i++ {
		cb.recordFailure(provider, fmt.Errorf("error %d", i))
	}

	// Should still be closed
	canAttempt, err := cb.canAttempt(provider)
	if !canAttempt {
		t.Errorf("Expected circuit to be closed below threshold, but got error: %v", err)
	}

	// One more failure should open it
	cb.recordFailure(provider, fmt.Errorf("final error"))

	// Should now be open
	canAttempt, _ = cb.canAttempt(provider)
	if canAttempt {
		t.Error("Expected circuit to be open at threshold")
	}
}
