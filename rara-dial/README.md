# rara-dial

The 2.0 **Podcast lane** collector. An isolated agent that polls operator-curated RSS feeds and
catalogs each episode (an RSS `<item>` carrying an audio enclosure) into its own domain tables.

Like every rara agent it shares nothing but the Neon database and never calls another agent. The
control plane (`rara-core`) reads `podcast_episodes` to build the items spine
(`lane=podcast`, `source_ref=guid`), and the `asr-direct-audio` worker reads `enclosure_url` to
transcribe the episode — both cross-agent SELECTs, never writes.

## Tables (`migrations/001_initial_schema.sql`)

- `podcast_feeds` — RSS feeds to poll (`feed_url` UNIQUE, `active`, `title` refreshed from the feed).
- `podcast_episodes` — one row per audio episode (`guid` UNIQUE = the spine's `source_ref`,
  `enclosure_url` = the direct CDN audio URL, `title`, `published_at`).

## Run

```bash
export DATABASE_URL=postgresql://...
go run .          # poll every active feed, upsert episodes (idempotent on guid)
```

Add feeds with `seed.sql` (or a direct `INSERT INTO podcast_feeds`). Re-running converges:
episodes upsert on `guid`, so a re-poll never duplicates and refreshes edited metadata.

## Design

The RSS parse (`parseRSS`) and the collector loop (`run`) are pure over two seams — a `Fetcher`
(HTTP) and a `Database` (Neon) — so the whole logic is unit-tested with zero I/O (`make test`).
Only audio enclosures are kept; an item with no GUID falls back to its enclosure URL as the
stable id. Pipeline downstream (gates, transcription via `asr-direct-audio`, distillation) is
driven entirely by `rara-core`.
