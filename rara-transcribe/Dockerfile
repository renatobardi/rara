# Multi-module, multi-arch build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO
# ROOT, not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. ONE image, two arches: amd64 (Cloud Run) + arm64 (VPC Oracle Ampere + Mac).
# Build with buildx from the root:
#   docker buildx build --platform linux/amd64,linux/arm64 -f rara-scribe/Dockerfile .
#
# `--platform=$BUILDPLATFORM` pins the builder stage to the native runner arch and Go cross-compiles
# via GOARCH=$TARGETARCH — no QEMU for the (slow) compile; only the tiny final alpine stage runs
# emulated for the foreign arch.
#
# This image is for the CLOUD provider only — SCRIBE_PROVIDER=asr-direct-audio, which fetches the
# enclosure over plain HTTP and transcribes via an API engine (Groq/Gemini). The asr-youtube
# provider runs natively on the Mac (residential IP, yt-dlp) via `make build` + launchd and
# does NOT use this Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK first (the `replace` target), 2) this module's go.mod/go.sum.
COPY rara-addon/ ./rara-addon/
COPY rara-scribe/go.mod rara-scribe/go.sum ./rara-scribe/

WORKDIR /build/rara-scribe
RUN go mod download

# Go sources only — rara-scribe has no go:embed assets.
COPY rara-scribe/*.go ./

# Static, cross-compiled binary for the target arch (set by buildx per --platform entry).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-w -s" -o /scribe-job .

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

# ffmpeg: used by asr-direct-audio to chunk audio before sending to Groq/Gemini.
# yt-dlp is intentionally absent — it is only used by the asr-youtube provider,
# which runs on the Mac (residential IP) and never inside this container.
RUN apk add --no-cache ca-certificates ffmpeg

WORKDIR /app

COPY --from=builder /scribe-job /app/scribe-job

USER nobody

ENTRYPOINT ["/app/scribe-job"]
