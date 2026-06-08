# Infrastructure — rara ecosystem

The concrete infrastructure behind the four agents: GCP (for the Cloud Run agents harvest,
shelf and distill), the shared Neon database, GitHub Actions CI/CD, and the local Mac setup
(for the scribe transcriber).

> Placeholders `<PROJECT_ID>` and `<REGION>` are stored in GitHub Variables `GCP_PROJECT_ID` and
> `GCP_REGION`; real values are not committed.

## Runtime topology

| Agent | Runtime | Trigger | Where |
|-------|---------|---------|-------|
| rara-harvest | GCP Cloud Run Job | daily | GCP datacenter |
| rara-shelf | GCP Cloud Run Job | daily | GCP datacenter |
| rara-scribe | macOS `launchd` agent | daily at 02:00 | owner's Mac (residential IP) |
| rara-distill | GCP Cloud Run Job | daily, after scribe | GCP datacenter |

All four read/write the **same Neon database**, using isolated tables.

## GCP (Cloud Run: harvest + shelf + distill)

| Component | Value |
|-----------|-------|
| Project | `<PROJECT_ID>` |
| Region | `<REGION>` |
| Artifact Registry | `<REGION>-docker.pkg.dev/<PROJECT_ID>/rara/` |
| Deployer SA | `rara-deployer@<PROJECT_ID>.iam.gserviceaccount.com` |
| Auth | Workload Identity Federation (no SA key files) |
| Build | Cloud Build → amd64 images → Cloud Run Job |

Images are built **amd64** (the early arm64 default was a bug, corrected in
[rara-harvest/DEPLOY.md](./rara-harvest/DEPLOY.md)).

### Secret Manager

| Secret | Used by |
|--------|---------|
| `youtube-api-key` | rara-harvest |
| `database-url` | rara-harvest + rara-shelf |
| `shelf-oauth-client-id` | rara-shelf |
| `shelf-oauth-client-secret` | rara-shelf |
| `shelf-oauth-refresh-token` | rara-shelf |
| `gemini-api-key` | rara-distill (curation LLM, default engine) |
| `anthropic-api-key` / `groq-api-key` | rara-distill (only if `CURATE_ENGINE` is switched) |

The runtime SA (compute default) and `rara-deployer` hold `secretmanager.secretAccessor` at
project level, so adding a new secret needs no IAM change — but the secret value itself must
exist **before** the first deploy. The first distill deploy failed precisely because
`gemini-api-key` had not been created yet; the job mounts it at boot via `--set-secrets`.

## Local Mac (transcriber: scribe)

rara-scribe is **not** on GCP. It is installed by `rara-scribe/install-local.sh`, which builds
the binary, writes config, and registers a `launchd` agent.

### On-disk layout

```
~/.rara-scribe/
  rara-scribe        # compiled binary
  .env               # DATABASE_URL, GROQ_API_KEY, YT_DLP_BIN, FFMPEG_BIN, BATCH_SIZE
  run.sh             # wrapper: exports Homebrew PATH, sources .env, execs the binary
~/Library/LaunchAgents/com.rara.scribe.plist   # launchd job (daily 02:00)
~/Library/Logs/rara-scribe/{output,error}.log  # logs
```

### Why the wrapper exports PATH

`launchd` runs with a minimal `PATH` that excludes `/opt/homebrew/bin`. yt-dlp needs to find
`ffmpeg`/`ffprobe` at runtime, so `run.sh` exports `PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"`
before exec. The binary paths in `.env` (`YT_DLP_BIN`, `FFMPEG_BIN`) are absolute for the same
reason — no reliance on `$PATH` lookup.

### Secrets

scribe reads `DATABASE_URL` and `GROQ_API_KEY` from `~/.rara-scribe/.env` (gitignored, never
committed). It does **not** use GCP Secret Manager. The `groq-api-key` and `yt-dlp-cookies`
secrets from its old Cloud Run deployment can be deleted — see
[rara-scribe/DEPLOY.md](./rara-scribe/DEPLOY.md).

### Useful commands

```bash
launchctl start com.rara.scribe              # force a run now
tail -f ~/Library/Logs/rara-scribe/output.log
launchctl unload ~/Library/LaunchAgents/com.rara.scribe.plist  # stop
```

## Database (Neon, shared)

One Neon PostgreSQL instance (free tier, 0.5 GB). Tables are isolated per agent; migrations are
applied per agent by the `database-*.yml` workflows (BEGIN/ROLLBACK validation on PR, apply on
merge to `main`). Storage usage is tiny — the full transcript backlog is well under 20 MB.

## CI/CD (GitHub Actions)

Fourteen workflows, path-filtered per agent. See [.github/workflows/README.md](./.github/workflows/README.md)
for details.

| Workflow | Agent | Purpose |
|----------|-------|---------|
| `ci.yml` | harvest | fmt/vet/test/security |
| `ci-shelf.yml` | shelf | fmt/vet/test/security |
| `ci-scribe.yml` | scribe | fmt/vet/test/security |
| `ci-distill.yml` | distill | fmt/vet/test/security |
| `ci-feed.yml` | feed | fmt/vet/test/security |
| `database.yml` | harvest | migrations |
| `database-shelf.yml` | shelf | migrations |
| `database-scribe.yml` | scribe | migrations |
| `database-distill.yml` | distill | migrations |
| `database-feed.yml` | feed | migrations |
| `deploy.yml` | harvest | Cloud Run deploy |
| `deploy-shelf.yml` | shelf | Cloud Run deploy |
| `deploy-distill.yml` | distill | Cloud Run deploy |
| `deploy-feed.yml` | feed | Cloud Run deploy |

scribe has **no deploy workflow** — it is installed and updated locally with
`make build && bash install-local.sh`.

## Security controls (WIF / IAM)

This is a **public** repository. No secret values are committed — secrets live in GitHub Secrets
+ GCP Secret Manager, and GCP auth is keyless via Workload Identity Federation. Because the GCP
project ID and the `rara-deployer` SA email appear in git history, the control that prevents
abuse is the **WIF attribute condition**, not the secrecy of those identifiers. Verify
periodically:

- [ ] The WIF provider restricts token issuance to this repo only, e.g.
      `attribute.repository == 'renatobardi/rara'` (a missing/loose condition would let any
      GitHub repo impersonate `rara-deployer`).
- [ ] `rara-deployer` holds only the minimal roles it needs (Cloud Run deploy, Artifact Registry
      write, Secret Manager accessor, Cloud Build) — **never** `roles/editor` or `roles/owner`.
- [ ] Secret Manager secrets are readable only by the runtime SA, and the OAuth refresh token is
      rotated periodically.

Verify the WIF condition with:
```bash
gcloud iam workload-identity-pools providers describe <PROVIDER> \
  --location=global --workload-identity-pool=<POOL> --project <PROJECT_ID> \
  --format='value(attributeCondition)'
```

## Cost

| Item | Cost |
|------|------|
| GitHub Actions | Free (public repo) |
| Neon DB | Free tier |
| Cloud Run (harvest + shelf + distill) | ~$0.02/month each |
| Cloud Build | Free tier |
| rara-scribe compute | $0 (local Mac) |
| Groq ASR | ~$0.111/h of audio (backlog is a one-time few tens of dollars) |
| Curation LLM (distill) | per transcript; cheap on Gemini Flash, more on `gemini-2.5-pro` |
