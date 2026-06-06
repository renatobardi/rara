# Deploy — rara-shelf (Cloud Run via GitHub Actions)

The [`deploy-shelf.yml`](../.github/workflows/deploy-shelf.yml) builds the image with
**Cloud Build** and deploys a **Cloud Run Job** `rara-shelf`, authenticating to GCP
via **Workload Identity Federation** (no service account keys).

Reuses all rara-harvest infrastructure (project `${PROJECT_ID}`, Artifact Registry `rara`,
WIF, service account `rara-deployer`, bucket `${PROJECT_ID}_cloudbuild`, Neon database). The
only new piece is **OAuth authentication** to read your playlists — 3 secrets in Secret Manager.

> Set `export PROJECT_ID=<your-project>` before running the commands below. The actual value
> lives in the GitHub Variable `GCP_PROJECT_ID`, not in this file.

Unlike harvest (public API key), shelf requires OAuth because it reads **your** playlists
(including private ones), which requires a Google login. **Watch Later / History are not
accessible via the Data API** and are skipped by the app.

---

## 1. Create OAuth credentials (once)

In the GCP console for project `${PROJECT_ID}`:

### 1.1 OAuth consent screen
APIs & Services → **OAuth consent screen**:
- User type: **External**
- Fill in app name and support email
- Under **Test users**, add your own Google email (the playlist owner)
- Required scope: `https://www.googleapis.com/auth/youtube.readonly`

### 1.2 OAuth client
APIs & Services → **Credentials** → **Create credentials** → **OAuth client ID**:
- Application type: **Desktop app**
- Copy the **Client ID** and **Client secret**

### 1.3 Enable the API
Make sure the **YouTube Data API v3** is enabled:
```bash
gcloud services enable youtube.googleapis.com --project "${PROJECT_ID}"
```

---

## 2. Obtain the refresh token (once)

Via **OAuth 2.0 Playground** (no code needed):
1. Open https://developers.google.com/oauthplayground
2. Gear icon (⚙) → check **Use your own OAuth credentials** → paste Client ID + Secret
3. In the left panel, under "Input your own scopes", enter:
   `https://www.googleapis.com/auth/youtube.readonly`
4. **Authorize APIs** → sign in with your account → consent
5. **Exchange authorization code for tokens** → copy the **Refresh token**

> The refresh token is long-lived; the app exchanges it for an access token on each run.

---

## 3. Store the secrets (Cloud Shell)

```bash
printf '%s' 'YOUR_CLIENT_ID'     | gcloud secrets create shelf-oauth-client-id     --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
printf '%s' 'YOUR_CLIENT_SECRET' | gcloud secrets create shelf-oauth-client-secret --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
printf '%s' 'YOUR_REFRESH_TOKEN' | gcloud secrets create shelf-oauth-refresh-token --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
```

`database-url` already exists (reused from harvest). The `rara-deployer` service account and
the runtime SA (compute default) already have `secretmanager.secretAccessor` at project level,
so the 3 new secrets are accessible **without any IAM changes**.

Future rotation: `gcloud secrets versions add shelf-oauth-refresh-token --data-file=-`.

---

## 4. Deploy

- **Automatic**: merging anything under `rara-shelf/**` to `main` triggers `deploy-shelf.yml`.
- **Manual**: Actions → **Deploy rara-shelf to Cloud Run** → *Run workflow*.

The workflow builds the image, creates/updates the Cloud Run Job `rara-shelf` (mounting the
4 secrets), and executes it once.

---

## 5. Verify

```sql
SELECT p.title, p.privacy_status, COUNT(pv.id) AS videos
FROM playlists p
LEFT JOIN playlist_videos pv ON pv.playlist_id = p.id
GROUP BY p.id
ORDER BY videos DESC;
```

## 6. Scheduling (optional)

```bash
gcloud scheduler jobs create http rara-shelf-daily \
  --location=us-central1 --schedule="0 3 * * *" \
  --uri="https://us-central1-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-shelf:run" \
  --http-method=POST \
  --oauth-service-account-email="rara-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```
