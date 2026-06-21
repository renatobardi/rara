FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder
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

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

# bdata CLI is an npm package; Node.js >= 20 required (Alpine 3.21 ships 22.x).
RUN apk add --no-cache ca-certificates nodejs npm && \
    npm install -g @brightdata/cli

RUN bdata --version

WORKDIR /app

COPY --from=builder /build/clip-job /app/clip-job

USER nobody

ENTRYPOINT ["/app/clip-job"]
