# Multi-module build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO ROOT,
# not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. Invoke from the root: docker build -f rara-core/Dockerfile .
FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK (the `replace` target), 2) this module's deps.
COPY rara-addon/ ./rara-addon/
COPY rara-core/go.mod rara-core/go.sum ./rara-core/

WORKDIR /build/rara-core
RUN go mod download

COPY rara-core/*.go ./

# Static binary for linux/amd64 (Cloud Run default arch).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /core-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /core-job /app/core-job

USER nobody

ENTRYPOINT ["/app/core-job"]
