# rara-shelf

Second agent in the **rara** ecosystem. While `rara-harvest` collects videos from external
public channels, **rara-shelf** does the opposite: it catalogs **your own YouTube playlists**
(public, private, and unlisted) and the videos in each one, recording **which playlist each
video belongs to**.

Fully isolated from harvest (own code, tables, Cloud Run Job, and workflows), reusing the
same GCP + Neon infrastructure.

## How it works

1. Exchanges a **refresh token** for an access token via `oauth2.googleapis.com/token`.
2. Discovers all your playlists via `playlists.list?mine=true` (paginated).
3. For each playlist, lists the videos via `playlistItems.list` (paginated) and writes them to Neon.

```
OAuth refresh token → access token
   → playlists.list?mine=true          → table `playlists`
   → playlistItems.list (per playlist) → table `playlist_videos` (with playlist_id)
```

## Key differences from rara-harvest

| | rara-harvest | rara-shelf |
|---|---|---|
| Auth | API key (public) | **OAuth** (refresh token) |
| Source | channel seed table | **discovered** `mine=true` |
| Video uniqueness | global (`youtube_video_id`) | **composite** `(playlist_id, youtube_video_id)` |
| Endpoint | `/playlistItems` (uploads) | `/playlists` + `/playlistItems` |

The composite uniqueness means the **same video can appear in multiple playlists** —
stored once per playlist.

## Data model

- **`playlists`**: `youtube_playlist_id` (unique), `title`, `description`,
  `privacy_status`, `item_count`, `active`, timestamps.
- **`playlist_videos`**: `playlist_id` (FK), `youtube_video_id`, `title`, `url`,
  `published_at` (nullable), `position`, `collected_at`, `UNIQUE(playlist_id, youtube_video_id)`.

## Environment variables

| Var | Description |
|-----|-------------|
| `DATABASE_URL` | Neon connection string (shared with harvest) |
| `GOOGLE_OAUTH_CLIENT_ID` | OAuth client ID |
| `GOOGLE_OAUTH_CLIENT_SECRET` | OAuth client secret |
| `GOOGLE_OAUTH_REFRESH_TOKEN` | Refresh token (scope `youtube.readonly`) |

## Limitations

- **Watch Later / History**: not accessible via the YouTube Data API since 2016. The app
  logs a warning and skips them.

## Development

```bash
make test          # tests (TDD harness)
make test-race     # with race detector
make fmt lint      # formatting + vet
make build         # local binary
```

## Deploy

See [DEPLOY.md](DEPLOY.md) — OAuth setup + Cloud Run via GitHub Actions.
