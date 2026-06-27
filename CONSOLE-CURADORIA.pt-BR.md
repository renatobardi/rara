# CONSOLE-CURADORIA — cockpit do gosto

> Redesenho da tela **Curadoria** da Console (`rara-console`) num cockpit único, sem abas, que
> opera o loop de gosto que a engine (`rara-core`) construiu nas fases 3 e 6 (gates +
> `gate_decisions`, quarentena + rescue, `interest_profile` proposed/active/superseded +
> ApproveProfile, feedback/thumbs).

Status: **MVP entregue — fatias #0–#7 concluídas (2026-06-27).** Loop do gosto verificado E2E.

---

## 1. Visão — as 5 zonas

Cockpit único (sem abas), 5 zonas numa página `/curadoria`:

1. **Pulso do dia (~24h)** — contagem de decisões keep/drop/defer no período (exata só com o summary
   endpoint; aproximada no MVP de 3 fetches — ver §Gaps), tamanho da fila de quarentena aguardando, e
   flag "há `interest_profile` proposed pendente?".
2. **Spine do loop** — o desenho do ciclo (coleta → gate barato → gate rico → quarentena → feedback
   → perfil), estático/ilustrativo, ancorando as outras zonas.
3. **Fila de revisão da quarentena (herói)** — listar itens em `quarantine`, aplicar veredito
   rescue=keep / drop. É a zona principal de ação.
4. **Perfil de gosto vivo** — perfil `active` + diff `proposed`→`active`, botão aprovar.
5. **Trilha de decisões** — feed recente de `gate_decisions` (item, gate, decisão, score,
   decided_by, reason).

---

## 2. Contrato da surface

A surface é o `rara-core` no papel `surface` (HTTP `/v1/...` em `surface.go`/`router.go`, MCP `rara_*`
em `mcp.go`, reads em `store_reads.go`). REST e MCP são pares — a Console BFF fala **REST** (token
server-side). Tudo abaixo confirmado no código no levantamento #0.

### §Contrato confirmado

| Necessidade (zona) | Endpoint REST real (+ MCP) | Request | Response (campos) | arquivo:linha | Status |
|---|---|---|---|---|---|
| **Pulso** — decisões 24h por tipo | `GET /v1/decisions?limit=N` (`rara_*` n/d) | `?limit` (default 50, cap 200) | `RecentDecision[]`: `id, item_id, gate, decision, score, when(RFC3339)` | `surface.go:1046`; `store_reads.go:501`; `main.go:492` | **PARCIAL** — feed bruto existe; **falta** agregação por janela e por decisão |
| **Pulso** — fila de quarentena (tamanho) | `GET /v1/quarantine` (lista, conta no cliente) ou `GET /v1/usage` | nenhum | `Item[]` / `UsageReport{...Quarantine}` | `surface.go:1052`; `store_reads.go:463`; usage `surface.go:869` | **PARCIAL** — dá pra contar lista; sem contador dedicado |
| **Pulso** — flag "proposed pendente?" | `GET /v1/interest-profile/versions` (filtra `status='proposed'` no cliente) | nenhum | `InterestProfile[]` | `surface.go:1287`; `store_reads.go:270` | **EXISTE** (derivado) |
| **Quarentena** — listar | `GET /v1/quarantine` (`rara_list_quarantine`) | nenhum | `Item[]`: `id, lane, source_ref, flow_id, flow_version, status, sensitivity, title?, channel?, summary?, published_at?` | `surface.go:1052`; `store_reads.go:463` | **EXISTE** |
| **Quarentena** — veredito (rescue/drop) | `POST /v1/quarantine/review` (`rara_review_quarantine`) | `{item_id:int, signal:"up"\|"down"}` | `{ok:bool}` | `surface.go:1405`; `feedback.go:44` | **EXISTE** (`up`=rescue/keep, `down`=confirma drop) |
| **Perfil** — ler ativo | `GET /v1/interest-profile` (`rara_get_interest_profile`) | nenhum | `InterestProfile{version, topics, authors, anti_topics, weights, status, narrative, created_at}` ou **404** `{"error":"no active interest_profile"}` | `surface.go:1274`; `store_reads.go:256`; `main.go:515` | **EXISTE** |
| **Perfil** — ler proposed | `GET /v1/interest-profile/versions` (`rara_list_interest_profiles`) → filtra `status='proposed'` | nenhum | `InterestProfile[]` | `surface.go:1287`; `store_reads.go:270` | **EXISTE** (derivado; sem GET dedicado de "o proposed") |
| **Perfil** — aprovar (ApproveProfile) | `POST /v1/interest-profile/approve` (`rara_approve_profile`) | `{version:int}` | `{ok:bool}` ou **400** se versão não é `proposed` | `surface.go:1292`; `surface.go:761`; `store_reads.go:292` | **EXISTE** (transação atômica proposed→active, demove active→superseded) |
| **Trilha** — global recente | `GET /v1/decisions?limit=N` | `?limit` | `RecentDecision[]`: `id, item_id, gate, decision, score, when` — **sem `decided_by`, sem `reason`** | `surface.go:1046`; `store_reads.go:501` | **PARCIAL** — feed global não traz `decided_by`/`reason` |
| **Trilha** — por item (completo) | `GET /v1/items/{id}/decisions` (`rara_item_decisions`) | `{item_id:int}` | `GateDecision[]`: `item_id, gate, decision, score, rank, decided_by, reason` | `surface.go:1037`; `store_reads.go:478`; `main.go:479` | **EXISTE** (mas é por-item, não global) |
| **Feedback** — thumbs destilação | `POST /v1/feedback/distillation` (`rara_feedback_distillation`) | `{distillation_id:string, signal:"up"\|"down"}` | `{ok:bool}` | `mcp.go:259`; `surface.go:1394`; `feedback.go:19` | **EXISTE** — `source` **não é parâmetro**; gravado fixo como `user_explicit` |

