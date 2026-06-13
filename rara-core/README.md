# rara-core

The 2.0 **orchestrated control plane** — a new, isolated agent that decides *what runs
next*, *where it runs*, and *whether it's worth doing*, while the existing workers
(harvest, shelf, scribe, distill, feed) stay exactly as decoupled as they are in 1.0.
The contract between the control plane and the workers remains the **database table**,
never a direct call.

See [ARCHITECTURE-2.0.md](../ARCHITECTURE-2.0.md) for the full design and build order.

## Status — Phase 3 (Curation gates — where 2.0 stops distilling everything)

The gates are **real workers** now, not pass-through. `gate_barato` (on metadata, before
ASR) and `gate_rico` (on full text, before distillation) are capabilities routed and pulled
exactly like `transcrever`/`destilar`, and they actually **select**:

- **Cheap→expensive cascade** ([gates.go](gates.go)): `rules` (deterministic allow/deny) →
  `interest_profile` match (cheap, deterministic) → **LLM-judge** (via LiteLLM, only the
  borderline middle). Each layer decides or escalates; the cascade is a pure function, unit-
  tested with a fake judge. Profiles never auto-drop (absence ≠ rejection) — only an explicit
  deny rule or the LLM drops.
- **keep / drop / defer** — the gate worker records a `gate_decisions` row (decision, score,
  `decided_by`, reason) and marks the step done; the **reconciler reads it and routes**:
  `keep` advances, `drop` → terminal `filtered`, `defer` → terminal `quarantine`. Judgement
  in the worker, routing in the control plane.
- **`interest_profile` + `gate_rules`**: a living, versioned preferences document (topics /
  authors / anti-topics / weights, seeded at v1) read by the profile layer and injected as
  LLM-judge context; the allow/deny rules live in their own `gate_rules` table.
- **Quarantine** ([feedback.go](feedback.go)): `defer` parks an item in `quarantine` (the
  cold-start review sample). `quarantine list` surfaces it; `quarantine review --signal up`
  rescues it (overrides the gate with a keep, resumes the pipeline), `--signal down` confirms
  the drop. The review is captured as `feedback` (source=`quarantine_review`).
- **Explicit thumbs**: `feedback --distillation <id> --signal up|down` records explicit
  signal (target=`distillation`, source=`user_explicit`). Revising the profile *from* this
  feedback is the Phase 6 learning loop (a deliberate stub here).

The LLM-judge is the only paid layer and runs only on what rules + the profile could not
decide. See [.env.example](.env.example) for the `LITELLM_*` gateway config.

## Status — Phase 1 (Reconciler MVP over existing lanes)

The control plane now drives the **YouTube** lane end to end — collect → transcribe →
distill — by orchestrating the existing 1.0 workers through the control tables, without
changing any worker's domain logic. Phase 1 ships:

- **Lane config seed** ([seed.go](seed.go)): the YouTube lane as data — capabilities,
  four providers (`harvest`/`shelf` = collectors, `asr-youtube` = scribe on the Mac
  with the residential constraint, `distill` = Cloud Run), one `youtube` flow and its
  five ordered steps (`coletar → gate_barato → transcrever → gate_rico → destilar`).
- **Spine ingest** ([ingest.go](ingest.go)): populates `items` from
  `channel_videos ∪ playlist_videos`, deduped globally on `youtube_video_id`. Idempotent
  — re-discovery never regresses an in-flight item's status (`DiscoverItem`).
- **Reconciler** ([reconciler.go](reconciler.go)): a level-triggered loop. It observes
  flow steps vs `item_steps` and takes the single next action — auto-satisfy `coletar`
  (the item exists ⇒ collection happened) and the pass-through gates, assign a provider
  for real work steps, advance the item, terminate. Boring, idempotent, no domain work.
- **Claim contract** ([store_reads.go](store_reads.go)): workers pull their assignment
  with `SELECT … WHERE status='pending' ORDER BY id FOR UPDATE SKIP LOCKED` — no broker,
  no double-claim. The claim moves the step `pending → running`.
- **Worker shims** ([worker.go](worker.go), [runners.go](runners.go)): a thin adapter
  that translates an `item_steps` assignment into the existing binary's current
  entrypoint (`scribe --source <url> --limit 1`; an idempotent `distill` batch drain)
  and writes the produced domain row id back as `output_ref`. Scribe/distill domain
  logic is untouched.
