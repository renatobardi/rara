.PHONY: test test-race test-coverage lint build build-arm64 install-local clean all

test:
	go test -v ./...

test-race:
	go test -race -v ./...

test-coverage:
	go test -coverprofile=coverage.out -v ./...
	go tool cover -func=coverage.out

lint:
	go vet ./...
	@which staticcheck > /dev/null || (echo "Installing staticcheck..." && go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

build:
	go build -ldflags="-w -s" -o rara-agent .

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o rara-agent-arm64 .

install-local:
	@bash deploy/install-agent-mac.sh

clean:
	rm -f rara-agent rara-agent-arm64 coverage.out

all: clean lint test build
