# rara-harvest

A zero-idle-cost video harvesting pipeline for collecting latest videos from target channels (YouTube, TikTok, etc.) and upserting them into a PostgreSQL database (Neon DB). Part of the **rara** ecosystem of agent-based applications.

## Overview

**rara-harvest** is a TDD-built ETL pipeline that autonomously collects and indexes video content from multiple platforms. It's designed as a scalable agent that can be deployed once and run indefinitely with minimal operational overhead.

## Architecture

- **Language**: Go 1.23+
- **Database**: Neon DB (PostgreSQL Serverless)
- **Driver**: pgx/v5
- **Container Runtime**: Docker (ARM64-optimized)
- **Deployment**: Google Cloud Run (serverless, pay-per-execution)
- **Testing**: TDD with fluent harness pattern

## Features

- **Idempotent Harvesting**: Uses `ON CONFLICT DO NOTHING` to prevent duplicates
- **Multi-Channel Support**: Fetch from unlimited active channels
- **Platform Agnostic**: Designed to support YouTube, TikTok, and more
- **Quota Efficient**: Uses playlistItems endpoint (1 unit vs 100 for search)
- **ARM64 Native**: Cross-compiled for Oracle Cloud and GCP ARM environments
- **Minimal Container**: ~50MB Docker image with ca-certificates
- **Zero Idle Costs**: Serverless Cloud Run with pay-per-execution billing
- **Production Ready**: Comprehensive TDD test suite with fluent harness pattern

## Quick Start

### Local Development

1. **Set up environment variables**:
   ```bash
   export YOUTUBE_API_KEY="your-youtube-api-key"
   export DATABASE_URL="postgresql://user:password@host:port/database"
   ```

2. **Run locally**:
   ```bash
   go run main.go
   ```

3. **Run tests**:
   ```bash
   make test              # Run all 14 tests
   make test-coverage     # View coverage report
   ```

### Docker Build

```bash
# Build for ARM64
docker build --platform linux/arm64 -t rara-harvest:latest .

# Run container
docker run -e YOUTUBE_API_KEY="your-key" \
           -e DATABASE_URL="your-db-url" \
           rara-harvest:latest
```

## Deployment

### Prerequisites

- Google Cloud Project with billing enabled
- gcloud CLI configured
- Docker installed locally
- YouTube Data API v3 key
- Neon DB connection string

### Deploy to Cloud Run

1. **Customize deploy.sh**:
   ```bash
   # Edit these variables:
   PROJECT_ID="your-gcp-project-id"
   REGION="us-central1"
   REPO_NAME="rara"
   JOB_NAME="rara-harvest"
   ```

2. **Create secrets in Secret Manager**:
   ```bash
   # YouTube API Key
   gcloud secrets create youtube-api-key \
     --replication-policy=automatic \
     --data-file=- <<< 'YOUR_API_KEY'

   # Database URL
   gcloud secrets create database-url \
     --replication-policy=automatic \
     --data-file=- <<< 'postgresql://...'
   ```

3. **Grant Cloud Run service account access**:
   ```bash
   PROJ_ID="your-project-id"
   SERVICE_ACCOUNT="${PROJ_ID}@appspot.gserviceaccount.com"

   gcloud secrets add-iam-policy-binding youtube-api-key \
     --member=serviceAccount:${SERVICE_ACCOUNT} \
     --role=roles/secretmanager.secretAccessor

   gcloud secrets add-iam-policy-binding database-url \
     --member=serviceAccount:${SERVICE_ACCOUNT} \
     --role=roles/secretmanager.secretAccessor
   ```

4. **Run deploy script**:
   ```bash
   make deploy
   # or
   ./deploy.sh
   ```

### Manual Cloud Run Deployment

```bash
gcloud run jobs create rara-harvest \
  --image=us-central1-docker.pkg.dev/PROJECT_ID/rara/rara-harvest:latest \
  --region=us-central1 \
  --set-env-vars="YOUTUBE_API_KEY=projects/PROJECT_ID/secrets/youtube-api-key/versions/latest,DATABASE_URL=projects/PROJECT_ID/secrets/database-url/versions/latest" \
  --memory=512Mi \
  --cpu=1 \
  --task-timeout=1800s
```

### Trigger Manual Execution

```bash
gcloud run jobs execute rara-harvest --region=us-central1
```

### Set Up Daily Schedule (Cloud Scheduler)

