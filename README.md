# rara

**Autonomous Agent Ecosystem** — Agents for collecting, cataloguing and processing YouTube data, built with Go, TDD, and deployed serverless on GCP Cloud Run.

## About

`rara` is an umbrella repository where each agent is:
- 🔒 **Isolated** — independent codebase, tables, Cloud Run Job and workflows
- 🧪 **TDD-built** — Red-Green-Refactor with fluent harness, 100% business logic coverage
- ☁️ **Cloud-Native** — Docker (amd64), GCP Cloud Run Jobs, Neon PostgreSQL
- 💰 **Cost-Efficient** — pay-per-execution, ~$0.02/month per agent
- 🔐 **Secure** — Workload Identity Federation (no SA key files), Secret Manager, pinned action SHAs

## Production Agents

### 🎬 rara-harvest
Harvests the latest videos from **external YouTube channels** (public) and stores them in Neon.

- **Auth**: YouTube Data API v3 key (1 quota point per channel via `/playlistItems`)
- **Source**: `target_channels` table — 102 channels seeded
- **Tables**: `target_channels`, `channel_videos`
- **Uniqueness**: global `UNIQUE(youtube_video_id)` — one video per row
- **Tests**: 14/14 passing
- **Status**: ✅ Production — collecting daily

```bash
cd rara-harvest && make test
```

[README →](./rara-harvest/README.md) | [DEPLOY →](./rara-harvest/DEPLOY.md)

---

### 📚 rara-shelf
Catalogues the **owner's own YouTube playlists** (public, unlisted and private) and the videos in each, recording which playlist each video belongs to.

- **Auth**: OAuth 2.0 refresh token (scope `youtube.readonly`) — reads private playlists
- **Discovery**: automatic via `playlists.list?mine=true` — no seed table needed
- **Tables**: `playlists`, `playlist_videos`
- **Uniqueness**: composite `UNIQUE(playlist_id, youtube_video_id)` — same video can be in many playlists
- **Tests**: 12/12 passing
- **Status**: ✅ Production — first run completed

```bash
cd rara-shelf && make test
```

[README →](./rara-shelf/README.md) | [DEPLOY →](./rara-shelf/DEPLOY.md)

---

### 🔮 rara-pulse *(coming soon)*
### 🌊 rara-stream *(coming soon)*

---

## Infrastructure

| Component | Detail |
|-----------|--------|
| **GCP Project** | `oute-rara` |
| **Region** | `us-central1` |
| **Artifact Registry** | `us-central1-docker.pkg.dev/oute-rara/rara/` |
| **Database** | Neon PostgreSQL (free tier) |
| **Auth to GCP** | Workload Identity Federation — no SA key files |
| **Service Account** | `rara-deployer@oute-rara.iam.gserviceaccount.com` |
| **Secrets** | GCP Secret Manager (`youtube-api-key`, `database-url`, `shelf-oauth-*`) |
| **CI/CD** | GitHub Actions — path-filtered per agent, actions pinned by SHA |

### GCP Secrets in Secret Manager

| Secret | Used by |
|--------|---------|
| `youtube-api-key` | rara-harvest (YouTube Data API v3) |
| `database-url` | rara-harvest + rara-shelf (Neon connection string) |
| `shelf-oauth-client-id` | rara-shelf (OAuth Web app client) |
| `shelf-oauth-client-secret` | rara-shelf |
| `shelf-oauth-refresh-token` | rara-shelf (scope: youtube.readonly) |

### GitHub Secrets / Variables

| Name | Type | Purpose |
|------|------|---------|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Secret | WIF provider resource name |
| `GCP_SERVICE_ACCOUNT` | Secret | `rara-deployer@oute-rara.iam.gserviceaccount.com` |
| `NEON_HOST/PORT/DATABASE/USERNAME/PASSWORD` | Secret | Neon DB credentials for CI migrations |
| `GCP_PROJECT_ID` | Variable | `oute-rara` |
| `GCP_REGION` | Variable | `us-central1` |

---

## Repository Structure

```
rara/
├── .github/workflows/
│   ├── ci.yml              # Code quality + tests (rara-harvest)
│   ├── ci-shelf.yml        # Code quality + tests (rara-shelf)
│   ├── database.yml        # Migrations (rara-harvest)
│   ├── database-shelf.yml  # Migrations (rara-shelf)
│   ├── deploy.yml          # Cloud Run deploy (rara-harvest)
│   └── deploy-shelf.yml    # Cloud Run deploy (rara-shelf)
├── rara-harvest/           # YouTube channel video harvester
│   ├── main.go
│   ├── main_test.go        # 14 TDD tests, ETLHarness
│   ├── migrations/
│   │   ├── 001_initial_schema.sql
│   │   └── 002_schema_refinements.sql
│   └── ...
├── rara-shelf/             # Personal playlist cataloguer
│   ├── main.go
│   ├── main_test.go        # 12 TDD tests, ShelfHarness
│   ├── migrations/
│   │   └── 001_initial_schema.sql
│   └── ...
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
- 100% business logic coverage

---

## Adding a New Agent

1. `mkdir rara-<name>` — create directory
2. Write failing tests first (Red)
3. Implement until tests pass (Green)
4. Add `migrations/`, `Dockerfile`, `Makefile`
5. Copy and adapt `deploy.yml` → `deploy-<name>.yml` (path filter, JOB_NAME, IMAGE, secrets)
6. Copy and adapt `database.yml` → `database-<name>.yml`
7. Update this README

---

## Cost

| Agent | Execution | Est. monthly |
|-------|-----------|--------------|
| rara-harvest | Daily | ~$0.02 |
| rara-shelf | Daily | ~$0.02 |
| Cloud Build | Per deploy | ~$0.00 (free tier) |
| Neon DB | Always-on | ~$0.00 (free tier) |
| **Total** | | **< $0.10/month** |

---

## License

MIT
