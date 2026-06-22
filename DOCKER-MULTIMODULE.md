# Multi-module Docker build for SDK workers

Workers that import the `rara-addon` SDK (`rara-distill`, `rara-gate`, `rara-transcribe`,
`rara-extract`) couple to it via a **`replace rara-addon => ../rara-addon`** in their `go.mod`
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
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder    # pin builder to the native runner
ARG TARGETOS                                                    # arch; buildx sets these per
ARG TARGETARCH                                                  # --platform entry
WORKDIR /build
RUN apk add --no-cache git
COPY rara-addon/ ./rara-addon/                              # 1) the replace target
COPY rara-<app>/go.mod rara-<app>/go.sum ./rara-<app>/      # 2) this module's manifests
WORKDIR /build/rara-<app>
RUN go mod download
COPY rara-<app>/*.go ./
# + any go:embed asset dirs THIS app has (distill: patterns/ contexts/ strategies/)
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-w -s" -o /<app>-job .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /<app>-job /app/<app>-job
USER nobody
ENTRYPOINT ["/app/<app>-job"]
```

Only copy the `go:embed` directories that the app actually has — `rara-distill` embeds
`patterns/contexts/strategies`; `gate`, `extract`, and the cloud `transcribe` have none.

## Multi-arch (the `rara-gate` pattern)

The shipped artifact is **one image, two arches** — `amd64` for Cloud Run (the only x86 host) and
`arm64` for the VPC Oracle (Ampere) and Mac (Apple Silicon) runner hosts, which both `docker run`
the same image. Built with `docker buildx` as a **manifest list**; Cloud Run pulls the `amd64` leaf
automatically, the runner hosts pull `arm64`.

The Dockerfile above is already multi-arch: `--platform=$BUILDPLATFORM` keeps the builder stage on
the runner's native arch and Go **cross-compiles** via `GOOS=$TARGETOS GOARCH=$TARGETARCH` — so the
slow compile never runs under QEMU. Only the tiny final `alpine` stage (`apk add ca-certificates` +
`COPY`) runs emulated for the foreign arch, which is cheap.

Build + push the manifest list (context = repo root, Dockerfile via `-f`):

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -f rara-<app>/Dockerfile -t "$IMAGE" --push .
docker buildx imagetools inspect "$IMAGE"   # must list amd64 + arm64
```

`rara-gate` is the first app on this pattern (`deploy-gate.yml` builds the manifest list once in a
`build` job, then deploys its Cloud Run Jobs from it in a `deploy` matrix). The other SDK workers
(`distill`/`extract`/`transcribe`) are **not migrated yet** — they still build single-arch via the shared
`_reusable-deploy-worker.yml` + Cloud Build below. Copy the `deploy-gate.yml` shape to migrate them
(do not change the shared reusable workflow until they all move at once).

## Cloud Build (single-arch — pre-migration apps)

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

Each `ci-<app>.yml` has a `docker-build` job that builds the multi-module image on every PR without
GCP creds. Pre-migration apps run `docker build -f rara-<app>/Dockerfile .` (single-arch, build
only). `ci-gate.yml` is the multi-arch reference: it buildx-builds **both** arches and pushes to a
throwaway `registry:2` service container, then `docker buildx imagetools inspect` asserts the
manifest list carries amd64 + arm64 — the PR-time proof of the F2 acceptance.

## Deploys

`deploy-<app>.yml` is `workflow_dispatch`-only (gated, no `push: main`) until the lane-activation
wave. Per-provider env (e.g. `DISTILL_PROVIDER`, `SIFT_GATE`, `SCRIBE_PROVIDER`) and app-specific
secrets are wired in there — see each deploy file's comments. `deploy-gate.yml` is self-contained
(buildx multi-arch → manifest list in Artifact Registry → deploy); the rest still call
`_reusable-deploy-worker.yml` (single-arch Cloud Build) until migrated.
