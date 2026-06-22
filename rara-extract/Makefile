.PHONY: help test test-verbose test-coverage test-race lint fmt clean build build-arm64 docker-build deps all

help:
	@echo "rara-extract - Makefile Commands"
	@echo ""
	@echo "Testing:"
	@echo "  make test              Run all tests"
	@echo "  make test-verbose      Run tests with verbose output"
	@echo "  make test-coverage     Generate coverage report"
	@echo "  make test-race         Run tests with race detector"
	@echo ""
	@echo "Code Quality:"
	@echo "  make lint              Run go vet and staticcheck"
	@echo "  make fmt               Format code with go fmt"
	@echo ""
	@echo "Build:"
	@echo "  make build             Build binary for current OS"
	@echo "  make build-arm64       Build ARM64 binary for cloud"
	@echo "  make docker-build      Build Docker image"
	@echo ""
	@echo "Maintenance:"
	@echo "  make clean             Clean build artifacts"
	@echo "  make deps              Download dependencies"

# Testing targets
test:
	@echo "Running tests..."
	go test -v

test-verbose:
	@echo "Running tests (verbose)..."
	go test -v -race -timeout 30s

test-coverage:
	@echo "Generating coverage report..."
	go test -coverprofile=coverage.out -v
	go tool cover -func=coverage.out
	@echo ""
	@echo "Detailed HTML report:"
	@echo "  go tool cover -html=coverage.out"

test-race:
	@echo "Running tests with race detector..."
	go test -race -v

# Code quality targets
lint:
	@echo "Running linters..."
	go vet ./...
	@which staticcheck > /dev/null || (echo "Installing staticcheck..." && go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

fmt:
	@echo "Formatting code..."
	go fmt ./...
	@echo "Code formatted successfully"

# Build targets
build:
	@echo "Building for current OS..."
	go build -ldflags="-w -s" -o extract-job .
	@echo "Built: extract-job"

build-arm64:
	@echo "Building ARM64 binary..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o extract-job-arm64 .
	@echo "Built: extract-job-arm64"

docker-build:
	@echo "Building Docker image..."
	docker build -f rara-extract/Dockerfile -t rara-extract:latest ..
	@echo "Docker image built: rara-extract:latest"

# Maintenance
clean:
	@echo "Cleaning build artifacts..."
	rm -f extract-job extract-job-arm64 coverage.out
	@echo "Clean complete"

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod verify
	@echo "Dependencies verified"

# Convenience target: run everything
all: clean lint test build
	@echo "All checks passed!"
