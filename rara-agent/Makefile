.PHONY: test test-race test-coverage lint build install-local clean all

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

install-local:
	@bash deploy/install-agent-mac.sh

clean:
	rm -f rara-agent coverage.out

all: clean lint test build
