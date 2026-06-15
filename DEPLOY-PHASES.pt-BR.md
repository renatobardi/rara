# rara-core 2.0 — Fases de Deploy (do "pronto em código" ao "usando")

Roteiro faseado pra ajustar o que falta e subir tudo. Visão geral em
[ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md); contrato/apps em
[ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md); inferência em
[INFERENCE-ROUTING.pt-BR.md](./INFERENCE-ROUTING.pt-BR.md).

**Ponto de partida real:** as 7 fases de build estão prontas e o schema está no Neon. O
`rara-core` atual já tem o reconciler, a superfície e os runners (hoje nativos, via
`core-job <papel>`). A reestruturação **bridge-total** (P1) extrai o miolo num SDK e separa as
capacidades em apps independentes; o resto é **deploy/provisionamento**.

Legenda: 🟦 código (Claude Code) · 🟧 ops/provisionamento · ✅ critério de "feito".

---

## P1 — A reestruturação (bridge total) 🟦

> **Decisão bridge-total** (ver [ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md)): cada
> capability vira um **app independente** anexado só pelo contrato. O `rara-core` encolhe pro
> orquestrador (`reconcile` + `surface`) + o contrato/SDK. A lógica de domínio é **relocada**, não
> reescrita. Esta é a maior fatia de código — dá pra sub-fasear.

- **P1a — Extrair o SDK `rara-addon`** do `worker.go` + `ClaimPendingStep` + heartbeat + listener de
  poke. **Corrigir o claim** pra `(capability, assigned_provider)` — hoje filtra só por capability,
  o que deixaria um item privado atribuído ao `distill-local` ser pego pelo worker de terceiro.
- **P1b — Activators reais**: Cloud Run Jobs `run` (hoje é `logActivator` noop) + o **poke simétrico**
  no tailnet; `pull` + polling de fallback continuam.
- **P1c — Separar as capacidades em apps** que usam o SDK: mover os runners nativos (gate, revise,
  asr-direct, extract, podcast/email/linkedin) pra apps próprios; adaptar os 1.0
  (harvest/shelf/feed/scribe/distill) pro contrato em vez do cron+fila deles.
