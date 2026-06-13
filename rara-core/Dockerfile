FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

# Copy only the Go sources (not a recursive COPY . .): keeps the build context tight
# and avoids leaking stray files into the image. rara-core has no go:embed assets.
COPY *.go ./

# Build a static binary. rara-core is the always-on control plane (VPC resident); this
# image is what runs the reconciler once it lands. Phase 0 ships the scaffold binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o core-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/core-job /app/core-job

USER nobody

ENTRYPOINT ["/app/core-job"]
