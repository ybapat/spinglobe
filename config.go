package main

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// RouteConfig maps a URL prefix to one or more backend URLs.
type RouteConfig struct {
	Prefix   string   `json:"prefix"`
	Backends []string `json:"backends"`
}

// TierLimits holds rate-limit parameters for a single tier.
type TierLimits struct {
	Capacity   int64   // max tokens (burst size)
	RatePerSec float64 // token refill rate
}

// Config holds all runtime configuration for the gateway.
type Config struct {
	Port string

	RedisURL string

	JWTSecret string

	// When true, a Redis failure allows the request through (fail-open).
	// When false, a Redis failure blocks the request (fail-closed).
	RateLimitFailOpen bool

	// Routes loaded from ROUTES_JSON env var.
	Routes []RouteConfig

	// Per-tier rate limits.
	TierFree       TierLimits
	TierPremium    TierLimits
	TierEnterprise TierLimits

	// Circuit-breaker settings.
	CBErrorThreshold float64       // 0.0–1.0, e.g. 0.10 = 10%
	CBCooldown       time.Duration // duration before OPEN→HALF-OPEN transition

	// Maximum retries toward alternative backends on transport errors.
	MaxRetries int

	// Initial backoff for retry loop.
	RetryBaseDelay time.Duration
}

// LoadConfig reads configuration from environment variables with sensible defaults.
func LoadConfig() Config {
	cfg := Config{
		Port:              envStr("GATEWAY_PORT", "8080"),
		RedisURL:          envStr("REDIS_URL", "redis://localhost:6379"),
		JWTSecret:         envStr("JWT_SECRET", "change-me-in-production"),
		RateLimitFailOpen: envBool("RATE_LIMIT_FAIL_OPEN", true),

		TierFree: TierLimits{
			Capacity:   envInt64("TIER_FREE_CAPACITY", 10),
			RatePerSec: envFloat64("TIER_FREE_RPS", 10),
		},
		TierPremium: TierLimits{
			Capacity:   envInt64("TIER_PREMIUM_CAPACITY", 100),
			RatePerSec: envFloat64("TIER_PREMIUM_RPS", 100),
		},
		TierEnterprise: TierLimits{
			Capacity:   envInt64("TIER_ENTERPRISE_CAPACITY", 1000),
			RatePerSec: envFloat64("TIER_ENTERPRISE_RPS", 1000),
		},

		CBErrorThreshold: envFloat64("CB_ERROR_THRESHOLD", 0.10),
		CBCooldown:       time.Duration(envInt64("CB_COOLDOWN_SECONDS", 30)) * time.Second,

		MaxRetries:     int(envInt64("MAX_RETRIES", 3)),
		RetryBaseDelay: time.Duration(envInt64("RETRY_BASE_MS", 50)) * time.Millisecond,
	}

	if raw := os.Getenv("ROUTES_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Routes); err != nil {
			panic("ROUTES_JSON is not valid JSON: " + err.Error())
		}
	}

	return cfg
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envFloat64(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
