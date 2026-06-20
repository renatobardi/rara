# Console — Tela "Workers" (redesign) + menu "Agents"

> Design doc / ADR da rodada de redesign do item de menu antigo **"Providers & Roteamento"**.
> Companheiro de [CONSOLE-PLAN.pt-BR.md](./CONSOLE-PLAN.pt-BR.md), [INFERENCE-ROUTING.pt-BR.md](./INFERENCE-ROUTING.pt-BR.md)
> e [ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md). Código e comentários em inglês; este doc em pt-BR.

## 1. Contexto

A tela `/providers` (label "Providers & Roteamento") existe num estado mínimo: uma tabela de
providers com toggle ativar/desativar e uma tabela de políticas de roteamento **read-only** — mesmo
o backend já expondo `PUT /v1/routing-policies`. Além disso ela mostra uma coluna **"Peso latência"
que não existe no core** (a `RoutingPolicy` só tem `cost_weight`, `quality_weight` e `fallback`).

Esta rodada faz um **redesign completo**, reposiciona o vocabulário e separa um domínio futuro.

## 2. Decisões travadas

1. **Renomear para "Workers".** O conceito de domínio é *worker* (a implementação concreta de uma
   capability: `distill`, `distill-local`, …). "Provider" no código permanece (é o nome da tabela e
   do struct); a **UI** passa a dizer "Worker". Rota `/providers` → `/workers`.
2. **"Agents" vira item de menu separado**, no mesmo nível, com **página stub "em breve"**. Agents é
   um domínio futuro (IA agêntica, cadastro de agentes, ciclo de vida próprio) — não tem relação de
   dados com Workers e não deve ser casado na mesma tela.
3. **Latência sai de toda a UI** — coluna "Latência (ms)" da tabela, "Peso latência" das políticas e
   os strings correspondentes. O campo `providers.latency_ms` fica dormente no banco (dropar exige
   migration; fica para uma limpeza opcional depois — ver §10).
4. **Roteamento editável**: dois pesos (custo↔qualidade, somam 1) via slider único + ordem de
   `fallback` reordenável, por escopo (global ou por capability).
5. **CRUD de workers**: adicionar + editar (sem delete — worker é referenciado por
   `item_steps.assigned_provider` via FK; desativar > deletar). Toggle Ativar/Desativar **mantido
   como está**.
6. **Métricas v1**: 4 cards derivados de `item_steps`/`providers`, **zero tabela nova**. Custo real
   em \$ e tempo de execução ficam para fase 2 (exigem instrumentação).
7. **Simular rota**: painel *dry-run* que roda o router em seco e mostra o ranking + o porquê de cada
   worker, sem disparar job.

## 3. Arquitetura de informação da tela `/workers`

Empilhada, de cima pra baixo:

1. **Header** — título "Workers" + botão "Novo worker".
2. **4 cards de métrica** (linha responsiva): Saúde & atividade · Confiabilidade · Volume & share ·
   Fila atual.
3. **Tabela de workers** — uma linha por worker, com edit inline e o toggle de status.
4. **Roteamento** (card) — seletor de escopo + slider custo↔qualidade + lista de fallback
   reordenável.
5. **Simular rota** (card, ao lado de Roteamento) — seletor de capability/lane/sensibilidade +
   ranking explicado.

O mockup aprovado define o layout: Roteamento e Simular rota ficam lado a lado num grid responsivo
abaixo da tabela.

## 4. Contratos de API

### 4.1 Já existem — reusar (nenhuma mudança no core)

| Endpoint core | BFF | Uso na tela |
|---|---|---|
| `GET /v1/providers` | `GET /api/workers` | lista de workers |
| `PUT /v1/providers` (upsert) | `PUT /api/workers` | **CRUD add/edit** e toggle |
| `GET /v1/routing-policies` | `GET /api/routing-policies` | políticas por escopo |
| `PUT /v1/routing-policies` (upsert) | `PUT /api/routing-policies` | **editar pesos + fallback** |

> O `PUT /v1/providers` é upsert (`ON CONFLICT (name)`), então "adicionar worker" e "editar worker"
> são a mesma chamada — a UI só precisa do formulário. Idem para políticas.

**Modelo de autenticação e acesso:** O BFF injeta o bearer token (`SURFACE_TOKEN`) server-side em
todas as chamadas ao core — o cliente nunca vê nem envia o token. A console só escuta em endereço
tailnet (`CONSOLE_ADDR`, e.g. `100.x.x.x:8081`), portanto só é acessível por operadores com acesso
à VPN Tailscale. Não há autenticação por usuário: a proteção de acesso é o isolamento de rede. Os
endpoints PUT não têm proteção CSRF adicional porque a console não usa cookies de sessão.

