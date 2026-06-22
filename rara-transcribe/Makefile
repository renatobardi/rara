.PHONY: help test test-verbose test-coverage test-race lint fmt clean build run install-local deps all

help:
	@echo "rara-transcribe - Makefile Commands"
	@echo ""
	@echo "Local execution:"
	@echo "  make run               Build and run one batch (requires .env)"
	@echo "  make install-local     Install binary + launchd agent (first-time setup)"
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
	go build -ldflags="-w -s" -o transcribe-job .
	@echo "Built: transcribe-job"

# Claim & drain the transcrever queue for SCRIBE_PROVIDER, then exit. Requires a .env in this
# directory (copy .env.example → .env and fill in your values, incl. SCRIBE_PROVIDER).
run: build
	@if [ ! -f .env ]; then \
		echo "Missing .env — copy .env.example and fill in real values:"; \
		echo "  cp .env.example .env && \$$EDITOR .env"; \
		exit 1; \
	fi
	@echo "Running transcribe worker (drain once)..."
	@set -a; . ./.env; set +a; ./transcribe-job

# Install the binary as a macOS launchd agent that runs daily at 02:00.
# Re-run after `make build` to update the installed binary.
install-local:
	@bash install-local.sh

clean:
	@echo "Cleaning build artifacts..."
	rm -f transcribe-job coverage.out
	@echo "Clean complete"

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod verify
	@echo "Dependencies verified"

all: clean lint test build
	@echo "All checks passed!"
