# Multi-module build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO ROOT,
# not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. Invoke from the root: docker build -f rara-sift/Dockerfile .
FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK first (the `replace` target), 2) this module's go.mod/go.sum.
COPY rara-addon/ ./rara-addon/
COPY rara-sift/go.mod rara-sift/go.sum ./rara-sift/

WORKDIR /build/rara-sift
RUN go mod download

# Go sources only — rara-sift has no go:embed assets.
COPY rara-sift/*.go ./

# Static binary for the build platform (amd64 on Cloud Build), which is what Cloud Run runs.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /sift-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /sift-job /app/sift-job

USER nobody

ENTRYPOINT ["/app/sift-job"]