### 4.2 Novos — implementar no core (+ proxy no BFF)

**`GET /v1/workers/metrics`** — rollup por worker para os 4 cards. Uma query agregada; sem tabela
nova. Query param `?days=N` é opcional; validado no BFF e no core: deve ser inteiro entre 1 e 365
(fora do intervalo → 400); ausente = all-time.

```
SELECT assigned_provider,
       status,
       COUNT(*)                AS n,
       MAX(updated_at)         AS last_at,
       AVG(attempt)            AS avg_attempt
FROM item_steps
WHERE assigned_provider IS NOT NULL
GROUP BY assigned_provider, status;
```

Combinado com `GET /v1/providers` (que já traz `heartbeat_at`, `activation`, `enabled`) no lado do
BFF/cliente, alimenta:

- **Saúde & atividade** — heartbeat fresh/stale por worker (resident vs `on_demand` isento, mesma
  regra do router: `defaultHealthTTL`), + `last_at`.
- **Confiabilidade** — `done / (done+failed)` por worker, `avg_attempt`, último `error`.
- **Volume & share** — soma de `n` por worker no período (share = % do total).
- **Fila atual** — `pending`/`assigned`/`running` por capability e worker.

**`GET /v1/route/preview`** — *dry-run* do router. Query params: `capability` (obrigatório),
`lane`, `sensitivity` (opcionais; default = item neutro). Retorna **todos** os candidatos com a
disposição de cada um:

```jsonc
{
  "capability": "distill",
  "winner": "distill",
  "candidates": [
    { "name": "distill",       "eligible": true,  "healthy": true,
      "cost_credit": 0.00, "quality": 0.90, "score": 0.27,
      "fallback_pos": 1, "selected": true,  "reason": "on_demand: saúde isenta" },
    { "name": "distill-local", "eligible": true,  "healthy": true,
      "cost_credit": 1.00, "quality": 0.70, "score": 0.91,
      "fallback_pos": 2, "selected": false, "reason": "" }
  ]
}
```

Detalhe de implementação no §9.

### 4.3 Resumo de proxies a adicionar no BFF

`GET /api/workers/metrics` → `/v1/workers/metrics`; `GET /api/route/preview` → `/v1/route/preview`
(repassa query string). Padrão idêntico aos handlers `fetchCore` existentes.

## 5. Componentes & estados

Cada seção mantém o tripé já usado na console (loading / error / empty), com os strings
externalizados em `lib/strings.ts` (sem copy hardcoded). Reusar `Paginator`/padrões de tabela
existentes onde couber.

| Seção | loading | error | empty |
|---|---|---|---|
| Cards | skeleton | "Não foi possível carregar as métricas." | "Sem atividade no período." |
| Tabela workers | "Carregando workers…" | "Não foi possível carregar os workers." | "Nenhum worker registrado." |
| Roteamento | "Carregando políticas…" | "Não foi possível carregar as políticas." | "Nenhuma política — usando default neutro." |
| Simular rota | "Simulando…" | "Não foi possível simular a rota." | "Nenhum worker elegível (o item aguardaria)." |

## 6. Roteamento na UI

- **Slider único custo↔qualidade.** Um valor `0..1`; `cost_weight = v`, `quality_weight = 1 - v`.
  Move um, o outro completa. Sem latência. Salva via `PUT /v1/routing-policies`.
- **Fallback reordenável.** Lista ordenada de nomes de worker; reordenar por setas/drag (mesmo
  padrão de "hosts" já existente em Fontes & Flows — `step_hosts`). Persistida no campo `fallback`
  (JSONB) da política.
- **Por escopo.** Seletor: `global` ou uma capability. O router já resolve capability → global →
  default neutro (`policyFor`).

## 7. CRUD de workers

Formulário (add/edit) com os campos reais do struct `Provider`:

| Campo | Tipo / valores | Notas |
|---|---|---|
| `name` | string | chave (upsert por `name`); read-only no modo edit |
| `capability` | string | deve referenciar capability existente (FK) |
| `runtime` | `local \| cloudrun \| vpc` | select |
| `activation` | `resident \| on_demand` | select |
| `cost` | number ≥ 0 | peso relativo p/ o router (não é \$) |
| `quality` | number `0..1` | |
| `constraints` | JSON | Estrutura: `{"requires": "string", "accepts": ["string"], "sensitivity": "public\|private"}` — todos os campos opcionais; campos desconhecidos são rejeitados pelo core |
| `enabled` | bool | toggle Ativar/Desativar |
| `runner_url` | string | URL tailnet do rara-runner (ex: `http://100.x.x.x:PORT`); vazio para cloudrun. Só relevante para workers `vpc`/`local`. O campo não é validado pelo BFF (passthrough); o core usa para chamar o runner. |
| `env` | JSON | Config **não-secreta** por run — chaves de ambiente passadas ao job. **Nunca armazenar segredos** (tokens, senhas, API keys): o campo é visível em texto claro no banco e nos logs. |

