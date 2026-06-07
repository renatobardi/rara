# rara

**Autonomous Agent Ecosystem** — Agents for collecting, cataloging, and processing YouTube data, built with Go and TDD.

## About

`rara` is an umbrella repository where each agent is:
- 🔒 **Isolated** — independent codebase, tables, and workflows
- 🧪 **TDD-built** — Red-Green-Refactor with a fluent harness, 100% business-logic coverage
- 🗄️ **Shared storage** — one Neon PostgreSQL database, isolated tables per agent
- 🔐 **Secure** — Workload Identity Federation (no SA key files), Secret Manager, pinned action SHAs

Runtime differs per agent: the **collectors** (harvest, shelf) run serverless on GCP Cloud Run
Jobs; the **transcriber** (scribe) runs locally on a Mac via `launchd`, because YouTube blocks
audio downloads from datacenter IPs (see [rara-scribe](#-rara-scribe)).

## Production Agents

### 🎬 rara-harvest
Harvests the latest videos from **external YouTube channels** (public) and stores them in Neon.

- **Auth**: YouTube Data API v3 key (1 quota point per channel via `/playlistItems`)
- **Source**: `target_channels` table — 102 channels seeded
- **Tables**: `target_channels`, `channel_videos`
- **Uniqueness**: global `UNIQUE(youtube_video_id)` — one video per row
- **Runtime**: GCP Cloud Run Job (daily)
- **Tests**: 14/14 passing
- **Status**: ✅ Production — collecting daily

[README →](./rara-harvest/README.md) | [DEPLOY →](./rara-harvest/DEPLOY.md)

---

### 📚 rara-shelf
Catalogs the **owner's own YouTube playlists** (public, unlisted, and private) and the videos in each, recording which playlist each video belongs to.

- **Auth**: OAuth 2.0 refresh token (scope `youtube.readonly`) — reads private playlists
- **Discovery**: automatic via `playlists.list?mine=true` — no seed table needed
- **Tables**: `playlists`, `playlist_videos`
- **Uniqueness**: composite `UNIQUE(playlist_id, youtube_video_id)` — same video can be in many playlists
- **Runtime**: GCP Cloud Run Job (daily)
- **Tests**: 13/13 passing
- **Status**: ✅ Production — collecting daily

[README →](./rara-shelf/README.md) | [DEPLOY →](./rara-shelf/DEPLOY.md)

---

### ✍️ rara-scribe
Produces **high-quality transcripts in the audio's native language** for the videos collected by harvest (`channel_videos`) and shelf (`playlist_videos`), replacing YouTube's weak auto-captions with specialist ASR.

- **Engine**: pluggable via `TRANSCRIBE_ENGINE` — `local` (default in prod: whisper.cpp large-v3, Metal, ~11× real-time), `groq` (whisper-large-v3, fallback per-chunk), `gemini` (gemini-2.5-flash)
- **Pipeline**: `yt-dlp` (download) → `ffmpeg` (16 kHz mono, 60-min chunks for local / 10-min for API) → ASR → stitched transcript
- **Tables**: `transcripts`, `transcript_segments`
- **Uniqueness**: global `UNIQUE(youtube_video_id)` — idempotent, resumes the backlog
- **Runtime**: **local Mac via `launchd`** (daily at 02:00) — residential IPs bypass YouTube's datacenter bot-check
- **Tests**: 29/29 passing
- **Status**: ✅ Production — running locally

[README →](./rara-scribe/README.md) | [DEPLOY →](./rara-scribe/DEPLOY.md)

---

### 🧪 rara-distill
Curates the **raw transcripts** produced by scribe into **knowledge documents ready for RAG ingestion**, using a Fabric-style library of editable Markdown patterns. Reads `transcripts`, writes its own `distillations` table. The **Kura** "second brain" (separate project) consumes `distillations` later to build its own RAG — total isolation: distill never calls Kura.

- **Engine**: pluggable via `CURATE_ENGINE` — `gemini` (default), `claude` or `groq`
- **Curation**: Fabric-style **patterns** + optional **contexts** + **strategies** + **sessions** (pattern chains), all Markdown embedded via `go:embed`
- **Output**: per `(source, recipe)` — human `content` (Markdown) **plus** queryable `structured` and a `doc_context` for Contextual Retrieval, in a single LLM pass ("compile once")
- **Tables**: `distillations` (own); reads `transcripts`, `channel_videos`, `playlist_videos`, and (news lane) `news_items`
- **Source lanes**: `DISTILL_SOURCE=transcripts` (default) or `news` — the news lane reads rara-feed's `news_items` with a fixed `summarize_news` + `software-ai` recipe, as its own Cloud Run Job so it never starves the transcript lane
- **Uniqueness**: `UNIQUE(source_key, COALESCE(session_patterns, pattern))` — idempotent; reprocesses when the source or the recipe changes (dual hash)
- **Runtime**: GCP Cloud Run Job (daily, after scribe; the news lane runs after feed)
- **Tests**: 35/35 passing (+ an opt-in Postgres integration test for the pending-queue SQL)
- **Status**: ✅ Production

[README →](./rara-distill/README.md) | [DEPLOY →](./rara-distill/DEPLOY.md)

---

### 📰 rara-feed
Collects **AI/ML news** from RSS feeds, Hacker News (Algolia) and HTML pages into a `news_items` table — an upstream source the distill **news lane** curates. Reads its work queue from `feed_sources`, writes its own `news_items` table.

- **Sources**: RSS 2.0 **and** Atom (auto-detected), Hacker News by search term, HTML via a generic **JSON-LD** extractor (v1)
- **Resilience**: a source that fails (block / JS / timeout) is skipped — never brings down the batch; idempotent re-runs via `content_sha256` staleness
- **Full-text**: best-effort article fetch when the feed ships no inline body; `fetch_status` (`full|excerpt|failed`) records coverage
- **Fetcher seam**: 3-tier transport — `HTTPFetcher` (default) | `UnlockerFetcher` (Bright Data Web Unlocker, opt-in via `SCRAPE_PROVIDER=brightdata`) | `routingFetcher` dispatcher — per-source `fetch_strategy` column routes each URL to the right tier
- **Tables**: `news_items` (own) + `feed_sources` (work queue, seeded); deduped on `url` (HN text posts keyed by permalink)
- **Runtime**: GCP Cloud Run Job (daily, **before** the distill news lane). Only `database-url` secret needed in v1; Bright Data unlocker is an opt-in follow-up
- **Tests**: 28/28 passing (MockFetcher with call/strategy tracking + MockDatabase, zero I/O)
- **Status**: ✅ Production

[README →](./rara-feed/README.md) | [DEPLOY →](./rara-feed/DEPLOY.md)

---

### 🔮 rara-pulse *(coming soon)*
### 🌊 rara-stream *(coming soon)*

---

## Infrastructure

| Component | Detail |
|-----------|--------|
| **GCP Project** | `<PROJECT_ID>` (real value in GitHub Variable `GCP_PROJECT_ID`) |
| **Region** | `<REGION>` (real value in GitHub Variable `GCP_REGION`) |
| **Artifact Registry** | `<REGION>-docker.pkg.dev/<PROJECT_ID>/rara/` (harvest + shelf images) |
| **Database** | Neon PostgreSQL (free tier) — shared by all agents, isolated tables |
| **Auth to GCP** | Workload Identity Federation — no SA key files |
| **Service Account** | `rara-deployer@<PROJECT_ID>.iam.gserviceaccount.com` |
| **Secrets** | GCP Secret Manager (harvest/shelf); local `~/.rara-scribe/.env` (scribe) |
| **CI/CD** | GitHub Actions — path-filtered per agent, actions pinned by SHA |

See [INFRASTRUCTURE.md](./INFRASTRUCTURE.md) for the full layout and [ARCHITECTURE.md](./ARCHITECTURE.md) for the system design.

### GCP Secrets in Secret Manager

| Secret | Used by |
|--------|---------|
| `youtube-api-key` | rara-harvest (YouTube Data API v3) |
| `database-url` | rara-harvest, rara-shelf, rara-feed, rara-distill (shared Neon connection string) |
| `shelf-oauth-client-id` | rara-shelf (OAuth Web app client) |
| `shelf-oauth-client-secret` | rara-shelf |
| `shelf-oauth-refresh-token` | rara-shelf (scope: youtube.readonly) |
| `gemini-api-key` | rara-distill (curation LLM; default engine — both lanes) |
| `anthropic-api-key` / `groq-api-key` | rara-distill (only if `CURATE_ENGINE` switched) |
| `brightdata-token` | rara-feed (only if `SCRAPE_PROVIDER=brightdata`; unlocker tier — not in v1) |

> **rara-scribe does not use Secret Manager.** It runs locally and reads `DATABASE_URL` and
> `GROQ_API_KEY` from `~/.rara-scribe/.env`. The old `groq-api-key` and `yt-dlp-cookies`
> Secret Manager entries from its Cloud Run era can be deleted (see scribe DEPLOY.md).

### GitHub Secrets / Variables

| Name | Type | Purpose |
|------|------|---------|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Secret | WIF provider resource name |
| `GCP_SERVICE_ACCOUNT` | Secret | `rara-deployer@<PROJECT_ID>.iam.gserviceaccount.com` |
| `NEON_HOST/PORT/DATABASE/USERNAME/PASSWORD` | Secret | Neon DB credentials for CI migrations |
| `GCP_PROJECT_ID` | Variable | the GCP project ID |
| `GCP_REGION` | Variable | the GCP region |

---

## Repository Structure

```
rara/
├── .github/workflows/
│   ├── ci.yml              # Code quality + tests (rara-harvest)
│   ├── ci-shelf.yml        # Code quality + tests (rara-shelf)
│   ├── ci-scribe.yml       # Code quality + tests (rara-scribe)
│   ├── ci-distill.yml      # Code quality + tests (rara-distill)
│   ├── ci-feed.yml         # Code quality + tests (rara-feed)
│   ├── database.yml        # Migrations (rara-harvest)
│   ├── database-shelf.yml  # Migrations (rara-shelf)
│   ├── database-scribe.yml # Migrations (rara-scribe)
│   ├── database-distill.yml# Migrations (rara-distill)
│   ├── database-feed.yml   # Migrations (rara-feed)
│   ├── deploy.yml          # Cloud Run deploy (rara-harvest)
│   ├── deploy-shelf.yml    # Cloud Run deploy (rara-shelf)
│   ├── deploy-distill.yml  # Cloud Run deploy (rara-distill + news lane)
│   └── deploy-feed.yml     # Cloud Run deploy (rara-feed)
│                           # (no deploy-scribe.yml — scribe runs locally)
├── rara-harvest/           # YouTube channel video harvester (Cloud Run)
│   ├── main.go
│   ├── main_test.go        # 14 TDD tests, ETLHarness
│   ├── migrations/
│   │   ├── 001_initial_schema.sql
│   │   └── 002_schema_refinements.sql
│   └── ...
├── rara-shelf/             # Personal playlist cataloger (Cloud Run)
│   ├── main.go
│   ├── main_test.go        # 13 TDD tests, ShelfHarness
│   ├── migrations/
│   │   └── 001_initial_schema.sql
│   └── ...
├── rara-scribe/            # Transcriber (local Mac via launchd)
│   ├── main.go
│   ├── main_test.go        # 29 TDD tests, ScribeHarness
│   ├── install-local.sh    # launchd installer (no Dockerfile/deploy)
│   ├── migrations/
│   │   ├── 001_initial_schema.sql
│   │   ├── 002_widen_language.sql
│   │   └── 003_attempt_count.sql
│   └── ...
├── rara-distill/           # Transcript curator → RAG material (Cloud Run)
│   ├── main.go
│   ├── main_test.go        # 35 TDD tests, DistillHarness + mock LLM
│   ├── patterns/           # Fabric-style curation library (go:embed)
│   │   └── summarize_news/ # news-lane pattern (DISTILL_SOURCE=news)
│   ├── contexts/
│   ├── strategies/
│   ├── migrations/
│   │   └── 001_initial_schema.sql
│   └── ...
├── rara-feed/              # AI/ML news collector → news_items (Cloud Run)
│   ├── main.go
│   ├── main_test.go        # 28 TDD tests, FeedHarness + MockFetcher
│   ├── migrations/
│   │   ├── 001_initial_schema.sql  # schema + seed (feed_sources, news_items)
│   │   └── 002_semi_rss.sql        # SemiAnalysis html → rss
│   └── ...
├── ARCHITECTURE.md
├── INFRASTRUCTURE.md
└── README.md
```

---

## TDD Pattern (all agents)

Every agent uses the same **Red-Green-Refactor** cycle with a **fluent harness**:

```go
// Example: ShelfHarness (rara-shelf)
harness := NewShelfHarness(t).
    WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "My List"}}).
    WithVideosForPlaylist("PL1", makePlaylistItem("vid1", "Video 1"))

harness.Execute(context.Background())
harness.AssertPlaylistCount(1)
harness.AssertVideoCount(1)
```

- `MockDatabase` — in-memory, mirrors real SQL constraints
- Zero I/O in tests — all external deps mocked
- 100% business-logic coverage

---

## Adding a New Agent

1. `mkdir rara-<name>` — create directory
2. Write failing tests first (Red)
3. Implement until tests pass (Green)
4. Add `migrations/`, `Makefile`
5. Choose a runtime:
   - **Cloud Run** (like harvest/shelf): add a `Dockerfile`, copy `deploy.yml` → `deploy-<name>.yml`
   - **Local** (like scribe): add an `install-local.sh` + launchd plist — no Dockerfile, no deploy workflow
6. Copy and adapt `database.yml` → `database-<name>.yml` (migrations apply to Neon regardless of runtime)
7. Copy and adapt `ci.yml` → `ci-<name>.yml`
8. Update this README

---

## Cost

| Agent | Execution | Est. monthly |
|-------|-----------|--------------|
| rara-harvest | Daily (Cloud Run) | ~$0.02 |
| rara-shelf | Daily (Cloud Run) | ~$0.02 |
| rara-scribe | Daily (local Mac) | $0 compute + Groq ASR (~$0.111/h of audio) |
| rara-distill | Daily (Cloud Run) | ~$0.02 compute + curation LLM per transcript (default `gemini-2.5-pro`; cheaper on a Flash model) |
| Cloud Build | Per deploy | ~$0.00 (free tier) |
| Neon DB | Always-on | ~$0.00 (free tier) |

rara-scribe's only ongoing cost is the Groq API. The one-time backlog (~1,200 videos) is a few
tens of dollars; incremental daily runs are cents.

---

## License

MIT
