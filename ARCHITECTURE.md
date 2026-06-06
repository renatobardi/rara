# rara-harvest 🎬

**First agent in the kura ecosystem** - An autonomous video harvesting pipeline built with TDD.

## What is rara-harvest?

`rara-harvest` is a zero-idle-cost ETL pipeline that autonomously collects and indexes videos from YouTube (and other platforms in the future). It's designed to run on serverless infrastructure with minimal operational overhead.

### Key Characteristics

- ✅ **Fully Tested**: 13 comprehensive TDD tests, all passing
- ✅ **Production Ready**: Deploy to GCP Cloud Run in minutes
- ✅ **Cost Efficient**: Pay only for executions (~$0.01/month for daily runs)
- ✅ **Idempotent**: Safe to run multiple times, no duplicates
- ✅ **Extensible**: Built for multi-platform harvesting
- ✅ **Cloud Native**: ARM64-optimized Docker image

## Quick Links

- 📖 **[README.md](rara-harvest/README.md)** - Complete project documentation
- 🧪 **[TESTING.md](rara-harvest/TESTING.md)** - TDD workflow and harness architecture
- 🔄 **[TDD_SUMMARY.md](rara-harvest/TDD_SUMMARY.md)** - Red-Green-Refactor cycle results
- 📋 **[PROJECT_SUMMARY.md](rara-harvest/PROJECT_SUMMARY.md)** - Full implementation guide

## Getting Started

### 1. Build & Test Locally

```bash
cd rara-harvest
make test              # Run 13 tests
make build            # Build local binary
```

### 2. Deploy to GCP

```bash
./deploy.sh
# Or:
make deploy
```

### 3. Monitor

```bash
gcloud run jobs log rara-harvest --region=us-central1
```

## Project Structure

```
rara-harvest/
├── main.go              # 193 lines - Core ETL logic
├── main_test.go         # 380 lines - 13 TDD tests + harness
├── schema.sql           # Database schema
├── Dockerfile           # Multi-stage ARM64 build
├── deploy.sh            # GCP deployment automation
├── Makefile             # Build & test targets
├── README.md            # Full documentation
├── TESTING.md           # Test strategy & harness
├── TDD_SUMMARY.md       # Red-Green-Refactor results
└── PROJECT_SUMMARY.md   # Implementation summary
```

## Test Harness

Built with fluent builder pattern for readable, maintainable tests:

```go
harness := NewETLHarness(t).
    WithChannels([]Channel{...}).
    WithVideo(PlaylistItem{...})

err := harness.Execute(context.Background())
harness.AssertVideoCount(1)
```

## TDD Results

```
Phase 1: RED    → 4 failing tests (requirements documented)
Phase 2: GREEN  → 13 passing tests (implementation complete)
Phase 3: REFACTOR → Code quality & documentation improved

Total: 13/13 tests passing ✅
Execution: 199ms (all local, no I/O)
Coverage: 100% business logic, 0% I/O (intentional)
```

## Architecture

**Technology Stack**:
- Go 1.23+ (minimal, fast)
- PostgreSQL (Neon DB serverless)
- Docker (ARM64-native)
- GCP Cloud Run (pay-per-execution)
- Cloud Scheduler (daily triggers)

**Design Patterns**:
- TDD (Test-Driven Development)
- Fluent Builder (test harness)
- Mock Database (dependency isolation)
- Idempotent Upserts (safety guarantee)

## Cost Analysis

### Monthly Cost (Daily Execution)

| Component | Cost |
|-----------|------|
| Cloud Run Compute | < $0.01 |
| Database (Neon) | < $0.01 |
| YouTube API | Free (10k units/day) |
| **Total** | **< $0.02/month** |

## Part of kura Ecosystem

`rara-harvest` is the first autonomous agent in the **kura** system:

```
kura/
├── rara-harvest    (video collection) ← YOU ARE HERE
├── rara-pulse      (metrics/data)
├── rara-stream     (real-time feeds)
└── ... more agents
```

Each agent is:
- ✅ Independently deployable
- ✅ TDD-built with full test suites
- ✅ Cost-optimized for serverless
- ✅ Designed for autonomous operation

## Next Steps

1. **Explore**: Read [README.md](rara-harvest/README.md) for full setup guide
2. **Test**: Run `make test` to see all 13 tests pass
3. **Deploy**: Run `./deploy.sh` to deploy to GCP
4. **Extend**: Add more video sources (TikTok, Instagram, etc.)

## Development

```bash
# Testing
make test                 # Run all tests
make test-coverage       # Coverage report
make test-race           # Race detection

# Building
make build              # Build binary
make build-arm64        # Build for cloud
make docker-build       # Build Docker image

# Code Quality
make lint               # Run linters
make fmt                # Format code

# Deployment
make deploy             # Full deployment flow
```

## Questions?

See the detailed documentation:
- **How do I test?** → [TESTING.md](rara-harvest/TESTING.md)
- **How does it work?** → [README.md](rara-harvest/README.md)
- **What was built?** → [PROJECT_SUMMARY.md](rara-harvest/PROJECT_SUMMARY.md)
- **How was it tested?** → [TDD_SUMMARY.md](rara-harvest/TDD_SUMMARY.md)

---

**Status**: ✅ Production Ready
**Tests**: 13/13 Passing
**TDD Cycle**: Red-Green-Refactor Complete
**Deployment**: GCP Cloud Run Ready