```bash
gcloud scheduler jobs create http rara-harvest-scheduler \
  --schedule="0 2 * * *" \
  --location=us-central1 \
  --uri="https://us-central1-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/PROJECT_ID/jobs/rara-harvest:run" \
  --http-method=POST \
  --oidc-service-account-email=PROJECT_ID@appspot.gserviceaccount.com
```

## Database Schema

See [schema.sql](./schema.sql) for the complete DDL. Tables:

- **target_channels**: Stores YouTube channel metadata
- **channel_videos**: Stores harvested videos with idempotency on `youtube_video_id`

Indexes:
- `idx_channels_youtube_id`: Fast channel lookup
- `idx_videos_published_at`: Fast time-range queries

## API Rate Limiting

The harvest uses YouTube Data API v3 playlistItems endpoint which is more quota-efficient than search:
- **playlistItems**: 1 quota unit per request
- **search**: 100 quota units per request

For 50 channels × 5 videos per channel = 50 quota units per execution.

## Monitoring

### View Logs

```bash
gcloud run jobs log rara-harvest --region=us-central1 --limit=50
```

### Metrics

Cloud Run provides built-in metrics:
- Execution count
- Duration
- Memory usage
- Errors

View in [Cloud Console](https://console.cloud.google.com/run/jobs).

## Cost Estimation

With daily execution:
- **Compute**: ~$0.00002 per execution (first 180k vCPU-seconds free)
- **Storage**: Minimal (~10MB for years of data)
- **API Calls**: YouTube API is free tier up to 10k units/day
- **Database**: Neon serverless pay-per-use (~$0.0001 per 10k reads)

**Estimated monthly cost**: < $1 (for most use cases)

## Idempotency Guarantee

The harvest is fully idempotent:

```sql
INSERT INTO channel_videos (...)
VALUES (...)
ON CONFLICT (youtube_video_id) DO NOTHING
```

This ensures:
- Re-running harvest produces no duplicates
- Safe to trigger multiple times within a period
- No manual cleanup required

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `YOUTUBE_API_KEY` | Yes | YouTube Data API v3 key |
| `DATABASE_URL` | Yes | PostgreSQL connection string |

## Testing

### Run Tests

```bash
make test                 # Run all 14 tests
make test-verbose        # Verbose output with race detection
make test-coverage       # Generate coverage report
make test-race           # Race detector
```

### Test Architecture

**TDD Implementation** with fluent harness pattern:
- ✓ 14 comprehensive test cases
- ✓ 100% coverage on business logic
- ✓ MockDatabase for isolation
- ✓ ETLHarness for orchestration
- ✓ Execution time: < 200ms

See [TESTING.md](./TESTING.md) for detailed testing strategy.

## Troubleshooting

### Database Connection Failed
- Check DATABASE_URL format: `postgresql://user:password@host:port/database`
- Ensure network connectivity (Cloud Run has outbound internet)
- Verify Neon DB is accessible from Cloud Run region

### No Videos Harvested
- Check if target_channels has any rows with `active = true`
- Verify YOUTUBE_API_KEY is valid
- Check Cloud Run logs for API errors

### High Latency
- Increase memory allocation to 1GB (Cloud Run job property)
- Check database query performance
- Reduce max videos per channel if YouTube API is slow

## Development

### Build & Test

```bash
# Format code
make fmt

# Lint
make lint

# Run tests
make test

# Build binary
make build

# Build ARM64 for cloud
make build-arm64
```

### Code Quality

```bash
go fmt ./...
go vet ./...
go test -v -race ./...
```

## Project Structure

```
rara-harvest/
├── main.go              # Core ETL logic
├── main_test.go         # 14 comprehensive tests with harness
├── schema.sql           # Database DDL reference
├── Dockerfile           # Multi-stage ARM64 build
├── deploy.sh            # GCP Cloud Run deployment
├── Makefile             # Build & test automation
├── README.md            # This file
├── TESTING.md           # TDD workflow & harness docs
├── TDD_SUMMARY.md       # Red-Green-Refactor results
└── PROJECT_SUMMARY.md   # Complete implementation guide
```

## Part of the rara Ecosystem

**rara-harvest** is the first agent in the **rara** ecosystem of autonomous data collection applications. Other production agents:
- **rara-shelf** — catalogs the owner's own YouTube playlists (OAuth)
- **rara-transcribe** — transcribes collected videos with Groq/Gemini ASR (runs locally)

See the [root README](../README.md) for the full ecosystem overview.

## License

MIT
