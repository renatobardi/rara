# rara

**Autonomous agent ecosystem** — independent Go agents that collect, transcribe, gate, curate, and
learn from content (YouTube, podcasts, email, LinkedIn, news), built with Go 1.26 and TDD.

## What this repo is

`rara` is a monorepo of **independent agents** that share one Neon PostgreSQL database but own
**isolated tables**. Each agent is a separate Go module (its own `go.mod`, flat `package main`)
that builds and tests on its own — there is intentionally **no `go.work`** and no top-level
Makefile. Agents are decoupled **through the database**: a worker reads another's table, it never
calls it.

The agents fall into two generations:

- **1.0 workers** — batch ETL jobs (harvest, shelf, dial, courier, feed). Each collects and writes
  its own table on a daily schedule.
- **2.0 control plane** — `rara-core` (binary `core-job`) decides *what runs next, where, and
  whether it's worth doing*, orchestrating the workers through control tables (`items`,
  `item_steps`, `flows`, `flow_steps`, `routing_policies`) without touching their domain logic.

The two generations converge via the **bridge-total split**: workers are being rewritten from batch
drivers into per-item **claim-workers** on the shared `rara-addon` SDK
(`addon.Run(Config{Capability, Provider}, handler)` over a pgxpool) — the handler does only the
domain work, the SDK owns the contract tables. `rara-scribe`, `rara-distill`, `rara-glean`,
`rara-sift`, and `rara-clip` are already on the SDK; the rest are still batch drivers.

Read [ARCHITECTURE.md](./ARCHITECTURE.md) (1.0) and
[ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md) (2.0 control plane) for the full design.
Per-agent detail lives in each `rara-*/README.md`.

## The pipeline

```
collectors ──► domain tables ──► [rara-core ingest] ──► items / item_steps
   harvest  channel_videos                                    │
   shelf    playlist_videos          rara-core orchestrates flows:
   dial     podcast_episodes                                  │
   courier  emails                    to-text ──► gate ──► curate ──► learn
   clip     linkedin_posts            scribe     sift     distill   hone
   feed     news_items                glean       │          │        │
                                      transcripts  gate_      distil-  interest_
                                                   decisions  lations  profile
                                                                 │
                                                       Kura (external) builds RAG
```

`rara-console` is the operator UI sitting in front of the whole thing.

## Agents

### Control plane (2.0)

| Agent | Role | Owns | Runtime | Tests |
|-------|------|------|---------|-------|
| **rara-core** (`core-job`) | Orchestrator + reconciler + HTTP/MCP surface. Decides what runs, routes items, runs gate decisions. | `items`, `item_steps`, `flows`, `flow_steps`, `capabilities`, `providers`, `routing_policies`, `gate_*`, `feedback`, `interest_profile` | Always-on VPC (Oracle VPS, systemd) | 228 |
| **rara-console** (`console`) | Operator UI — Go BFF embedding a SvelteKit SPA; thin proxy over the core surface (bearer token stays server-side). | — (reads via core) | Go HTTP service on tailnet | 104 |
| **rara-addon** | Shared SDK for claim-workers (claim / heartbeat / result / requeue / poke). Not an agent. | — | Library | 18 |

### Collectors (1.0 workers → bridge-total)

| Agent | Collects | Owns | On SDK? | Tests |
|-------|----------|------|---------|-------|
| **rara-harvest** (`etl-job`) | Latest videos from target YouTube channels (Data API v3) | `target_channels`, `channel_videos` | batch | 15 |
| **rara-shelf** (`shelf-job`) | Owner's own playlists (public/unlisted/private) via OAuth | `playlists`, `playlist_videos` | batch | 13 |
| **rara-dial** (`dial-job`) | Episodes from operator-curated podcast RSS feeds | `podcast_feeds`, `podcast_episodes` | batch | 15 |
| **rara-courier** (`courier-job`) | Emails from Gmail (OAuth refresh token) | `emails` | batch | 9 |
| **rara-clip** (`clip-job`) | LinkedIn posts via Bright Data | `linkedin_posts` | **claim-worker** | 14 |
| **rara-feed** (`feed-job`) | AI/ML news — RSS/Atom, Hacker News, HTML (JSON-LD) | `feed_sources`, `news_items` | batch | 28 |

All collectors run as **GCP Cloud Run Jobs** (daily) except where noted in their README.

### Process (to-text → gate → curate → learn)

| Agent | Does | Reads → Writes | Engine seam | Tests |
|-------|------|----------------|-------------|-------|
| **rara-scribe** (`scribe-job`) | ASR — native-language transcripts (yt-dlp + ffmpeg + Whisper) | video/episode tables → `transcripts`, `transcript_segments` | `TRANSCRIBE_ENGINE` = local \| groq \| gemini | 43 |
| **rara-glean** (`glean-job`) | Normalize text-lane inputs into the to-text store | `emails`, `linkedin_posts` → `transcripts` | `GLEAN_PROVIDER` = extrair-email \| extrair-linkedin | 20 |
| **rara-sift** (`sift-job`) | Curation gate — keep/drop/defer via rules → profile → LLM-judge cascade | `interest_profile`, `gate_rules`, `items` → `gate_decisions` | `SIFT_GATE` = gate_barato \| gate_rico | 20 |
| **rara-distill** (`distill-job`) | Curate transcripts into RAG-ready knowledge docs (Fabric-style patterns) | `transcripts` → `distillations` | `CURATE_ENGINE` = gemini \| claude \| groq \| litellm | 43 |
| **rara-hone** (`hone-job`) | Learning loop — turn feedback into a revised interest profile | `feedback`, `distillations` → `interest_profile` | LiteLLM narrator + deterministic engine | 19 |

