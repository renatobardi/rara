# Architecture — rara ecosystem

How the three production agents fit together, the data they share, and why each one runs where
it does.

## Overview

`rara` is a set of independent Go agents that share a single Neon PostgreSQL database but own
isolated tables. Two agents **collect** video references; one agent **transcribes** them.

```
                      ┌──────────────────────────────────────────┐
                      │            Neon PostgreSQL                │
                      │  (shared database, isolated tables)       │
                      └──────────────────────────────────────────┘
                         ▲                ▲                 ▲
        writes channel_videos    writes playlist_videos    reads both,
                │                        │            writes transcripts
   ┌────────────┴───────┐   ┌────────────┴───────┐   ┌────┴──────────────┐
   │   rara-harvest     │   │    rara-shelf      │   │    rara-scribe    │
   │  Cloud Run Job     │   │   Cloud Run Job    │   │  local Mac/launchd│
   │  (datacenter IP)   │   │   (datacenter IP)  │   │ (residential IP)  │
   └────────┬───────────┘   └────────┬───────────┘   └────────┬──────────┘
            │                        │                        │
   YouTube Data API v3      YouTube Data API v3        yt-dlp + ffmpeg
     (API key, public)        (OAuth, private)         + Groq/Gemini ASR
```

## Data flow

1. **rara-harvest** pulls the latest uploads from ~100 external channels and upserts them into
   `channel_videos` (one row per video, globally unique by `youtube_video_id`).
2. **rara-shelf** discovers the owner's own playlists via OAuth and upserts every video into
   `playlist_videos` (unique per `(playlist_id, youtube_video_id)` — the same video can live in
   multiple playlists).
3. **rara-scribe** treats `channel_videos ∪ playlist_videos` as its work queue. For each video
   without a `done` transcript, it downloads the audio, runs ASR, and writes a `transcripts`
   header row plus N `transcript_segments` (timestamped) in a single transaction.

The collectors and the transcriber are decoupled by the database: scribe never calls harvest or
shelf, it just reads their tables. Adding a fourth agent (e.g. enrichment over transcripts)
follows the same pattern — read upstream tables, write your own.

## Why scribe runs locally

harvest and shelf hit the YouTube **Data API** (JSON, key/OAuth) — datacenter IPs are fine, so
they run as serverless Cloud Run Jobs. scribe instead downloads **audio** with `yt-dlp`, and
YouTube blocks audio downloads from GCP datacenter IPs with a bot-check ("Sign in to confirm
you're not a bot"), regardless of cookies. A residential IP (the owner's Mac) is not blocked, so
scribe runs locally via `launchd` on a daily schedule. This is the single deliberate runtime
divergence in the system.

## Per-agent design

| | rara-harvest | rara-shelf | rara-scribe |
|---|---|---|---|
| **Purpose** | latest videos from external channels | catalog owner's playlists | transcribe collected videos |
| **Auth** | API key (public) | OAuth refresh token (private) | none (Groq API key for ASR) |
| **External I/O** | YouTube Data API | YouTube Data API | yt-dlp, ffmpeg, Groq/Gemini |
| **Tables** | `target_channels`, `channel_videos` | `playlists`, `playlist_videos` | `transcripts`, `transcript_segments` |
| **Runtime** | Cloud Run Job | Cloud Run Job | local Mac (launchd, 02:00) |
| **Pagination** | single recency page (latest N) | full `nextPageToken` loop | n/a (queue from DB) |
| **Tests** | 14 | 12 | 13 |

## Shared conventions

- **Language**: Go, single `main.go` per agent, `pgx/v5` driver.
- **Config**: environment variables; required vars fail fast with `log.Fatalf`.
- **TDD**: every agent has a fluent harness + in-memory `MockDatabase` mirroring real SQL
  constraints; zero I/O in tests.
- **Idempotency**: all writes are upserts (`ON CONFLICT`), so any agent is safe to re-run.
- **Isolation**: tables, migrations, CI, and (for collectors) deploy workflows are per-agent.
  Even the `updated_at` trigger functions are namespaced (`set_updated_at` vs `shelf_set_updated_at`)
  to avoid collisions in the shared database.

## Database schema (high level)

- `target_channels` → seed list of channels harvest pulls from.
- `channel_videos` → harvested videos (`youtube_video_id` unique).
- `playlists` / `playlist_videos` → shelf's catalog, composite uniqueness.
- `transcripts` → one row per transcribed video (`youtube_video_id` unique, `language` TEXT,
  `engine`, `status` `done`/`failed`, full `transcript` text).
- `transcript_segments` → timestamped segments (`start_seconds`, `end_seconds`, `text`),
  re-indexed to a global timeline across audio chunks.

## Technology stack

- **Go 1.23+** — minimal, fast, single binary per agent.
- **Neon PostgreSQL** — serverless Postgres, free tier, shared across agents.
- **GCP Cloud Run Jobs** — harvest + shelf runtime (amd64 images via Cloud Build).
- **launchd** — scribe runtime on macOS (daily schedule, local logs).
- **yt-dlp + ffmpeg** — audio acquisition and resampling for scribe.
- **Groq `whisper-large-v3` / Gemini `gemini-2.5-flash`** — pluggable ASR engines.

See [INFRASTRUCTURE.md](./INFRASTRUCTURE.md) for the concrete infrastructure layout (GCP, WIF,
secrets, local Mac setup).
