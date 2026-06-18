# Multi-module, multi-arch build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO
# ROOT, not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. ONE image, two arches: amd64 (Cloud Run) + arm64 (VPC Oracle Ampere + Mac).
# Build with buildx from the root:
#   docker buildx build --platform linux/amd64,linux/arm64 -f rara-distill/Dockerfile .
#
# `--platform=$BUILDPLATFORM` pins the builder stage to the native runner arch and Go cross-compiles
# via GOARCH=$TARGETARCH — no QEMU for the (slow) compile; only the tiny final alpine stage runs
# emulated for the foreign arch.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK first (the `replace` target), 2) this module's go.mod/go.sum.
COPY rara-addon/ ./rara-addon/
COPY rara-distill/go.mod rara-distill/go.sum ./rara-distill/

WORKDIR /build/rara-distill
RUN go mod download

# Go sources + the go:embed library directories of THIS app (patterns/contexts/strategies).
COPY rara-distill/*.go ./
COPY rara-distill/patterns ./patterns
COPY rara-distill/contexts ./contexts
COPY rara-distill/strategies ./strategies

# Static, cross-compiled binary for the target arch (set by buildx per --platform entry).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-w -s" -o /distill-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /distill-job /app/distill-job

USER nobody

ENTRYPOINT ["/app/distill-job"]
