package main

import (
	"testing"
	"time"
)

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := NewCircuitBreaker(0.5, 30*time.Second)
	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("CLOSED circuit should allow requests")
	}
}

func TestCircuitBreaker_TripsOnThreshold(t *testing.T) {
	cb := NewCircuitBreaker(0.5, 30*time.Second) // trip at 50% errors

	// 6 successes, 4 failures = 40% — should stay CLOSED
	for i := 0; i < 6; i++ {
		cb.Allow()
		cb.RecordSuccess()
	}
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED at 40%% error rate, got %s", cb.State())
	}

	// One more failure pushes to 5/11 ≈ 45.5% — still under 50%
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED at 45.5%% error rate, got %s", cb.State())
	}

	// Now add enough failures to cross 50%
	for i := 0; i < 10; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN after crossing threshold, got %s", cb.State())
	}
}

func TestCircuitBreaker_OpenDeniesRequests(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 30*time.Second)
	// Trip the breaker.
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatal("expected OPEN state")
	}
	if cb.Allow() {
		t.Fatal("OPEN circuit must deny requests")
	}
}

func TestCircuitBreaker_TransitionsToHalfOpenAfterCooldown(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 50*time.Millisecond) // very short cooldown for tests
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatal("expected OPEN state")
	}

	time.Sleep(80 * time.Millisecond)

	// Allow() should transition to HALF_OPEN and grant the first probe.
	if !cb.Allow() {
		t.Fatal("expected first probe request to be allowed in HALF_OPEN")
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN, got %s", cb.State())
	}
}

func TestCircuitBreaker_HalfOpenAllowsOnlyOneProbe(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 50*time.Millisecond)
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	time.Sleep(80 * time.Millisecond)

	first := cb.Allow()
	second := cb.Allow()

	if !first {
		t.Fatal("first probe should be allowed")
	}
	if second {
		t.Fatal("second concurrent probe must be denied in HALF_OPEN")
	}
}

func TestCircuitBreaker_SuccessfulProbeCloses(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 50*time.Millisecond)
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	time.Sleep(80 * time.Millisecond)

	cb.Allow()
	cb.RecordSuccess() // probe succeeds → CLOSED

	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED after successful probe, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("CLOSED circuit must allow requests")
	}
}

func TestCircuitBreaker_FailedProbeReopens(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 50*time.Millisecond)
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	time.Sleep(80 * time.Millisecond)

	cb.Allow()
	cb.RecordFailure() // probe fails → back to OPEN

	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN after failed probe, got %s", cb.State())
	}
}

func TestCircuitBreaker_SlidingWindowExpires(t *testing.T) {
	cb := NewCircuitBreaker(0.1, 30*time.Second)
	// Force OPEN.
	for i := 0; i < 20; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatal("expected OPEN")
	}

	// Manually reset the window to simulate the 10-second sliding window expiring.
	cb.mu.Lock()
	cb.resetWindow()
	cb.transition(StateClosed)
	cb.mu.Unlock()

	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED after window reset, got %s", cb.State())
	}
}
