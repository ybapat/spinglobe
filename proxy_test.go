package main

import (
	"net/url"
	"testing"
	"time"
)

func makeEntry(rawURL string) *BackendEntry {
	u, _ := url.Parse(rawURL)
	return &BackendEntry{
		URL: u,
		CB:  NewCircuitBreaker(0.5, 30*time.Second),
	}
}

// TestRoutingTable_LongestPrefixMatch ensures deeper prefixes take priority.
func TestRoutingTable_LongestPrefixMatch(t *testing.T) {
	table := NewRoutingTable()
	short := []*BackendEntry{makeEntry("http://short.svc")}
	long := []*BackendEntry{makeEntry("http://long.svc")}

	table.Register("/api/", short)
	table.Register("/api/v2/", long)

	tests := []struct {
		path    string
		wantURL string
	}{
		{"/api/v2/users", "http://long.svc"},
		{"/api/v1/orders", "http://short.svc"},
		{"/api/", "http://short.svc"},
	}

	for _, tc := range tests {
		pool := table.Lookup(tc.path)
		if pool == nil {
			t.Fatalf("Lookup(%q) returned nil", tc.path)
		}
		got := pool.entries[0].URL.String()
		if got != tc.wantURL {
			t.Errorf("Lookup(%q) → %q, want %q", tc.path, got, tc.wantURL)
		}
	}
}

// TestRoutingTable_NoMatch returns nil for unregistered paths.
func TestRoutingTable_NoMatch(t *testing.T) {
	table := NewRoutingTable()
	table.Register("/api/", []*BackendEntry{makeEntry("http://a.svc")})

	if pool := table.Lookup("/health"); pool != nil {
		t.Fatalf("expected nil for /health, got %v", pool)
	}
}

// TestRoutingTable_Register_Overwrites confirms re-registering a prefix replaces backends.
func TestRoutingTable_Register_Overwrites(t *testing.T) {
	table := NewRoutingTable()
	table.Register("/api/", []*BackendEntry{makeEntry("http://old.svc")})
	table.Register("/api/", []*BackendEntry{makeEntry("http://new.svc")})

	pool := table.Lookup("/api/anything")
	if pool == nil || len(pool.entries) != 1 {
		t.Fatal("expected exactly one backend after overwrite")
	}
	if pool.entries[0].URL.String() != "http://new.svc" {
		t.Errorf("expected http://new.svc, got %s", pool.entries[0].URL)
	}
}

// TestBackendPool_RoundRobin verifies even distribution across backends.
func TestBackendPool_RoundRobin(t *testing.T) {
	pool := &BackendPool{
		entries: []*BackendEntry{
			makeEntry("http://a.svc"),
			makeEntry("http://b.svc"),
			makeEntry("http://c.svc"),
		},
	}

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		e := pool.next()
		if e == nil {
			t.Fatalf("next() returned nil on iteration %d", i)
		}
		counts[e.URL.Host]++
	}

	for host, count := range counts {
		if count != 3 {
			t.Errorf("host %s was selected %d times, expected 3", host, count)
		}
	}
}

// TestBackendPool_SkipsOpenCircuits verifies tripped backends are excluded.
func TestBackendPool_SkipsOpenCircuits(t *testing.T) {
	healthy := makeEntry("http://healthy.svc")
	tripped := makeEntry("http://tripped.svc")

	// Trip the circuit breaker on the second backend.
	for i := 0; i < 20; i++ {
		tripped.CB.Allow()
		tripped.CB.RecordFailure()
	}
	if tripped.CB.State() != StateOpen {
		t.Fatal("expected tripped CB to be OPEN")
	}

	pool := &BackendPool{entries: []*BackendEntry{healthy, tripped}}

	for i := 0; i < 10; i++ {
		e := pool.next()
		if e == nil {
			t.Fatal("next() should always return healthy backend")
		}
		if e.URL.Host == "tripped.svc" {
			t.Fatal("round-robin selected an OPEN circuit breaker backend")
		}
	}
}

// TestBackendPool_AllOpen returns nil when no healthy backends exist.
func TestBackendPool_AllOpen(t *testing.T) {
	e := makeEntry("http://down.svc")
	for i := 0; i < 20; i++ {
		e.CB.Allow()
		e.CB.RecordFailure()
	}

	pool := &BackendPool{entries: []*BackendEntry{e}}
	if got := pool.next(); got != nil {
		t.Fatalf("expected nil when all backends are OPEN, got %v", got.URL)
	}
}

// TestPathPrefix verifies the low-cardinality label extractor.
func TestPathPrefix(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/api/v1/users/123", "/api/v1"},
		{"/api/v2/orders", "/api/v2"},
		{"/healthz", "/healthz"},
		{"/", "/"},
		{"", ""},
	}
	for _, tc := range tests {
		got := pathPrefix(tc.path)
		if got != tc.want {
			t.Errorf("pathPrefix(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
