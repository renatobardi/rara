# rara-feed

Collector agent for the **rara** pipeline. It crawls AI/ML news from RSS feeds,
Hacker News (Algolia) and HTML pages into the `news_items` table — an upstream
source the **rara-distill** work-queue reads (`status='ready'`).

Like the other agents, it is isolated: its own tables in the shared Neon database, no
direct calls to any other agent.

## What it does

On boot it reads the work queue — the enabled rows in `feed_sources`. For each source:

- **rss** — GET the feed XML, parse RSS 2.0 **or** Atom (auto-detected), extract
  title / link / published date / excerpt / inline content.
- **hn** — build an Algolia `search_by_date` URL from the search term, filter by
  `HN_MIN_POINTS`, and parse the hits. Stories with no external url (Ask HN / text
  posts) are keyed by their HN item permalink.
- **html** — fetch the page and run the generic extractor (**JSON-LD only in v1**).
  Pages that hide their content behind JS/CSS yield nothing until the unlocker/bespoke
  follow-up.

Each item is upserted into `news_items` (`ON CONFLICT (url) DO UPDATE`). When
`FEED_FULLTEXT=true` and the feed shipped no inline body, the article URL is fetched
best-effort and the body extracted; `fetch_status` records the coverage
(`full | excerpt | failed`).

Idempotent and resilient: re-runs skip unchanged items (staleness via
`content_sha256`), and a source that fails (block / JS / timeout) is logged and
skipped — it never brings down the batch.

## Schema

- `feed_sources` — the work queue (name, source_type, endpoint, cls, fetch_strategy,
  parser, enabled). Seeded by `migrations/001_initial_schema.sql`.
- `news_items` — collected items, deduped on `url`. Columns of note: `fetch_status`
  (coverage), `content_sha256` (staleness), `status` (`ready` for distill | `failed`
  when there is no usable text).

## Configuration (env vars)

| Var | Default | Meaning |
|-----|---------|---------|
| `DATABASE_URL` | — (required) | Neon connection string |
| `FEED_BATCH_SIZE` | `25` | max items per source per run |
| `FEED_FULLTEXT` | `true` | best-effort full-text fetch |
| `FEED_SOURCES_FILTER` | (all) | CSV of source names to restrict the run |
| `HN_MIN_POINTS` | `20` | HN story points threshold |
| `ITEM_MAX_AGE_DAYS` | `30` | discard items older than this |
| `FEED_HTTP_TIMEOUT` | `30` | per-request timeout (seconds) |
| `FEED_MAX_RETRIES` | `4` | transient (429/5xx) retries |

No LLM secrets. The Bright Data unlocker tier (`SCRAPE_PROVIDER` / `BRIGHTDATA_*`) is
**not wired in v1** — html sources that get blocked stay excerpt-only until then.

## Develop

```bash
make test         # unit tests (mock Fetcher + MockDatabase + fixtures, zero I/O)
make lint         # go vet + staticcheck
make build        # build the feed-job binary
```

Local smoke (after applying the migration to a Neon branch):

```bash
cp .env.example .env   # fill DATABASE_URL
FEED_SOURCES_FILTER="OpenAI,Hacker News" FEED_BATCH_SIZE=3 go run .
```

## Downstream contract (rara-distill)

`rara-distill` extends its work-queue to read `news_items WHERE status='ready'` as an
additional upstream, mapping `url → source_ref` / `source_key`,
`COALESCE(body, excerpt) → transcript`, `source_type='news'`, and curates them with a
dedicated news pattern. See `rara-distill/`.
