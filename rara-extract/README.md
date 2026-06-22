# rara-extract — the already-text extractor

`rara-extract` is the **to-text worker for the text lanes** of the rara 2.0 pipeline: a bridge-total
**claim-worker** on the [`rara-addon`](../rara-addon) SDK. Some sources arrive as text, not audio —
an **email body**, a pasted **LinkedIn post**, a **news article** — so they need no ASR. They need
**normalization**: strip the noise the source carries and leave the human-written message the gates
and distill judge.

`extrair` is that capability — a **peer of `transcrever`**, not a special case. Its output lands in
the **same `transcripts` store** rara-transcribe writes, keyed on `(source_ref, source_type)`, so
`gate_rico` and distill consume an extracted email exactly like a transcript.

One app, **three workers** by lane (`GLEAN_PROVIDER`):

| Worker | Provider (`GLEAN_PROVIDER`) | Lane | Description |
|---|---|---|---|
| **glean** | `glean` | news | Normalizador — notícia |
| **winnow** | `winnow` | email | Normalizador — e-mail |
| **scrub** | `scrub` | linkedin | Normalizador — post LinkedIn |

The handler dispatches by `item.Lane` — one codebase, three workers. Codebases ≪ providers.

## What it does

Per claimed item:

1. **Read** the raw body from the collector's domain table (a cross-app **SELECT**, the 1.0 isolation
   convention — read a sibling's table, never call it), dispatching by lane.
2. **Clean** it with the lane's **pure, deterministic** cleaner (`cleanEmailText` / `cleanPostText`).
3. **Upsert** the cleaned text into `transcripts` (`UPDATE` else `INSERT`, idempotent on
   `(source_ref, source_type)`) and return its id as the step's `output_ref`.

The cleaning is **pure** (zero I/O), so the whole normalization policy is unit-tested. The SDK
(`addon.Run`) owns claim/heartbeat/result/requeue/poke around it; this app supplies only the
`extrair` domain (`gleanHandler`). The reconciler **routes** and **activates** it; it never decides
*what* to extract.

## Failure handling

- **Source not ready** — the body row has not landed yet (a collector race against ingest) →
  `addon.ErrRetryable`: the SDK requeues up to the attempt ceiling rather than failing a good item.
- **Empty extraction** — a body that cleans to nothing (pure signature/quote) is benign no-content:
  the to-text row is written `status='empty'`, the step is **done**, and the item is **curated out**
  (`Filtered`) rather than marched into a distill that must fail.
- A **read** or **write** error is terminal (surfaced as-is).

## Run

```bash
export GLEAN_PROVIDER=winnow    # or scrub, glean
make test              # zero-I/O unit tests (pure cleaners + handler with a mock store)
make build             # local binary (extract-job)
export DATABASE_URL=...                # the shared Neon database (not needed for make test)
go run .               # drain the queue once for (extrair, GLEAN_PROVIDER) and exit (on_demand)
```

Default is **on_demand** (drain once, exit — the woken Cloud Run job). A resident deploy opts into
the long-running loop + symmetric activation via `WORK_POLL_INTERVAL` and/or `POKE_ADDR` +
`POKE_TOKEN`.

## Schema

`rara-extract` owns **no** table of its own. It **reads** the collectors' `emails` / `linkedin_posts`
/ `news_items` and **writes** the to-text owner's (rara-transcribe's) shared `transcripts` table — the
universal to-text store, shared by design. All live in the one shared Neon database, so there is
**no `migrations/`** here and no `database-extract.yml`.