Sem **delete** (FK em `item_steps`; o caminho é desativar). Validação espelha os CHECKs do schema
(`cost >= 0`, `quality 0..1`, enums de runtime/activation).

## 8. Métricas v1 — fonte e limites

Tudo deriva de `item_steps` (status, `assigned_provider`, `attempt`, `error`, `updated_at`) +
`providers` (`heartbeat_at`, `activation`, `enabled`). **Importante:** `providers.cost` é **peso
relativo do router, não dinheiro**; `item_steps` **não tem custo, token nem timestamp de início/fim**.

Logo, **fase 2** (fora deste escopo): custo real em \$ (instrumentar LiteLLM → persistir custo/token)
e tempo de execução/throughput (timestamps `started_at`/`finished_at` por step). Latência observada
foi descartada de propósito (§2.3).

### 8.1 Filtro de período (janela × ao vivo)

Nem todo card é histórico. O seletor de período afeta **só** os cards que acumulam no tempo:

| Card | Tipo | Fonte |
|---|---|---|
| Volume & share | **janela** | `GET /api/workers/metrics?days=N` — conta só `done` (share de sucesso por worker) |
| Confiabilidade | **janela** | mesmo fetch — taxa de sucesso `done/(done+failed)` + `failed` por worker (share de erro) |
| Fila atual | **ao vivo** | `pending/assigned/running` agora |
| Saúde & atividade | **ao vivo** | `heartbeat_at` (de `/api/workers`) + última atividade |

Implicações:

- **Dois fetches.** Um *all-time* (`/api/workers/metrics` sem `days`) para Fila + última atividade, e
  um *windowed* (`?days=N`) para Volume + Confiabilidade. "Última atividade" precisa ser all-time —
  senão some quando você filtra 24h.
- **Seletor segmentado** no topo da seção de métricas: **24h · 7d · 30d · Tudo**. `days=1|7|30` e
  "Tudo" = omitir `days`. Default **7d**. A escolha persiste em `localStorage`.
- Os cards ao vivo levam um selo **"agora"** para deixar claro que o seletor não os afeta.

## 9. Simular rota — implementação

O `rankProviders`/`scoreProviders` (router.go) é função pura, mas hoje retorna **só os sobreviventes
ordenados** — perde o "porquê" de quem caiu. Para o preview:

1. Extrair uma variante **`explainProviders(...) []Candidate`** que roda os mesmos filtros
   (`constraintsSatisfied`, `providerHealthy`) e o scoring, mas **não descarta** — marca cada
   candidato com `eligible`, `healthy`, `reason`, `cost_credit`, `quality`, `score`, `fallback_pos`,
   `selected`. `rankProviders` passa a ser um *thin filter* sobre o mesmo resultado (sem duplicar
   regra). Pura, testável sem I/O.
2. Surface `GET /v1/route/preview` monta um `Item` sintético a partir de `lane`/`sensitivity`, chama
   `explainProviders`, serializa.
3. O painel: seletor (capability/lane/sensibilidade) → ranking com breakdown. **What-if opcional**:
   checkbox por worker que passa o nome em `exclude` (mecanismo que o router **já tem** no
   timeout→fallback) e re-rankeia — só na simulação, não persiste. O BFF repassa os valores de
   `exclude` ao core sem limite explícito; nomes inválidos (worker inexistente) são simplesmente
   ignorados pelo router na re-classificação.

## 10. Remoção de latência — onde mexer

- `web/src/routes/providers/+page.svelte` (→ `workers/+page.svelte`): remover coluna "Latência (ms)"
  da tabela de workers e a coluna "Peso latência" das políticas.
- `web/src/lib/strings.ts`: remover `colLatency`, `colLatencyWeight` (e qualquer label de peso de
  latência).
- **Banco (opcional, fora do v1):** `providers.latency_ms` pode ser dropado numa migration de
  limpeza; recomendação é **deixar dormente** (sumir só da UI/serialização). Decisão do operador.

## 11. Menu "Agents" (stub)

Novo item de nav no mesmo nível (seção "Treinar", logo após Workers). Página `/agents` com um estado
"em breve" caprichado (não o placeholder cinza padrão): copy curta explicando que é o futuro de IA
agêntica + cadastro de agentes. Sem chamadas ao core. O design de Agents é uma rodada futura.

