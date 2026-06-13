# rara-distill

Curates the raw transcripts produced by [rara-scribe](../rara-scribe) into
**knowledge documents ready for RAG ingestion**, using a Fabric-style library of
editable Markdown patterns. Reads upstream (`transcripts`), writes its own isolated
table (`distillations`) in the same Neon database. The **Kura** "second brain"
(separate project) consumes `distillations` later to build its own RAG — total
isolation: rara-distill never calls Kura.

- **Engine**: pluggable via `CURATE_ENGINE` — `gemini` (default), `claude`, `groq`, or
  `litellm` (a self-hosted [LiteLLM gateway](./litellm/): distill speaks OpenAI-compatible
  to it and the real model is a gateway alias — the 2.0 anti-lock-in path)
- **Curation**: Fabric-style **patterns** (system prompts) + optional **contexts**
  (injected reference material) + optional **strategies** (reasoning wrappers) +
  **sessions** (chain several patterns over one transcript, each stage sees the
  previous output). All Markdown, embedded in the binary via `go:embed`.
- **Output**: one row per `(source, recipe)` in `distillations` — human `content`
  (Markdown) **plus** queryable `structured` (concepts/insights/entities/claims) and
  a `doc_context` for Contextual Retrieval, all produced in a single LLM pass
  ("compile once").
- **Tables**: `distillations` (own); reads `transcripts`, `channel_videos`,
  `playlist_videos`
- **Runtime**: GCP Cloud Run Job (daily, after scribe)

## How it works

```
transcripts (scribe)  ──read──▶  rara-distill  ──write──▶  distillations  ──read──▶  Kura
(raw text + metadata)           apply pattern(s)          (curated, RAG-ready)      (chunk+embed
                                via LLM (Fabric-style)                               in SurrealDB)
```

The work queue is every `transcripts` row with `status='done'` and a non-empty
transcript that does not yet have a **fresh** distillation for the current recipe.
"Fresh" means: a `done` row exists with the same `source_sha256` (transcript
unchanged) **and** the same `recipe_sha256` (recipe unchanged). Editing a pattern,
swapping the engine/model, or changing the context/strategy changes `recipe_sha256`
and triggers reprocessing — there is no silent skip.

## The curation library (Fabric-style)

```
patterns/<name>/system.md   # a pattern: the system prompt for one curation pass
contexts/<name>.md          # reference material injected into every call
strategies/<name>.md        # a reasoning wrapper (e.g. chain-of-thought)
```

Add or edit a Markdown file to change the curation — that is the whole point. Shipped
patterns: `extract_wisdom` (SUMMARY / CONCEPTS / INSIGHTS / REFERENCES / CONNECTIONS)
and `summary`. Shipped context: `software-ai`. Shipped strategies: `cot`, `tot`.

**Sessions**: set `DISTILL_PATTERNS` to a CSV chain (e.g. `summary,extract_wisdom`).
Each stage receives the original transcript plus the previous stage's output; the
final stage's output is stored. The unique key uses `COALESCE(session_patterns,
pattern)`, so a session never collides with a standalone pattern.

## Output contract (for Kura)

`distillations` columns map directly to a vault ingestion. The shape of `structured`:

```json
{
  "concepts":    ["..."],
  "insights":    ["..."],
  "references":  ["..."],
  "connections": ["..."],
  "entities":    [{"name": "...", "type": "person|tech|org|concept"}],
  "claims":      [{"text": "...", "evidence": "quote/snippet", "ts_start": 123}]
}
```

Suggested Kura mapping (implemented on the Kura side, out of scope here): read
`distillations WHERE status='done'`; `title→title`, `content→content`,
`source_type='rara-distill'`; use `structured` for the graph/precise retrieval and
`doc_context` as the per-chunk prefix (Contextual Retrieval). Check
`structured_status` (`ok`/`empty`/`parse_failed`) to decide whether to trust the
extraction or fall back to the markdown. Stable `source_id`:
`rara:<source_type>:<source_key>:<COALESCE(session_patterns,pattern)>`.

`structured_status` also gives observability of the extraction quality:

```sql
SELECT structured_status, count(*) FROM distillations GROUP BY 1;
```

## Inspecting the generated data

The curated rows live in the `distillations` table on the shared Neon database. To
query them, connect with any Postgres client using the same `DATABASE_URL` the job
uses — the Neon web console SQL editor (zero install) or `psql`
(`brew install libpq`, then `psql "$DATABASE_URL"`).

```sql
-- Overview of what has been distilled
SELECT id, youtube_video_id, title, pattern, engine,
       structured_status, status, created_at
FROM distillations
ORDER BY created_at DESC;

-- Read the latest curated document (human Markdown + situational context)
SELECT title, doc_context, content
FROM distillations
ORDER BY created_at DESC
LIMIT 1;

-- Inspect the queryable extraction (pretty-printed JSON)
SELECT title, jsonb_pretty(structured)
FROM distillations
ORDER BY created_at DESC
LIMIT 1;

-- Extraction-quality breakdown
SELECT structured_status, count(*) FROM distillations GROUP BY 1;

-- Any failures, with the captured error
SELECT source_key, attempt_count, error
FROM distillations
WHERE status = 'failed';
```

### source_key normalization

`source_key` is the stable dedup key, never NULL. For YouTube sources it is the
`youtube_video_id` (v1 is YouTube-only). When url/local sources arrive, normalize the
`source_ref` before using it as `source_key`: lowercase scheme+host, drop the
trailing slash and fragment (`#...`), drop tracking params (`utm_*`, `fbclid`,
`gclid`), and keep the meaningful query.

### Uniqueness (Option A — "current view")

The unique key is `(source_key, COALESCE(session_patterns, pattern))` and does **not**
include the recipe. Two distillations differing only by context/strategy overwrite
each other in place; production runs a single recipe. To compare recipe variants
side by side, run them against a Neon branch / locally, not the production table.

## Local development

```bash
cp .env.example .env          # fill DATABASE_URL + the engine's API key
make test                     # run the TDD suite
make lint                     # go vet + staticcheck
set -a && source .env && set +a
go run .                      # distill DISTILL_BATCH_SIZE transcripts
```

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | — (required) | Neon connection string (shared) |
| `CURATE_ENGINE` | `gemini` | `gemini` \| `claude` \| `groq` \| `litellm` |
| `GEMINI_API_KEY` / `ANTHROPIC_API_KEY` / `GROQ_API_KEY` | — | per engine |
| `GEMINI_MODEL` / `CLAUDE_MODEL` / `GROQ_MODEL` | sane defaults | model override |
| `LITELLM_BASE_URL` / `LITELLM_API_KEY` / `LITELLM_MODEL` | — / — / `claude-sonnet-4-6` | self-hosted gateway (OpenAI-compatible); key optional. See [litellm/](./litellm/) |
| `DISTILL_PATTERNS` | `extract_wisdom` | CSV; many = session chain |
| `DISTILL_CONTEXT` | (none) | context file in `contexts/<name>.md` |
| `DISTILL_STRATEGY` | (none) | strategy file in `strategies/<name>.md` |
| `DISTILL_BATCH_SIZE` | `1` | transcripts per run (code default `1`; the deployed Cloud Run Job uses `25` — balanced for ~13 min runs with Gemini Pro) |

See [DEPLOY.md](./DEPLOY.md) for the Cloud Run Job deployment.
