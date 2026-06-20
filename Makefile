BINARY     := spinglobe
IMAGE      := spinglobe
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -s -w -X main.version=$(VERSION)
GOFLAGS    := CGO_ENABLED=0

.PHONY: all build test test-race lint fmt vet clean docker-build docker-run run help

all: build

## build: compile the gateway binary
build:
	$(GOFLAGS) go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

## test: run all unit tests
test:
	go test ./... -count=1

## test-race: run all tests with the data race detector
test-race:
	go test ./... -count=1 -race

## test-cover: generate HTML coverage report
test-cover:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go source files
fmt:
	gofmt -w -s .

## tidy: tidy and verify go.mod/go.sum
tidy:
	go mod tidy
	go mod verify

## clean: remove build artefacts
clean:
	rm -f $(BINARY) coverage.out coverage.html

## docker-build: build the Docker image
docker-build:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## docker-run: run a local instance (requires Docker + Redis)
docker-run:
	docker run --rm -p 8080:8080 \
		-e REDIS_URL=redis://host.docker.internal:6379 \
		-e JWT_SECRET=dev-secret-change-me \
		-e ROUTES_JSON='[{"prefix":"/api/v1/","backends":["http://httpbin.org"]}]' \
		$(IMAGE):latest

## run: run locally against a Redis on localhost:6379
run: build
	REDIS_URL=redis://localhost:6379 \
	JWT_SECRET=dev-secret-change-me \
	ROUTES_JSON='[{"prefix":"/api/v1/","backends":["http://httpbin.org"]}]' \
	./$(BINARY)

## redis: start a local Redis 7 container for development
redis:
	docker run -d --name spinglobe-redis -p 6379:6379 redis:7-alpine || docker start spinglobe-redis

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
