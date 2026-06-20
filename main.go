package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	cfg := LoadConfig()
	logger := newLogger()
	defer logger.Sync() //nolint:errcheck

	logger.Info("spinglobe starting",
		zap.String("port", cfg.Port),
		zap.String("redis", cfg.RedisURL),
		zap.Int("routes", len(cfg.Routes)),
	)

	rdb, err := connectRedis(cfg.RedisURL)
	if err != nil {
		logger.Warn("Redis unavailable at startup; rate limiter will use local fallback",
			zap.Error(err),
		)
	}

	// Prometheus registry — isolated from the default global registry so tests
	// can spin up multiple instances without label collisions.
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	m := newMetrics(reg)

	table := buildRoutingTable(cfg.Routes, cfg.CBErrorThreshold, cfg.CBCooldown)
	proxyHandler := NewProxyHandler(table, cfg, logger)
	rateLimiter := NewRateLimiter(rdb, cfg.RateLimitFailOpen, logger, m)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.Handle("/metrics", MetricsHandler(reg))

	// Admin API — hot-reload routes and inspect gateway state (no auth middleware).
	admin := NewAdminHandler(table, cfg, logger)
	mux.Handle("/admin/", admin)

	// Main middleware chain:
	// Metrics instrumentation → RequestLogger → X-Request-ID → JWTAuth → RateLimiter → ProxyHandler
	chain := m.instrumentedMiddleware()(
		requestLogger(logger)(
			requestIDMiddleware()(
				JWTAuthMiddleware(cfg.JWTSecret, logger)(
					rateLimiter.Middleware(cfg)(
						proxyHandler,
					),
				),
			),
		),
	)
	mux.Handle("/", chain)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("server listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", zap.Error(err))
	} else {
		logger.Info("server stopped gracefully")
	}
}

// healthzHandler returns 200 OK for Kubernetes liveness and readiness probes.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// requestLogger emits a structured JSON log line per request including the
// X-Request-ID so logs can be correlated across the middleware chain.
func requestLogger(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusCapturingResponseWriter{ResponseWriter: w}

			next.ServeHTTP(rw, r)

			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}

			logger.Info("request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", status),
				zap.Duration("latency", time.Since(start)),
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("user_agent", r.UserAgent()),
				zap.String("request_id", r.Header.Get("X-Request-ID")),
				zap.String("tier", fromContext(r.Context(), ctxKeyTier)),
				zap.String("subject", fromContext(r.Context(), ctxKeySubject)),
			)
		})
	}
}

// requestIDMiddleware generates a unique X-Request-ID for every inbound request
// (or preserves an existing one from an upstream caller) and echoes it in the
// response headers so clients can correlate logs.
func requestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = newRequestID()
				r.Header.Set("X-Request-ID", id)
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r)
		})
	}
}

// newRequestID returns a 16-byte cryptographically random hex string.
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// connectRedis parses redisURL and pings the server. Returns the client even on
// ping failure so the caller can decide whether to use local fallback.
func connectRedis(redisURL string) (*redis.Client, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return rdb, fmt.Errorf("redis ping failed: %w", err)
	}
	return rdb, nil
}

// newLogger builds a production zap logger outputting JSON to stdout.
func newLogger() *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig = encCfg
	zapCfg.OutputPaths = []string{"stdout"}

	logger, err := zapCfg.Build()
	if err != nil {
		panic("failed to initialise logger: " + err.Error())
	}
	return logger
}
