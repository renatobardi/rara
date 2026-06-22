# rara-core

The 2.0 **orchestrated control plane** — a new, isolated agent that decides *what runs
next*, *where it runs*, and *whether it's worth doing*, while the existing workers
(harvest, shelf, scribe, distill, feed) stay exactly as decoupled as they are in 1.0.
The contract between the control plane and the workers remains the **database table**,
never a direct call.

See [ARCHITECTURE-2.0.md](../ARCHITECTURE-2.0.md) for the full design and build order.

## Status — Phase 5 (Surface & LinkedIn)

rara-core now exposes a **control surface** — MCP over HTTP — so a person or an agent
(Cowork) can drive the running system, plus a manual **LinkedIn** lane. The surface is mounted
**in the same always-on VPC process** as the reconciler (alongside its ticker).

- **HTTP núcleo** ([surface.go](surface.go)): one operations layer (`Core`) behind a REST
  adapter. Reads STATE (`GET /v1/items?status=`, `…/items/{id}/steps`, `…/items/{id}/decisions`,
  `/v1/quarantine`) and config as DATA (`/v1/flows`, `…/flows/{id}/steps`, `/v1/providers`,
  `/v1/routing-policies`, `/v1/gate-rules`, `/v1/interest-profile`), edits config through the
  existing idempotent upserts (`PUT`), and drives the two human-in-the-loop signals
  (`POST /v1/feedback/distillation`, `POST /v1/quarantine/review`) by **reusing the Phase 3
  functions verbatim**.
- **MCP adapter** ([mcp.go](mcp.go)): a thin JSON-RPC front-end at `POST /mcp`
  (`initialize` / `tools/list` / `tools/call`) over the **same** `Core` — 19 tools
  (`rara_list_items`, `rara_upsert_provider`, `rara_submit_linkedin_post`, …). MCP is the open,
  vendor-neutral standard (anti-lock-in); the mapping tool→operation is the whole adapter.
- **Auth** ([surface.go](surface.go)): a single service token (`Authorization: Bearer
  $SURFACE_TOKEN`), constant-time, **fail-closed** — an unset token refuses to serve. `/healthz`
  is the only open route.
- **LinkedIn lane** ([linkedin.go](linkedin.go)): a `stash` collector — `POST
  /v1/linkedin/inbox` (or the `rara_submit_linkedin_post` tool) takes a post's URL + text, upserts
  it into `linkedin_posts` and discovers the spine item (lane=linkedin, sensitivity=public). The
  flow uses `extrair` (the post is already text), pinned with `accepts:["linkedin"]`. The
  collector is **swappable for Bright Data** (Phase 6) behind the same `linkedin_posts` contract —
  the flow, extractor and gates never change.

The surface is unit-tested end to end against the `MockDatabase` + `httptest` (zero real I/O):
handlers, the auth gate, and the MCP tool→operation mapping.

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
  feedback is the learning loop, which lives in its own periodic job — **[rara-hone](../rara-hone)**
  — that PROPOSES a new `interest_profile` version. The core keeps only the human **approval**
  of a proposal (the surface's `approve` action / `rara_approve_profile`).

The LLM-judge is the only paid layer and runs only on what rules + the profile could not
decide. See [.env.example](.env.example) for the `LITELLM_*` gateway config.

## Status — Phase 1 (Reconciler MVP over existing lanes)

The control plane now drives the **YouTube** lane end to end — collect → transcribe →
distill — by orchestrating the existing 1.0 workers through the control tables, without
changing any worker's domain logic. Phase 1 ships:

- **Lane config seed** ([seed.go](seed.go)): the YouTube lane as data — capabilities,
  four providers (`harvest`/`shelf` = collectors, `caption` = transcriber on the Mac
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
  entrypoint (`transcribe --source <url> --limit 1`; an idempotent `distill` batch drain)
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
core-job seed                      # seed the lane config (idempotent)
core-job ingest                    # items spine <- channel_videos ∪ playlist_videos
core-job reconcile [--loop]        # one pass, or always-on (VPC) on RECONCILE_INTERVAL_SECONDS
core-job feedback --distillation <id> --signal up|down   # explicit thumbs
core-job quarantine list                 # the cold-start review sample (deferred items)
core-job quarantine review --item <id> --signal up|down  # up rescues, down confirms drop
core-job surface [--addr :8080]    # serve the control surface (HTTP núcleo + MCP) standalone
core-job status                    # health check: control tables reachable
```

rara-core no longer runs a `work` role: every capability is its own bridge-total claim-worker
app on the [rara-addon](../rara-addon) SDK — `transcrever` ([rara-transcribe](../rara-transcribe)),
`destilar` ([rara-distill](../rara-distill)), the curation gates `gate_barato`/`gate_rico`
([rara-gate](../rara-gate)), and the already-text extractor `extrair`
([rara-extract](../rara-extract)). The core only **routes** and **activates** them through the
contract tables; it never executes a capability.

The reconciler also mounts the surface in-process when run with `--loop` and `SURFACE_ADDR`
set (`SURFACE_TOKEN` required) — the always-on VPC deployment.

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
alongside the transcribe worker on the Mac, `work --capability destilar` and the gate workers
(`gate_barato`/`gate_rico`) as woken Cloud Run jobs. The gate workers reach the VPC LiteLLM
gateway for the LLM-judge.

### Symmetric activation (P1b)

Trabalho = pull always; ativação = symmetric. On every assignment the reconciler persists the
desired state; `rara-runner dispatch` reads it and calls `Runner.Run`, routing by provider shape:

- **runtime=cloudrun** → **Cloud Run Jobs `run`**: an authenticated `POST .../jobs/<job>:run`
  starts a fresh execution. The job is named after the provider (`CLOUD_RUN_JOB_PREFIX` + name);
  `CLOUD_RUN_PROJECT`/`CLOUD_RUN_REGION` and a token (`CLOUD_RUN_OAUTH_TOKEN`, a seam for a
  service-account source with `run.jobs.run`) come from env.
- **runtime=vpc / local with `runner_url`** → **tailnet POST to `rara-runner agent`**: the
  dispatcher (`rara-runner dispatch`) POSTs `<providers.runner_url>/run` (Bearer `RUNNER_TOKEN`),
  and the agent does `docker run` of the worker image.

Both are **best-effort**: a failed activation is logged, never fatal. The worker's own slow poll is
the safety net, so the queue always drains regardless. With no activation env configured the
dispatcher is a no-op (logged), which is the correct posture on a box without activation credentials.

#### Self-healing re-activation (anti-stampede)

A resident has its own poll as a safety net; an **on_demand cloudrun** provider (scale-to-zero)
does **not** — if its assignment-time wake fails or times out on a cold start (the default
`ACTIVATE_TIMEOUT_SECONDS` is **30s** for that reason), the step would sit `pending` forever, with
no `gcloud run jobs execute` short of a human. So each pass the reconciler **re-fires** the wake for
any on_demand cloudrun provider that still holds pending work. The catch is not spawning a **swarm**
of concurrent executions running the wrong model: a single woken worker claims until its queue drains
(one execution pulling via `SKIP LOCKED`). So a **successful** wake anchors a per-provider timestamp
and the reconciler stays quiet for `REACTIVATE_BACKOFF_SECONDS` (default **180s**); a **failed** wake
anchors nothing and is retried next pass (no execution started → nothing to swarm). Residents are
excluded — they already have poll (and optional on-demand wakes via `rara-runner`).
