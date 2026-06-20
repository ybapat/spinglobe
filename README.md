# spinglobe

A production-grade **API Gateway & Distributed Rate Limiter** written in Go.

[![CI](https://github.com/ybapat/spinglobe/actions/workflows/ci.yml/badge.svg)](https://github.com/ybapat/spinglobe/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.24-blue)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

---

## Features

| Capability | Implementation |
|---|---|
| **Reverse Proxy** | `net/http/httputil.ReverseProxy` with custom Director and ErrorHandler |
| **Load Balancing** | Round-robin across backend pool; Open circuit breakers are skipped |
| **Distributed Rate Limiting** | Redis Lua token bucket (`EVALSHA`), mathematically replenished per-request |
| **Local Fallback** | In-memory token buckets (`sync.Map`) when Redis is unreachable |
| **JWT Authentication** | HS256 Bearer token validation; tier extracted from claims |
| **Tiered Quotas** | Free / Premium / Enterprise — configurable capacity and refill rate |
| **Circuit Breaker** | Closed → Open → Half-Open FSM; 10-bucket 1s sliding-window error rate |
| **Retries with Jitter** | Exponential backoff ± 25% jitter; retries across alternative healthy nodes |
| **Prometheus Metrics** | Request count, latency histogram, CB state gauge, rate-limit hits |
| **Admin API** | Live routing table hot-reload without restart (`/admin/routes`) |
| **X-Request-ID** | Cryptographically random per-request correlation ID |
| **Kubernetes-ready** | Deployment, ClusterIP Service, HPA (CPU 70%, 2–20 replicas), probes |
| **Distroless Docker** | Multi-stage build; ~12 MB static binary, runs as non-root |

---

## Architecture

```
                          ┌─────────────────────────────────────────────┐
                          │                  spinglobe                  │
                          │                                             │
Client ──HTTP──►  ┌───────┴──────┐    ┌──────────────┐                │
                  │  Middleware  │    │  Routing     │                │
                  │  Chain       │    │  Table       │                │
                  │              │    │  (prefix map,│                │
                  │ 1. Metrics   │    │  RWMutex)    │                │
                  │ 2. Logger    │    └──────┬───────┘                │
                  │ 3. RequestID │           │ longest-prefix          │
                  │ 4. JWTAuth  │           │ match                   │
                  │ 5. RateLimiter──Redis   │                         │
                  │    (Lua/EVALSHA)        ▼                         │
                  │    fallback: local  ┌──────────┐  round-robin     │
                  │    sync.Map         │ Backend  ├─────────────►  Upstream A
                  │ 6. ProxyHandler     │  Pool    │                  │
                  │    + CB check       │          ├─────────────►  Upstream B
                  │    + retry+jitter   └──────────┘                  │
                  └───────┬──────┘                                     │
                          │                                             │
                  /healthz│/metrics│/admin/                            │
                          └─────────────────────────────────────────────┘

Circuit Breaker FSM (per backend):

  [CLOSED] ──(error rate > threshold)──► [OPEN] ──(cooldown)──► [HALF-OPEN]
     ▲                                                                 │
     └──────────────────(probe success)──────────────────────────────┘
                                 └─(probe fails)──► [OPEN]
```

---

## Quick Start

### Prerequisites

- Go 1.24+
- Docker (for Redis)

### Run locally

```bash
# 1. Start Redis
make redis

# 2. Run the gateway (routes all /api/v1/* to httpbin.org)
make run
```

### Test the endpoints

```bash
# Health check
curl http://localhost:8080/healthz

# Prometheus metrics
curl http://localhost:8080/metrics

# Admin — current routing table
curl http://localhost:8080/admin/routes

# Admin — per-backend health + error rates
curl http://localhost:8080/admin/health

# Hot-reload routes (no restart needed)
curl -X POST http://localhost:8080/admin/routes \
  -H "Content-Type: application/json" \
  -d '{"routes":[{"prefix":"/api/v2/","backends":["http://httpbin.org"]}]}'
```

### Generate a test JWT

```bash
# Requires: pip install PyJWT
python3 - <<'EOF'
import jwt, time
token = jwt.encode(
    {"sub": "user-123", "tier": "premium", "exp": int(time.time()) + 3600},
    "dev-secret-change-me", algorithm="HS256"
)
print(token)
EOF
```

### Authenticated gateway request

```bash
TOKEN="<paste token here>"
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/get
# Response includes X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset
```

### Rate limit test

```bash
# Fire 20 requests as a free-tier user (capacity=10) — expect 429 after 10
for i in $(seq 1 20); do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    http://localhost:8080/api/v1/get
done
```

---

## Configuration

All configuration is read from environment variables. There are no config files.

| Variable | Default | Description |
|---|---|---|
| `GATEWAY_PORT` | `8080` | HTTP listen port |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection string |
| `JWT_SECRET` | `change-me-in-production` | HS256 signing secret |
| `RATE_LIMIT_FAIL_OPEN` | `true` | Allow requests on Redis failure (`true`) or block (`false`) |
| `ROUTES_JSON` | `[]` | JSON array of `{prefix, backends[]}` route definitions |
| `TIER_FREE_CAPACITY` | `10` | Free tier: token bucket capacity (burst size) |
| `TIER_FREE_RPS` | `10` | Free tier: token refill rate (tokens/second) |
| `TIER_PREMIUM_CAPACITY` | `100` | Premium tier: burst size |
| `TIER_PREMIUM_RPS` | `100` | Premium tier: refill rate |
| `TIER_ENTERPRISE_CAPACITY` | `1000` | Enterprise tier: burst size |
| `TIER_ENTERPRISE_RPS` | `1000` | Enterprise tier: refill rate |
| `CB_ERROR_THRESHOLD` | `0.10` | Circuit breaker: error rate to trip (0.0–1.0) |
| `CB_COOLDOWN_SECONDS` | `30` | Circuit breaker: seconds in OPEN before probing |
| `MAX_RETRIES` | `3` | Max retries across alternative backends on 5xx |
| `RETRY_BASE_MS` | `50` | Initial retry backoff in milliseconds |

---

## Kubernetes Deployment

```bash
# Apply all manifests (ConfigMap, Secret, Deployment, Service, HPA)
kubectl apply -f deployments/deployment.yaml

# Watch pods scale
kubectl get hpa spinglobe-hpa --watch
```

The HPA scales between **2 and 20 replicas** targeting **70% CPU utilization**, with a 30s scale-up and 2-minute scale-down stabilisation window to avoid flapping. `maxUnavailable: 0` guarantees zero-downtime rolling updates.

---

## Endpoints

| Endpoint | Auth | Description |
|---|---|---|
| `GET /healthz` | None | Kubernetes liveness/readiness probe |
| `GET /metrics` | None | Prometheus metrics (OpenMetrics format) |
| `GET /admin/routes` | None* | List all registered route prefixes + CB states |
| `POST /admin/routes` | None* | Hot-reload routing table |
| `GET /admin/health` | None* | Per-backend health with error rates |
| `/* ` | JWT Bearer | Proxied to upstream backends |

*Protect `/admin/` with a Kubernetes NetworkPolicy or mTLS in production.

---

## Response Headers

Every proxied response includes:

| Header | Value |
|---|---|
| `X-Request-ID` | Unique per-request hex ID for log correlation |
| `X-RateLimit-Limit` | Token bucket capacity for this client's tier |
| `X-RateLimit-Remaining` | Tokens remaining after this request |
| `X-RateLimit-Reset` | Unix timestamp when the bucket refills to ≥1 token |
| `Retry-After` | Seconds until the next allowed request (on 429 only) |

---

## Project Layout

```
spinglobe/
├── main.go              # Bootstrap, middleware chain, graceful shutdown
├── config.go            # Env-var configuration with typed helpers
├── auth.go              # JWT middleware and tier mapping
├── limiter.go           # Distributed token bucket (Redis Lua + local fallback)
├── circuitbreaker.go    # Closed/Open/Half-Open FSM with sliding-window tracking
├── proxy.go             # Routing table, round-robin LB, httputil.ReverseProxy
├── metrics.go           # Prometheus instruments and instrumentation middleware
├── admin.go             # Hot-reload admin API
├── circuitbreaker_test.go
├── limiter_test.go
├── Dockerfile           # Multi-stage build, distroless runtime
├── Makefile             # build / test / docker / dev targets
└── deployments/
    └── deployment.yaml  # K8s Deployment + Service + HPA
```

---

## Development

```bash
make build        # compile binary
make test         # run tests
make test-race    # run tests with race detector
make test-cover   # generate HTML coverage report
make fmt          # format source files
make vet          # static analysis
make docker-build # build Docker image
```