**Runtime split:** every agent is a Cloud Run Job *except* `rara-scribe` (and the resident
claim-workers it neighbours), which can run on a **local Mac via `launchd`** — YouTube blocks audio
downloads from datacenter IPs, so transcription needs a residential IP. `rara-core reconcile --loop`
runs always-on in the VPC. SDK claim-workers (`scribe`, `glean`, `sift`, `distill`) run either
`on_demand` (Cloud Run, activated by core) or **resident** (poll + poke), chosen by env.

The **Kura** "second brain" (separate project) consumes `distillations` to build its own RAG — total
isolation: distill never calls Kura.

## Conventions

- **TDD is mandatory** — Red→Green→Refactor with a fluent harness
  (`NewShelfHarness(t).With…().Execute(ctx)`) over an in-memory `MockDatabase` that mirrors real SQL
  constraints. Tests do **zero real I/O** — never hit Neon or an external API in a unit test.
- **Idempotency everywhere** — all writes are upserts (`ON CONFLICT`); every agent and migration is
  safe to re-run. The reconciler is level-triggered for the same reason.
- **Shared-DB isolation** — no foreign keys cross an agent boundary (links are logical only). Even
  `updated_at` triggers are namespaced per agent (`set_updated_at` / `shelf_set_updated_at` / …).
- **Config is environment-only** — required vars fail fast with `log.Fatalf`. See each agent's
  `.env.example`. Pluggable engines are chosen by env behind an interface seam, never an `if` on a
  provider name.

## Build / test / lint

Everything is **per-agent** — `cd` into the agent dir first. Each has an identical Makefile:

```bash
cd rara-distill          # (or any rara-* dir)
make test                # go test -v   (zero I/O: MockDatabase mirrors the SQL contract)
make test-race           # go test -race
make test-coverage       # coverage.out + func summary
make lint                # go vet + staticcheck (auto-installs staticcheck if missing)
make build               # local binary
make build-arm64         # CGO_ENABLED=0 GOOS=linux GOARCH=arm64 — the VPC/Mac arch (Ampere/Apple Silicon); Cloud Run runs amd64
make all                 # clean + lint + test + build
```

Run a single test: `cd rara-core && go test -run TestReconcileOnce -v`.

Modules on the SDK couple to it via a `replace rara-addon => ../rara-addon` directive (deliberately
*not* a committed `go.work` — that's gitignored, and would break each module's isolated CI). Each
still builds standalone.

## CI / migrations / deploy

In `.github/workflows/`, each agent gets its own path-filtered trio, triggered only when its path
changes:

- `ci-<agent>.yml` — vet + staticcheck + tests.
- `database-<agent>.yml` — applies that agent's `migrations/` to Neon on merge to `main`; on a PR it
  validates inside `BEGIN; … ROLLBACK;` (never commits). Migrations apply regardless of where the
  binary runs (even local scribe).
- `deploy-<agent>.yml` — builds the image and deploys the Cloud Run Job (Cloud Run runs **amd64**).
  Most SDK claim-workers share `_reusable-deploy-worker.yml` (single-arch via Cloud Build);
  `rara-sift` is the first on a single **multi-arch** image (amd64 for Cloud Run + arm64 for the
  VPC/Mac runner hosts) built with `docker buildx` — see `DOCKER-MULTIMODULE.md`.
  `deploy-litellm.yml` ships the inference router. No deploy for scribe when it runs locally.

Auth to GCP is **Workload Identity Federation** (no SA key files); secrets live in **GCP Secret
Manager** (except scribe, which reads `~/.rara-scribe/.env`). When adding an agent, copy an existing
agent's Makefile + the three workflow files and adapt the path filters.

## Infrastructure

| Component | Detail |
|-----------|--------|
| **GCP Project** | real value in GitHub Variable `GCP_PROJECT_ID` |
| **Region** | real value in GitHub Variable `GCP_REGION` |
| **Artifact Registry** | `<REGION>-docker.pkg.dev/<PROJECT_ID>/rara/` |
| **Database** | Neon PostgreSQL — shared by all agents, isolated tables |
| **Always-on control plane** | `rara-core` on an Oracle VPS via `systemd` (not Cloud Run) |
| **Inference** | LiteLLM router (`deploy-litellm.yml`) fronts the curation/gate LLMs |
| **Auth to GCP** | Workload Identity Federation — no SA key files |
| **Secrets** | GCP Secret Manager; local `~/.rara-scribe/.env` for scribe |
| **CI/CD** | GitHub Actions — path-filtered per agent, actions pinned by SHA |

See [INFRASTRUCTURE.md](./INFRASTRUCTURE.md) for the full layout,
[DATABASE_SCHEMA.md](./DATABASE_SCHEMA.md) for the data model, and
[ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md) for the claim-worker contract.

## Adding a new agent

1. `mkdir rara-<name>` — separate Go module, flat `package main`.
2. Write the failing test first (Red), implement until green, refactor — over `MockDatabase`.
3. Add `migrations/`, a `Makefile` (copy an existing one), and a `.env.example`.
4. Decide: batch driver or **claim-worker on `rara-addon`** (prefer the SDK for new workers).
5. Choose a runtime — Cloud Run Job (`Dockerfile` + `deploy-<name>.yml`) or local launchd
   (`install-local.sh` + plist, no deploy workflow).
6. Copy `ci.yml` / `database.yml` → `ci-<name>.yml` / `database-<name>.yml` and adapt path filters.
7. Update this README.

## License

MIT
