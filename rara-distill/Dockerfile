FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

# Copy only what the build needs (not a recursive COPY . .): the Go sources plus the
# go:embed library directories. Keeps build context tight and avoids leaking stray
# files into the image.
COPY *.go ./
COPY patterns ./patterns
COPY contexts ./contexts
COPY strategies ./strategies

# Build a static binary for the build platform (amd64 on Cloud Build), which is what
# Cloud Run runs. The curation library (patterns/contexts/strategies) is embedded via
# go:embed, so the binary is self-contained.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o distill-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/distill-job /app/distill-job

USER nobody

ENTRYPOINT ["/app/distill-job"]
