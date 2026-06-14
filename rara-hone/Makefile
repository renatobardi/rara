.PHONY: help test test-verbose test-coverage test-race lint fmt clean build build-linux deps all

help:
	@echo "rara-hone - Makefile Commands"
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
	@echo "  make build-linux       Build linux/amd64 binary for the VPC (systemd timer)"
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
	go build -ldflags="-w -s" -o hone-job .
	@echo "Built: hone-job"

build-linux:
	@echo "Building linux/amd64 binary for the VPC..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o hone-job-linux-amd64 .
	@echo "Built: hone-job-linux-amd64"

# Maintenance
clean:
	@echo "Cleaning build artifacts..."
	rm -f hone-job hone-job-linux-amd64 coverage.out
	@echo "Clean complete"

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod verify
	@echo "Dependencies verified"

# Convenience target: run everything
all: clean lint test build
	@echo "All checks passed!"
