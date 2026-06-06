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
# ffmpeg (audio decode/resample/segment) and yt-dlp (download).
FROM debian:stable-slim

# yt-dlp is installed via pip (not the standalone binary).
# The standalone binary omits the EJS solver scripts that decode YouTube's
# n-parameter challenge. Without them yt-dlp cannot bypass the challenge and
# Cloud Run IPs are rejected with "Sign in to confirm you're not a bot".
# pip install "yt-dlp[default]" pulls in brotli, pycryptodomex, websockets,
# etc. and places the solver scripts alongside the package — the same path
# yt-dlp looks for them at runtime.
#
# Node.js provides the JavaScript runtime yt-dlp uses to execute the solver;
# priority order is deno → node → phantomjs. Bump YT_DLP_VERSION periodically
# (same maintenance as before — YouTube breaks extractors often).
ARG YT_DLP_VERSION=2026.3.17

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates ffmpeg python3 python3-pip nodejs \
    && pip3 install --break-system-packages "yt-dlp[default]==${YT_DLP_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/scribe-job /app/scribe-job

# Run as an unprivileged user (writes only to /tmp).
USER nobody

ENTRYPOINT ["/app/scribe-job"]
