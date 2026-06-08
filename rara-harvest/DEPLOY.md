# Deploy — Cloud Run via GitHub Actions

The [`deploy.yml`](../.github/workflows/deploy.yml) workflow builds the image with
**Cloud Build** and deploys a **Cloud Run Job**, authenticating to GCP with
**Workload Identity Federation** (no service-account key files).

You run the one-time setup below **once** in **Cloud Shell**
(open https://console.cloud.google.com → "Activate Cloud Shell", gcloud is preinstalled).
After that, every merge to `main` (or a manual run) deploys automatically.

---

## 1. One-time GCP setup (run in Cloud Shell)

```bash
# --- Edit these ---
export PROJECT_ID="your-gcp-project-id"
export REGION="us-central1"
export GITHUB_REPO="renatobardi/rara"      # owner/repo
# ------------------

gcloud config set project "$PROJECT_ID"
export PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"

# Enable required APIs
gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  secretmanager.googleapis.com \
  iamcredentials.googleapis.com

# Artifact Registry repo (the workflow pushes images here)
gcloud artifacts repositories create rara \
  --repository-format=docker --location="$REGION" \
  --description="rara ecosystem images" || true

# Deploy service account (GitHub Actions impersonates this)
gcloud iam service-accounts create rara-deployer \
  --display-name="rara-harvest GitHub deployer" || true
export SA="rara-deployer@${PROJECT_ID}.iam.gserviceaccount.com"

# Roles for the deployer: build, push, deploy Cloud Run, act as runtime SA, read secrets
# roles/storage.objectAdmin  — Cloud Build stages source in a GCS bucket
# roles/serviceusage.serviceUsageConsumer — required for any SA to call GCP APIs
for ROLE in \
  roles/run.admin \
  roles/cloudbuild.builds.editor \
  roles/artifactregistry.writer \
  roles/iam.serviceAccountUser \
  roles/secretmanager.secretAccessor \
  roles/storage.objectAdmin \
  roles/serviceusage.serviceUsageConsumer; do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member="serviceAccount:${SA}" --role="$ROLE" --condition=None
done

# Let Cloud Build's default SA push to Artifact Registry
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role="roles/artifactregistry.writer" --condition=None
```

### Workload Identity Federation (keyless GitHub → GCP auth)

```bash
gcloud iam workload-identity-pools create github-pool \
  --location=global --display-name="GitHub Actions" || true

gcloud iam workload-identity-pools providers create-oidc github-provider \
  --location=global --workload-identity-pool=github-pool \
  --display-name="GitHub OIDC" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition="assertion.repository=='${GITHUB_REPO}'" \
  --issuer-uri="https://token.actions.githubusercontent.com" || true

export WIF_PROVIDER="$(gcloud iam workload-identity-pools providers describe github-provider \
  --location=global --workload-identity-pool=github-pool --format='value(name)')"

# Allow this repo to impersonate the deployer SA
gcloud iam service-accounts add-iam-policy-binding "$SA" \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github-pool/attribute.repository/${GITHUB_REPO}"

echo "GCP_WORKLOAD_IDENTITY_PROVIDER = $WIF_PROVIDER"
echo "GCP_SERVICE_ACCOUNT            = $SA"
```

### App secrets (Secret Manager)

> ⚠️ You enter these values — never commit them.

```bash
# YouTube Data API v3 key
printf '%s' 'YOUR_YOUTUBE_API_KEY' | \
  gcloud secrets create youtube-api-key --replication-policy=automatic --data-file=-

# Neon connection string
printf '%s' 'postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require' | \
  gcloud secrets create database-url --replication-policy=automatic --data-file=-
```

To rotate later: `gcloud secrets versions add youtube-api-key --data-file=-` (then paste).

---

## 2. Configure GitHub

In **Settings → Secrets and variables → Actions**:

**Variables** (the `Variables` tab):
| Name | Value |
|------|-------|
| `GCP_PROJECT_ID` | your project id |
| `GCP_REGION` | `us-central1` (or your region) |

**Secrets** (the `Secrets` tab):
| Name | Value |
|------|-------|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | the `GCP_WORKLOAD_IDENTITY_PROVIDER` printed above |
| `GCP_SERVICE_ACCOUNT` | `rara-deployer@<project>.iam.gserviceaccount.com` |

---

## 3. Deploy

- **Automatic**: merge anything under `rara-harvest/**` to `main`.
- **Manual**: Actions → **Deploy rara-harvest** → *Run workflow*.

The workflow builds the image, creates/updates the `rara-harvest` Cloud Run Job,
and executes it once. View logs:

```bash
gcloud run jobs executions list --job rara-harvest --region "$REGION"
```

## 4. Schedule (optional, daily harvest)

```bash
gcloud scheduler jobs create http rara-harvest-daily \
  --location="$REGION" --schedule="0 2 * * *" \
  --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-harvest:run" \
  --http-method=POST \
  --oauth-service-account-email="$SA"
```

---

## Notes

- The image is built **amd64** (Cloud Run's architecture). The previous
  hardcoded `GOARCH=arm64` in the Dockerfile would not have run on Cloud Run.
- `deploy.sh` (local-machine deploy) remains for reference but requires
  gcloud + Docker installed locally; the GitHub Actions path above needs neither.
- The Cloud Run Job's runtime identity is the default compute SA; it reads the
  two secrets via `--set-secrets`. If you harden later, give a dedicated runtime
  SA only `roles/secretmanager.secretAccessor` on those two secrets.
