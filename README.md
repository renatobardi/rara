# rara

**Autonomous Agent Ecosystem** - A collection of independently deployable agents for data collection, processing, and delivery.

## About

`rara` is an umbrella repository for the kura ecosystem of agents. Each agent is:
- 🔒 **Isolated**: Independent codebase, dependencies, and deployment
- 🧪 **Fully Tested**: TDD-built with comprehensive test suites
- ☁️ **Cloud-Native**: Containerized and serverless-ready
- 💰 **Cost-Efficient**: Pay only for what you use
- 📈 **Scalable**: Deploy to any region, any time

## Agents

### 🎬 rara-harvest
Video harvesting pipeline for YouTube, TikTok, and more.
- **Status**: ✅ Production Ready
- **Tests**: 13/13 passing
- **Cost**: ~$0.02/month (daily execution)

**Quick Start**:
```bash
cd rara-harvest
make test
./deploy.sh
```

[Read More →](./rara-harvest/README.md)

### 🔮 rara-pulse (Coming Soon)
Metrics and data aggregation agent.

### 🌊 rara-stream (Coming Soon)
Real-time event streaming agent.

## Project Structure

```
rara/
├── rara-harvest/          # Video harvesting agent
│   ├── main.go
│   ├── main_test.go
│   ├── Dockerfile
│   ├── deploy.sh
│   ├── Makefile
│   ├── README.md
│   └── ...
│
├── rara-pulse/            # Metrics agent (future)
│   └── ...
│
├── rara-stream/           # Streaming agent (future)
│   └── ...
│
├── shared/                # Shared utilities (optional)
│   ├── config/
│   └── utils/
│
└── README.md (this file)
```

## Getting Started

### Clone the Repository

```bash
git clone https://github.com/renatobardi/rara.git
cd rara
```

### Work on an Agent

```bash
# Navigate to agent
cd rara-harvest

# Run tests
make test

# Run coverage
make test-coverage

# Deploy
./deploy.sh
```

## Architecture & Design

Each agent in rara is designed as an autonomous system:

```
┌─────────────────────────────────────────┐
│           Agent (rara-harvest)          │
├─────────────────────────────────────────┤
│ • Independent tests & CI/CD             │
│ • Isolated dependencies (go.mod)        │
│ • Self-contained deployment (Dockerfile)│
│ • Own pipeline configuration (deploy.sh)│
│ • Serverless execution (Cloud Run)      │
└─────────────────────────────────────────┘
```

## Documentation

- 📖 [rara-harvest/README.md](./rara-harvest/README.md) - Agent setup guide
- 🧪 [rara-harvest/TESTING.md](./rara-harvest/TESTING.md) - TDD workflow
- 🏗️ [ARCHITECTURE.md](./ARCHITECTURE.md) - Ecosystem architecture

## Development

### Running Tests (All Agents)

```bash
# Test rara-harvest
cd rara-harvest && make test

# Test other agents as they're added
cd rara-pulse && make test
```

### Code Standards

- Language: Go 1.23+
- Testing: TDD with 100% business logic coverage
- Quality: go vet, go fmt required
- Documentation: Comprehensive README per agent

## Deployment

Each agent has its own deployment pipeline:

```bash
cd rara-harvest
./deploy.sh  # Deploys to GCP Cloud Run

# Customize deployment variables in deploy.sh
PROJECT_ID="your-gcp-project-id"
REGION="us-central1"
```

## Cost Analysis

| Agent | Execution | Monthly Cost |
|-------|-----------|--------------|
| rara-harvest | Daily | ~$0.02 |
| (More agents coming) | - | - |

## Contributing

When adding new agents:
1. Create a directory: `mkdir rara-<name>`
2. Follow TDD pattern (tests first)
3. Include Dockerfile & deploy.sh
4. Update root README.md
5. Document in agent's README.md

## License

MIT
