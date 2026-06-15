# Multi-module Docker build for SDK workers

Workers that import the `rara-addon` SDK (`rara-distill`, `rara-sift`, `rara-scribe`,
`rara-glean`) couple to it via a **`replace rara-addon => ../rara-addon`** in their `go.mod`
(deliberately not a committed `go.work` — that would break the siblings' isolated CI). The
replace points one directory up, so the build needs *two* modules on disk: the app and its
sibling SDK.

A single-module Dockerfile (`COPY go.mod go.sum ./` from the app dir) can't see `../rara-addon`,
so `go mod download` / `go build` fail. The fix is to make the **build context the monorepo root**
and copy both modules in.

## The pattern

**Build context = repo root** (contains `rara-addon/` + `rara-<app>/`). The Dockerfile lives in
the app dir and is selected with `-f`:

```bash
# from the repo root
docker build -f rara-<app>/Dockerfile .
```

The Dockerfile copies the SDK first (the `replace` target), then the app, keeping `WORKDIR` at
`/build/rara-<app>` so `../rara-addon` resolves exactly as it does on disk:

```dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /build
RUN apk add --no-cache git
COPY rara-addon/ ./rara-addon/                              # 1) the replace target
COPY rara-<app>/go.mod rara-<app>/go.sum ./rara-<app>/      # 2) this module's manifests
WORKDIR /build/rara-<app>
RUN go mod download
COPY rara-<app>/*.go ./
# + any go:embed asset dirs THIS app has (distill: patterns/ contexts/ strategies/)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /<app>-job .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /<app>-job /app/<app>-job
USER nobody
ENTRYPOINT ["/app/<app>-job"]
```

Only copy the `go:embed` directories that the app actually has — `rara-distill` embeds
`patterns/contexts/strategies`; `sift`, `glean`, and the cloud `scribe` have none.

## Cloud Build

Same idea: the source tarball must contain both modules, and the docker step references the
Dockerfile with `-f`:

```bash
tar -czf /tmp/source.tgz rara-addon rara-<app>     # not `-C rara-<app> .`
# cloudbuild.yaml docker args:
#   ['build', '-t', '$IMAGE', '-f', 'rara-<app>/Dockerfile', '.']
```

See `deploy-<app>.yml` for the full workflow (modeled on `deploy-dial.yml`).

## Why not `go.work`?

A committed `go.work` would pull all sibling modules into one build graph and break each agent's
isolated, path-filtered CI. The `replace` directive keeps modules independent on disk while still
letting the SDK-coupled apps build standalone (`cd rara-<app> && make all`). The Dockerfile is
independent of `make` — it reproduces the same `../rara-addon` layout inside the container.

## CI

Each `ci-<app>.yml` has a `docker-build` job that runs `docker build -f rara-<app>/Dockerfile .`
(build only, no push) — proving the multi-module image compiles on every PR without GCP creds.

## Deploys

`deploy-<app>.yml` is `workflow_dispatch`-only (gated, no `push: main`) until the lane-activation
wave. Per-provider env (e.g. `DISTILL_PROVIDER`, `SIFT_GATE`, `SCRIBE_PROVIDER`) and app-specific
secrets are wired in at that point — see each deploy file's comments.
