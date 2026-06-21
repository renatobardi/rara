FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags="-w -s" \
    -o clip-job .

FROM alpine:3.21

# bdata CLI is an npm package; Node.js >= 20 required (Alpine 3.21 ships 22.x).
RUN apk add --no-cache ca-certificates nodejs npm && \
    npm install -g @brightdata/cli

RUN bdata --version

WORKDIR /app

COPY --from=builder /build/clip-job /app/clip-job

USER nobody

ENTRYPOINT ["/app/clip-job"]
