# Multi-module build (see ../DOCKER-MULTIMODULE.md). Build context is the MONOREPO ROOT,
# not this dir, so the `replace rara-addon => ../rara-addon` in go.mod resolves inside the
# container. Invoke from the root: docker build -f rara-scribe/Dockerfile .
#
# This image is for the CLOUD provider only — SCRIBE_PROVIDER=asr-direct, which fetches the
# enclosure over plain HTTP and transcribes via an API engine (Groq/Gemini). The asr-youtube
# provider runs natively on the Mac (residential IP, yt-dlp) via `make build` + launchd and
# does NOT use this Dockerfile.
FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

# 1) the sibling SDK first (the `replace` target), 2) this module's go.mod/go.sum.
COPY rara-addon/ ./rara-addon/
COPY rara-scribe/go.mod rara-scribe/go.sum ./rara-scribe/

WORKDIR /build/rara-scribe
RUN go mod download

# Go sources only — rara-scribe has no go:embed assets.
COPY rara-scribe/*.go ./

# Static binary for the build platform (amd64 on Cloud Build), which is what Cloud Run runs.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /scribe-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates ffmpeg

WORKDIR /app

COPY --from=builder /scribe-job /app/scribe-job

USER nobody

ENTRYPOINT ["/app/scribe-job"]
