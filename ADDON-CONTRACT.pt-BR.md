# rara — Contrato de Addon (workers/agentes independentes)

Decisão: **bridge total.** O `rara-core` é só o **orquestrador** (reconcile + superfície). Cada
capability é um **app independente** (repo/build/versão/linguagem próprios) que se anexa ao sistema
**só pelo contrato** abaixo — nunca por chamada direta. Companheiro de
[ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md).

## Princípio

Um **addon** é um worker/agente soberano. Ele se acopla ao `rara-core` por **duas coisas, ambas no
Neon**: a linha de `provider` (registro) e o protocolo de `item_steps` (trabalho). Nada além disso.
Isso dá isolamento total: cada addon tem seu processo, seu deploy, sua versão — e pode ser escrito
em qualquer linguagem.

> Um mesmo **app** pode servir **vários providers** por configuração/deploy (ex.: o app `gate`
> roda como `gate-barato` na nuvem e `gate-barato-local` no Mac). Codebases ≪ providers.

## Dois tipos de addon

### A. Worker por item (claim-worker)
Executa uma capability sobre um item (`transcrever`, `extrair`, `gate_*`, `destilar`, `revise`).
Segue o **protocolo de claim**:

1. **Heartbeat** — periodicamente `UPDATE providers SET heartbeat_at = now() WHERE name = $provider`.
   É o que diz ao router "estou vivo" (residentes); on_demand é isento.
2. **Claim** — numa transação:
   ```sql
   SELECT item_id, seq, capability, assigned_provider, attempt
   FROM item_steps
   WHERE capability = $cap AND assigned_provider = $provider AND status = 'pending'
   ORDER BY id
   FOR UPDATE SKIP LOCKED
   LIMIT 1;
   -- depois: UPDATE ... SET status='running', attempt=attempt+1, heartbeat_at=now()
   ```
   > Importante: o claim é por **(capability, assigned_provider)** — não só por capability. Com
   > múltiplos providers por capability (que já temos: `*-local` vs terceiro), filtrar por
   > `assigned_provider` é o que garante que um item **privado** atribuído ao `distill-local` não
   > seja pego pelo worker de terceiro. *(Hoje o `ClaimPendingStep` filtra só por capability — isso
   > precisa virar `(capability, assigned_provider)` na extração do SDK.)*
3. **Run** — lê o item / a linha de domínio (transcript, texto, metadata), faz o trabalho.
4. **Resultado** — escreve a linha de domínio (`transcripts` / `distillations` / `gate_decisions` /
   versão de `interest_profile` …) e `UPDATE item_steps SET status='done', output_ref=$id`.
5. **Falha** — re-enfileira (`status='pending'`, `heartbeat_at=NULL`) até o teto de `attempt`, ou
   `status='failed'`.
6. **Poke (opcional, ativação simétrica)** — expõe um endpoint HTTP no tailnet; ao receber poke,
   drena a fila na hora. Sem poke, o polling lento cobre.

### B. Coletor (producer)
Descobre conteúdo (`coletar`): roda independente (cron/triggered/woken), escreve a tabela de
domínio dele (`channel_videos`, `podcast_episodes`, `emails`, `linkedin_posts`…) e dá **ingest** na
spine `items` (upsert idempotente em `(lane, source_ref)`, carimba `flow_version`). **Não** claima
`item_steps` — `coletar` é auto-satisfeito pelo reconciler (o item existir já prova a coleta).

## Três formatos de peça × ativação × host

Os tipos **A** e **B** acima são os dois *addons* propriamente ditos. Somando o `hone` (um job
periódico, não-addon), o projeto tem **três formatos de peça**. Todos compartilham o mesmo Go, o
mesmo contrato Neon e a **mesma mecânica de build/deploy nos três hosts** — imagem amd64 p/ Cloud
Run; binário nativo arm64 p/ VPC e Mac; `deploy-<app>.yml` no mesmo formato; VPC = rsync+ssh+systemd;
Mac = launchd. O que **difere por formato** é só *como a peça é acionada* — e isso é intrínseco ao
papel dela, não inconsistência de arquitetura.

| Formato | Peças | Importa `rara-addon`? | Como roda | Ativação | Deploya em |
|---|---|---|---|---|---|
| **Claim-worker** | distill, sift, scribe, glean | **Sim** (`addon.Run`) | Claima `item_steps` por `(capability, provider)`; heartbeat; result | **Roteado por item** pelo reconciler: poke (residente) / Cloud Run `run` (on_demand) | **Os 3, de verdade** — mesmo binário; o host é config do provider row (`*-mac`/`*-vpc`/`*-cloud`) |
| **Producer/coletor** | dial, feed, shelf, harvest, courier, clip | Não | Varre a fonte, faz upsert na tabela de domínio, sai; o core dá `ingest` na spine | **Cadência** (Cloud Scheduler / launchd / systemd timer) — não é acordado pelo reconciler | Os 3, mas a colocação é **forçada por restrição** (ex.: harvest/scribe-youtube = Mac por IP residencial), não roteada |
| **Timer** | hone | Não | Run-once-and-exit periódico (revisa o `interest_profile`) | systemd timer | VPC por natureza (perto do core, sempre disponível) |

