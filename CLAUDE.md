# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

`rara` is a monorepo of **independent Go agents** that share one Neon PostgreSQL database but own
isolated tables. Each agent is a separate Go module (its own `go.mod`) with a flat `package main`
layout (`main.go` + `main_test.go` + `migrations/`). There is intentionally **no `go.work`** and no
top-level Makefile — agents are isolated by design and build/test independently. Go 1.26.

The agents fall into two generations:

- **1.0 workers** — `rara-harvest`, `rara-shelf`, `rara-transcribe`, `rara-distill`, `rara-feed`,
  `rara-dial`, `rara-courier`. Each collects/transcribes/curates and is decoupled from the others
  **only through the database** — a worker reads another's table, never calls it.
- **2.0 control plane** — `rara-core` (binary `core-job`) decides *what runs next, where, and
  whether it's worth doing*, orchestrating the 1.0 workers through control tables without touching
  their domain logic. The contract stays the database table. `rara-addon` is a shared SDK module
  (claim/heartbeat/result/poke), not an agent.

The two generations are converging via the **bridge-total** split: workers are being rewritten from
batch ETL into per-item claim-workers on the `rara-addon` SDK (`addon.Run(Config{Capability,
Provider}, handler)` over a pgxpool), while `rara-core` sheds the corresponding domain logic and
stays a pure orchestrator. `rara-distill` is the first (P1c, commit 485ee9d): its handler does only
the domain (resolve recipe → load artifact → distill → save), the SDK owns the contract tables
(`item_steps`/`providers`/`items`), and the recipe is per-item config from `flow_steps.options.recipe`
(env `DISTILL_*` is only the fallback default). When touching a worker, check whether it's already on
the SDK before assuming a batch driver.

Read `ARCHITECTURE.md` (1.0) and `ARCHITECTURE-2.0.pt-BR.md` (2.0 control plane) for the full
design. Per-agent detail lives in each `rara-*/README.md`. Many planning docs are in pt-BR
(`*.pt-BR.md`); code, code comments, and committed docs are in English.

## Build / test / lint

Everything is **per-agent** — `cd` into the agent dir first. Each has an identical Makefile:

```bash
cd rara-distill          # (or any rara-* dir)
make test                # go test -v   (zero I/O: MockDatabase mirrors the SQL contract)
make test-race           # go test -race
make test-coverage       # coverage.out + func summary
make lint                # go vet + staticcheck (auto-installs staticcheck if missing)
make build               # local binary
make build-arm64         # CGO_ENABLED=0 GOOS=linux GOARCH=arm64 — the VPC/Mac arch (Ampere/Apple Silicon); Cloud Run runs amd64
make all                 # clean + lint + test + build
```

Run a single test directly: `cd rara-core && go test -run TestReconcileOnce -v`.

Modules on the `rara-addon` SDK (`rara-core`, `rara-distill`, …) couple to it via a
**`replace rara-addon => ../rara-addon`** directive (deliberately *not* a committed `go.work` —
that's gitignored — which would pull the sibling modules into one build and break their isolated CI).
Each still builds standalone: `cd rara-core && go test ./...`. (Multi-module Docker/deploy for the
SDK-coupled workers is P2 — not done yet.)

## Conventions that aren't obvious from a single file

- **TDD is mandatory.** Every agent is Red→Green→Refactor with a fluent harness
  (`NewShelfHarness(t).With…().Execute(ctx)` + asserts) over an in-memory `MockDatabase` that
  mirrors real SQL constraints. Tests do zero real I/O — add a fake/mock at the seam, never hit
  Neon or an external API in a unit test. Write the failing test before the implementation.
- **Idempotency everywhere.** All writes are upserts (`ON CONFLICT`); every agent and every
  migration is safe to re-run. The control plane reconciler is level-triggered for the same reason.
- **Shared-DB isolation.** No foreign keys cross an agent boundary (e.g. `item_steps.output_ref`
  links to `transcripts.id` *logically only*). Even `updated_at` trigger functions are namespaced
  per agent (`set_updated_at` / `shelf_set_updated_at` / `core_set_updated_at` / …) to avoid
  colliding in the one Neon database.
- **Config is environment-only** — required vars fail fast with `log.Fatalf`. See each agent's
  `.env.example`. Pluggable engines are chosen by env (`TRANSCRIBE_ENGINE`, `CURATE_ENGINE`,
  `DISTILL_SOURCE`, `SCRAPE_PROVIDER`, …) behind an interface seam, never an `if` on a provider name.
- **One deliberate runtime split:** every agent is a Cloud Run Job *except* `rara-transcribe`, which
  runs on a local Mac via `launchd` — YouTube blocks audio downloads from datacenter IPs, so it
  needs a residential IP. `core-job reconcile --loop` is meant to run always-on in the VPC.

## CI / migrations / deploy (GitHub Actions, path-filtered per agent)

In `.github/workflows/`, each agent gets its own trio, triggered only when its path changes:

- `ci-<agent>.yml` — vet + staticcheck + tests.
- `database-<agent>.yml` — applies that agent's `migrations/` to Neon on merge to `main`; on a PR
  it validates inside `BEGIN; … ROLLBACK;` (never commits). Migrations apply regardless of where the
  agent's binary runs (even local scribe).
- `deploy-<agent>.yml` — builds the image and deploys the Cloud Run Job (none for transcribe). Cloud Run
  runs **amd64**; the SDK workers are migrating to a single multi-arch image (amd64 for Cloud Run +
  arm64 for the VPC/Mac runner hosts) — `rara-gate` is the first (see `DOCKER-MULTIMODULE.md`).

Auth to GCP is Workload Identity Federation (no SA key files); secrets live in GCP Secret Manager,
except transcribe which reads `~/.rara-transcribe/.env`. When adding an agent, copy an existing agent's
Makefile + the three workflow files and adapt the path filters.

## Code review loop (CodeRabbit reports → Claude Code fixes)

**Standard workflow before opening any PR.** Run a CodeRabbit review locally and fix every finding
*through Claude Code* (not via CodeRabbit's one-click suggestion), so each fix goes through the
mandatory TDD cycle and respects the conventions above.

The loop, inside a Claude Code session in the agent dir:

1. Implement the change (Red→Green→Refactor with the fluent harness over `MockDatabase`).
2. Trigger the review: `/coderabbit:review uncommitted` (or just ask "review my changes with
   CodeRabbit"). The CLI reads this `CLAUDE.md`, so findings are scoped to our conventions.
3. Claude Code turns the findings into a task list and fixes each one — **security findings first**
   (the `.coderabbit.yaml` makes security priority #1), then quality. Every fix that touches logic
   gets a failing test written first, then `make test` must pass.
4. Re-review until clean, then commit and push.

**Severity policy:** fix *all* findings, including low/medium and nitpicks (profile is `assertive`).
Only apply CodeRabbit's one-click committable suggestion for truly trivial, logic-free diffs
(typos, import order); everything else goes through Claude Code so it lands with a test.

The GitHub PR review (CodeRabbit app + `.coderabbit.yaml`) stays on as an independent safety net;
`request_changes_workflow: true` blocks approval until every finding is resolved. Config lives in
`.coderabbit.yaml` at the repo root and overrides any dashboard UI setting.
