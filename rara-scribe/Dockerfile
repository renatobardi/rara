FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

# Copy only Go sources (avoids pulling unrelated files into the image layer).
COPY *.go ./

# Build a static binary (CGO disabled) so it runs unchanged on the Debian
# runtime image below. amd64 on Cloud Build, which is what Cloud Run runs.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o scribe-job .

# Debian (glibc) runtime: unlike the other agents, rara-scribe shells out to
# ffmpeg (audio decode/resample/segment) and yt-dlp (download). The self-contained
# yt-dlp_linux binary is glibc-based, so Alpine/musl is not an option here.
FROM debian:stable-slim

# Pinned yt-dlp release. YouTube breaks extractors often — bump this periodically
# (same maintenance as `pip install -U yt-dlp`).
ARG YT_DLP_VERSION=2026.03.17

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg curl \
    && curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${YT_DLP_VERSION}/yt-dlp_linux" \
         -o /usr/local/bin/yt-dlp \
    && chmod +x /usr/local/bin/yt-dlp \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/scribe-job /app/scribe-job

# Run as an unprivileged user (writes only to /tmp).
USER nobody

ENTRYPOINT ["/app/scribe-job"]
