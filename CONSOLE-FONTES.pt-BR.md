# CONSOLE-FONTES — CRUD de Fontes de Flows (design + plano de fatias)

> Status: **plano** (Cowork organiza, Claude Code implementa). Este doc é o desenho;
> as fatias estão em `prompts/CONSOLE-FONTES-{n}.md`.
> Antes de codar a fatia #0, o Claude Code **valida o schema real** (estas notas têm ~1 semana).

---

## 0. O que o Renato pediu

Isolar **Fontes** como item próprio no menu da Console (separado de Flows/Workers) e
entregar um **CRUD amplo**: adicionar, editar, excluir, **pausar/retomar**, **taguear**,
**filtrar** e **ações em lote** — para cada tipo de fonte (YouTube, URLs, RSS/feed,
podcast, LinkedIn, **Email de leitura**). Tudo "completo já no MVP".

Decisão do Renato sobre fluxo de cadastro: **escolher o tipo da fonte primeiro** e o
formulário se monta a partir dali, gravando nas **tabelas atuais** de cada coletor.

---

## 1. Recomendação de modelo de dados — **HÍBRIDO** (escolhido)

### Achados do schema real (Neon `neondb`, introspecção 24/jun/2026)
Confirmado contra o banco — **as fontes que você cadastra já existem em 4 tabelas**, e
**3 delas já têm flag de ativação** (ou seja, "pausar" já é meio-suportado hoje):

