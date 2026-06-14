# rara-console

The operator/curator UI for the rara 2.0 control plane. One Go binary that serves a SvelteKit SPA
(embedded via `embed.FS`) and acts as a thin BFF in front of the **rara-core surface** (Phase 5).
It holds the surface bearer token **server-side** — the SPA never sees it — and never touches Neon
(rara-core is the single source of truth). It binds to the **tailnet** only.

Visual: Option B (ChatGPT-style, Clean by default, Dark opt-in) — see
[`CONSOLE-PLAN.pt-BR.md`](../CONSOLE-PLAN.pt-BR.md). Tokens live as a Tailwind preset
(`web/tailwind.config.js` + `web/src/app.css`).

## Status — C0 (Fundação)

Scaffold + shell + the **Visão geral** screen + VPC deploy. The overview calls the BFF's
`/api/overview`, which aggregates the live core (`/v1/flows` + `/v1/providers`), proving the BFF end
to end. The other screens (Pipeline, Quarentena, …) are shell placeholders, filled in C1+.

## Layout

```
main.go              BFF: serve embedded SPA + /api/overview + /healthz; tailnet bind
main_test.go         BFF handler tests (httptest core, zero I/O)
web/                 SvelteKit (adapter-static) — shell + Visão geral
  src/routes/        +layout.svelte (shell), +page.svelte (Visão geral)
  src/lib/strings.ts externalized PT strings (i18n-ready)
  tailwind.config.js Option B tokens as a preset
  build/             SvelteKit output, embedded by Go (.gitkeep keeps it compilable)
deploy/              rara-console.service, env.example, RUNBOOK.md
```

## Build / test

```bash
make test         # Go BFF tests (no Node needed)
make build        # npm ci && npm run build (SPA) -> embed -> local binary
make build-arm64  # the VPC arch
```

## Run locally

```bash
# Terminal 1: the Go BFF (needs a reachable core surface)
CORE_SURFACE_URL=http://100.x.x.x:8080 SURFACE_TOKEN=... CONSOLE_ADDR=127.0.0.1:8081 ./console

# or, for live frontend dev with HMR (proxies /api to :8081):
cd web && npm run dev
```

## Config (env)

| Variable | Meaning |
|----------|---------|
| `CORE_SURFACE_URL` | rara-core surface base URL (tailnet) |
| `SURFACE_TOKEN` | bearer token — must match rara-core's `SURFACE_TOKEN` |
| `CONSOLE_ADDR` | tailnet IP + port to bind (never `0.0.0.0`) |

Deploy: `deploy-console.yml` (`workflow_dispatch`) → ARM64 binary → rsync + systemd. See
[`deploy/RUNBOOK.md`](deploy/RUNBOOK.md).
