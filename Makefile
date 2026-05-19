# Gaap — Model-agnostic multi-agent orchestrator
# Makefile for build, test, bench, and deployment

BINARY := gaap
CMD_DIR := cmd/gaap
GO := go
GOFLAGS := -ldflags="-s -w"

# Build targets
.PHONY: build
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./$(CMD_DIR)/

.PHONY: build-linux
build-linux:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BINARY)-linux-amd64 ./$(CMD_DIR)/

.PHONY: build-all
build-all: build build-linux

# Test targets
.PHONY: test
test:
	$(GO) test -race -count=1 ./...

.PHONY: test-v
test-v:
	$(GO) test -race -count=1 -v ./...

.PHONY: bench
bench:
	$(GO) test -bench=. -benchtime=1s -count=3 -run='^$$' .

.PHONY: cover
cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

# Development
.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: lint
lint: fmt vet

.PHONY: ci
ci: lint test
	@echo "CI check complete: all tests green"

# Deployment
STUDIO := studio
STUDIO_BIN := ~/.local/bin/$(BINARY)

.PHONY: deploy-studio
deploy-studio: build
	scp $(BINARY) $(STUDIO):$(STUDIO_BIN)
	ssh $(STUDIO) "chmod +x $(STUDIO_BIN)"
	@echo "Deployed to $(STUDIO):$(STUDIO_BIN)"

# Clean
.PHONY: clean
clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64 coverage.out
