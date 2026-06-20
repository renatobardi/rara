# Infrastructure — rara ecosystem

The rara topology is four zones: a VPC always-on control plane, a residential Mac for
YouTube transcription, GCP Cloud Run for on-demand workers, and a shared Neon database.
The control plane (core + runner + console) lives on the VPC; workers are dispatched from there.

For the full 2.0 design see `ARCHITECTURE-2.0.pt-BR.md`. For the 1.0 agent-by-agent design
see `ARCHITECTURE.md`.

> Placeholders `<PROJECT_ID>` and `<REGION>` are stored in GitHub Variables
> `GCP_PROJECT_ID` and `GCP_REGION`; real values are not committed.

---

## Runtime topology

| Component | Where | Runtime | How it runs |
|-----------|-------|---------|-------------|
| **rara-core** (reconciler + surface) | VPC Oracle | systemd | always-on; `reconcile --loop` embeds HTTP/MCP surface on Tailnet |
| **rara-console** (operator panel) | VPC Oracle | systemd | always-on; SvelteKit SPA embedded in Go binary, Tailnet |
| **rara-runner-dispatch** | VPC Oracle | systemd | always-on; reads `item_steps` + collectors, wakes workers by transport |
| **rara-runner-agent** | VPC Oracle | systemd | always-on; HTTP daemon → `docker run` for VPC-local workers |
| **LiteLLM (VPC)** | VPC Oracle | Docker container | serves VPC-local workers (groq models, bridge `172.17.0.1:4010`) |
| **rara-scribe / asr-youtube** | Mac | launchd | residential IP; daily 02:00; yt-dlp + ffmpeg native |
| **Cloud Run Jobs** (collectors + workers) | GCP | Cloud Run Job | on-demand; dispatched via `jobs:run` by rara-runner-dispatch |
| **LiteLLM (cloud)** | GCP | Cloud Run Service | gateway for Cloud Run workers; scales to zero |
| **Neon** | managed | PostgreSQL | shared state for all components |

All components share the same Neon database via isolated tables. Cross-component coupling
is database-only — no component calls another's API.

---

## VPC Oracle (Oracle ARM VM, aarch64)

Always-on services run as native arm64 binaries under systemd. No Docker required for core,
console, or runner — Docker is installed on the VM only for the VPC-local workers the agent spawns.

### rara-core

| Detail | Value |
|--------|-------|
| Binary | `core-job` (CGO_ENABLED=0 GOOS=linux GOARCH=arm64) |
| Install path | `/opt/rara-core/bin/core-job` |
| Config | `/etc/rara-core/env` (chmod 640) |
| Service | `rara-core.service` (Type=exec, Restart=on-failure) |
| Trigger | push to `main` on `rara-core/**` or `rara-addon/**` paths |
| GCP auth | SA key at `GOOGLE_APPLICATION_CREDENTIALS` in env (used by activator) |

Subcommands in production:

| Command | Purpose |
|---------|---------|
| `core-job reconcile --loop` | Always-on reconciler; embeds HTTP/MCP surface when `SURFACE_ADDR` is set |
| `core-job seed` | Idempotent lane/provider config seed (preserves runner_url, env, heartbeats) |
| `core-job ingest --lane <lane>` | Populate items from collector tables |

### rara-console

| Detail | Value |
|--------|-------|
| Binary | `console` (SvelteKit SPA embedded via `embed.FS`, arm64) |
| Install path | `/opt/rara-console/bin/console` |
| Service | `rara-console.service` |
| Trigger | `workflow_dispatch` |

### rara-runner (dispatch + agent)

| Detail | Value |
|--------|-------|
| Binary | `rara-runner` (arm64, single binary with `dispatch` and `agent` subcommands) |
| Install path | `/opt/rara-runner/bin/rara-runner` |
| Config (dispatch) | `/etc/rara-runner/env` |
| Config (agent) | `/etc/rara-runner/agent.env` — `RUNNER_ALLOWED_IMAGES` is path-only (no digest pins) |
| Services | `rara-runner-dispatch.service`, `rara-runner-agent.service` |
| Trigger | `workflow_dispatch` only (deliberate; VM must have both env files ready) |

`dispatch` reads `item_steps` with assigned providers and collectors past their `collect_cadence_seconds`
+ `last_collect_at`, then wakes each by transport: `runtime=cloudrun` → `gcloud run jobs execute`;
`runner_url` set → POST with Bearer to the agent on Tailnet. `agent` does `docker run --pull=always`
with an allowlist check on the image path; fail-closed (Bearer + allowlist).

### Network

- **Public IP**: SSH only (port 22). Port 8080 blocked at the Oracle firewall.
- **Tailscale**: surface (`SURFACE_ADDR`) and agent bind to the Tailscale IP — never `0.0.0.0`.
  Reachable from Mac, KURA, and any authorized Tailnet node.

---

## Mac (rara-scribe / asr-youtube)

`asr-youtube` runs natively under `launchd` — not containerized by design (residential IP +
yt-dlp needs frequent updates outside of a fixed image).

```
~/.rara-scribe/
  rara-scribe          # compiled binary
  .env                 # DATABASE_URL, GROQ_API_KEY, SCRIBE_PROVIDER, YT_DLP_BIN, FFMPEG_BIN, ...
  run.sh               # prepends Homebrew PATH; sources .env; execs binary
~/Library/LaunchAgents/com.rara.scribe.plist   # daily 02:00
~/Library/Logs/rara-scribe/{output,error}.log
```

