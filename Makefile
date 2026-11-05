GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REDIS_URL ?= redis://localhost:6379/15

.PHONY: build test test-integration bench lint run up down loadtest

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/tollgate ./cmd/tollgate

test:
	$(GO) test -race ./...

test-integration:
	TOLLGATE_TEST_REDIS_URL=$(REDIS_URL) $(GO) test -race -count=1 ./...

bench:
	TOLLGATE_TEST_REDIS_URL=$(REDIS_URL) $(GO) test -run '^$$' -bench . -benchmem ./internal/ratelimit/

lint:
	golangci-lint run ./...

run:
	TOLLGATE_BACKEND=memory $(GO) run ./cmd/tollgate -config config/tollgate.yaml

up:
	docker compose up -d --build

down:
	docker compose --profile loadtest down

loadtest:
	docker compose --profile loadtest run --rm loadtest
