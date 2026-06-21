# rara-gate — the curation gate

`rara-gate` is the **curation gate** of the rara 2.0 pipeline: a bridge-total **claim-worker** on the
[`rara-addon`](../rara-addon) SDK. 1.0 distilled everything; 2.0 **selects** — gate decides what is
worth the next (expensive) step.

It serves **two workers**, both gates, selected by the `SIFT_GATE` env var:

| Worker | `SIFT_GATE` value | Capability | What it judges |
|---|---|---|---|
| **sift** | `gate_barato` | `gate_barato` | Item **metadata** (title/channel) — cheap, runs before transcription |
| **assay** | `gate_rico` | `gate_rico` | Item **full text** — runs before distillation |

One binary, two workers. The dispatcher sets `SIFT_GATE` + `SIFT_PROVIDER` per execution.

## The cascade (cheap → expensive)

The verdict is reached by a cost-ordered cascade, so the paid layer rarely runs:

```
rules (deterministic allow/deny, ~free)
   │ undecided
   ▼
interest_profile match (cheap, deterministic; keep-or-escalate, never auto-drops)
   │ on the fence
   ▼
LLM-judge (expensive — only the borderline middle, via the LiteLLM gateway)
```

Each layer **decides** (returns a verdict) or **escalates** (falls through). The cascade
(`runCascade` and below in `main.go`) is **pure** — it takes the parsed profile + rules + an
`LLMJudge` seam and returns a verdict with zero I/O — so the whole selection policy is unit-tested
with a fake judge.

## Decide, don't route

The split with `rara-core` is strict:

- **gate DECIDES and RECORDS.** The handler reads the live `interest_profile` + `gate_rules`
  (rara-core's tables, **SELECT only** — the 1.0 cross-agent isolation convention) and the item's
  metadata/text (the collector/scribe domain tables), runs the cascade, and writes a
  **`gate_decisions`** row. The step's `output_ref` is that row's id.
- **rara-core ROUTES.** The reconciler reads the latest decision and routes the item:
  `keep → advance`, `drop → filtered`, `defer → quarantine`. gate never touches item status.

This keeps judgement in the worker and routing in the control plane.

## Failure handling

- **Input not ready** (gate_rico before the to-text artifact landed) → `addon.ErrRetryable`: the SDK
  requeues up to the attempt ceiling rather than failing a good item for good.
- **Transient LLM-judge error** (a gateway blip) → also retryable. A gateway hiccup must not
  permanently drop a good item.
- A **profile/rules read** or **write** error is terminal.

## Run

```bash
cp .env.example .env   # fill DATABASE_URL, SIFT_GATE, SIFT_PROVIDER, LITELLM_BASE_URL
make test              # zero-I/O unit tests (pure cascade + handler with a mock store + fake judge)
make build             # local binary (gate-job)
go run .               # drain the queue once for (SIFT_GATE, SIFT_PROVIDER) and exit (on_demand)
```

Default is **on_demand** (drain once, exit — the woken Cloud Run job). A resident deploy (the
`*-vpc`/`*-mac` providers on the Mac/VPC) opts into the long-running loop + symmetric activation via
`WORK_POLL_INTERVAL` and/or `POKE_ADDR` + `POKE_TOKEN`.

## Schema

gate owns **no** table of its own. It reads rara-core's `interest_profile`/`gate_rules` and the
collector/scribe domain tables, and writes rara-core's append-only `gate_decisions` (which the
reconciler also reads to route, and rara-core also writes on a quarantine rescue). All of these live
in the one shared Neon database — there is **no `migrations/`** here and no `database-gate.yml`.
