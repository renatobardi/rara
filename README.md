# rara-harvest

The first agent in the **kura** ecosystem - autonomous video harvesting pipeline.

## About

`rara-harvest` is a zero-idle-cost ETL pipeline for collecting and indexing videos from YouTube and other platforms. Built with TDD and deployed on serverless infrastructure.

## Quick Start

```bash
git clone https://github.com/renatobardi/rara-harvest.git
cd rara-harvest

# Run tests
make test

# Deploy to GCP
./deploy.sh
```

## What's Inside

- ✅ **13 comprehensive TDD tests** - Red-Green-Refactor cycle complete
- ✅ **Production-ready code** - Go 1.23+ with PostgreSQL (Neon DB)
- ✅ **Fluent test harness** - MockDatabase & ETLHarness patterns
- ✅ **Cloud-native** - ARM64-optimized Docker, GCP Cloud Run ready
- ✅ **Cost-efficient** - Pay ~$0.02/month for daily execution

## Documentation

- 📖 [README.md](./README.md) - Complete setup & deployment guide
- 🧪 [TESTING.md](./TESTING.md) - TDD workflow & test harness architecture
- 🔄 [TDD_SUMMARY.md](./TDD_SUMMARY.md) - Red-Green-Refactor cycle results
- 📋 [PROJECT_SUMMARY.md](./PROJECT_SUMMARY.md) - Implementation details

## Part of kura Ecosystem

```
kura/
├── rara-harvest    (video collection) ← You are here
├── rara-pulse      (metrics/data)
├── rara-stream     (real-time feeds)
└── ... more agents
```

## Licença

MIT
