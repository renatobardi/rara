.PHONY: help web test test-race test-coverage lint fmt clean build build-arm64 deps all

BIN := console

help:
	@echo "rara-console - Makefile Commands"
	@echo "  make web            Build the SvelteKit SPA into web/build (embedded by Go)"
	@echo "  make test           Run Go tests (BFF handlers; zero I/O)"
	@echo "  make test-race      Run tests with the race detector"
	@echo "  make lint           go vet + staticcheck"
	@echo "  make build          Build the SPA + local Go binary"
	@echo "  make build-arm64    Build the SPA + ARM64 binary (the VPC arch)"
	@echo "  make all            clean + lint + test + build"

# Frontend: SvelteKit (adapter-static) -> web/build, which the Go binary embeds.
web:
	cd web && npm ci && npm run build

# Go tests don't need the real SPA — the embed placeholder (web/build/.gitkeep) satisfies the
# compile, and the BFF handlers are tested against an httptest core.
test:
	go test -v

test-race:
	go test -race -v

test-coverage:
	go test -coverprofile=coverage.out -v
	go tool cover -func=coverage.out

lint:
	go vet ./...
	@which staticcheck > /dev/null || (echo "Installing staticcheck..." && go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

fmt:
	go fmt ./...

build: web
	go build -ldflags="-w -s" -o $(BIN) .

build-arm64: web
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o $(BIN)-arm64 .

deps:
	go mod download && go mod verify

clean:
	rm -f $(BIN) $(BIN)-arm64 coverage.out
	rm -rf web/.svelte-kit web/build
	mkdir -p web/build && touch web/build/.gitkeep   # keep the embed dir compilable

all: clean lint test build
	@echo "All checks passed!"
