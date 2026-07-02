.PHONY: help build test vet fmt lint tidy run run-bwrap linux smoke image image-lean image-multiarch clean

BIN       := bin/hostel
ADDR      := :44772
WS_ROOT   := ./.workspace
IMAGE     := hostel:dev
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := linux/amd64,linux/arm64

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the hostel binary for the current platform
	go build -o $(BIN) ./cmd/hostel

test: ## Run all tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go sources
	gofmt -w .

lint: vet ## gofmt check + go vet (CI gate)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

tidy: ## Sync go.mod/go.sum
	go mod tidy

run: build ## Run locally with no isolation (dev, any platform)
	$(BIN) --isolation direct --workspace-root $(WS_ROOT) --addr $(ADDR)

run-bwrap: build ## Run with bwrap isolation (Linux with bubblewrap installed)
	$(BIN) --isolation bwrap --workspace-root $(WS_ROOT) --addr $(ADDR)

linux: ## Cross-compile static Linux binaries (amd64 + arm64) into bin/
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hostel-linux-amd64 ./cmd/hostel
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/hostel-linux-arm64 ./cmd/hostel

smoke: build ## Boot on a scratch port and curl the core endpoints end to end
	@set -e; \
	tmp=$$(mktemp -d); \
	$(BIN) --isolation direct --workspace-root $$tmp/ws --addr :44799 & pid=$$!; \
	trap "kill $$pid 2>/dev/null; rm -rf $$tmp" EXIT; \
	sleep 1; \
	curl -sf localhost:44799/ping >/dev/null; \
	curl -sf localhost:44799/healthz >/dev/null; \
	curl -sfN -XPOST localhost:44799/command -H 'Content-Type: application/json' \
	  -d '{"command":"echo smoke > s.txt && cat s.txt"}' | grep -q smoke; \
	curl -sf 'localhost:44799/files/download?path=/workspace/s.txt' | grep -q smoke; \
	curl -sf -o /dev/null -w '%{http_code}' 'localhost:44799/files/download?path=/workspace/s.txt' \
	  -H 'X-Hostel-Bed: other' | grep -q 404; \
	echo "smoke OK"

image: ## Build the container image (bwrap + chromium)
	docker build -f build/Dockerfile -t $(IMAGE) --build-arg VERSION=$(VERSION) .

image-lean: ## Build the lean image (bwrap only, no browser amenity)
	docker build -f build/Dockerfile -t $(IMAGE)-lean --build-arg VERSION=$(VERSION) --build-arg WITH_CHROMIUM=false .

image-multiarch: ## Build + push a multi-arch image (needs buildx + a registry; set IMAGE=repo/name:tag)
	docker buildx build -f build/Dockerfile --platform $(PLATFORMS) \
		-t $(IMAGE) --build-arg VERSION=$(VERSION) --push .

clean: ## Remove build artifacts and the dev workspace
	rm -rf bin .workspace
