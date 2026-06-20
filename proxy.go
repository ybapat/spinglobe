package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// BackendEntry represents a single upstream backend with its own circuit breaker.
type BackendEntry struct {
	URL    *url.URL
	CB     *CircuitBreaker
	active atomic.Int64 // active connections (reserved for least-conn extension)
}

// BackendPool holds all backends registered for a route prefix.
type BackendPool struct {
	entries []*BackendEntry
	counter atomic.Uint64 // round-robin index
}

// next returns the next healthy backend via round-robin, skipping Open circuits.
// Returns nil if every backend is unavailable.
func (p *BackendPool) next() *BackendEntry {
	n := uint64(len(p.entries))
	if n == 0 {
		return nil
	}
	start := p.counter.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		e := p.entries[(start+i)%n]
		if e.CB.Allow() {
			return e
		}
	}
	return nil
}

// RoutingTable is a thread-safe prefix-to-pool mapping.
type RoutingTable struct {
	mu      sync.RWMutex
	routes  map[string]*BackendPool // key = prefix, e.g. "/api/v1/"
	sorted  []string                // prefixes sorted longest-first for O(n) lookup
}

// NewRoutingTable creates an empty routing table.
func NewRoutingTable() *RoutingTable {
	return &RoutingTable{routes: make(map[string]*BackendPool)}
}

// Register adds or replaces a prefix → backends mapping.
func (rt *RoutingTable) Register(prefix string, entries []*BackendEntry) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.routes[prefix] = &BackendPool{entries: entries}
	rt.rebuildSorted()
}

// Lookup finds the backend pool whose prefix is the longest match for path.
func (rt *RoutingTable) Lookup(path string) *BackendPool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, prefix := range rt.sorted {
		if strings.HasPrefix(path, prefix) {
			return rt.routes[prefix]
		}
	}
	return nil
}

// rebuildSorted must be called with rt.mu held.
func (rt *RoutingTable) rebuildSorted() {
	keys := make([]string, 0, len(rt.routes))
	for k := range rt.routes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	rt.sorted = keys
}

// ProxyHandler is the http.Handler that performs routing, load balancing, and proxying.
type ProxyHandler struct {
	table      *RoutingTable
	maxRetries int
	baseDelay  time.Duration
	logger     *zap.Logger
}

func NewProxyHandler(table *RoutingTable, cfg Config, logger *zap.Logger) *ProxyHandler {
	return &ProxyHandler{
		table:      table,
		maxRetries: cfg.MaxRetries,
		baseDelay:  cfg.RetryBaseDelay,
		logger:     logger,
	}
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pool := h.table.Lookup(r.URL.Path)
	if pool == nil {
		http.Error(w, "no route found", http.StatusNotFound)
		return
	}

	var lastErr error
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		if attempt > 0 {
			delay := jitter(h.baseDelay * (1 << uint(attempt-1)))
			select {
			case <-r.Context().Done():
				http.Error(w, "client disconnected", http.StatusServiceUnavailable)
				return
			case <-time.After(delay):
			}
		}

		entry := pool.next()
		if entry == nil {
			http.Error(w, "all backends unavailable", http.StatusServiceUnavailable)
			return
		}

		rw := &statusCapturingResponseWriter{ResponseWriter: w}
		h.serveWithEntry(rw, r, entry)

		if rw.status == 0 || (rw.status < 500 && rw.status != 0) {
			return // success or client error — do not retry
		}

		lastErr = fmt.Errorf("backend returned %d", rw.status)
		h.logger.Warn("backend error, will retry",
			zap.Int("attempt", attempt+1),
			zap.String("backend", entry.URL.String()),
			zap.Int("status", rw.status),
		)
	}

	h.logger.Error("all retries exhausted", zap.Error(lastErr))
	// Response has already been (partially) written by the last attempt; nothing more to do.
}

func (h *ProxyHandler) serveWithEntry(w http.ResponseWriter, r *http.Request, entry *BackendEntry) {
	proxy := newReverseProxy(entry, h.logger)
	entry.active.Add(1)
	defer entry.active.Add(-1)

	isHalfOpen := entry.CB.State() == StateHalfOpen
	defer func() {
		if isHalfOpen {
			entry.CB.ProbeFinished()
		}
	}()

	proxy.ServeHTTP(w, r)
}

// newReverseProxy builds an httputil.ReverseProxy targeting the given backend entry.
func newReverseProxy(entry *BackendEntry, logger *zap.Logger) *httputil.ReverseProxy {
	target := entry.URL
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Preserve the original path; strip the matched prefix if desired
			// via Director — currently we pass the full path through.
			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "spinglobe/1.0")
			}
			req.Header.Set("X-Forwarded-Host", req.Host)
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Error("proxy transport error",
				zap.String("backend", target.String()),
				zap.Error(err),
			)
			entry.CB.RecordFailure()
			// Only write header if not already written.
			w.WriteHeader(http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 500 {
				entry.CB.RecordFailure()
			} else {
				entry.CB.RecordSuccess()
			}
			return nil
		},
	}
	return proxy
}

// jitter adds ±25% randomness to a base duration to spread retry storms.
func jitter(d time.Duration) time.Duration {
	jitterRange := float64(d) * 0.25
	return d + time.Duration((rand.Float64()*2-1)*jitterRange) //nolint:gosec
}

// statusCapturingResponseWriter wraps http.ResponseWriter to capture the status code
// written by the proxy without consuming the response body.
type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusCapturingResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming responses work correctly.
func (w *statusCapturingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// buildRoutingTable constructs a RoutingTable from config routes.
func buildRoutingTable(routes []RouteConfig, cbThreshold float64, cbCooldown time.Duration) *RoutingTable {
	table := NewRoutingTable()
	for _, r := range routes {
		var entries []*BackendEntry
		for _, raw := range r.Backends {
			u, err := url.Parse(raw)
			if err != nil {
				panic(fmt.Sprintf("invalid backend URL %q: %v", raw, err))
			}
			entries = append(entries, &BackendEntry{
				URL: u,
				CB:  NewCircuitBreaker(cbThreshold, cbCooldown),
			})
		}
		table.Register(r.Prefix, entries)
	}
	return table
}

// contextKey is a package-level unexported type to avoid context key collisions.
type contextKey string

const (
	ctxKeyTier    contextKey = "tier"
	ctxKeySubject contextKey = "subject"
	ctxKeyAPIKey  contextKey = "api_key"
)

// withValue stores a string value in the request context.
func withValue(r *http.Request, key contextKey, val string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), key, val))
}

// fromContext retrieves a string value from context; returns "" if absent.
func fromContext(ctx context.Context, key contextKey) string {
	if v, ok := ctx.Value(key).(string); ok {
		return v
	}
	return ""
}
