# rara-runner

The **activation arm** of the 2.0 control plane (see [ATIVACAO-UNIFICADA.pt-BR.md](../ATIVACAO-UNIFICADA.pt-BR.md)).
One binary, role subcommands — the same shape as `core-job reconcile|surface|...`:

| Subcommand | Role | Phase |
|---|---|---|
| `agent` | Resident per-host daemon (VPC + Mac): `POST /run` over the tailnet → `docker run`. The **"portable Cloud Run"**. | **F1 (this PR)** |
| `dispatch` | Perennial VPC service: reads desired state from Neon and wakes providers via the `Runner`. | F3 (not built) |

`rara-runner` is the piece that *runs* containers; `rara-addon` stays the contract workers *import*.
This module has **no database and no `rara-addon` dependency** — it is pure stdlib.

## `rara-runner agent`

The wake contract the GCP `jobs:run` serves natively, replicated on hosts that receive wake over HTTP
(VPC Oracle + Mac): an authenticated `POST /run` starts the worker's container locally. The worker
then pulls its own work from Neon and exits — the agent only *starts* it.

```text
POST /run   {"app":"rara-distill","env":{"DISTILL_RECIPE":"opus"}}   →  202 Accepted
GET  /healthz                                                        →  200 (no auth)
```

### Security

This runs containers on a personal machine (the Mac), so the trust boundary is the whole point:

- **Tailnet-only bind.** `RUNNER_ADDR` must be an explicit host:port — a wildcard bind (`0.0.0.0`,
  `::`, or a bare `:port`) is refused at startup, so the agent is never reachable off the tailnet.
- **Bearer fails closed.** Auth is a constant-time compare (mirrors `rara-addon/poke.go`); an empty
  `RUNNER_TOKEN` rejects *every* request.
- **Image allowlist.** The image is resolved **only** from `RUNNER_ALLOWED_IMAGES` (`app` → digest-pinned
  image). A request names an `app`, never an image — an arbitrary image can never be run.
- **No secrets from the wire.** `env` in the body is non-secret per-run config. Secret injection from
  the provider row / Secret Manager is resolved locally by the runner (a later phase), never trusted
  from the request body.

### Configuration (env-only; required vars fail fast)

| Var | Required | Meaning |
|---|---|---|
| `RUNNER_ADDR` | ✅ | Tailnet bind `host:port` (never `0.0.0.0`/`:port`). |
| `RUNNER_TOKEN` | ✅ | Shared tailnet Bearer; empty ⇒ all requests rejected. |
| `RUNNER_ALLOWED_IMAGES` | ✅ | `app=image,app2=image2`; each image **must** be pinned by digest (`@sha256:`). A duplicate app or an unpinned image fails startup. |
| `RUNNER_DOCKER_BIN` | — | Launcher binary; default `docker`. Must be `docker`, `podman`, or an absolute path (a relative name is refused so PATH can't be hijacked). `podman` is rootless — a container escape stays in the user namespace instead of reaching the host as root. |

See [.env.example](.env.example).

### Run

```bash
make test          # zero-I/O unit tests (container launcher is a fake)
make build         # ./rara-runner
RUNNER_ADDR=100.x.x.x:8473 RUNNER_TOKEN=… RUNNER_ALLOWED_IMAGES=rara-distill=…@sha256:… \
  ./rara-runner agent
```

## CI / deploy

`ci-runner.yml` runs vet + staticcheck + tests + govulncheck/gitleaks on every change.

There is **no `database-runner.yml`** (no tables / migrations) and **no `deploy-runner.yml`** (the agent
is a host daemon, not a Cloud Run job). How it's installed on the VPC/Mac (systemd / launchd) lands
with the multi-arch image work in F2/F3.
