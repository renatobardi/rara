# GitHub Actions CI/CD Workflows

Complete CI/CD pipeline for rara-harvest with automated database migrations and code quality checks.

## Workflows

### 1. Database Migrations (`database.yml`)

Manages all database schema changes via migrations.

**Triggers:**
- Push to `main` branch (migrations/, cleanup.sql, seed.sql changed)
- Pull requests to `main` branch
- Manual trigger via workflow_dispatch

**Jobs:**

#### Validate Schema
- Tests database connection
- Validates migration file syntax
- Checks current schema state
- Runs on every PR (fail if migrations invalid)

#### Apply Migrations
- Runs only on `main` branch after PR merge
- Executes all migrations in order
- Verifies schema after migration
- Shows database statistics

#### Load Seed Data (Optional)
- Manual trigger only
- Loads test data into database
- Shows loaded channels and videos count

#### Health Check
- Runs after all other jobs
- Tests connection
- Counts tables and indexes
- Reports overall status

**Manual Triggers:**
```bash
# Via GitHub UI: Actions → Database Migrations → Run workflow
# Choose action: validate, migrate, or seed
```

### 2. CI/CD Pipeline (`ci.yml`)

Comprehensive code quality, security, and build validation.

**Triggers:**
- Push to `main` or `develop`
- Pull requests to `main` or `develop`

**Jobs:**

#### Code Quality & Tests
- Format check (`go fmt`)
- Linting (`go vet`)
- Unit tests with race detector
- Coverage report generation
- **Requirement**: Must pass for deployment

#### Security Scan
- Hardcoded secrets detection
- Dependency audit
- **Requirement**: Warnings noted but don't block

#### Docker Build
- Builds ARM64 Docker image
- Runs on `main` branch only
- **Requirement**: Must pass for deployment

#### Database Validation
- Validates database connectivity
- Checks migration file syntax
- **Requirement**: Must pass for deployment

#### Workflow Summary
- Reports all job statuses
- Shows final pass/fail result

## Environment Variables & Secrets

Required GitHub Secrets (Opção A):

```
NEON_HOST          # Neon endpoint
NEON_PORT          # Usually 5432
NEON_DATABASE      # Database name
NEON_USERNAME      # Database user
NEON_PASSWORD      # Database password
```

Workflows construct DATABASE_URL automatically:
```
postgresql://USERNAME:PASSWORD@HOST:PORT/DATABASE
```

## How It Works

### On Pull Request

1. **Code Quality** runs:
   - Format check
   - Lint analysis
   - Tests
   - Coverage

2. **Security** scans:
   - Hardcoded secrets
   - Dependencies

3. **Docker** builds:
   - ARM64 image validation
   - No push (just validation)

4. **Database** validates:
   - Connection test
   - Migration syntax check

**Result**: Green checkmark required to merge

### On Merge to Main

1. **All PR checks** repeat
2. **Database migrations** automatically apply
3. **Docker image** builds (if needed)
4. **Health check** verifies deployment readiness

**Result**: Production ready after merge

## Monitoring & Debugging

### View Workflow Status

```bash
# via GitHub CLI
gh run list --repo renatobardi/rara --limit 10

# via GitHub Web UI
https://github.com/renatobardi/rara/actions
```

### View Logs

```bash
# Get last run ID
RUN_ID=$(gh run list --repo renatobardi/rara --limit 1 --json databaseId -q '.[0].databaseId')

# View logs
gh run view $RUN_ID --repo renatobardi/rara --log
```

### Debug Failed Workflow

1. Check job logs in GitHub UI
2. Common issues:
   - **Connection failed**: Verify NEON secrets are set correctly
   - **Format failed**: Run `go fmt ./...` locally
   - **Tests failed**: Run `go test -v ./...` locally
   - **Docker failed**: Check Dockerfile syntax

## Customization

### Add a New Job

Edit `.github/workflows/ci.yml`:

```yaml
  my-new-job:
    name: My New Job
    runs-on: ubuntu-latest
    needs: [code-quality]  # Wait for this job
    
    steps:
      - uses: actions/checkout@v4
      - run: echo "My custom check"
```

### Change Trigger Events

Edit top of workflow file:

```yaml
on:
  push:
    branches: [main, develop, staging]  # Add more branches
  pull_request:
    branches: [main]
  schedule:
    - cron: '0 2 * * *'  # Run daily at 2 AM UTC
```

### Add Environment Variables

```yaml
env:
  GO_VERSION: '1.23'
  COVERAGE_THRESHOLD: '80'
```

## Performance

### Build Times

- **Code Quality**: ~30s
- **Security Scan**: ~15s
- **Docker Build**: ~1m (first time), ~20s (cached)
- **Database Check**: ~5s
- **Total**: ~2-3 minutes per workflow

### Caching

- Go modules cached: Saves ~30s per run
- Docker layers cached: Saves ~40s per run

## Cost

GitHub Actions Free Tier:
- 2,000 minutes/month
- Unlimited runs on public repos
- We use <50 minutes/month

Pricing: **Free** ✅

## Troubleshooting

### "Database connection failed"

```bash
# Check secrets are set
gh secret list --repo renatobardi/rara

# Verify Neon connection locally
export DATABASE_URL='postgresql://...'
psql $DATABASE_URL -c "SELECT 1"
```

### "Permission denied" on push

```bash
# Ensure branch protection allows CI
# GitHub → repo → Settings → Branches → main
# Require status checks to pass before merging
```

### "Migration already applied"

This is normal! Migrations use `IF NOT EXISTS`, so:
```bash
# Safe to re-run without errors
./migrate.sh migrate
./migrate.sh migrate  # Still works, no duplicates
```

## Next Steps

1. ✅ Workflows created
2. ✅ Secrets configured
3. → Push a change to main and watch workflows run!

## Resources

- [GitHub Actions Docs](https://docs.github.com/en/actions)
- [PostgreSQL Client in Actions](https://github.com/actions/setup-go)
- [Docker Build Action](https://github.com/docker/build-push-action)
- [Our CI Config](./)