## 12. Mudanças de arquivos (checklist)

**Frontend (`rara-console/web/src`)**
- `routes/providers/` → `routes/workers/` (`+page.svelte` reescrito).
- `routes/agents/+page.svelte` (novo, stub).
- `+layout.svelte`: nav (`/providers`→`/workers`, label "Workers", + item "Agents"), `pageTitles`,
  `CommandPalette` (entradas).
- `lib/strings.ts`: bloco `workers` (renomeado de `providers`, sem latência) + `agents` + `nav`.

**Backend core (`rara-core`)**
- `router.go`: `explainProviders` + refactor de `rankProviders` sobre ele (TDD).
- `surface.go`: `GET /v1/route/preview`, `GET /v1/workers/metrics`.
- `store_reads.go`: query agregada por `assigned_provider`.
- `main_test.go` (MockDatabase) + testes: `router_test.go`, surface tests.

**BFF (`rara-console/main.go`)**
- `handleWorkerMetrics`, `handleRoutePreview` + registro no mux (✅ fatias 2–3).
- **Rename `/api/providers` → `/api/workers`** (GET+PUT) — fatia 4, junto do rename do front (mesmo
  módulo). Core fica `/v1/providers`; `/api/routing-policies` permanece.

## 13. Trade-offs & questões em aberto

- **`/api/providers` → `/api/workers` (DECIDIDO).** O BFF renomeia `/api/providers` (GET+PUT) para
  `/api/workers`; o core mantém `/v1/providers` (é o nome do domínio/tabela). `/api/routing-policies`
  permanece (é roteamento, não worker). Entra na fatia 4 (BFF + front são o mesmo módulo
  `rara-console`). Ver §8.1 e §12.
- **Período das métricas (DECIDIDO):** ver §8.1. Seletor 24h·7d·30d·Tudo (default 7d) afeta só os
  cards históricos; Fila e heartbeat são sempre "agora".
- **Share de volume (DECIDIDO):** o card *Volume & share* conta só execuções com **sucesso** (`done`)
  — share de sucesso por worker. O **share de erro** (`failed` por worker) mora no card
  *Confiabilidade*, não no de Volume. Tudo já vem do `WorkerMetric` (`Done`/`Failed`/`ByStatus`).

## 14. Plano de implementação (fatias, TDD)

| # | Fatia | Status | PR |
|---|---|---|---|
| 1 | Core — preview (`explainProviders` + `/v1/route/preview`) | ✅ | #155 |
| 2 | Core — métricas (query agregada + `/v1/workers/metrics`) | ✅ | #155 |
| 3 | BFF — proxies dos dois + alias `/api/workers` | ✅ | #156 |
| 4 | Front — rename + nav (`/workers`, `/agents` stub, strings, palette) | ✅ | #156 |
| 5 | Front — cards de métrica | ✅ | #159 |
| 6 | Front — CRUD de workers (formulário add/edit) | ✅ | #160 |
| 7 | Front — roteamento (slider + fallback reordenável) | ✅ | #161 |
| 8 | Front — simular rota (painel + what-if exclude) | ✅ | #162 |
| 9 | Verificação — smoke ponta a ponta + deploy | ✅ | — |

### MVP entregue — 2026-06-20

A tela `/workers` entrega em produção:

- **4 cards de métrica** derivados de `item_steps` + `providers`: Saúde & atividade,
  Confiabilidade, Volume & share, Fila atual. Seletor 24h·7d·30d·Tudo; cards "ao vivo"
  com badge "agora" não afetados pelo seletor.
- **Tabela de workers** com CRUD add/edit (formulário com validação) e toggle Ativar/Desativar.
  Campos `constraints` e `env` preservados no upsert.
- **Editor de roteamento** por escopo (global ou por capability): slider custo↔qualidade
  (somam 1) + lista de fallback reordenável. Persistido via `PUT /v1/routing-policies`.
- **Painel "Simular rota"**: dry-run do router ao vivo — mostra vencedor, ranking completo
  com score/motivo por candidato e what-if exclude por worker.
- **Stub `/agents`** com copy "em breve" explicando o domínio futuro.

### Backlog fase 2

- **Custo real em \$** — exige instrumentar LiteLLM → persistir custo/token em `item_steps`
  (campo `cost_usd` ou similar).
- **Tempo de execução / throughput** — exige `started_at`/`finished_at` por step.
- **`latency_ms` no banco** — campo dormente em `providers`; pode ser dropado numa migration
  de limpeza quando não houver mais risco de referência externa.
- **Domínio Agents** — cadastro de agentes de IA, ciclo de vida próprio; rodada futura
  sem relação de dados com Workers.
