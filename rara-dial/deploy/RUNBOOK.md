# rara-dial — deploy runbook

`rara-dial` is the **podcast collector**: it reads `podcast_feeds`, fetches each RSS feed, and
upserts audio episodes into `podcast_episodes`. It's a pure producer (its own Go module, does **not**
import `rara-addon`) and runs as a **Cloud Run Job on a cadence** — it is *not* woken by the
reconciler.

## What the CI does (automatic)

`.github/workflows/deploy-dial.yml` runs on `push: main` touching `rara-dial/**` (or via
`workflow_dispatch`):

1. Builds the amd64 image with Cloud Build and pushes it to Artifact Registry.
2. `gcloud run jobs deploy/replace` the `rara-dial` job (`--set-secrets DATABASE_URL=database-url:latest`).
3. Executes the job once after deploy (push, or `run_after_deploy=true`).

Nothing below is done by CI — it's the one-time GCP setup the operator does by hand.

## Ops checklist (run once, in GCP)

1. **Artifact Registry** — repo `rara` must exist in the region:
   ```bash
   gcloud artifacts repositories create rara \
     --repository-format=docker --location="$GCP_REGION" --project "$GCP_PROJECT_ID"
   ```
   (Already exists — shared with `rara-feed` et al. Skip if present.)

2. **WIF + deployer SA** — reuse the same SA `deploy-feed` already uses. It needs:
   `roles/artifactregistry.writer`, `roles/run.developer`, `roles/iam.serviceAccountUser`.
   GitHub repo secrets `GCP_WORKLOAD_IDENTITY_PROVIDER` and `GCP_SERVICE_ACCOUNT` are already set.

3. **Runtime SA + secret** — the job's runtime SA needs egress to Neon and
   `roles/secretmanager.secretAccessor` on the `database-url` secret (Neon URL, `sslmode=require`).
   Shared with the other jobs; nothing dial-specific.

4. **Seed `podcast_feeds`** — without real feeds the job runs and collects **zero**. Edit
   `rara-dial/seed.sql` (uncomment + add feed URLs) and run it against Neon, or directly:
   ```sql
   INSERT INTO podcast_feeds (feed_url, active) VALUES
       ('https://feeds.example.com/your-podcast.xml', true)
   ON CONFLICT (feed_url) DO NOTHING;
   ```

5. **Cloud Scheduler** — dial is cadence-driven, so schedule the run (the reconciler does *not*
   wake it):
   ```bash
   gcloud scheduler jobs create http rara-dial-cron \
     --location="$GCP_REGION" --schedule="0 */6 * * *" \
     --uri="https://${GCP_REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${GCP_PROJECT_ID}/jobs/rara-dial:run" \
     --http-method=POST \
     --oauth-service-account-email="<scheduler-sa>@${GCP_PROJECT_ID}.iam.gserviceaccount.com"
   ```
   The scheduler SA needs `roles/run.invoker` on the job.

6. **GitHub repo vars** — `GCP_REGION`, `GCP_PROJECT_ID` (already set for `deploy-feed`).

## Verify

- `gcloud run jobs execute rara-dial --region "$GCP_REGION" --wait` → logs show
  `Podcast job completed: N episodes catalogued`.
- `SELECT count(*) FROM podcast_episodes;` grows.
- On the next `core-job reconcile`, `IngestPodcast` turns new episodes into `items`/`item_steps`
  (status `pending`, capability `transcrever`) — visible in the console overview.
