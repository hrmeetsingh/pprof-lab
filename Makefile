SHELL := /bin/bash
GO ?= go
MODULE := github.com/harmeetsingh/pprof-lab
BIN_DIR := bin

GRPC_ADDR ?= :50051
ADMIN_ADDR ?= :9090
ADMIN_URL ?= http://127.0.0.1:9090
LOAD_ADDR ?= 127.0.0.1:50051
LOAD_MODE ?= mixed
LOAD_DURATION ?= 45s
CONCURRENCY ?= 4
DOC_BYTES ?= 2048
PROFILE_SECONDS ?= 30
PPROF_PORT ?= 8081
TOP ?= 20

.DEFAULT_GOAL := help
.PHONY: help build run load test race cover bench fmt vet lint tidy proto \
	profile-cpu profile-heap profile-allocs profile-goroutine profile-block profile-mutex trace \
	stats demo docker-build docker-up docker-down docker-load clean

help: ## Show this help
	@echo "pprof-lab targets:"
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build server and load generator into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o $(BIN_DIR)/server ./cmd/server
	$(GO) build -trimpath -o $(BIN_DIR)/loadgen ./cmd/loadgen

run: ## Run the gRPC server (gRPC :50051, admin/pprof :9090)
	$(GO) run ./cmd/server

load: build ## Generate load against a running server
	$(BIN_DIR)/loadgen -addr $(LOAD_ADDR) -mode $(LOAD_MODE) -duration $(LOAD_DURATION) -concurrency $(CONCURRENCY) -doc-bytes $(DOC_BYTES)

test: ## Run all tests
	$(GO) test ./...

race: ## Run tests with the race detector
	$(GO) test -race ./...

cover: ## Run tests and print coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

bench: ## Benchmark the analysis path
	$(GO) test -run '^$$' -bench=. -benchmem -count=5 ./internal/processing

fmt: ## Format the tree
	gofmt -w .

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint (must be installed)
	@command -v golangci-lint >/dev/null || { \
		echo "golangci-lint not found. Install it with:"; \
		echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	golangci-lint run

tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

.bin/protoc-gen-go:
	@mkdir -p .bin
	$(GO) build -o $@ google.golang.org/protobuf/cmd/protoc-gen-go

.bin/protoc-gen-go-grpc:
	@mkdir -p .bin
	$(GO) build -o $@ google.golang.org/grpc/cmd/protoc-gen-go-grpc

proto: .bin/protoc-gen-go .bin/protoc-gen-go-grpc ## Regenerate gRPC code (requires protoc)
	PATH="$(CURDIR)/.bin:$$PATH" protoc -I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		proto/docproc/v1/docproc.proto

profile-cpu: ## Capture a CPU profile and open the pprof UI
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/profile?seconds=$(PROFILE_SECONDS)" -o profiles/cpu.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) profiles/cpu.pprof

profile-heap: ## Capture heap (in-use) profile and open the pprof UI
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/heap" -o profiles/heap.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) -sample_index=inuse_space profiles/heap.pprof

profile-allocs: ## Capture allocation profile and open the pprof UI
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/allocs" -o profiles/allocs.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) -sample_index=alloc_space profiles/allocs.pprof

profile-goroutine: ## Capture goroutine profile and open the pprof UI
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/goroutine" -o profiles/goroutine.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) profiles/goroutine.pprof

profile-block: ## Capture block profile (start server with BLOCK_PROFILE_RATE=1)
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/block" -o profiles/block.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) profiles/block.pprof

profile-mutex: ## Capture mutex profile (start server with MUTEX_PROFILE_FRACTION=1)
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/mutex" -o profiles/mutex.pprof
	$(GO) tool pprof -http=:$(PPROF_PORT) profiles/mutex.pprof

trace: ## Capture a 5s execution trace and open the trace UI
	@mkdir -p profiles
	curl -fsS "$(ADMIN_URL)/debug/pprof/trace?seconds=5" -o profiles/trace.out
	$(GO) tool trace profiles/trace.out

stats: ## Print the server's JSON stats
	curl -fsS "$(ADMIN_URL)/stats"

demo: ## Run server + load, capture CPU/heap profiles for the current branch
	./scripts/demo.sh

docker-build: ## Build the Docker image
	docker compose build

docker-up: ## Start the service in Docker (detached)
	docker compose up --build -d

docker-down: ## Stop Docker services
	docker compose --profile load down

docker-load: ## Start service + load generator in Docker (foreground)
	docker compose --profile load up --build

clean: ## Remove local binaries and generated profile artifacts
	rm -rf $(BIN_DIR) .bin coverage.out
	find profiles -type f ! -name .gitkeep -delete
