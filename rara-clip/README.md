# rara-clip

The 2.0 **LinkedIn lane** automated collector. An isolated agent that collects posts from
LinkedIn via **Bright Data** and catalogs each into its own domain table, `linkedin_posts`.

Like every rara agent it shares nothing but the Neon database and never calls another agent. The
control plane (`rara-core`) reads `linkedin_posts` to build the items spine
(`lane=linkedin`, `source_ref=url`, **`sensitivity=public`**), and the `extrair-linkedin` worker
(`rara-glean`) reads `body` to clean it into a to-text artifact — both cross-agent SELECTs, never
writes. rara-clip writes **only** its domain table; it never touches the spine.

## Two producers, one contract table

`linkedin_posts` is a **contract table** with two producers, both idempotent on the canonical post
URL — multiple producers are fine:

- **rara-clip** (this agent) — the **automated** Bright Data crawl, the default collector.
- **rara-core**'s **manual inbox** — a person pastes a post's URL + text through the surface (an
  MCP tool / HTTP endpoint), kept as a fallback for posts the crawl misses.

Both write the same table behind the same contract; nothing downstream knows which producer filled
a row. Turning `linkedin_posts` into spine items is `rara-core`'s ingest bridge (it reads
`linkedin_posts` the same way it reads `emails` / `podcast_episodes`), unchanged by this app.

**Sensitivity.** LinkedIn posts are world-readable, so (unlike email) third-party models may
process them — `sensitivity=public`. This agent only *stores* the post; the routing guarantee is
enforced by `rara-core`'s router.

## Table (`migrations/001_initial_schema.sql`)

- `linkedin_posts` — one row per post (`url` UNIQUE = the spine's `source_ref`, `author` optional,
  `body` raw post text). Self-contained, additive twin of `rara-core`'s definition (whichever agent
  applies first creates it; the other is a no-op).

## Run

```bash
export DATABASE_URL=postgresql://...
export BRIGHTDATA_API_KEY=...                                   # read by the bdata CLI itself
export BRIGHTDATA_LINKEDIN_URLS="https://www.linkedin.com/in/example/recent-activity/all/"
export BRIGHTDATA_LINKEDIN_ARGS="pipelines linkedin-posts --json"  # optional
export BDATA_BIN=bdata                                          # optional; the Bright Data CLI
go run .
```

Re-running converges: posts upsert on `url`, so a re-collect never duplicates and refreshes edited
metadata.

## Design

The Bright Data fetch (a shell-out to the `bdata` CLI) lives behind a `LinkedInCollector` seam; the
Neon write behind a `Database` seam. The JSON normalization (`decodeBrightDataPosts`, which matches
the dataset's varying key names flexibly) and the collector loop (`run` — skip partial rows, upsert
each, idempotent on URL) are pure over those two seams, so the whole logic is unit-tested with zero
I/O (`make test`). The Bright Data API key is read by the CLI from its own env, so rara-clip never
handles the credential. Downstream cleaning, gating, and distillation are driven entirely by
`rara-core`.

## Deploy

No deploy workflow yet — a Cloud Run Job (datacenter IP, like the other collectors) lands in P2.
