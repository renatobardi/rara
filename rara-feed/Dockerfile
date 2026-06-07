FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

# Copy only the Go sources (not a recursive COPY . .): keeps the build context tight
# and avoids leaking stray files into the image. rara-feed has no go:embed assets.
COPY *.go ./

# Build a static binary for the build platform (amd64 on Cloud Build), which is what
# Cloud Run runs.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o feed-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/feed-job /app/feed-job

USER nobody

ENTRYPOINT ["/app/feed-job"]
