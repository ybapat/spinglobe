package main

import (
	"sync"
	"testing"
	"time"
)

// TestLocalBucket_AllowsUpToCapacity verifies the initial burst fills to capacity.
func TestLocalBucket_AllowsUpToCapacity(t *testing.T) {
	capacity := int64(5)
	rate := float64(5) // 5 tokens/sec
	b := &localBucket{
		tokens:     float64(capacity),
		lastRefill: time.Now(),
	}

	for i := 0; i < int(capacity); i++ {
		ok, remaining, _ := b.consume(capacity, rate)
		if !ok {
			t.Fatalf("request %d should be allowed, got denied (remaining=%d)", i+1, remaining)
		}
	}

	// Bucket should now be empty.
	ok, _, _ := b.consume(capacity, rate)
	if ok {
		t.Fatal("request beyond capacity should be denied")
	}
}

// TestLocalBucket_RefillsOverTime verifies mathematical replenishment.
func TestLocalBucket_RefillsOverTime(t *testing.T) {
	capacity := int64(10)
	rate := float64(10) // 10 tokens/sec → 1 token per 100ms

	b := &localBucket{
		tokens:     0,
		lastRefill: time.Now().Add(-200 * time.Millisecond), // 200ms ago → 2 tokens available
	}

	ok1, rem1, _ := b.consume(capacity, rate)
	ok2, rem2, _ := b.consume(capacity, rate)
	ok3, _, _ := b.consume(capacity, rate)

	if !ok1 {
		t.Fatal("first request after 200ms should be allowed")
	}
	if !ok2 {
		t.Fatalf("second request should be allowed (rem after first: %d)", rem1)
	}
	if ok3 {
		t.Fatalf("third request should be denied (rem after second: %d)", rem2)
	}
}

// TestLocalBucket_ConcurrentSafety hammers the bucket from multiple goroutines
// and ensures the allowed count never exceeds capacity.
func TestLocalBucket_ConcurrentSafety(t *testing.T) {
	capacity := int64(50)
	rate := float64(50)
	b := &localBucket{
		tokens:     float64(capacity),
		lastRefill: time.Now(),
	}

	const goroutines = 20
	const perGoroutine = 10

	var wg sync.WaitGroup
	allowedCh := make(chan int, goroutines*perGoroutine)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count := 0
			for j := 0; j < perGoroutine; j++ {
				ok, _, _ := b.consume(capacity, rate)
				if ok {
					count++
				}
			}
			allowedCh <- count
		}()
	}

	wg.Wait()
	close(allowedCh)

	total := 0
	for c := range allowedCh {
		total += c
	}

	if total > int(capacity) {
		t.Fatalf("allowed %d requests but capacity is %d — data race or logic error", total, capacity)
	}
}

// TestClientIdentifier_Priority verifies the key selection hierarchy.
func TestClientIdentifier_Priority(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() *fakeRequest
		wantPfx  string
	}{
		{
			name: "X-API-Key wins",
			setup: func() *fakeRequest {
				return &fakeRequest{apiKey: "mykey", fwd: "1.2.3.4", sub: "user1"}
			},
			wantPfx: "apikey:",
		},
		{
			name: "subject wins over IP",
			setup: func() *fakeRequest {
				return &fakeRequest{sub: "user1", fwd: "1.2.3.4"}
			},
			wantPfx: "sub:",
		},
		{
			name: "X-Forwarded-For used as fallback",
			setup: func() *fakeRequest {
				return &fakeRequest{fwd: "1.2.3.4,5.6.7.8"}
			},
			wantPfx: "ip:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := tc.setup()
			key := clientIdentifierFrom(fr.apiKey, fr.sub, fr.fwd)
			if len(key) < len(tc.wantPfx) || key[:len(tc.wantPfx)] != tc.wantPfx {
				t.Fatalf("expected key with prefix %q, got %q", tc.wantPfx, key)
			}
		})
	}
}

type fakeRequest struct {
	apiKey, sub, fwd string
}

// clientIdentifierFrom is a testable extraction helper — mirrors the logic in
// clientIdentifier() without requiring a real *http.Request.
func clientIdentifierFrom(apiKey, sub, fwd string) string {
	if apiKey != "" {
		return "apikey:" + apiKey
	}
	if sub != "" {
		return "sub:" + sub
	}
	if fwd != "" {
		for i, c := range fwd {
			if c == ',' {
				fwd = fwd[:i]
				break
			}
		}
		return "ip:" + fwd
	}
	return "ip:unknown"
}

// TestTierFor verifies tier mapping returns correct limits.
func TestTierFor(t *testing.T) {
	cfg := Config{
		TierFree:       TierLimits{Capacity: 10, RatePerSec: 10},
		TierPremium:    TierLimits{Capacity: 100, RatePerSec: 100},
		TierEnterprise: TierLimits{Capacity: 1000, RatePerSec: 1000},
	}

	tests := []struct {
		tier         string
		wantCapacity int64
	}{
		{"free", 10},
		{"Free", 10},
		{"premium", 100},
		{"PREMIUM", 100},
		{"enterprise", 1000},
		{"unknown", 10},  // defaults to free
		{"", 10},         // defaults to free
	}

	for _, tc := range tests {
		got := TierFor(tc.tier, cfg)
		if got.Capacity != tc.wantCapacity {
			t.Errorf("TierFor(%q) capacity = %d, want %d", tc.tier, got.Capacity, tc.wantCapacity)
		}
	}
}
