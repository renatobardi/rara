# Infrastructure — rara ecosystem (2.0)

The rara 2.0 topology is four zones: a VPC always-on brain (`rara-core`), a
residential Mac (audio transcription), GCP Cloud Run on-demand workers, and the
shared Neon database. The control plane lives in the VPC; workers are dispatched
from there.

For the 1.0 agent-by-agent design see `ARCHITECTURE.md`. For the 2.0 control
plane design see `ARCHITECTURE-2.0.pt-BR.md`.

> Placeholders `<PROJECT_ID>` and `<REGION>` are stored in GitHub Variables
> `GCP_PROJECT_ID` and `GCP_REGION`; real values are not committed.

---

## Runtime topology

| Component | Runtime | Where | How it runs |
|-----------|---------|-------|-------------|
| **rara-core** (reconciler + surface) | Oracle ARM VM | VPC | systemd service, always-on; `reconcile --loop` embeds the HTTP/MCP surface when `SURFACE_ADDR` is set |
| **rara-scribe** | macOS `launchd` | owner's Mac (residential IP) | daily at 02:00 via launchd; YouTube blocks datacenter IPs |
| **rara-distill** | GCP Cloud Run Job | GCP datacenter | on-demand, dispatched by rara-core (P2) |
| **rara-sift** | GCP Cloud Run Job | GCP datacenter | on-demand, dispatched by rara-core (P2) |
| **rara-harvest** | GCP Cloud Run Job | GCP datacenter | daily scheduled job |
| **rara-shelf** | GCP Cloud Run Job | GCP datacenter | daily scheduled job |
| **rara-feed** | GCP Cloud Run Job | GCP datacenter | daily scheduled job |
| **rara-dial** | GCP Cloud Run Job | GCP datacenter | on-demand / scheduled |
| **rara-courier** | GCP Cloud Run Job | GCP datacenter | on-demand / scheduled |

All components read/write the **same Neon database**, using isolated tables.
Cross-agent coupling is database-only — no agent calls another's API.

---

## VPC (rara-core: Oracle ARM VM)

### Deploy pattern

Mirrors the kura-server pattern: native binary + rsync + systemd, no Docker.

| Detail | Value |
|--------|-------|
| VM | Oracle ARM (Ampere A1, Ubuntu 22.04 aarch64) |
| Binary | `core-job` (CGO_ENABLED=0 GOOS=linux GOARCH=arm64, cross-compiled on `ubuntu-latest`) |
| Install path | `/opt/rara-core/bin/core-job` |
| Config | `/etc/rara-core/env` (chmod 640, gitignored) |
| Service | `/etc/systemd/system/rara-core.service` (Type=exec, Restart=on-failure) |
| Trigger | `workflow_dispatch` on `.github/workflows/deploy-core.yml` |
| Auth | SSH key (`SSH_PRIVATE_KEY` GitHub Secret) |

Deploy artifacts: `rara-core/deploy/` — service unit, env.example, RUNBOOK.md.

### Network access

- **Public IP**: SSH only (port 22). Port 8080 should be blocked at the Oracle firewall or OS firewall (`ufw`).
- **Tailscale**: the control surface (`SURFACE_ADDR=100.x.x.x:8080`) is reachable only over the Tailnet — from your Mac, from Kura, from any authorized node.
- The surface binds explicitly to the Tailscale IP; it never listens on `0.0.0.0`.

### Subcommands in production

| Command | Purpose |
|---------|---------|
| `core-job reconcile --loop` | Always-on (systemd); embeds surface when `SURFACE_ADDR` set |
| `core-job seed` | One-shot lane config seed (idempotent) |
| `core-job ingest --lane <lane>` | Populate items spine from collector tables |

---

## Local Mac (rara-scribe)

rara-scribe runs under `launchd`, installed by `rara-scribe/install-local.sh`.

```
~/.rara-scribe/
  rara-scribe          # compiled binary
  .env                 # DATABASE_URL, GROQ_API_KEY, SCRIBE_PROVIDER, ...
  run.sh               # exports Homebrew PATH; sources .env; execs binary
~/Library/LaunchAgents/com.rara.scribe.plist   # daily 02:00
~/Library/Logs/rara-scribe/{output,error}.log
```

