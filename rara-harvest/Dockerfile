FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a static binary for the build platform (amd64 on Cloud Build),
# which is what Cloud Run runs. No hardcoded GOARCH cross-compile.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o etl-job .

FROM alpine:latest

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/etl-job /app/etl-job

ENTRYPOINT ["/app/etl-job"]