**Por que não dá pra unificar tudo em "claim-worker roteável":** um producer **cria** itens (descobre
vídeo/episódio/email novo) — não há `item_step` pra claimar; um claim-worker **processa** um item que
já existe. Você roteia *quem processa*, mas *quem coleta* roda por agenda. Logo o "deploy configurável
em qualquer host por regra" é **literal** nos 4 claim-workers (é exatamente onde o router
host=Mac→VPC→Cloud Run escolhe por item) e é **schedulable nos 3** nos producers/`hone`, com a
colocação decidida por custo/restrição.

**Regra fixada:** toda peça compila e deploya igual nos três hosts (build/deploy uniformes); só os
**claim-workers** têm o host escolhido *por regra do core, por item*. Producers e `hone` são
*schedulable* em qualquer host, mas sua casa é decidida por restrição (IP residencial, proximidade do
core), não por roteamento por item.

## O SDK `rara-addon` (pra não reimplementar o protocolo)

Um módulo Go pequeno, extraído do atual `worker.go` + `ClaimPendingStep` + heartbeat + poke. Um
worker Go fica:

```go
addon.Run(addon.Config{Capability: "destilar", Provider: "distill-local", DB: db},
    func(ctx, item, step) (addon.Result, error) {
        // só a lógica de domínio; claim/heartbeat/result/poke são do SDK
    })
```

Workers em **outra linguagem** implementam o protocolo de fio (o SQL acima + heartbeat + poke
HTTP). O `rara-core` não sabe nem se importa em que linguagem o addon está.

## Os apps (codebases independentes)

**Convenção de nome:** o **app** ganha um nome evocativo de palavra única, no estilo dos 1.0
(`harvest`/`scribe`/`distill`); o **provider** segue descritivo (`asr-youtube`, `distill-local`).
A cadeia lê como ofício: colher → peneirar → destilar, e afiar o gosto. Um app **serve vários
providers** por config (codebases ≪ providers).

| App | Capability → provider(es) | Tipo | Host |
|---|---|---|---|
| `rara-core` | orquestrador: reconcile + surface | — | VPC |
| `rara-harvest` | coletar → harvest (canais YT) | coletor | Cloud Run |
| `rara-shelf` | coletar → shelf (playlists YT) | coletor | Cloud Run |
| `rara-feed` | coletar → feed (news) | coletor | Cloud Run |
| `rara-dial` | coletar → podcast (RSS de áudio) | coletor | Cloud Run |
| `rara-courier` | coletar → email (Gmail) | coletor | Cloud Run |
| `rara-clip` | coletar → linkedin (manual-inbox + brightdata) | coletor | Cloud Run + superfície |
| `rara-scribe` | transcrever → asr-youtube (Mac) + asr-direct-audio (Cloud Run) | worker | Mac + Cloud Run |
| `rara-glean` | extrair → extrair-email + extrair-linkedin | worker | Cloud Run |
| `rara-sift` | gate_barato + gate_rico → terceiro + `*-local` | worker | Cloud Run + Mac |
| `rara-distill` | destilar → distill (terceiro) + distill-local | worker | Cloud Run + Mac |
| `rara-hone` | revise → interest_profile (aprendizado) | timer (run-once) | VPC |

Nomes novos: **dial** (podcast), **courier** (email), **clip** (linkedin), **glean** (extrair),
**sift** (curadoria/gates), **hone** (revise). Os 1.0 (harvest/shelf/feed/scribe/distill) **adotam o
contrato/SDK** em vez do cron+fila próprios; `scribe` e `distill` ganham um provider novo cada
(asr-direct-audio e distill-local). Os runners nativos que estavam dentro do `rara-core` (gates →
`sift`, revise → `hone`, e os coletores novos) saem pra esses apps.

> Os **nomes de capability** no banco (`coletar`/`transcrever`/`extrair`/`gate_barato`/`gate_rico`/
> `destilar`) **não mudam** — são a tarefa lógica, já no schema/seed. Só o nome do **app** é
> evocativo.

## O que o `rara-core` mantém

- **reconcile** (atribui: escreve `assigned_provider` no `item_step`, acorda on_demand / poke
  residente);
- **surface** (HTTP/MCP de estado, config e human-in-the-loop);
- e **define/versiona o contrato + o SDK**.

Nada de lógica de capability. O orquestrador nunca executa trabalho — só decide, observa e acorda.

## Impacto no deploy (reescreve o P1)

O P1 das fases de deploy deixa de ser "ajustes no core" e passa a ser **a reestruturação**:
(1) extrair o SDK `rara-addon` (com o claim por `(capability, assigned_provider)` corrigido);
(2) mover os runners nativos (gates, revise, asr-direct, extract, novos coletores) pra apps que usam
o SDK; (3) adaptar os 1.0 (harvest/shelf/feed/scribe/distill) pro contrato; (4) os Activators reais
(Cloud Run `run` + poke). A lógica de domínio é preservada — só muda de casa.
