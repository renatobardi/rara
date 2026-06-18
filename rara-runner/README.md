# rara-runner

The **activation arm** of the 2.0 control plane (see [ATIVACAO-UNIFICADA.pt-BR.md](../ATIVACAO-UNIFICADA.pt-BR.md)).
One binary, role subcommands — the same shape as `core-job reconcile|surface|...`:

| Subcommand | Role | Phase |
|---|---|---|
| `agent` | Resident per-host daemon (VPC + Mac): `POST /run` over the tailnet → `docker run`. The **"portable Cloud Run"**. | **F1 (this PR)** |
| `dispatch` | Perennial VPC service: reads desired state from Neon and wakes providers via the `Runner`. | F3 (not built) |

`rara-runner` is the piece that *runs* containers; `rara-addon` stays the contract workers *import*.
This module has **no `rara-addon` dependency**. The `dispatch` subcommand uses `pgx/v5` for Neon reads; the `agent` subcommand is stdlib-only.

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
| `RUNNER_WORKER_ENV_FILE` | — | Path to a `KEY=VAL` file with host-side secrets and config (e.g. `DATABASE_URL`, `LITELLM_BASE_URL`) injected into **every** container started by this agent. **Must have restrictive permissions (`chmod 600` or `640`)** — the file contains secrets. The body's `env` is merged on top — body wins on conflict, so the caller can override non-secret config but secrets only live on the host. Missing file or empty var → no base env (doesn't fail). Format: one `KEY=VAL` per line; `#` comments and blank lines are ignored; values may contain `=`. |

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

There is **no `database-runner.yml`** (no tables / migrations).

### `dispatch` on the VPC (systemd)

`deploy-runner.yml` ships `rara-runner dispatch` as an always-on systemd service on the Oracle VM —
the same native-binary + rsync + systemd pattern as `rara-core` (no Docker on the VPC). It re-arms the
`on_demand` workers: the reconciler is assignment-only, so without this nothing wakes the Cloud Run
jobs.

| Detail | Value |
|---|---|
| Binary | `rara-runner` (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, cross-compiled on `ubuntu-latest`) |
| Install path | `/opt/rara-runner/bin/rara-runner` (fixed in the workflow) |
| Config | `/etc/rara-runner/env` (chmod 640, gitignored) — see [deploy/env.example](deploy/env.example) |
| Service | `/etc/systemd/system/rara-runner-dispatch.service` ([deploy/](deploy/rara-runner-dispatch.service); Type=exec, Restart=on-failure) |
| Trigger | `workflow_dispatch` only — the first bring-up must be deliberate, after the VM has `/etc/rara-runner/env` |
| Auth | SSH key (`SSH_PRIVATE_KEY`); reuses `DEPLOY_HOST`/`DEPLOY_USER` (same VM as core) |

Prod state is **cloud-run-only**: `dispatch` wakes GCP Cloud Run jobs via `jobs:run`, authenticating
with ADC through `GOOGLE_APPLICATION_CREDENTIALS=/etc/rara-core/sa-key.json` (the SA key `rara-core`
already uses — its `rara-core-activator` holds `run.invoker`). `RUNNER_TOKEN` is intentionally unset
(no host transport yet), so no `agent` is required.

First bring-up (on the VM, once — same env convention as `rara-core`, so the `ubuntu`-run
service can read it):

```bash
sudo mkdir -p /etc/rara-runner && sudo chown ubuntu:ubuntu /etc/rara-runner && sudo chmod 750 /etc/rara-runner
cp env.example /etc/rara-runner/env && chmod 640 /etc/rara-runner/env
nano /etc/rara-runner/env   # fill in REAL values before deploying — required vars fail fast at startup:
                            #   DATABASE_URL, CLOUD_RUN_PROJECT, CLOUD_RUN_REGION, and an existing
                            #   GOOGLE_APPLICATION_CREDENTIALS file (reuse /etc/rara-core/sa-key.json).
```

Then run the workflow: `gh workflow run deploy-runner.yml`.

The `agent` daemon (VPC/Mac, `docker run`) has no deploy workflow yet — it lands with the multi-arch
image work.
