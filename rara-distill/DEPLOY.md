# Deploy — Cloud Run via GitHub Actions

The [`deploy-distill.yml`](../.github/workflows/deploy-distill.yml) workflow builds
the image with **Cloud Build** and deploys a **Cloud Run Job**, authenticating to GCP
with **Workload Identity Federation** (no service-account key files). It reuses the
shared `rara` ecosystem infrastructure (Artifact Registry repo, deployer SA, WIF
pool) set up in [rara-harvest/DEPLOY.md](../rara-harvest/DEPLOY.md) — if that is
already done, you only need the curation secret below.

## 1. App secret (Secret Manager)

rara-distill reuses the existing `database-url` secret and adds the LLM key(s) for the
engine(s) you intend to run. Create only what you need:

```bash
# Gemini (default engine)
printf '%s' 'YOUR_GEMINI_API_KEY' | \
  gcloud secrets create gemini-api-key --replication-policy=automatic --data-file=-

# Optional: Anthropic / Groq if you switch CURATE_ENGINE
printf '%s' 'YOUR_ANTHROPIC_API_KEY' | \
  gcloud secrets create anthropic-api-key --replication-policy=automatic --data-file=-
printf '%s' 'YOUR_GROQ_API_KEY' | \
  gcloud secrets create groq-api-key --replication-policy=automatic --data-file=-
```

Grant the deployer/runtime SA `roles/secretmanager.secretAccessor` on the new
secret(s) if you use a hardened runtime SA (the default compute SA already has access
via the project-level grant from the harvest setup).

> **Create the secret before merging.** The Cloud Run Job mounts `gemini-api-key` at
> boot via `--set-secrets`, so the deploy fails (`Secret .../gemini-api-key/versions/latest
> was not found`) if the secret does not exist yet. If you hit this, create the secret
> and re-run the failed deploy: `gh run rerun <run_id> --failed`.

## 2. Deploy

- **Automatic**: merge anything under `rara-distill/**` to `main`.
- **Manual**: Actions → **Deploy distill to Cloud Run** → *Run workflow*.

The workflow builds the image, creates/updates the `rara-distill` Cloud Run Job, and
executes it once. The job is configured with `--set-secrets` for `DATABASE_URL`,
`GEMINI_API_KEY` (and the other LLM keys if present) and the non-secret env
(`CURATE_ENGINE`, `DISTILL_PATTERNS`, `DISTILL_BATCH_SIZE`, etc.) via `--set-env-vars` —
adjust in the workflow as needed. `DISTILL_BATCH_SIZE` defaults to `1` while validating;
raise it (in the workflow or with a one-off `gcloud run jobs update`) to drain the
backlog:

```bash
gcloud run jobs update rara-distill --region "$REGION" \
  --update-env-vars DISTILL_BATCH_SIZE=25
```

View executions and logs:

```bash
gcloud run jobs executions list --job rara-distill --region "$REGION"
```

## 3. Schedule (daily, after scribe)

scribe runs locally at 02:00; schedule distill a bit later so fresh transcripts are
available:

```bash
gcloud scheduler jobs create http rara-distill-daily \
  --location="$REGION" --schedule="0 4 * * *" \
  --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-distill:run" \
  --http-method=POST \
  --oauth-service-account-email="rara-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```

## Notes

- The image is built **amd64** (Cloud Run's architecture).
- The curation library (`patterns/`, `contexts/`, `strategies/`) is embedded into the
  binary via `go:embed`, so the container is self-contained — no volume mounts.
- Memory: a long transcript plus a large completion is comfortable in 512Mi; bump
  `--memory`/`--task-timeout` in the workflow if you run large session chains.
- This agent only reads Neon and calls an LLM HTTP API — no audio download — so a
  datacenter IP is fine (unlike scribe, which must run locally).
