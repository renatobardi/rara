# Plano E5b+ — Rename 1:1 dos workers + multi-runtime, com cutover LIMPO (zero legado)

> Plano vivo da virada de taxonomia + multi-runtime da tela Workers. **Regenerado** após perda dos
> untracked (commitar este doc no repo para persistir). Companheiro de `CONSOLE-WORKERS.pt-BR.md`,
> `DEPLOY-MATRIX`, `ATIVACAO-UNIFICADA`, `DOCKER-MULTIMODULE`.

## 0. Princípios (inegociáveis)

1. **Cutover limpo, zero legado** — sem alias/compat/nome "_old". No fim, grep por nome antigo no
   repo inteiro = 0.
2. **1:1** — cada worker tem codinome próprio; nada de colapsar por app.
3. **Modelo:** `worker = (capability × fonte)` (identidade); `placement = host/runtime`. Fallback é
   só de placement.
4. **App/binário ≠ worker** — um app serve N workers por env (intencional, documentado).

## 1. Modelo-alvo de nomes (de → para)

Placement name = `<worker>-<runtime>` (sufixo `mac|vpc|cloud`; coluna `runtime` = enum
`local|vpc|cloudrun`). Cada worker tem **nome claro** (`providers.description`) exibido na UI.

| Antigo (provider) | Worker | Nome claro (UI) | Placements-alvo | App/Job |
|---|---|---|---|---|
| harvest | harvest | Coletor de canais (YouTube) | harvest-cloud (+vpc,+mac) | rara-harvest |
| shelf | shelf | Coletor de playlists (YouTube) | shelf-cloud (+vpc,+mac) | rara-shelf |
| dial | dial | Coletor de podcasts (RSS) | dial-cloud (+vpc,+mac) | rara-dial |
| feed | feed | Coletor de notícias (RSS/HN) | feed-cloud (+vpc,+mac) | rara-feed |
| clip | clip | Coletor de posts (LinkedIn) | clip-cloud (+vpc,+mac) | rara-clip |
| courier | courier | Coletor de e-mail (Gmail) | courier-cloud (+vpc,+mac) | rara-courier |
| manual-inbox | stash | Submissão manual (LinkedIn) | stash (surface) | rara-core surface |
| asr-youtube | caption | Transcritor — vídeo YouTube (Mac) | caption-mac (só Mac) | rara-transcribe |
| asr-direct-audio | echo | Transcritor — áudio/podcast | echo-cloud (+vpc,+mac) | rara-transcribe |
| extrair-news | glean | Normalizador — notícia | glean-cloud (+vpc,+mac) | rara-extract |
| extrair-email | winnow | Normalizador — e-mail | winnow-cloud (+vpc,+mac) | rara-extract |
| extrair-linkedin | scrub | Normalizador — post LinkedIn | scrub-cloud (+vpc,+mac) | rara-extract |
| gate-barato | sift | Filtro — metadados (barato) | sift-cloud + sift-vpc (+mac) | rara-gate |
| gate-rico | assay | Filtro — texto completo (rico) | assay-cloud + assay-vpc (+mac) | rara-gate |
| distill | distill | Destilador (LLM) | distill-cloud + distill-vpc (+mac) | rara-distill |

`*-local` → `*-vpc` (distill-local→distill-vpc; gate-barato-local→sift-vpc; gate-rico-local→assay-vpc).

### 1.1 UX → roteamento
Abrir um worker mostra seus placements; por placement: **enable/disable** (`providers.enabled`) +
**ordem** (`routing_policies.fallback`). "Só vpc enabled" → roda só na VPC; se cair e não houver outro
enabled, o item espera. Constraint dura (caption=Mac) → placements impossíveis travados na UI.

### 1.2 Apps renomeados (sem colisão worker↔app)
rara-glean→**rara-extract** (glean/winnow/scrub) · rara-sift→**rara-gate** (sift/assay) ·
rara-scribe→**rara-transcribe** (caption/echo). Single-worker (rara-distill, rara-harvest, …) ficam.

### 1.3 Provisionamento (pipeline) vs controle (console)
Artefato (imagem multi-arch, job, allowlist+agent) = **pipeline**. Config (provider row) = **console**.
A console **não dispara deploy**. "Adicionar placement" é config real; se faltar artefato, o dispatch
**falha visível** (`last_error` + log) — aceitável. Sem pré-semear tudo.

