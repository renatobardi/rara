# rara-core

The 2.0 **orchestrated control plane** — a new, isolated agent that decides *what runs
next*, *where it runs*, and *whether it's worth doing*, while the existing workers
(harvest, shelf, scribe, distill, feed) stay exactly as decoupled as they are in 1.0.
The contract between the control plane and the workers remains the **database table**,
never a direct call.

See [ARCHITECTURE-2.0.md](../ARCHITECTURE-2.0.md) for the full design and build order.

## Status — Phase 0 (Foundations)

Scaffold only. This phase ships:

- The agent skeleton (Go, `pgx/v5`, single `main.go`, Makefile, migrations + CI),
  following the 1.0 per-agent conventions.
- The **control tables** (`migrations/001_initial_schema.sql`): `capabilities`,
  `providers`, `flows`, `flow_steps`, `routing_policies`, `items`, `item_steps`,
  `gate_decisions`, `feedback`, `interest_profile` — isolated in the shared Neon DB
  with a namespaced `core_set_updated_at` trigger and the claim indexes that make the
  `SELECT … FOR UPDATE SKIP LOCKED` work-queue efficient.
- The persistence seam: idempotent upserts (`ON CONFLICT`) for the config + spine
  tables, append-only inserts for the audit tables, mirrored by an in-memory
  `MockDatabase` in the tests.

**There is no behavior yet** — no reconciler loop, no router, no curation gates.
`main()` connects, confirms the control tables are reachable, and exits.

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
while the Mac sleeps and Cloud Run scales to zero). Phase 0 ships only the schema and
scaffold; the reconciler, router, and gates land in Phases 1–3.