Secrets are in `~/.rara-scribe/.env` (gitignored). No deploy workflow — installed and updated
manually with `make build && bash install-local.sh` from the `rara-scribe` directory.

---

## GCP Cloud Run

| Detail | Value |
|--------|-------|
| Project | `<PROJECT_ID>` |
| Region | `<REGION>` |
| Artifact Registry | `<REGION>-docker.pkg.dev/<PROJECT_ID>/rara/` |
| Deployer SA | `rara-deployer@<PROJECT_ID>.iam.gserviceaccount.com` |
| Auth | Workload Identity Federation (no SA key files in Cloud Run) |

### Cloud Run Jobs

On-demand, dispatched by `rara-runner dispatch` via `gcloud run jobs execute`.
Job name = `rara-` + provider name (what the dispatcher wakes).

| Job(s) | App image | Capability |
|--------|-----------|------------|
| `rara-harvest`, `rara-shelf`, `rara-feed` | harvest / shelf / feed | coletar (YouTube API, Spotify, RSS) |
| `rara-dial` | dial | coletar (podcasts) |
| `rara-courier` | courier | coletar (email) |
| `rara-clip` | clip | coletar (LinkedIn via Bright Data proxies) |
| `rara-gate-barato`, `rara-gate-rico` | sift | gate_barato / gate_rico |
| `rara-distill` | distill | destilar (llama-3.3-70b via LiteLLM) |
| `rara-asr-direct-audio` | scribe | transcrever (audio via direct URL, no residential IP needed) |
| `rara-extrair-email`, `rara-extrair-linkedin`, `rara-extrair-news` | glean | extrair |
| `rara-hone` | hone | revise (triggered by Cloud Scheduler daily) |

Workers built with `docker buildx` produce multi-arch manifest lists (amd64 + arm64). Cloud Run
pulls the amd64 leaf automatically; the same image is also pulled by the VPC agent for arm64.

### Cloud Run Service

| Service | Purpose |
|---------|---------|
| `litellm` | LiteLLM gateway for Cloud Run workers; scales to zero. Routes: `groq-fast` (gates), `groq-llama` (distill). Config baked into image from `rara-distill/litellm/`. |

### Secret Manager

| Secret | Used by |
|--------|---------|
| `database-url` | all Cloud Run workers |
| `youtube-api-key` | rara-harvest |
| `shelf-oauth-client-id`, `shelf-oauth-client-secret`, `shelf-oauth-refresh-token` | rara-shelf |
| `gemini-api-key`, `groq-api-key`, `anthropic-api-key`, `deepseek-api-key` | LiteLLM Service |
| `litellm-api-key` | LiteLLM master key |

---

## Database (Neon)

One Neon PostgreSQL instance. Connection string is the **direct** endpoint (no `-pooler`); pgx
uses simple protocol. Tables are isolated per agent; no FK crosses agent boundaries.

Migrations are applied per agent by `database-*.yml`:
- **PR**: runs inside `BEGIN; … ROLLBACK;` (validates, never commits).
- **Merge to `main`**: applies for real.

Branch-per-PR is enabled via Neon's GitHub integration.

---

## CI/CD (GitHub Actions)

Path-filtered per agent — a change to `rara-core/**` only triggers core's workflows.

**VPC deploys** (rsync + SSH + systemd):

| Workflow | Trigger |
|----------|---------|
| `deploy-core.yml` | push to `main` (paths: `rara-core/**`, `rara-addon/**`) + `workflow_dispatch` |
| `deploy-console.yml` | `workflow_dispatch` |
| `deploy-runner.yml` | `workflow_dispatch` |

**Cloud Run deploys** (WIF keyless auth, Cloud Build or `docker buildx`):

| Workflow | Trigger |
|----------|---------|
| `deploy-harvest.yml`, `deploy-shelf.yml`, `deploy-feed.yml` | push to `main` + `workflow_dispatch` |
| `deploy-dial.yml`, `deploy-courier.yml`, `deploy-clip.yml`, `deploy-hone.yml` | push to `main` + `workflow_dispatch` |
| `deploy-sift.yml`, `deploy-distill.yml`, `deploy-scribe.yml`, `deploy-glean.yml` | `workflow_dispatch` only |
| `deploy-litellm.yml` | push to `main` (path: `rara-distill/litellm/**`) + `workflow_dispatch` |

The 2.0 claim-workers (sift, distill, scribe, glean) are manual-dispatch only because they only
do work once the reconciler routes `item_steps` to their provider — deploying them on every push
would be noise.

---

## Security controls

Public repo — no secret values committed. GCP auth is keyless (WIF). Verify periodically:

- [ ] WIF provider restricts token issuance to `attribute.repository == 'renatobardi/rara'`.
- [ ] `rara-deployer` holds only minimal roles (Cloud Run deploy, Artifact Registry write,
      Secret Manager accessor).
- [ ] Oracle VM port 8080 is blocked on the public interface (surface is Tailscale-only).
- [ ] `/etc/rara-core/env` is `chmod 640` (owner ubuntu, no world read).

```bash
# Verify WIF attribute condition
gcloud iam workload-identity-pools providers describe <PROVIDER> \
  --location=global --workload-identity-pool=<POOL> --project <PROJECT_ID> \
  --format='value(attributeCondition)'
```
