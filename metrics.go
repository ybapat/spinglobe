package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// gatewayMetrics holds all Prometheus instruments for the gateway.
type gatewayMetrics struct {
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	rateLimitHits    *prometheus.CounterVec
	circuitBreakerState *prometheus.GaugeVec
	activeConnections   prometheus.Gauge
	redisErrors      prometheus.Counter
}

// newMetrics registers and returns all Prometheus instruments.
func newMetrics(reg prometheus.Registerer) *gatewayMetrics {
	f := promauto.With(reg)
	return &gatewayMetrics{
		requestsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "spinglobe_requests_total",
			Help: "Total number of HTTP requests handled by the gateway, by method, path prefix, status, and tier.",
		}, []string{"method", "path_prefix", "status", "tier"}),

		requestDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "spinglobe_request_duration_seconds",
			Help:    "End-to-end HTTP request latency in seconds.",
			Buckets: prometheus.DefBuckets, // 5ms … 10s
		}, []string{"method", "path_prefix", "tier"}),

		rateLimitHits: f.NewCounterVec(prometheus.CounterOpts{
			Name: "spinglobe_rate_limit_hits_total",
			Help: "Number of requests rejected by the rate limiter, by tier.",
		}, []string{"tier"}),

		circuitBreakerState: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "spinglobe_circuit_breaker_state",
			Help: "Current circuit breaker state per backend (0=CLOSED, 1=OPEN, 2=HALF_OPEN).",
		}, []string{"backend"}),

		activeConnections: f.NewGauge(prometheus.GaugeOpts{
			Name: "spinglobe_active_connections",
			Help: "Number of in-flight upstream proxy connections.",
		}),

		redisErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "spinglobe_redis_errors_total",
			Help: "Total number of Redis errors encountered by the rate limiter.",
		}),
	}
}

// MetricsHandler returns the Prometheus HTTP handler for /metrics.
func MetricsHandler(reg prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// instrumentedMiddleware wraps the full handler chain and emits per-request
// Prometheus metrics (total count and latency histogram).
func (m *gatewayMetrics) instrumentedMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusCapturingResponseWriter{ResponseWriter: w}

			m.activeConnections.Inc()
			defer m.activeConnections.Dec()

			next.ServeHTTP(rw, r)

			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}

			tier := fromContext(r.Context(), ctxKeyTier)
			prefix := pathPrefix(r.URL.Path)

			m.requestsTotal.WithLabelValues(
				r.Method,
				prefix,
				strconv.Itoa(status),
				tier,
			).Inc()

			m.requestDuration.WithLabelValues(
				r.Method,
				prefix,
				tier,
			).Observe(time.Since(start).Seconds())
		})
	}
}

// ObserveRateLimit increments the rate-limit hit counter for the given tier.
func (m *gatewayMetrics) ObserveRateLimit(tier string) {
	m.rateLimitHits.WithLabelValues(tier).Inc()
}

// ObserveRedisError increments the Redis error counter.
func (m *gatewayMetrics) ObserveRedisError() {
	m.redisErrors.Inc()
}

// UpdateCircuitBreakerState sets the CB gauge for the given backend URL.
func (m *gatewayMetrics) UpdateCircuitBreakerState(backend string, state State) {
	m.circuitBreakerState.WithLabelValues(backend).Set(float64(state))
}

// pathPrefix extracts the first two path segments (e.g. "/api/v1") as a low-cardinality label.
func pathPrefix(path string) string {
	segments := 0
	for i, c := range path {
		if c == '/' && i > 0 {
			segments++
			if segments == 2 {
				return path[:i]
			}
		}
	}
	if len(path) > 32 {
		return path[:32]
	}
	return path
}
