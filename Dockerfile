# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source so layer is reused on code-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /spinglobe .

# ── Stage 2: Runtime (distroless — no shell, no package manager) ─────────────
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="spinglobe" \
      org.opencontainers.image.description="Production-grade API Gateway with distributed rate limiting" \
      org.opencontainers.image.source="https://github.com/ybapat/spinglobe"

# Non-root user provided by distroless/static-debian12:nonroot (uid 65532).
USER nonroot:nonroot

COPY --from=builder --chown=nonroot:nonroot /spinglobe /spinglobe

EXPOSE 8080

ENTRYPOINT ["/spinglobe"]