| kind | lane | tabela (linhas) | flag de pausa hoje | dono |
|---|---|---|---|---|
| youtube_channel | youtube | `target_channels` (102) | **`active` bool** | rara-harvest |
| youtube_playlist | youtube | `playlists` (11) | **`active` bool** | **rara-shelf** (corrigido na #0; shelf não filtrava `active` — fix incluído) |
| podcast | podcast | `podcast_feeds` | **`active` bool** | rara-dial |
| feed / rss / url | news | `feed_sources` (`source_type`,`cls`,`parser`) | **`enabled` bool** | rara-feed |
| email_leitura | email | `email_sources` (criada na #0; `emails` é **saída**) | **`enabled` bool** | rara-courier |
| linkedin | linkedin | — (não há registro de fonte; `linkedin_posts` é **saída**) | — | rara-clip |

Três consequências diretas:
1. **`feed_sources` já é multi-tipo** (`source_type`/`cls`/`parser`/`fetch_strategy`) → cobre
   seu "urls/feed/rss" numa tabela só; o `kind` do wizard vem do `source_type`.
2. **Não inventar um enum `status`.** Reusar `active`/`enabled` como o mecanismo de pausa —
   os coletores já filtram por eles (confirmar na #0). Mecanismo já existe; falta unificar a
   leitura e a UI.
3. **Email e LinkedIn não têm tabela de fonte.** O que existe (`emails`, `linkedin_posts`) é
   conteúdo coletado, não cadastro. Pra virarem "fontes" gerenciáveis precisam de tabela nova
   (`email_sources` / `linkedin_sources`) — é trabalho **net-new**, fora do reuso (ver §2).

### O dilema (mantido)
Três caminhos: (A) tabela `sources` unificada no rara-core; (B) BFF agregando as tabelas
atuais sem migração; (C) **híbrido**. O schema acima **reforça o híbrido**: já há 4 tabelas
soberanas com flag de pausa — basta padronizar leitura + adicionar tags/rótulo.

### Por que híbrido, e não A nem B

**Contra A (registro unificado no core):** inverteria o **bridge-total**. Hoje cada coletor
é soberano e dono da sua tabela; mover o cadastro de fontes pro core faria o orquestrador
voltar a conhecer domínio. Custo alto, risco alto, contra a arquitetura 2.0.

**Contra B (BFF puro sobre tabelas atuais):** você pediu **pausar, taguear e filtrar de
forma uniforme já no MVP**. As tabelas de domínio hoje **provavelmente não têm** colunas
comuns de `status`, `tags`, `paused_at`. Sem padronizar isso, cada filtro/pausa vira um
caso especial por tipo — frágil e caro de manter.

**O híbrido (C) resolve os dois:** mantém a **posse no coletor** (sem inverter bridge-total)
e ainda dá um CRUD **homogêneo** porque todas as tabelas ganham o mesmo "contrato comum".

### O desenho híbrido em uma frase
> Cada tabela de fonte ganha **as mesmas colunas comuns**; uma **view `sources_v`**
> normaliza a leitura num formato único; a **escrita continua por tipo**; e um **registry
> `source_kinds`** (config no core) descreve os campos de cada tipo pro wizard se montar
> sozinho.

#### (a) Colunas comuns — só o que FALTA (migração aditiva mínima)
As tabelas já têm `id`, timestamps e o flag de pausa (`active`/`enabled`). Então o reuso é
máximo e a migração só adiciona o que não existe:
```
tags          text[]  not null default '{}'   -- nas 4 tabelas de fonte
display_name  text                            -- rótulo amigável (backfill do nome existente)
```
**Não** criar coluna `status`: a pausa continua sendo `active`/`enabled` (os coletores já
honram). **Não** criar `source_id` uuid: o id da API é composto **`kind:id`**
(ex.: `youtube_channel:42`), evitando migração de PK. Tudo aditivo, nada quebra. Cada
migração fica no repo **dono da tabela** (harvest/dial/feed; courier/clip nas novas de §2).

#### (b) View de leitura `sources_v` (normaliza tudo num shape só)
`UNION ALL` projetando de cada tabela pro shape único:
`api_id (kind:id), kind, lane, display_name, status (active|paused, derivado de
active/enabled), tags, created_at, updated_at, config_summary` (resumo legível: handle/URL/
feed_url/endpoint). É onde **lista, filtros, busca e contadores** batem. Mora no core
(orquestrador lê as tabelas de domínio no mesmo Neon) — a fatia #0 confirma e materializa.

#### (c) Escrita por tipo (o "type-first" que você quis)
Cada `kind` tem seu endpoint de escrita que valida e grava **na tabela do coletor**.
O wizard escolhe o tipo → renderiza os campos certos → chama o endpoint daquele tipo.

#### (d) Registry `source_kinds` (config-driven, mora no core)
Mapa versionado em Go (não precisa de tabela), exposto via `GET /v1/source-kinds`. Por tipo:
`kind, label, lane, icon, target_app, fields[] (name,label,type,required,validation,
placeholder), supports_pause, supports_tags`. É **a fonte da verdade do formulário** —
adicionar um tipo novo = uma entrada no registry + endpoint de escrita, sem mexer no front.

### Consequência crítica: **"pausar" tem que ser respeitado**
Pausar = setar `active=false` / `enabled=false`. Isso só vale se o coletor **filtra por esse
flag na descoberta**. Como o flag já existe nas 4 tabelas, é bem provável que harvest/dial/
feed **já filtrem** — a fatia #0 confirma (read-only) cada query e só adiciona o filtro onde
faltar. Verificado na #6. Sem isso, "pausar" é cosmético.

---

## 2. Email de leitura — o que é o "cadastro"

Você disse: *"Email de leitura, vc não está vendo meus emails do gmail?"*. Dois Gmails
diferentes convivem aqui, e o plano trata só do segundo:

- **Gmail do Cowork (este chat):** é o conector do Cowork, serve pra mim te ajudar agora.
  **Não** é o que alimenta o rara.
- **Gmail do `rara-courier`:** o coletor de email do pipeline, com **OAuth refresh token
  próprio** (autônomo, padrão rara-shelf). **É esse** que o cadastro de Fontes configura.

**Achado no schema (plano original):** não havia tabela de fonte de email — só `emails`
(saída: message_id, sender, subject, body, status); o courier puxava a caixa inteira.
**Resolvido na #0:** criada `email_sources` (dona = courier) com `enabled` + `tags` +
`display_name` + `gmail_query`/`label`/`from_filter`, e o courier passou a ler as regras via
`ListEmailSources` (`WHERE enabled=true`). Cada linha = uma regra de leitura
pausável/tagueável. Pausar = courier para de puxar daquela regra.

> **DECIDIDO: completo (N regras).** Cada regra (`gmail_query`/`label`/`from_filter`) é uma
> fonte pausável/tagueável, e o **courier passa a respeitar as regras na coleta** (itera as
> `enabled=true` e compõe a query Gmail; sem regra = não coleta; dedup por `message_id`).
> Semear 1 regra default = comportamento atual, pra não perder coleta no cutover. Detalhe na
> fatia #0, Passo 3. **LinkedIn** segue fora do MVP: `linkedin_posts` é saída; "fontes"
> (perfis/buscas a crawlear) seriam `linkedin_sources` net-new acoplada ao Bright Data do
> clip — vira toggle de lane até definirmos.

---

## 3. Contrato de Surface (BFF) — o que o core expõe

### ⭐ Precedente JÁ no código (validado em rara-core/surface.go, 24/jun)
O padrão que este plano propõe **já existe pra podcast** — é só generalizar, não inventar:
```
GET  /v1/sources/podcast  → listPodcastFeeds   (Core.PodcastFeeds → db.ListPodcastFeeds)
POST /v1/sources/podcast  → addPodcastFeed     (Core.AddPodcastFeed → db.UpsertPodcastFeed)
PUT  /v1/sources/podcast  → setPodcastFeedActive (toggle active; SEM hard-delete em v1)
```
Comentário do próprio código (surface.go:345): *"Managing what to collect is an operator
decision, so the core surface WRITES podcast_feeds — just as it owns flows/providers. The
table's DDL is rara-dial's; the collector keeps only READING active=true."* — **é
exatamente o híbrido deste plano.** O core já escreve a tabela do coletor (config de
operador); o coletor só lê `active`. Logo: estender esse padrão pra `youtube_channel`,
`youtube_playlist`, `feed` e `email`, e adicionar a lista unificada + tags.

> **Delete:** o precedente NÃO tem hard-delete (usa `active=false`). Você pediu "excluir".
> Recomendo **soft-delete** (toggle active/enabled, opção "arquivar") como default e, se quiser
> hard-delete real, adicionar `DELETE` explícito por kind — decisão registrada na fatia #2.

Tudo via a surface existente (`/v1/...`, auth bearer fail-closed; a BFF da Console guarda
o token server-side). Espelhos MCP `rara_*` onde fizer sentido.

**Leitura**
- `GET /v1/source-kinds` → registry (alimenta o wizard).
- `GET /v1/sources?kind=&status=&tag=&q=&page=&page_size=` → lista da `sources_v`
  com filtros, busca e paginação. Inclui contadores por status/tipo p/ os badges.
- `GET /v1/sources/{source_id}` → detalhe (join no kind certo).

**Escrita (por tipo, validada pelo registry)**
- `POST   /v1/sources/{kind}` → cria na tabela do coletor.
- `PATCH  /v1/sources/{source_id}` → edita campos do tipo + comuns (tags/display_name).
- `DELETE /v1/sources/{source_id}` → exclui (ou `status='archived'` se preferir soft).
- `POST   /v1/sources/{source_id}/pause` · `/resume`.

**Lote**
- `POST /v1/sources/bulk` `{action: pause|resume|tag|untag|delete, ids[], tag?}`.

Regras: `DisallowUnknownFields`, body cap, idempotência, validação por `kind`, fail-closed.

---

## 4. UX da Console — tela **Fontes** (item de menu isolado)

Visual Option B (ChatGPT-inspired, Clean default), tokens do `CONSOLE-PLAN.pt-BR.md` §0.

- **Nav:** novo item **"Fontes"** no sidebar, separado de Flows e Workers.
- **Lista:** tabela com `display_name`, tipo (ícone+label), lane, status (dot colorido +
  texto neutro), tags (chips), última atividade/erro. Paginação.
- **Filtros (topo):** por **tipo**, **status**, **tag**, e **busca** textual. Contadores.
- **Ações por linha:** pausar/retomar (toggle), editar, excluir, taguear inline.
- **Ações em lote:** checkbox de seleção + barra de ações (pausar/retomar/taguear/excluir).
- **Nova fonte (wizard type-first):** passo 1 escolhe o **tipo** (cards dos kinds do
  registry); passo 2 mostra o **form daquele tipo** (campos vindos de `source-kinds`);
  salva via o endpoint do tipo. Editar reusa o mesmo form.
- **Estados:** empty state por filtro, toasts de sucesso/erro, confirm de exclusão,
  badge de `error` com `last_error` no hover.
- **⌘K (opcional, polish):** "Nova fonte", "Pausar fonte…", saltar pra um tipo.

Relação com **Flows**: Flow = template da lane (coletar→gate→…); **Fonte = instância** que
alimenta uma lane. Telas separadas, por isso o "isolar no menu".

---

## 5. Plano de fatias (cada uma vira um prompt do Claude Code)

| # | Fatia | Entrega | Toca |
|---|---|---|---|
| 0 | **Fundação de dados** | Migrações aditivas (`tags`+`display_name`) em `target_channels`/`playlists`/`podcast_feeds`/`feed_sources` + view `sources_v` (normaliza `active`/`enabled`→status) + `email_sources` net-new (courier) + garantir coletores filtram o flag. | harvest, dial, feed (DDL por dono) + courier |
| 1 | **Surface — leitura** | `GET /v1/source-kinds` (registry) + `GET /v1/sources` (filtros/busca/paginação/contadores) + `GET /v1/sources/{id}` + espelho MCP `rara_list_sources`. | rara-core (surface) |
| 2 | **Surface — escrita + lote** | `POST/PATCH/DELETE /v1/sources...` por kind + `pause`/`resume` + `bulk`. Validação por registry, idempotente, fail-closed. | rara-core (surface) |
| 3 | **Console — nav + lista (read)** | Item de menu "Fontes" + tela lista (tabela, filtros tipo/status/tag/busca, badges, paginação) consumindo a BFF. | rara-console |
| 4 | **Console — wizard CRUD** | "Nova fonte" type-first dirigido pelo registry, editar, excluir (confirm), pausar/retomar inline, tags inline. | rara-console |
| 5 | **Console — lote + polish** | Seleção múltipla, barra de ações em lote, empty states, toasts, ⌘K opcional. | rara-console |
| 6 | **Verificação / E2E** | Smoke por tipo (criar→pausar→editar→excluir), provar que **fonte pausada não é ingerida**, CodeRabbit, deploy. | tudo |

Ordem de risco: #0 é a mais sensível (DDL multi-app + coletores respeitando pausa) — fazer
e validar isolada antes de seguir. #1→#2 destravam o front. #3→#5 são só console.

### Dependências
`#0 → #1 → #2 → #3 → #4 → #5 → #6` (em série; #3 pode começar mockado contra a BFF da #1
assim que #1 estiver verde).

---

## 6. Verificação contra o código — ✅ FEITA (repo montado, 24/jun)

Tudo confirmado lendo o repo `/Users/bardi/Projects/Github/rara`:

1. ✅ **Coletores filtram o flag** (pausa já é real hoje):
   - harvest `SELECT ... FROM target_channels WHERE active = true` (rara-harvest/main.go:124)
   - dial `... FROM podcast_feeds WHERE active = true` (rara-dial/main.go:280)
   - feed `... FROM feed_sources WHERE enabled = true` (rara-feed/main.go:946)
2. ✅ **Já existe CRUD de fonte na surface** pra podcast (`/v1/sources/podcast`, GET/POST/PUT
   em rara-core/surface.go:624-626) — o core escreve a tabela do coletor; padrão a generalizar.
3. ✅ **Core já escreve tabela de coletor** (UpsertPodcastFeed) — `sources_v` pode morar no
   core sem problema; a BFF consome via surface.
4. ✅ **Email hoje = 1 env** `GMAIL_QUERY` (default `newer_than:30d`, rara-courier/main.go:68);
   completo (N regras) = mover isso pra `email_sources` e iterar as regras.
5. ✅ **Console NÃO tem tela de fontes** ainda → fatias #3-5 são UI nova sobre a surface.
6. ⏳ Único item aberto (não bloqueia): como o Bright Data do clip mira alvos (futuro
   `linkedin_sources`). LinkedIn segue fora do MVP.

> Migrations de cada tabela (donos): `rara-harvest/migrations/001` (target_channels,
> playlists), `rara-dial/migrations/001` (podcast_feeds), `rara-feed/migrations/001`
> (feed_sources), `rara-courier/migrations/001` (emails → adicionar `email_sources` aqui).

---

## 7. Regras de operação (herdadas do fluxo Workers)

- Cada fatia = **prompt auto-contido**; 1ª linha = título `CONSOLE-FONTES-#n` (vira o nome
  da sessão). Branch dedicada `feat/console-fontes-N-slug`.
- DoD: commit → PR → CI verde → CodeRabbit (corrigir **todos** os apontamentos) → avisar o
  Renato e **PAUSAR** (ele faz o merge) → após aprovação, acompanhar deploy → resumo final.
- Cowork **não edita** código de rara-core/console/coletores — só organiza e valida (read-only).
- DDL: cada migração no **repo dono** da tabela (não centralizar no core).

---

## 8. Verificação / E2E (fatia #6) — ✅ MVP ENTREGUE (25/jun/2026)

Branch `feat/console-fontes-6-verify`. Evidências: queries read-only no Neon prod
(`sweet-math-91321704`) + leitura do código de produção + suítes de teste.

### Checklist

| # | Item | Resultado | Evidência |
|---|---|---|---|
| 1 | **Smoke por tipo** (5 lanes de fonte) | ✅ | `sources_v` retorna os kinds normalizados (youtube_channel, youtube_playlist, podcast, rss/hn/html→news, email). Write-path (POST/PATCH/DELETE/pause/resume) coberto por `rara-core/source_writes_test.go` + `rara-console/sources_write_test.go` (137 + 81 testes verdes). |
| 2 | **Pausa é real (CRÍTICO)** | ✅ | Mesmo booleano nos 3 lugares: UI flipa `active`/`enabled` → view deriva `status` via `CASE WHEN active/enabled` → coletor filtra `WHERE active/enabled=true`. Cross-check runtime **bate em todas as lanes**. |
| 3 | **Filtros / lote** | ✅ | `ListSources` lê `sources_v` com `kind`/`status`/`$N = ANY(tags)`/`ILIKE` (display_name OR config_summary) + GROUP BY p/ badges + LIMIT/OFFSET — tudo parametrizado. Bulk (pause\|resume\|tag\|untag\|delete) em `surface.go:1261`. |
| 4 | **Email respeita pausa+filtros** | ✅ | `courier ListEmailSources`: `FROM email_sources WHERE enabled=true AND deleted_at IS NULL`, itera as N regras compondo a query Gmail (`rara-courier/main.go:432`). Pausar regra (`enabled=false`) → não coleta dela. |
| 5 | **Segurança** | ✅ | BFF guarda `SURFACE_TOKEN` server-side (`mustEnv`), seta `Authorization: Bearer` no servidor (`console/main.go:54`); SPA nunca vê token nem URL da surface. Core fail-closed: `crypto/subtle.ConstantTimeCompare`, token vazio recusa tudo (`surface.go:979-988,1517`). Nenhum vazamento de token no bundle web. |

### Prova "pausa é real" (runtime, dados reais de prod)

```
                 coletor (WHERE flag=true, deleted_at IS NULL)  ==  sources_v status='active'
youtube_channel  target_channels:  71                           ==  71   (e 32 paused == 32 skip)
youtube_playlist playlists:        11                           ==  11
podcast          podcast_feeds:     1                           ==   1
news (feed)      feed_sources:     22                           ==  22
email            email_sources:     2                           ==   2
```

A view (`pg_get_viewdef`) confirma: cada branch deriva `status` do **mesmo** flag que o
coletor filtra, e todas filtram `deleted_at IS NULL` — soft-delete some da view E da
descoberta. Logo, pausar (flag=false) remove a fonte do conjunto de descoberta **exatamente**.

### Smoke do ciclo de vida por tipo (live, em branch efêmero do Neon)

Rodado num branch descartável (`e2e-fontes-6-verify`, já deletado) — **zero impacto em
prod**. Uma fonte sintética por tabela (tag `e2e6`), exercitando o CRUD completo e medindo
em cada passo: `sources_v.status` + a contagem da query de descoberta de **cada** coletor
(`collected`).

| Passo | youtube_channel | youtube_playlist | podcast | rss(feed) | email |
|---|---|---|---|---|---|
| **Criar** | view=active, collected=1 | active, 1 | active, 1 | active, 1 | active, 1 |
| **Editar** (nome+tag) | refletiu na view | ✓ | ✓ | ✓ | ✓ |
| **Pausar** | view=paused, **collected=0** | paused, **0** | paused, **0** | paused, **0** | paused, **0** |
| **Retomar** | collected=1 | 1 | 1 | 1 | 1 |
| **Excluir** (soft) | fora da view, **collected=0** | **0** | **0** | **0** | **0** |

→ **Pausada e excluída não são coletadas, confirmado por tipo** com a query real de cada
coletor.

Queries de coletor verificadas no código: harvest `main.go:124`, dial `main.go:280`,
feed `main.go:946`, shelf `filterActive`/`loadInactivePlaylists` (main.go:38-201),
courier `main.go:432`.

### Gotchas
- **Nenhum bug encontrado** — view ↔ coletores 100% consistentes em todas as lanes.
- `tags` ainda sem uso em prod (0 fontes taguadas); coluna e filtro `= ANY(tags)` existem e
  têm cobertura de teste — só não exercitados em dados reais ainda.
- E2E HTTP ao vivo (subir core-job + console) **não** foi feito: redundante. Os handlers reais
  de escrita já são testados sobre `MockDatabase` (espelha o SQL) e o round-trip
  write→view→coletor está provado pelo cross-check de contagens em dados reais.
