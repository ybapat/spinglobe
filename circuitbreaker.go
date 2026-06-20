package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// State represents the circuit breaker FSM state.
type State int32

const (
	StateClosed   State = iota // normal operation
	StateOpen                  // fast-fail, no requests pass
	StateHalfOpen              // single probe request allowed
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

const (
	windowBuckets  = 10            // number of 1-second buckets in the sliding window
	bucketDuration = time.Second
)

// windowBucket holds aggregated counts for one second-long slot.
type windowBucket struct {
	total    int64
	failures int64
	ts       int64 // unix second this bucket belongs to
}

// CircuitBreaker is a per-backend FSM with a sliding-window error-rate tracker.
// All state transitions are guarded by mu; the current State is also mirrored in
// stateAtomic so the hot-path can read it without acquiring the lock.
type CircuitBreaker struct {
	mu           sync.Mutex
	stateAtomic  atomic.Int32 // stores State, read lock-free
	openUntil    time.Time    // when OPEN transitions to HALF-OPEN
	cooldown     time.Duration
	threshold    float64 // error rate 0.0–1.0 that trips the breaker
	probeInFlight atomic.Int32 // 1 when a half-open probe is in progress

	buckets [windowBuckets]windowBucket
}

// NewCircuitBreaker creates a breaker starting in the Closed state.
func NewCircuitBreaker(threshold float64, cooldown time.Duration) *CircuitBreaker {
	cb := &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
	}
	cb.stateAtomic.Store(int32(StateClosed))
	return cb
}

// State returns the current FSM state without acquiring the mutex.
func (cb *CircuitBreaker) State() State {
	return State(cb.stateAtomic.Load())
}

// Allow reports whether the circuit breaker permits a request to pass through.
// For HALF-OPEN it grants access to exactly one concurrent probe.
func (cb *CircuitBreaker) Allow() bool {
	switch cb.State() {
	case StateClosed:
		return true
	case StateOpen:
		cb.mu.Lock()
		if time.Now().After(cb.openUntil) {
			cb.transition(StateHalfOpen)
		}
		cb.mu.Unlock()
		if cb.State() != StateHalfOpen {
			return false
		}
		fallthrough
	case StateHalfOpen:
		// Allow only one probe at a time.
		return cb.probeInFlight.CompareAndSwap(0, 1)
	}
	return false
}

// RecordSuccess records a successful response and may close an open circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.addToWindow(false)

	if cb.State() == StateHalfOpen {
		cb.probeInFlight.Store(0)
		cb.transition(StateClosed)
		cb.resetWindow()
	}
}

// RecordFailure records a failed response and may open the circuit.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.addToWindow(true)

	switch cb.State() {
	case StateClosed:
		if cb.errorRate() >= cb.threshold {
			cb.transition(StateOpen)
			cb.openUntil = time.Now().Add(cb.cooldown)
		}
	case StateHalfOpen:
		cb.probeInFlight.Store(0)
		cb.transition(StateOpen)
		cb.openUntil = time.Now().Add(cb.cooldown)
	}
}

// ProbeFinished must be called when a half-open probe completes (success or failure
// already recorded) to release the in-flight probe slot if it was never cleared.
func (cb *CircuitBreaker) ProbeFinished() {
	cb.probeInFlight.CompareAndSwap(1, 0)
}

// --- internal helpers (caller must hold mu) ---

func (cb *CircuitBreaker) transition(s State) {
	cb.stateAtomic.Store(int32(s))
}

func (cb *CircuitBreaker) addToWindow(failed bool) {
	now := time.Now().Unix()
	idx := int(now % windowBuckets)
	b := &cb.buckets[idx]
	if b.ts != now {
		// New second — reset this bucket.
		b.total = 0
		b.failures = 0
		b.ts = now
	}
	b.total++
	if failed {
		b.failures++
	}
}

func (cb *CircuitBreaker) errorRate() float64 {
	cutoff := time.Now().Unix() - int64(windowBuckets)
	var total, failures int64
	for i := range cb.buckets {
		b := &cb.buckets[i]
		if b.ts > cutoff && b.total > 0 {
			total += b.total
			failures += b.failures
		}
	}
	if total == 0 {
		return 0
	}
	return float64(failures) / float64(total)
}

func (cb *CircuitBreaker) resetWindow() {
	cb.buckets = [windowBuckets]windowBucket{}
}
