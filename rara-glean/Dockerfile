# Multi-module build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO ROOT,
# not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. Invoke from the root: docker build -f rara-glean/Dockerfile .
FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK first (the `replace` target), 2) this module's go.mod/go.sum.
COPY rara-addon/ ./rara-addon/
COPY rara-glean/go.mod rara-glean/go.sum ./rara-glean/

WORKDIR /build/rara-glean
RUN go mod download

# Go sources only — rara-glean has no go:embed assets.
COPY rara-glean/*.go ./

# Static binary for the build platform (amd64 on Cloud Build), which is what Cloud Run runs.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /glean-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /glean-job /app/glean-job

USER nobody

ENTRYPOINT ["/app/glean-job"]
