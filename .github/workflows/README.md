# GitHub Actions CI/CD Workflows

Path-filtered CI/CD for the rara ecosystem. Each agent has its own set of workflows so a change
to one agent never triggers another's pipeline.

## Workflow matrix

| Workflow | Agent | Path filter | Purpose |
|----------|-------|-------------|---------|
| `ci.yml` | rara-harvest | `rara-harvest/**` | Code quality, tests, security scan |
| `ci-shelf.yml` | rara-shelf | `rara-shelf/**` | Code quality, tests, security scan |
| `ci-scribe.yml` | rara-scribe | `rara-scribe/**` | Code quality, tests, security scan |
| `ci-distill.yml` | rara-distill | `rara-distill/**` | Code quality, tests, security scan |
| `ci-feed.yml` | rara-feed | `rara-feed/**` | Code quality, tests, security scan |
| `ci-core.yml` | rara-core | `rara-core/**` | Code quality, tests, security scan |
| `ci-glean.yml` | rara-glean | `rara-glean/**`, `rara-addon/**` | Code quality, tests, security scan |
| `database.yml` | rara-harvest | `rara-harvest/migrations/**` | Validate + apply migrations |
| `database-shelf.yml` | rara-shelf | `rara-shelf/migrations/**` | Validate + apply migrations |
| `database-scribe.yml` | rara-scribe | `rara-scribe/migrations/**` | Validate + apply migrations |
| `database-distill.yml` | rara-distill | `rara-distill/migrations/**` | Validate + apply migrations |
| `database-feed.yml` | rara-feed | `rara-feed/migrations/**` | Validate + apply migrations |
| `database-core.yml` | rara-core | `rara-core/migrations/**` | Validate + apply migrations |
| `deploy.yml` | rara-harvest | `rara-harvest/**` | Build image + deploy Cloud Run Job |
| `deploy-shelf.yml` | rara-shelf | `rara-shelf/**` | Build image + deploy Cloud Run Job |
| `deploy-distill.yml` | rara-distill | `rara-distill/**` | Build image + deploy Cloud Run Job |
| `deploy-feed.yml` | rara-feed | `rara-feed/**` | Build image + deploy Cloud Run Job |

> **rara-scribe has no deploy workflow.** It runs locally on a Mac via `launchd`, not on Cloud
> Run. Updating it is `cd rara-scribe && make build && bash install-local.sh` — there is no image
> build or deploy step. Its CI and migration workflows still run in GitHub Actions.
>
> **rara-feed has a full pipeline** (CI + Database + Deploy), same as harvest, shelf, and distill.
>
> **rara-core has CI + Database only (no deploy yet).** Phase 0 ships the control-plane schema
> and scaffold; the reconciler is not built. rara-core will run always-on in the VPC (not as a
> Cloud Run Job), so its deploy workflow lands with the reconciler in a later phase.
>
> **rara-glean (and rara-sift) have CI only.** They are new bridge-total claim-workers that own no
> table of their own (they read/write the shared Neon schema), so there is no `database-*.yml`; the
> Cloud Run deploy workflow lands in a later phase (no gate yet).

## Workflow types

### CI (`ci*.yml`)

Runs on push and pull request for the agent's path. Jobs:
- **Code Quality & Tests** — `go fmt` check, `go vet`, `staticcheck`, `go test -race`, coverage.
- **Security Scan** — secret detection (gitleaks) and dependency audit (`govulncheck`).

A green CI run is required before merging.

### Database (`database*.yml`)

Manages schema changes for one agent's `migrations/`.
- **On PR** — validates each migration inside a `BEGIN; ... ROLLBACK;` so syntax/constraint
  errors fail the check without touching the live schema.
- **On merge to `main`** — applies new migrations in order to Neon, then verifies.

Migrations are idempotent (`IF NOT EXISTS` / `ON CONFLICT`), so re-runs are safe. Note that
migrations apply to Neon regardless of where the agent runs — scribe's `database-scribe.yml`
keeps its tables in sync even though the agent itself runs on a local Mac.

### Deploy (`deploy*.yml`)

Only for the Cloud Run agents (harvest, shelf, distill). Authenticates to GCP via Workload
Identity Federation (no SA keys), builds an amd64 image with Cloud Build, pushes to Artifact
Registry, and creates/updates the Cloud Run Job. Triggered automatically on merge to `main` for
the agent's path, or manually via *Run workflow*.

## Secrets & variables

Required GitHub Secrets:

```
NEON_HOST / NEON_PORT / NEON_DATABASE / NEON_USERNAME / NEON_PASSWORD   # CI migrations
GCP_WORKLOAD_IDENTITY_PROVIDER / GCP_SERVICE_ACCOUNT                    # deploy (WIF)
```

GitHub Variables: `GCP_PROJECT_ID`, `GCP_REGION`.

Migration workflows construct `DATABASE_URL` from the `NEON_*` secrets. Deploy workflows mount
runtime secrets from GCP Secret Manager (see [INFRASTRUCTURE.md](../../INFRASTRUCTURE.md)).

## Monitoring

```bash
# List recent runs
gh run list --repo renatobardi/rara --limit 10

# View logs for the latest run
RUN_ID=$(gh run list --repo renatobardi/rara --limit 1 --json databaseId -q '.[0].databaseId')
gh run view "$RUN_ID" --repo renatobardi/rara --log
```

## Troubleshooting

- **Database connection failed** — verify the `NEON_*` secrets (`gh secret list`).
- **Format/vet failed** — run `go fmt ./...` and `go vet ./...` locally for the affected agent.
- **Tests failed** — run `go test -race ./...` in the agent directory.
- **"Migration already applied"** — expected; migrations use `IF NOT EXISTS` and are safe to
  re-run.

## Cost

GitHub Actions is free for public repos (unlimited minutes). Typical usage is well under the
free private-repo allowance anyway.
