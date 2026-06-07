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

## Coverage in v1 (what actually reaches distill)

Only items with a non-empty body or excerpt feed the distill news lane (the run logs a
per-type `Yield … upserted, … distillable` line). In v1 the lanes differ sharply:

- **RSS** is the high-yield lane: feeds ship a `description` (excerpt) and usually
  `content:encoded` (full body), so nearly every item is distillable.
- **HN** items reach distill only when the linked article exposes JSON-LD (HN gives no
  body itself); Ask HN / permalink posts become title-only rows distill skips.
- **html** index pages yield at most one JSON-LD node per run and often only a title —
  the unlocker/bespoke follow-up is what makes them productive.

After the first real runs, measure coverage to drive that follow-up:

```sql
SELECT source_type, fetch_status, count(*)
FROM news_items GROUP BY 1, 2 ORDER BY 1, 2;
```

A high `excerpt`/`failed` share on `html` (or low distillable HN yield) is the signal to
prioritise the unlocker tier below.

## Bright Data unlocker tier (optional)

The `UnlockerFetcher` is implemented. The job runs **direct HTTP by default** and only
uses Bright Data when `SCRAPE_PROVIDER=brightdata` is set — and even then only for
sources whose `feed_sources.fetch_strategy='unlocker'`. Everything else stays on the
cheap direct path. To enable it:

1. **Create the token secret first** (the deploy mounts it at boot — it must exist, see
   the secret-before-deploy lesson):

   ```bash
   printf %s 'YOUR_BRIGHTDATA_TOKEN' | gcloud secrets create brightdata-token \
     --data-file=- --replication-policy=automatic --project oute-rara
   ```

2. **Add the secret + env to the job** (update `deploy-feed.yml`'s `--set-secrets` /
   `--set-env-vars`, or one-off on the existing job):

   ```bash
   gcloud run jobs update rara-feed --region "$REGION" --project oute-rara \
     --update-secrets "BRIGHTDATA_TOKEN=brightdata-token:latest" \
     --update-env-vars "SCRAPE_PROVIDER=brightdata,BRIGHTDATA_ZONE=web_unlocker1"
   ```

3. **Flip the blocked sources** (data change, no redeploy) — driven by the coverage
   query above:

   ```sql
   UPDATE feed_sources SET fetch_strategy='unlocker'
   WHERE source_type='html' AND name IN ('Mistral', 'Cursor');  -- whichever are blocked
   ```

4. Re-run and confirm those sources now report `fetch_status='full'`.

## Follow-up (still open)

- **Runtime-block check**: run each `html` source from Cloud Run egress and use the
  `fetch_status` coverage query to decide which to flip to `unlocker`.