### 1.4 Campos do form (todos reais)
Nenhum campo é inócuo. Ajustes: `capability`+`runtime` read-only no edit (identidade); `runner_url`
só vpc/mac; chaves de identidade no `env` geridas pelo sistema.

### 1.5 Roteamento simplificado (DECIDIDO, ✅ feito na P0)
Decisão do router = `enabled` + constraint + saúde + ordem (`fallback`); **sem score**. Removidos
`cost`/`quality`/`latency_ms`, `cost_weight`/`quality_weight`, o slider, e o **Simular rota** inteiro.
Observabilidade: `providers.last_error` (runner grava na falha; console mostra).

### 1.6 Targeting por `app` (DECIDIDO, ✅ feito na P1a)
`providers.app` (binário/imagem). Dispatcher mira `job = jobPrefix + app` e imagem por `app` (não pelo
nome). Identidade do worker vem do `env` (injetado no wake). Decoupla nome ↔ targeting.

## 2. Inventário de varredura (grep dos nomes antigos)

Termos: `gate-barato(-local)`, `gate-rico(-local)`, `asr-youtube`, `asr-direct-audio`,
`extrair-email/news/linkedin`, `distill-local`, `manual-inbox`, jobs `rara-gate-barato/rico`,
`rara-asr-direct-audio`, `rara-extrair-*`, módulos `rara-sift/glean/scribe`. Superfícies: DB, Go core,
**workers que ramificam pelo nome** (só `rara-scribe` tinha — corrigido na P1c; glean=lane, sift=SIFT_GATE,
distill=identidade ✓), workers/deploy, jobs/imagens, allowlist, docs, mermaid, READMEs.

## 3. Cutover (sem alias → janela coordenada)
Validar na branch Neon do PR; aplicar em baixa atividade; migration atômica; redeploy coordenado;
**residents precisam restart** (env no startup); rollback via revert + restore Neon.

## 4. Fases

- **P0 ✅** — roteamento simplificado + observabilidade. P0a (UI cut), P0b (core: drop score/colunas),
  P0c (last_error col), P0d (runner grava last_error), P0e (console mostra + limpa pass-through).
- **P1 ✅** — rename + targeting. P1a-1 (col `app`), P1a-2 (runner usa app), P1b (rename
  name/worker/description/env/fallback + cascade), P1c (fix scribe: ramifica por lane).
  Pós-cutover: residents (caption-mac) restart feito.
- **P2a-1 ✅** — multi-arch (harvest/shelf/dial/courier/feed/clip/hone).
- **P2b — cutover acoplado, POR APP em 2 fases** (gate, extract, transcribe):
  - **Fase A (aditiva):** rename do módulo (rara-sift→rara-gate…) + dir/`go.mod`/Dockerfile +
    workflows → imagem nova multi-arch + **job consolidado novo**; **NÃO flipa `app`** (prod nos jobs
    antigos). Zero gap.
  - **Fase B (cutover+limpeza):** migration/seed flipa `providers.app` → gate/extract/transcribe +
    allowlist (ops) → próximo dispatch vai pro job novo; remove jobs/imagens órfãos.
  - ~6 fatias (3 apps × A/B), ordem aditiva.
- **P3** — ops/runners: subir agent no Mac; operador adiciona placements vpc/mac via console.
- **P4** — console: mostra `description`; enable/ordem por placement; `capability`/`runtime` RO no
  edit; `runner_url` só vpc/mac; mostra `last_error`; constraints travadas; sem deploy pela UI.
- **P5** — sweep de docs/mermaid/READMEs (taxonomia nova; remover obsoletos).
- **P6** — gate de zero-legado: grep dos nomes antigos no repo = 0; jobs/imagens órfãos removidos no
  GCP; `assigned_provider`/`fallback` sem nome velho.

## 5. Riscos & decisões
- Cutover por app limita blast radius. Fase A aditiva (cria antes de remover) evita gap.
- Workers que ramificam por nome quebram no rename → auditar (só scribe tinha, ✅ P1c).
- Placements Mac exigem agent no Mac (P3); até lá `*-mac` `enabled=false`.
- **Persistência:** commitar os docs de planejamento no repo (este, study, log) — foram perdidos uma
  vez por estarem untracked.
