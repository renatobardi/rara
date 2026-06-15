# RARA → KURA — Integration Contract

The stable, versioned interface between `rara` (producer) and `KURA` (the second brain,
consumer). KURA is early; this fixes the contract now so integration is mechanical later.

> Principle (from [ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md)): **the contract is the table, never
> a direct call.** `rara` stays KURA-agnostic — it never imports, calls, or knows about KURA.

## 1. rara → KURA: read `distillations`

KURA ingests by reading the `distillations` table (read-only). No new surface, no coupling, no
push. In 2.0 a `distillation` row exists **only for items that passed curation**, so KURA receives
the curated output for free — no filtering of junk on its side.

**Stable columns KURA may rely on** (see [DATABASE_SCHEMA.md](./DATABASE_SCHEMA.md)):

| Column | Use |
|---|---|
| `source_key` | stable identity of the source (dedup/upsert key on KURA's side) |
| `source_type` | `youtube \| podcast \| email \| linkedin \| url` (provenance) |
| `source_ref` | the external id/url |
| `title` | display title |
| `content` | human Markdown (the distilled knowledge doc) |
| `structured` | JSONB: concepts / insights / entities / claims (RAG-ready) |
| `doc_context` | the Contextual-Retrieval preface |
| `source_sha256`, `recipe_sha256` | **staleness hashes** — re-ingest when either changes |
| `status` | KURA reads only `status = 'done'` |
| `created_at`, `updated_at` | KURA's ingest watermark |

**Watermarking (KURA-side, rara stays passive):** KURA tracks the max `updated_at` it has ingested
and pulls rows newer than that; for already-seen `source_key`s it re-ingests when
`source_sha256`/`recipe_sha256` differ from its stored copy. rara never tracks KURA's progress.

**Access:** a read-only Neon role scoped to `SELECT` on `distillations` (and the collector title
tables if KURA wants richer provenance). KURA holds its own DB credential; rara is unaware.

## 2. KURA → rara: implicit feedback via the existing surface

The "implicit KURA usage" learning signal (what you actually read/keep in the second brain) flows
back through the **already-built control surface** — no new rara table or endpoint:

- KURA calls the MCP tool `rara_feedback_distillation` (or `POST /v1/feedback/distillation`) with
  `{ distillation_id, signal: up|down }` when you engage with (or dismiss) a distilled doc.
- These land in the existing `feedback` table with a `source` value `kura_implicit` (distinct from
  `user_explicit` thumbs and `quarantine_review`), and feed the interest_profile revision loop
  (Phase 6) like any other signal.

This keeps the coupling one-directional in *code* (KURA depends on rara's stable surface; rara
depends on nothing) while closing the learning loop.

## 3. What is deliberately NOT in the contract

- No rara→KURA push, webhook, or MCP feed. KURA pulls.
- No shared deployment, no shared process, no cross-service FK.
- rara does not model KURA's spaces/graph/RAG — that is entirely KURA's concern downstream.

## Status

Contract **defined**; integration **deferred** until KURA matures. When KURA is ready: (1) create
the read-only Neon role, (2) implement KURA-side ingest + watermark, (3) wire KURA's engagement to
`rara_feedback_distillation`. No rara code change is required for (1)–(2); (3) reuses the Phase 5
surface (only a new `source` enum value `kura_implicit` may be added).
