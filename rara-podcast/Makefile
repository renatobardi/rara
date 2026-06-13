.PHONY: help test test-verbose test-coverage test-race lint fmt clean build docker-build deps all

help:
	@echo "rara-podcast - Makefile Commands"
	@echo ""
	@echo "Testing:"
	@echo "  make test              Run all tests"
	@echo "  make test-verbose      Run tests with verbose output + race"
	@echo "  make test-coverage     Generate coverage report"
	@echo "  make test-race         Run tests with race detector"
	@echo ""
	@echo "Code Quality:"
	@echo "  make lint              Run go vet and staticcheck"
	@echo "  make fmt               Format code with go fmt"
	@echo ""
	@echo "Build:"
	@echo "  make build             Build binary for current OS"
	@echo "  make docker-build      Build Docker image"
	@echo ""
	@echo "Maintenance:"
	@echo "  make clean             Clean build artifacts"
	@echo "  make deps              Download dependencies"

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

test-race:
	@echo "Running tests with race detector..."
	go test -race -v

lint:
	@echo "Running linters..."
	go vet ./...
	@which staticcheck > /dev/null || (echo "Installing staticcheck..." && go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

fmt:
	@echo "Formatting code..."
	go fmt ./...
	@echo "Code formatted successfully"

build:
	@echo "Building for current OS..."
	go build -ldflags="-w -s" -o podcast-job .
	@echo "Built: podcast-job"

docker-build:
	@echo "Building Docker image..."
	docker build -t rara-podcast:latest .
	@echo "Docker image built: rara-podcast:latest"

clean:
	@echo "Cleaning build artifacts..."
	rm -f podcast-job coverage.out
	@echo "Clean complete"

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod verify
	@echo "Dependencies verified"

all: clean lint test build
	@echo "All checks passed!"
