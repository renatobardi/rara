# Deploying rara-feed

rara-feed runs as a **Cloud Run Job**, triggered daily by Cloud Scheduler **before**
the rara-distill job (so the day's news is available when distill runs). It mirrors
the deploy of rara-harvest, with **no LLM and no Bright Data secrets** in v1.

## Prerequisites

- The shared Neon `DATABASE_URL` in Secret Manager (reused, secret name `database-url`).
- Workload Identity Federation already configured for the repo (same as the other
  agents) — no service-account key files.
- GitHub repo variables: `GCP_PROJECT_ID`, `GCP_REGION`, plus the WIF provider/SA vars
  used by the existing deploy workflows.

## Database migration

Applied by the `Database Migrations (feed)` workflow
(`.github/workflows/database-feed.yml`):

1. On PR: validates each file in `rara-feed/migrations/` with `BEGIN; … ROLLBACK;`.
2. On merge to `main` (or manual `migrate` dispatch): applies the migrations and
   verifies `feed_sources` / `news_items` exist with their indexes.

Dry-run locally against a Neon branch:

```bash
psql "$DATABASE_URL" -c 'BEGIN;' -f migrations/001_initial_schema.sql -c 'ROLLBACK;'
```

## Deploy

The `Deploy rara-feed` workflow (`.github/workflows/deploy-feed.yml`) on push to
`main` (paths `rara-feed/**`) or manual dispatch:

1. Authenticates to Google Cloud via WIF.
2. Builds the image with Cloud Build and pushes to Artifact Registry.
3. Creates/updates the Cloud Run Job `rara-feed` with `--set-secrets DATABASE_URL=database-url:latest`.
4. Optionally executes the job once (`run_after_deploy=true`).

## Scheduling

Create the daily trigger once (adjust the cron to run before the distill schedule):

```bash
gcloud scheduler jobs create http rara-feed-daily \
  --schedule="0 6 * * *" --time-zone="America/Sao_Paulo" \
  --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-feed:run" \
  --http-method=POST --oauth-service-account-email="${RUNNER_SA}"
```

## Follow-up (not in v1)

- **Runtime-block check**: run each `html` source from Cloud Run; any that returns an
  empty/blocked page → `UPDATE feed_sources SET fetch_strategy='unlocker' WHERE …`
  (data change, no redeploy).
- **Unlocker tier**: add the `UnlockerFetcher`, wire `--set-secrets brightdata-token`,
  set `SCRAPE_PROVIDER=brightdata`, and smoke a protected site (e.g. Mistral).