- **⚠️ Gate do deploy (OBRIGATÓRIO na adaptação de um agente 1.0):** ao importar `rara-addon`, o
  `deploy-X.yml` do agente quebra no `docker build` (ele empacota só o módulo, e o `replace
  ../rara-addon` não está no contexto). No MESMO PR da adaptação, gate o deploy pra
  `workflow_dispatch` (remove o `push: main`); o binário 1.0 segue rodando onde já está. (distill
  já gated — PR #47.) Coletores (producers) que NÃO importam `rara-addon` não quebram. O rework
  multi-módulo + reativação fica pro P2.
- **Config**: `gate-*-local`/`distill-local` → `runtime=local` (Mac); conferir
  `activation`/`accepts`/`sensitivity` no seed.
- TDD por app; PRs; CI verde.

✅ `go test` verde em cada app; cada worker claima/processa um item de teste localmente; o
orquestrador atribui e (P1b) acorda.

## P2 — Imagem + CI/CD de deploy 🟦🟧

- **Imagem por app** (bridge-total — cada capability é um app que usa o SDK `rara-addon`):
  **amd64** p/ Cloud Run; **binário nativo arm64** p/ VPC (`core`) e Mac (`scribe`, `*-local`).
- **Docker multi-módulo + reativar deploys gated:** cada Dockerfile de app que importa `rara-addon`
  copia `rara-addon` + o app no contexto do build (hoje empacota só o módulo). Fazer o padrão UMA
  vez, reusar, e **reativar os triggers** dos deploys que foram gated no P1c (distill, scribe, …),
  agora já no formato claim-worker (env `*_PROVIDER`, sem `*_SOURCE`/news-lane/`*_BATCH_SIZE`).
- **`deploy-*.yml` por app**: build+push (Cloud Build → Artifact Registry); `gcloud run jobs
  replace` pros apps de Cloud Run; update da VPC via **Tailscale SSH** (`core`); Mac via
  `make build && launchctl kickstart`.
- **Secret Manager**: criar os segredos novos — Gmail OAuth, Bright Data, chaves de modelo
  (gemini/anthropic/groq p/ o LiteLLM), `surface-token`.

✅ um push publica a imagem; os segredos existem no Secret Manager.

## P3 — Cérebro na VPC + Tailscale 🟧

- Provisionar a **VM Ampere** (SO, runtime, firewall **só-tailnet**); instalar e autenticar o
  **Tailscale** na VM e nos seus dispositivos.
- **SA GCP escopada** a `run.jobs.run` (só isso); credencial no env file da VM (gitignored).
- **systemd `rara-core-reconcile`** (papel `reconcile` + superfície), escutando **só no tailnet**,
  com `SURFACE_TOKEN`.
- Rodar o **seed uma vez** (popula capabilities/providers/flows).

✅ `/healthz` responde pelo tailnet; a superfície lista a config; o reconciler está vivo (ainda
sem workers → nada processa, mas o cérebro está observável).

## P4 — Inferência (router=host × LiteLLM=modelo) 🟧

Ver [INFERENCE-ROUTING.pt-BR.md](./INFERENCE-ROUTING.pt-BR.md).

- **LiteLLM por host** (Mac, Cloud Run; VPC se usar Ollama-VPC) — cada worker aponta pro LiteLLM
  do seu host e escolhe o modelo.
- **Mac**: instalar **Ollama** (`ollama pull <modelo>`) + o **shim CLI→endpoint local**
  (`claude-cli`/`gemini-cli` como modelo do LiteLLM, best-effort).
- **Cloud Run**: LiteLLM com as chaves de terceiro (gemini/anthropic/groq) como **fallback pago**.
- Semear os provider rows `*-mac`/`*-vpc`/`*-cloud` com **custo/qualidade/fallback** (Mac-first).

✅ chamada de teste responde no LiteLLM do Mac (via CLI) e no de Cloud Run (via API); Ollama
responde local.

## P5 — Runners no Cloud Run (on_demand) 🟧

- Criar os **Cloud Run Jobs** (um por app, cada um sua imagem usando o SDK): coletores
  (`harvest`, `shelf`, `feed`, `dial`, `courier`, `clip`), `scribe`(asr-direct), `glean`,
  `sift`(3rd), `distill`(3rd).
- Conceder `run.jobs.run` à SA do reconciler; wire dos OAuth/keys (Gmail, Bright Data).

✅ o orquestrador **acorda** um job (Activator real) e ele claima/processa um item de teste.

## P6 — Mac (residencial/privado) 🟧

- **launchd**: `scribe`/asr-youtube (re-apontado pra claimar de `item_steps`), `sift`-local,
  `distill`-local + o **listener de poke**; Ollama + shim CLI já no ar (P4).
- Providers `*-local`/`*-mac` em `runtime=local`.

✅ o Mac dá **heartbeat**; o poke do orquestrador chega quando ele está acordado; o LLM roteia
**Mac-first** (assinatura barata), com fallback pra Cloud Run/API quando o Mac está fora.

## P7 — Acender as raias e usar 🟧✅

- Habilitar os **flows um a um**: YouTube ponta a ponta → podcast → email → linkedin.
- Subir o **`revise` (cron)** na VPC.
- Verificar um item do `discovered` ao `done`; dar **thumbs** numa distillation; **revisar a
  quarentena** (por MCP/HTTP, até o console existir).
- Atualizar o **`INFRASTRUCTURE.md`** + um runbook curto.

✅ **está usando**: conteúdo entrando, sendo curado e destilado; o loop de feedback fechando.

---

## Resumo de esforço

| Fase | Tipo | Tamanho |
|---|---|---|
| P1 código (Activators + flip) | 🟦 | pequeno — o grosso do core já existe |
| P2 imagem + deploy-core.yml | 🟦🟧 | médio |
| P3 VPC + Tailscale + seed | 🟧 | médio (provisionamento one-off) |
| P4 LiteLLM + Ollama | 🟧 | pequeno |
| P5 Cloud Run jobs | 🟧 | médio (vários jobs, mesma imagem) |
| P6 Mac | 🟧 | pequeno |
| P7 acender + usar | 🟧 | pequeno por raia |

**Depois do P7 → `rara-console`** (C0…), que entra no mesmo tailnet e fala com o core já rodando.

## Ordem & dependências

P1 → P2 são pré-requisito de tudo. P3 (cérebro) destrava P5/P6 (os workers precisam de um
orquestrador pra atribuir/acordar). P4 (inferência) é pré-requisito dos gates/distill em P5/P6.
P7 só depois de P3+P5+P6. Dá pra **começar a usar parcialmente** já no fim do P5 (só as raias cujo
caminho é todo Cloud Run, ex.: podcast público) e completar com o Mac no P6.