- **Pass-through gates** (superseded in Phase 3): in Phase 1 `gate_barato`/`gate_rico` always
  `keep`. They are real cascade workers now — see the Phase 3 section above.

Routing (cost/quality/constraints + fallback) is **Phase 2**; until then the reconciler
selects the single enabled provider per capability and the residential constraint is
recorded but not yet enforced. A minimal **liveness backstop** is in, though: a claimed
(`running`) step whose heartbeat goes stale past `RECONCILE_STALE_SECONDS` is returned to
the pending frontier for re-claim. Other robustness edges handled this phase: a transient
worker miss (e.g. distill's batch hasn't reached a transcript yet) re-queues the step up to
an attempt ceiling instead of failing the item; an empty (no-speech) transcript curates the
item out (`filtered`) rather than driving it into a distill that must fail; and the loops
honour SIGINT/SIGTERM for graceful shutdown.

### Commands

```bash
core-job seed                      # seed the YouTube lane config (idempotent)
core-job ingest                    # items spine <- channel_videos ∪ playlist_videos
core-job reconcile [--loop]        # one pass, or always-on (VPC) on RECONCILE_INTERVAL_SECONDS
core-job work --capability transcrever   # scribe shim (resident, on the Mac)
core-job work --capability destilar      # distill shim (on_demand, Cloud Run)
core-job work --capability gate_barato   # metadata gate worker (cascade + LiteLLM judge)
core-job work --capability gate_rico     # full-text gate worker
core-job feedback --distillation <id> --signal up|down   # explicit thumbs
core-job quarantine list                 # the cold-start review sample (deferred items)
core-job quarantine review --item <id> --signal up|down  # up rescues, down confirms drop
core-job status                    # health check: control tables reachable
```

## Control tables

| Table | Purpose |
|---|---|
| `capabilities` | logical tasks with a fixed I/O contract (`coletar`, `transcrever`, `extrair`, `gate_barato`, `gate_rico`, `destilar`) |
| `providers` | concrete implementations of a capability (`runtime`, `activation`, `cost`, `quality`, `constraints`, `heartbeat_at`) |
| `flows` | one declarative pipeline per source lane; `version` is stamped onto items |
| `flow_steps` | ordered steps of a flow, with per-step `options` |
| `routing_policies` | cost⇄quality weighting + ordered fallback, global or per-capability |
| `items` | canonical materialized spine; one row per discovered work item (`flow_version`) |
| `item_steps` | mutable runtime state-rows; `output_ref` is a logical link to a worker domain row |
| `gate_decisions` | append-only curation audit + training substrate (decision, score, `decided_by`, reason) |
| `gate_rules` | deterministic allow/deny rules — the cheapest cascade layer |
| `feedback` | append-only learning signal that tunes the gates |
| `interest_profile` | living, versioned preferences document (each revision = a new immutable row) |

`item_steps` is the work queue: workers pull with
`SELECT … FROM item_steps WHERE capability = $1 AND status = 'pending' ORDER BY id FOR UPDATE SKIP LOCKED`,
backed by the partial index `idx_item_steps_claim (capability, id) WHERE status = 'pending'`.

## Isolation

Like every rara agent, rara-core owns its tables, migrations, and CI. There are **no
foreign keys across the agent boundary** — `item_steps.output_ref` links to
`transcripts.id` / `distillations.id` logically only. The `updated_at` trigger is
namespaced (`core_set_updated_at`) to avoid colliding with the other agents' variants
in the shared Neon database.

## Development

```bash
make test          # unit tests (zero I/O: mock mirrors the SQL contract + migration lint)
make lint          # go vet + staticcheck
make build         # build the scaffold binary (core-job)
```

Configuration is environment-only; see [.env.example](.env.example). The migration is
applied to Neon by `database-core.yml` on merge to `main` (and validated inside a
`BEGIN; … ROLLBACK;` on every PR).

## Runtime

rara-core is designed to run **always-on in the VPC** (the reconciler must stay awake
while the Mac sleeps and Cloud Run scales to zero): `core-job reconcile --loop`. The
worker shims run where their domain binary lives — `work --capability transcrever`
alongside scribe on the Mac, `work --capability destilar` and the gate workers
(`gate_barato`/`gate_rico`) as woken Cloud Run jobs. The gate workers reach the VPC LiteLLM
gateway for the LLM-judge.
