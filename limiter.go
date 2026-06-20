package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// tokenBucketLua is the atomic Redis Lua script for the distributed token bucket.
// KEYS[1] = bucket key
// ARGV[1] = capacity  (max tokens)
// ARGV[2] = rate      (tokens per second, float)
// ARGV[3] = now       (current Unix timestamp as float, e.g. "1718841600.123")
//
// Returns: {allowed (0|1), remaining (int), reset (unix seconds int)}
const tokenBucketLua = `
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])

local data     = redis.call('HMGET', key, 'tokens', 'ts')
local tokens   = tonumber(data[1])
local last_ts  = tonumber(data[2])

if tokens == nil then
    tokens  = capacity
    last_ts = now
end

-- Mathematical replenishment: no background goroutine required.
local elapsed     = math.max(0, now - last_ts)
local replenished = elapsed * rate
tokens = math.min(capacity, tokens + replenished)

local allowed   = 0
local remaining = math.floor(tokens)
local reset     = math.ceil(now + (1.0 / rate))

if tokens >= 1 then
    tokens    = tokens - 1
    allowed   = 1
    remaining = math.floor(tokens)
end

local ttl = math.ceil(capacity / rate) + 10
redis.call('HMSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('EXPIRE', key, ttl)

return {allowed, remaining, reset}
`

// localBucket is an in-memory token bucket used when Redis is unavailable.
type localBucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

func (b *localBucket) consume(capacity int64, rate float64) (allowed bool, remaining int64, reset int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = min64(float64(capacity), b.tokens+elapsed*rate)
	b.lastRefill = now

	reset = now.Unix() + int64(1.0/rate)

	if b.tokens >= 1 {
		b.tokens--
		return true, int64(b.tokens), reset
	}
	return false, 0, reset
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// RateLimiter holds Redis connection state and the local fallback map.
type RateLimiter struct {
	rdb       *redis.Client
	scriptSHA string // SHA of the loaded Lua script
	failOpen  bool
	logger    *zap.Logger

	localBuckets sync.Map // map[string]*localBucket
}

// NewRateLimiter connects to Redis, loads the Lua script, and returns a RateLimiter.
// A connection failure is not fatal here — the limiter will operate in local-fallback mode.
func NewRateLimiter(rdb *redis.Client, failOpen bool, logger *zap.Logger) *RateLimiter {
	rl := &RateLimiter{
		rdb:      rdb,
		failOpen: failOpen,
		logger:   logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sha, err := rdb.ScriptLoad(ctx, tokenBucketLua).Result()
	if err != nil {
		logger.Warn("failed to load rate-limit Lua script into Redis; will use EVAL fallback",
			zap.Error(err),
		)
	} else {
		rl.scriptSHA = sha
		logger.Info("rate-limit Lua script loaded", zap.String("sha", sha))
	}

	return rl
}

// Middleware returns an http.Handler middleware that enforces the token bucket.
// It reads the tier from context (set by JWTAuthMiddleware) to select limits.
func (rl *RateLimiter) Middleware(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tier := fromContext(r.Context(), ctxKeyTier)
			limits := TierFor(tier, cfg)

			clientKey := clientIdentifier(r)
			bucketKey := fmt.Sprintf("rl:%s:%s", tier, clientKey)

			allowed, remaining, reset, err := rl.check(r.Context(), bucketKey, limits)
			if err != nil {
				rl.logger.Error("rate-limit check failed", zap.String("key", bucketKey), zap.Error(err))
				if !rl.failOpen {
					http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
					return
				}
				// fail-open: allow the request through without headers
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(limits.Capacity, 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))

			if !allowed {
				rl.logger.Info("rate limit exceeded",
					zap.String("key", bucketKey),
					zap.String("path", r.URL.Path),
				)
				w.Header().Set("Retry-After", strconv.FormatInt(reset-time.Now().Unix(), 10))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// check attempts a Redis EVALSHA, falling back to EVAL if the script is not cached,
// and finally falling back to the local in-memory bucket on any Redis error.
func (rl *RateLimiter) check(
	ctx context.Context,
	key string,
	limits TierLimits,
) (allowed bool, remaining, reset int64, err error) {
	now := strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 6, 64)
	cap := strconv.FormatInt(limits.Capacity, 10)
	rate := strconv.FormatFloat(limits.RatePerSec, 'f', 6, 64)

	var result []int64
	result, err = rl.execLua(ctx, key, cap, rate, now)
	if err != nil {
		rl.logger.Warn("Redis rate-limit error; switching to local fallback",
			zap.String("key", key),
			zap.Error(err),
		)
		return rl.localCheck(key, limits)
	}

	if len(result) < 3 {
		return rl.localCheck(key, limits)
	}

	return result[0] == 1, result[1], result[2], nil
}

// execLua runs EVALSHA, re-loading the script on NOSCRIPT error.
func (rl *RateLimiter) execLua(ctx context.Context, key, capacity, rate, now string) ([]int64, error) {
	keys := []string{key}
	args := []interface{}{capacity, rate, now}

	if rl.scriptSHA != "" {
		res, err := rl.rdb.EvalSha(ctx, rl.scriptSHA, keys, args...).Int64Slice()
		if err == nil {
			return res, nil
		}
		// NOSCRIPT means Redis flushed its script cache — re-load and retry.
		if isNoScript(err) {
			sha, loadErr := rl.rdb.ScriptLoad(ctx, tokenBucketLua).Result()
			if loadErr == nil {
				rl.scriptSHA = sha
				return rl.rdb.EvalSha(ctx, sha, keys, args...).Int64Slice()
			}
		}
		// Fall through to EVAL.
	}

	return rl.rdb.Eval(ctx, tokenBucketLua, keys, args...).Int64Slice()
}

// localCheck runs the token bucket algorithm entirely in-process.
func (rl *RateLimiter) localCheck(key string, limits TierLimits) (bool, int64, int64, error) {
	val, _ := rl.localBuckets.LoadOrStore(key, &localBucket{
		tokens:     float64(limits.Capacity),
		lastRefill: time.Now(),
	})
	bucket := val.(*localBucket)
	allowed, remaining, reset := bucket.consume(limits.Capacity, limits.RatePerSec)
	return allowed, remaining, reset, nil
}

// clientIdentifier derives a stable key for the client in priority order:
// X-API-Key header → JWT subject → X-Forwarded-For → RemoteAddr.
func clientIdentifier(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return "apikey:" + k
	}
	if sub := fromContext(r.Context(), ctxKeySubject); sub != "" {
		return "sub:" + sub
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// Take only the first (client) IP in a comma-separated list.
		if idx := len(fwd); idx > 0 {
			for i, c := range fwd {
				if c == ',' {
					fwd = fwd[:i]
					break
				}
			}
		}
		return "ip:" + fwd
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	return "ip:" + ip
}

func isNoScript(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) >= 8 && err.Error()[:8] == "NOSCRIPT"
}
