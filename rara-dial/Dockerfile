FROM golang:1.26-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

# Copy only Go sources (avoids pulling unrelated files into the image layer).
COPY *.go ./

# Build a static binary for the build platform (amd64 on Cloud Build),
# which is what Cloud Run runs. No hardcoded GOARCH cross-compile.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o dial-job .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/dial-job /app/dial-job

# Run as an unprivileged user.
USER nobody

ENTRYPOINT ["/app/dial-job"]