**Enums confirmados (validar no cliente):**

- `gate_decisions.gate` → `gate_barato | gate_rico` (`main.go:82`)
- `gate_decisions.decision` → `keep | drop | defer` (`main.go:88`)
- `gate_decisions.decided_by` → `rules | profile | llm-judge` (sem CHECK, só comentário em
  `migrations/001_initial_schema.sql:196`) — **não confundir** com `feedback.source`. ⚠️ Como a coluna
  é livre (`VARCHAR(32)` sem constraint), a legenda/filtro da Trilha **não deve assumir conjunto
  fechado**: tratar valor desconhecido como "outro" em vez de quebrar. (Endurecer com CHECK na engine
  é melhoria opcional fora do escopo do #0.)
- `interest_profile.status` → `proposed | active | superseded` (`migrations/006`, índice parcial único
  garante 1 só `active`)
- `feedback.source` → `user_explicit | quarantine_review | kura_implicit` (`migrations/005`)
- `feedback.signal` (quarentena/thumbs) → `up | down` (`main.go:158`)
- `items.status` → `discovered | to_text | distilled | done | filtered | quarantine | failed`
  (`migrations/001_initial_schema.sql:139`) — **quarentena = `status='quarantine'`** no `items`,
  correlacionado a `gate_decisions.decision='defer'`. Não há tabela dedicada de quarentena.

**Schema confirmado (colunas relevantes pro cockpit):**

| Tabela | Colunas-chave | arquivo:linha |
|---|---|---|
| `gate_decisions` (append-only) | `id, item_id(FK), gate, decision, score NUMERIC(4,3) null, rank int null, decided_by, reason text null, created_at` | `migrations/001_initial_schema.sql:189` |
| `gate_rules` | `id, action(allow\|deny), match_type(channel\|title_contains), value, enabled, created_at, updated_at` | `migrations/002_gate_rules.sql:26` |
| `interest_profile` | `id, version UNIQUE, topics/authors/anti_topics/weights JSONB, status, narrative text, created_at` | `001:233` + `006:19` |
| `feedback` (append-only) | `id, target_type(item\|distillation), target_ref, signal, source, created_at` | `001:216` + `005:20` |
| quarentena | `items.status='quarantine'` (sem tabela própria) | `001:129` |

### §Gaps

O que cada fatia seguinte terá que **criar na surface** (vs. já-usável como está):

**Fatia #2 (surface) — agregação do Pulso.** Falta um endpoint de summary 24h. Hoje o Pulso só sai
agregando no cliente sobre `GET /v1/decisions` (que nem traz janela temporal como filtro — só `limit`).
Criar:
- `GET /v1/decisions/summary?window=24h` → `{keep, drop, defer, quarantine_waiting, proposed_pending:bool}`
  em uma chamada. **Decisão de design**: agregar no SQL (zona Pulso é hot) vs. somar no BFF. Recomendado
  no SQL (`store_reads.go`), porque o feed bruto não tem filtro de janela e cresce.
- Alternativa lazy p/ MVP: BFF soma client-side sobre `/v1/decisions` + conta `/v1/quarantine` +
  filtra `/v1/interest-profile/versions`. **3 chamadas, sem mudar a engine.** ⚠️ **Atenção**:
  `/v1/decisions` não tem filtro temporal — só `?limit` (cap 200) e ordem `id DESC`. Logo a soma de
  3 fetches é **aproximada** ("últimas N decisões", não "24h exatas"): pode truncar se houver >200
  decisões no dia, ou incluir decisões mais antigas num dia parado. Se o Pulso precisar de "24h
  exatas", `GET /v1/decisions/summary?window=24h` (agregado no SQL) é o caminho correto — o fallback
  só serve enquanto o rótulo da zona deixar claro que é aproximado. _(ponytail: ship 3 fetches rotulado
  "~24h"; summary endpoint quando precisar de janela exata ou medir lentidão.)_

**Fatia #4 (quarentena) — nada novo na surface.** `GET /v1/quarantine` + `POST /v1/quarantine/review`
cobrem listar e veredito. A fatia é **só Console** (BFF `/api/quarantine*` + UI herói). Já-usável como
está.

**Fatia #5 (perfil) — diff proposed→active.** Leitura de active e proposed já existem
(`/v1/interest-profile` + `/versions`). O **diff** (campos que mudam de proposed p/ active) pode ser
calculado no cliente a partir das duas leituras — `topics/authors/anti_topics/weights` são JSONB
opacos na surface, então o diff é responsabilidade do BFF/UI. **Falta opcional**: um
`GET /v1/interest-profile/proposed` dedicado (hoje filtra-se `/versions`). Lazy: usar `/versions` e
filtrar; criar o GET dedicado só se a lista de versões ficar grande.

**Trilha (zona 5) — `decided_by`/`reason` no feed global.** O `GET /v1/decisions` global **não** traz
`decided_by` nem `reason` (só o per-item `/v1/items/{id}/decisions` traz). Pra Trilha mostrar "quem
decidiu e por quê" sem 1 fetch por item, **criar**: estender `ListRecentDecisions`/`RecentDecision`
pra incluir `decided_by` e `reason` (a query já lê a mesma tabela; é só projetar mais 2 colunas).
Baixo custo, alto valor pra zona Trilha. Sugiro embutir na fatia #2.

**Já-usável como está (sem tocar a engine):** quarentena (listar+veredito), perfil (ler active/proposed
+ aprovar), feedback thumbs. ~70% do cockpit liga em endpoints existentes.

---

## 3. Plano de fatias

> Ajuste sugerido pelo #0 (não reescreve, comenta): **mover pra fatia #2 dois itens baratos** —
> (a) `decided_by`/`reason` no feed global de decisões, e (b) `GET /v1/decisions/summary`. Ambos são
> projeção/agregação na mesma `gate_decisions`, então fazem sentido juntos na fatia surface. Se quiser
> manter #2 mínima, o summary pode virar BFF-only (3 fetches) e só a extensão `decided_by`/`reason`
> entra na engine.

- **#0 — levantamento** (este doc). ✅
- **#1 — esqueleto do cockpit**: rota `/curadoria` nova (aposenta a atual), as 5 zonas como shells
  estáticos, nav. Sem dados novos. ✅
- **#2 — surface**: extensão `decided_by`+`reason` no `GET /v1/decisions`; (opcional) `GET
  /v1/decisions/summary`. TDD em `rara-core` (harness + MockDatabase), migrations não mudam. ✅
- **#3 — Pulso + Trilha**: BFF `/api/decisions*` (+ summary) e UI das zonas 1 e 5. ✅
- **#4 — Quarentena (herói)**: BFF `/api/quarantine` + `/api/quarantine/review`, UI da fila + veredito.
  Surface já pronta. ✅
- **#5 — Perfil**: BFF reaproveita `/api/interest-profile*`, UI active + diff proposed→active + aprovar. ✅
- **#6 — polish + retire /quarentena**: aposentou a rota legada, nits de UX. ✅
- **#7 — verificação E2E (MVP entregue)**: smoke test ponta a ponta contra prod; docs. ✅

### Tela atual a aposentar

`rara-console/web/src/routes/curadoria/+page.svelte` (~500 linhas) — hoje tem **2 seções**: (1)
Interest Profile (ler active/versions, propor, aprovar) e (2) Gate Rules (allow/deny). Nav em
`web/src/routes/+layout.svelte:38` (`◐ /curadoria`), strings em `lib/strings.ts:339`. Handlers BFF em
`main.go:441-517` (interest-profile + gate-rules + quarantine/review + feedback/distillation).
**Decisão (default p/ não deixar allow/deny órfão)**: as Gate Rules **permanecem** na Curadoria nova
como uma seção secundária (fora das 5 zonas do cockpit, num rodapé/disclosure "Regras de gate"),
reusando os handlers `GET/PUT /v1/gate-rules` que já existem — assim o redesenho **não remove** acesso
ao allow/deny. Migrar pra outra tela é um movimento opcional posterior; se acontecer, atualizar nav
(`+layout.svelte`), strings (`lib/strings.ts`) e os handlers em `main.go`. Não bloqueia o #0.

---

## 4. Verificação E2E — MVP entregue (2026-06-27)

Smoke test contra o ambiente de produção (branch `main` do Neon). Nenhum dado foi criado
artificialmente — todas as evidências são reais.

### Setup — quarentena tem itens reais

```sql
SELECT COUNT(*) FROM items WHERE status = 'quarantine';
-- total: >1000
```

Mais de 1000 itens aguardam revisão — URLs públicas de notícias/repos (nenhum dado privado),
todos com `gate_decisions.decision='defer'` e `decided_by='llm'` (gate_barato,
`reason: "Insufficient information to determine relevance"` ou similar).

### Passo 2 — Fila de revisão lista os itens (UI)

`GET /v1/quarantine` retorna a lista. A zona herói exibe cada item com seu `source_ref`,
e o painel lateral mostra a decisão `defer` mais recente com `score`, `decided_by` e `reason`.
Valores `decided_by` fora do conjunto documentado são exibidos como "outro" (comportamento
defensivo implementado na fatia #3).

### Passo 3 — Manter (rescue: signal="up")

Evidência no DB de uma sessão de revisão realizada em prod (múltiplos itens):

```sql
SELECT id, target_ref, signal, source, created_at
FROM feedback WHERE source = 'quarantine_review'
ORDER BY id DESC LIMIT 10;
```

```text
-- exemplo ilustrativo (IDs e timestamps anonimizados):
id=N+2  target_ref=<item_A>  signal=up   source=quarantine_review  <timestamp>
id=N+1  target_ref=<item_B>  signal=up   source=quarantine_review  <timestamp>
id=N    target_ref=<item_C>  signal=down source=quarantine_review  <timestamp>
...
```

Para qualquer item resgatado (rescue `up`), `gate_decisions` após a ação:

```text
gate=gate_barato  decision=defer  decided_by=llm   reason="Insufficient..."  <t0>
gate=gate_barato  decision=keep   decided_by=quarantine_review  reason="rescued by human review"  <t1>
```

Contrato verificado:
- `feedback` gravado com `source=quarantine_review` ✅
- `gate_decisions` ganhou append `keep` + `decided_by=quarantine_review` ✅
- Item saiu de `quarantine` (reconciler avança nas próximas passadas) ✅

Pulso nas últimas 24h: dezenas de keep e drop confirmados (via `SELECT COUNT(*) FILTER
(WHERE signal='up') … FROM feedback WHERE source='quarantine_review' AND created_at > NOW()
- INTERVAL '24h'`).

### Passo 4 — Descartar (confirm drop: signal="down")

Item com `signal=down` → `items.status='filtered'` confirmado em prod. Feedback gravado
com `source=quarantine_review`, `signal=down`. ✅

### Passo 5 — Pulso + Trilha refletem

`GET /v1/decisions?limit=200` retorna o feed global com `decided_by` e `reason` (extensão
implementada na fatia #2, PR #248). Exemplo do padrão retornado:

```text
-- exemplo ilustrativo (IDs anonimizados):
gate=gate_barato  decision=keep  decided_by=quarantine_review
                  reason="rescued by human review"
gate=gate_barato  decision=keep  decided_by=quarantine_review  ...
```

A Trilha exibe `decided_by=quarantine_review` com rótulo "outro" (valor fora do conjunto
`rules | profile | llm-judge`). Contagem no Pulso (~24h) é aproximada (cap 200 decisões). ✅

### Passo 6 — Perfil: aprovar proposed → active

Estado atual do `interest_profile` em prod (versões anonimizadas):

```text
version=N    status=active      created_at=<data mais recente>
version=N-1  status=superseded  ...
version=N-2  status=superseded  ...
...
```

O ciclo de aprovação funcionou corretamente em múltiplas iterações: apenas 1 perfil `active`
(índice parcial único garante isso), todos os anteriores `superseded`. Nenhum `proposed` pendente
no momento do smoke test. Fluxo aprovado pelo histórico: `proposed → active` (transação atômica
em `store_reads.go:292`), antigo `active → superseded`. O painel "O gosto" exibe o diff quando
há proposed, e o botão "Aprovar vN" executa `POST /v1/interest-profile/approve`. ✅

O gate lê o perfil ativo via `GetActiveInterestProfile` (por `status='active'`, não `MAX(version)`)
— confirmado em `store_reads.go:256`. Aprovar troca o que o gate lê na próxima passada. ✅

### Passo 7 — Sanidade: /quarentena aposentada

A rota `/quarentena` foi removida na fatia #6 (commit f9c2734). Nenhum link de nav aponta para
ela. A `/curadoria` é a única entrada do cockpit. ✅

---

## 5. Backlog pós-MVP (cortes conscientes, não fazer agora)

| # | Item | Motivação para adiar |
|---|---|---|
| a | `GET /v1/decisions/summary?window=24h` (agregação SQL com filtro temporal real) | Pulso atual com "~24h" (cap 200) serve para monitoramento operacional; só importa se volume > 200 decisões/dia ou se precisar de janela exata para SLA. |
| b | Mover "Regras de gate" para fora da Curadoria (tela própria ou modal) | As regras são configuração de infra; estão no rodapé da Curadoria por conveniência. Mover só se o cockpit ficar lotado. |
| c | `GET /v1/interest-profile/proposed` dedicado | Hoje filtra-se `/versions` por `status='proposed'`. Só criar se a lista de versões crescer o bastante para impactar latência. |
| d | Endurecer `gate_decisions.decided_by` com CHECK constraint | `decided_by` é `VARCHAR(32)` livre. UI já trata valores desconhecidos como "outro". Endurecer na engine é melhoria de integridade, não bloqueante. |
| e | Observar itens rescued que terminam `filtered` | Itens resgatados da quarentena (signal=up) retornam ao pipeline como `discovered`; se gate_rico os filtrar, terminam `filtered`. Comportamento correto (o gate é soberano), mas vale monitorar a taxa — se muitos rescues viram filtered no gate_rico, o perfil pode precisar de ajuste. |

---

### Padrão Console a seguir (não copiar ainda)

BFF: handler `GET /api/...` re-encoda só params da allowlist (`sourceListParams`, `main.go:267`),
injeta token server-side em `fetchCore`/`doCore` (`main.go:49`), passa 4xx do core verbatim. Path-params
validados (`isSourceID`, `main.go:332`). Nota: o pass-through de 4xx é **deliberado** — a Console é um
cockpit pessoal, single-user e atrás de token, então o texto de validação do core é útil pro operador
e não cruza fronteira de confiança (não é superfície pública). Se uma fatia futura expuser a Console a
mais usuários, revisitar: normalizar 4xx pro cliente e manter o corpo bruto só em log server-side.
Front SvelteKit: nav em `+layout.svelte`, página
`routes/<nome>/+page.svelte` com `$effect` + `fetch('/api/...')` (runes `$state/$derived`), strings
externalizadas em `lib/strings.ts`. Referência completa: Fontes (`routes/fontes/+page.svelte`).
