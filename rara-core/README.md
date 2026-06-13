# rara-core

The 2.0 **orchestrated control plane** — a new, isolated agent that decides *what runs
next*, *where it runs*, and *whether it's worth doing*, while the existing workers
(harvest, shelf, scribe, distill, feed) stay exactly as decoupled as they are in 1.0.
The contract between the control plane and the workers remains the **database table**,
never a direct call.

See [ARCHITECTURE-2.0.md](../ARCHITECTURE-2.0.md) for the full design and build order.

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
- **Pass-through gates**: `gate_barato`/`gate_rico` always `keep` (recorded in
  `gate_decisions`, `decided_by=passthrough`). Real curation is Phase 3.

Routing (cost/quality/constraints + fallback + heartbeat) is **Phase 2**; until then the
reconciler selects the single enabled provider per capability and the residential
constraint is recorded but not yet enforced.

### Commands

```bash
core-job seed                      # seed the YouTube lane config (idempotent)
core-job ingest                    # items spine <- channel_videos ∪ playlist_videos
core-job reconcile [--loop]        # one pass, or always-on (VPC) on RECONCILE_INTERVAL_SECONDS
core-job work --capability transcrever   # scribe shim (resident, on the Mac)
core-job work --capability destilar      # distill shim (on_demand, Cloud Run)
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
| `gate_decisions` | append-only curation audit + training substrate |
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
alongside scribe on the Mac, `work --capability destilar` as the woken Cloud Run job.
The policy-driven router lands in Phase 2 and the curation gates in Phase 3.