`launchd` runs with minimal `PATH`; `run.sh` prepends `/opt/homebrew/bin` so
`yt-dlp`/`ffmpeg` are found. Binary paths in `.env` are absolute.

Secrets are read from `~/.rara-scribe/.env` (gitignored). GCP Secret Manager is
not used by scribe.

---

## GCP (Cloud Run workers)

| Component | Value |
|-----------|-------|
| Project | `<PROJECT_ID>` |
| Region | `<REGION>` |
| Artifact Registry | `<REGION>-docker.pkg.dev/<PROJECT_ID>/rara/` |
| Deployer SA | `rara-deployer@<PROJECT_ID>.iam.gserviceaccount.com` |
| Auth | Workload Identity Federation (no SA key files) |
| Build | Cloud Build → arm64 images → Cloud Run Job |

Workers that import `rara-addon` via `replace rara-addon => ../rara-addon` require
a multi-module Docker build (rara-addon + worker in the same build context).
`rara-distill` and `rara-sift` have their deploy workflows gated until the
multi-module build is wired (P2).

### Secret Manager

| Secret | Used by |
|--------|---------|
| `youtube-api-key` | rara-harvest |
| `database-url` | all Cloud Run workers |
| `shelf-oauth-client-id/secret/refresh-token` | rara-shelf |
| `gemini-api-key` | rara-distill (default curate engine) |
| `anthropic-api-key` / `groq-api-key` | rara-distill (optional engines) |

---

## Database (Neon, shared)

One Neon PostgreSQL instance. Tables are isolated per agent; no FK crosses agent
boundaries. Migrations are applied per agent by `database-*.yml` (BEGIN/ROLLBACK
on PR, apply on merge to `main`).

---

## CI/CD (GitHub Actions)

Path-filtered per agent — a change to `rara-core/**` only triggers core's three
workflows.

| Workflow | Agent | Purpose |
|----------|-------|---------|
| `ci-core.yml` | core | vet + staticcheck + tests (also triggers on rara-addon changes) |
| `ci-addon.yml` | addon | SDK unit tests |
| `ci-distill.yml` | distill | vet + tests |
| `ci-scribe.yml` | scribe | vet + tests |
| `ci-sift.yml` | sift | vet + tests |
| `ci-shelf.yml` | shelf | vet + tests |
| `ci-feed.yml` | feed | vet + tests |
| `ci.yml` | harvest | vet + tests |
| `database-core.yml` | core | migrations |
| `database-distill.yml` | distill | migrations |
| `database-scribe.yml` | scribe | migrations |
| `database-shelf.yml` | shelf | migrations |
| `database-feed.yml` | feed | migrations |
| `database.yml` | harvest | migrations |
| `deploy-core.yml` | core | rsync + systemd (VPC, `workflow_dispatch`) |
| `deploy-feed.yml` | feed | Cloud Run |
| `deploy-shelf.yml` | shelf | Cloud Run |
| `deploy.yml` | harvest | Cloud Run |

rara-scribe: no deploy workflow — installed locally with `make build && bash install-local.sh`.  
rara-distill / rara-sift: deploy workflows gated pending multi-module Docker build (P2).

---

## Security controls

Public repo — no secret values committed. GCP auth is keyless (WIF). Verify
periodically:

- [ ] WIF provider restricts token issuance to `attribute.repository == 'renatobardi/rara'`.
- [ ] `rara-deployer` holds only minimal roles (Cloud Run deploy, Artifact Registry write,
      Secret Manager accessor, Cloud Build).
- [ ] Oracle VM port 8080 is blocked on the public interface (surface is Tailscale-only).
- [ ] `/etc/rara-core/env` is `chmod 640` (owner ubuntu, no world read).

```bash
# Verify WIF attribute condition
gcloud iam workload-identity-pools providers describe <PROVIDER> \
  --location=global --workload-identity-pool=<POOL> --project <PROJECT_ID> \
  --format='value(attributeCondition)'
```
