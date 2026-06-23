# rara 2.0 — Solução (fonte única da verdade)

Documento mestre da arquitetura 2.0 do `rara`. Reflete **todas as decisões finais** — qualquer
contradição em docs antigos foi removida nesta consolidação. Os docs de detalhe (linkados abaixo)
aprofundam cada parte sem divergir deste.

> **Status:** sistema em produção. VPC Oracle + Mac + Cloud Run ativos; todos os workers deployados.

## Índice dos docs canônicos

| Tema | Doc |
|---|---|
| **Este mestre** (visão única) | `ARCHITECTURE-2.0.pt-BR.md` |
| Contrato de addon + apps + SDK | [ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md) |
| Roteamento de inferência & custo | [INFERENCE-ROUTING.pt-BR.md](./INFERENCE-ROUTING.pt-BR.md) |
| Fases de deploy (do código ao uso) | [DEPLOY-PHASES.pt-BR.md](./DEPLOY-PHASES.pt-BR.md) |
| Matriz app × host × onda | [DEPLOY-MATRIX.pt-BR.md](./DEPLOY-MATRIX.pt-BR.md) |
| Console visual (frontend) | [CONSOLE-PLAN.pt-BR.md](./CONSOLE-PLAN.pt-BR.md) |
| Contrato rara → KURA | [KURA-CONTRACT.md](./KURA-CONTRACT.md) |
| Modelo de dados / fluxos (diagramas) | [DATA-MODEL.mermaid.md](./DATA-MODEL.mermaid.md) · [FLOWS.mermaid.md](./FLOWS.mermaid.md) |

Docs do 1.0 (`ARCHITECTURE.md`, `DATABASE_SCHEMA.md`) ficam como histórico do sistema legado.
`INFRASTRUCTURE.md` foi atualizado para o estado 2.0 atual.

---

## 1. O problema e a virada

O `rara` é o assistente pessoal de conhecimento: **coleta** (YouTube, podcasts, email, LinkedIn,
news) → **vira texto** (transcreve/extrai) → **cura** (seleciona o que interessa) → **destila** →
entrega pro **KURA** (segundo cérebro, projeto à parte).

- **1.0 = coreografia:** agentes Go independentes, cada um no seu cron, acoplados só por tabelas no
  Neon. Desacoplado, idempotente, à prova de cascata.
- **2.0 = orquestrado:** um **control plane determinístico** (`rara-core`) decide *o que roda*,
  *onde* e *se vale a pena* — sem perder o desacoplamento. **Regra de ouro: o contrato é a tabela,
  nunca a chamada direta.**

## 2. O modelo: capability · provider · flow · routing policy

Quatro conceitos de config carregam tudo ("LiteLLM um nível abaixo" — router aplicado a *onde o
trabalho roda*):

- **Capability** — tarefa lógica com contrato fixo: `coletar`, `transcrever`, `extrair`,
  `gate_barato`, `gate_rico`, `destilar`, `revise`.
- **Provider** — implementação concreta de uma capability, com tags `runtime`
  (`local|cloudrun|vpc`), `activation` (`resident|on_demand`), `cost`, `quality`, `latency`,
  `constraints` (JSONB), `enabled`, `heartbeat_at`. Adicionar worker = inserir um provider row.
- **Flow** — pipeline declarativo por raia, com toggles por passo.
- **Routing policy** — peso `custo ⇄ qualidade` + constraints duras + fallback ordenado.

## 3. O core (orquestrador) — na VPC, mandatório

`rara-core` no papel **orquestrador** roda **sempre** na VPC Oracle e é só:

- **reconciler** — loop level-triggered (estilo controller do K8s): observa flows + items vs
  item_steps + saúde dos providers, e age (atribui, acorda, roteia keep/drop/defer);
- **superfície** — HTTP REST + adaptador **MCP** (estado, config, human-in-the-loop), só no tailnet;
- **console visual** co-locado (ver §11).

O core **não executa trabalho de domínio** — só decide, observa e acorda. (`core` e `core console`
na VPC são mandatórios.)

> **Regra de host:** **always-on → VPC Oracle (systemd); on_demand → VPC Oracle via docker run (primário) + Cloud Run Jobs (fallback).**
> O `rara-core` (reconciler + surface) é always-on → mora na VM (systemd, custo marginal ~zero, a VM já é
> paga). Os workers (coletores, gates, glean, scribe-direct, distill) são on_demand — executam como
> containers Docker no VPC via `rara-runner agent` (`docker run --pull=always`) com **Cloud Run como
> fallback ordenado**: se o VPC não está disponível, o dispatcher rota para o `*-cloud` placement.
> O `hone` é cron → Cloud Run Job + Scheduler (ainda sem placement VPC). `caption` fica no Mac
> (IP residencial, sem fallback). Cloud Run não cobre mais o compute primário dos workers — o custo
> de execução foi zerado ao mover pro VPC (VM Oracle já é paga).
>
> **⚠️ Armadilha (já aconteceu — commit `b517553`, W1):** é tentador deployar o `core` como **Cloud Run
> Service** porque (a) o activator fica *keyless* (o token de `jobs:run` vem do metadata server do GCP de
> graça) e (b) "tudo já está no Cloud Run". **NÃO faça.** Um Cloud Run Service always-on (`min-instances=1`
> + `--no-cpu-throttling`, necessário pro ticker de background rodar) custa ~US$40-50/mês — exatamente o
> custo que a VPC elimina. O "atrito" do activator na VPC se resolve com **uma SA key** na VM
> (`GOOGLE_APPLICATION_CREDENTIALS`, que o `activator.go` já lê) — não mudando a casa do cérebro. Sintoma do
> desvio: o reconciler só ticka sob tráfego HTTP (CPU congelada entre requests), e/ou uma conta de Cloud
> Run crescendo no `rara-core`.

## 4. Despacho: pull universal + ativação simétrica

- **Trabalho = pull** sempre: o worker puxa do Neon com `SELECT … FOR UPDATE SKIP LOCKED`. Uniforme,
  NAT-friendly.
- **Ativação = simétrica:** o orquestrador chama `Activate(provider)` pra todo assignment — Cloud
  Run via `run` API; Mac/VPC residente via **poke HTTP no tailnet**. O poke é **best-effort** (não
  acorda um Mac dormindo); **polling lento é a rede de segurança**. Mac acordado → na hora; dormindo
  → espera/fallback.

## 5. Addons: bridge-total

**Decisão:** cada capability é um **app independente** (repo/build/versão/linguagem próprios),
anexado **só pelo contrato** (provider row + protocolo de `item_steps`). O `rara-core` é só o
orquestrador + dono do contrato/SDK. Detalhe completo em [ADDON-CONTRACT](./ADDON-CONTRACT.pt-BR.md).

- **SDK `rara-addon`** (Go) faz o miolo (claim, heartbeat, result, poke) — worker Go vira só
  `RunStep(item)`; outra linguagem implementa o protocolo de fio. O claim é por
  **`(capability, assigned_provider)`** (isola provider-a-provider).
- **Um app serve vários providers** por config (codebases ≪ providers).
- **Apps** (nome evocativo, estilo 1.0): `core` (orquestra) · coletores `harvest` `shelf` `feed`
  `dial`(podcast) `courier`(email) `clip`(linkedin) · workers `transcribe`(transcrever) `extract`(extrair)
  `gate`(gates) `distill`(destila) `hone`(revise/aprendizado). Metáfora: colher → peneirar →
  destilar, e afiar o gosto. **Nomes de capability no banco não mudam** — só o nome do app é
  evocativo.

## 6. Curadoria: dois portões + aprendizado

- **Dois portões cost-aware:** `gate_barato` (worker **`sift`**) em metadata (antes de transcrever),
  `gate_rico` (worker **`assay`**) no texto (antes de destilar). Ambos rodam no app **`rara-gate`**.
- **Cascata barato→caro:** regras (allow/deny) → match de `interest_profile` → LLM-judge só no meio
  duvidoso. Resultado: keep → avança; drop → `filtered`; **defer → quarentena** (combate cold-start).
- **Aprendizado (`hone`):** reescrita **híbrida** do `interest_profile` — motor determinístico decide
  o estruturado (Wilson lower-bound, tetos por revisão); LLM escreve só a narrativa. Nova versão é
  `proposed` (append-only) e **só entra após aprovação humana** (versão ativa ≠ última). Gatilho:
  cadência + limiar. Sinais: thumbs nas distillations + revisão da quarentena (+ KURA implícito,
  adiado).

## 7. Inferência & custo

Duas camadas (detalhe em [INFERENCE-ROUTING](./INFERENCE-ROUTING.pt-BR.md)):

- **Router do rara = ONDE** (host): pros workers de LLM (`gate`, `distill`), a cadeia de custo é
  **VPC → (Mac futuro) → Cloud Run**. Os `*-local` (VPC) têm custo menor e qualidade igual ao
  cloud (mesmo modelo), então o router os escolhe primeiro; Cloud Run entra só quando o VPC está
  offline. O slot do Mac entrará no meio quando o agente Mac for provisionado.
- **LiteLLM = QUAL modelo** (por host): cada worker aponta pra um LiteLLM e escolhe o modelo.
- **Privacidade do email: relaxada** — email pode usar CLI/API. A constraint de sensibilidade vira
  **opcional** (o mecanismo fica disponível pra blindar algo pontual; `*-local` viram *tier de
  custo*, não obrigação).

### Restrição residencial — coletores diretos

Coletores que fazem **scraping direto** de sites com bot-detection (Akamai, anti-scraping) são
**Mac-exclusivos por constraint hard**, sem fallback pra datacenter:

- **`caption`** (`requires: residential`): yt-dlp baixa o áudio do YouTube — bloqueado em IPs
  de datacenter. Roda só no Mac (`runtime=local`). O router é **fail-closed**: se o Mac estiver
  offline, o item aguarda em vez de cair pro Cloud Run/VPC (que tomariam bloqueio de qualquer forma).
  Não modelar como "preferir local + fallback cloud".
- **LinkedIn via Bright Data** (`brightdata-linkedin`, `rara-clip`): a coleta passa pelos proxies
  residenciais do Bright Data, que fazem o unblock. O IP do host não importa → **sem constraint
  residencial**; roteia normal (VPC → Mac → Cloud Run). **Não marcar como Mac-only por engano.**
- **YouTube metadata** (`harvest`, `shelf`): usa a YouTube Data API (só API key, sem scraping) →
  sem constraint de IP, prefência normal.

Regra geral: se o worker faz download direto de um site protegido, a constraint é **residencial
hard** (elimina VPC+GCP). Se passa por um proxy/API intermediária (Bright Data, CDN público), sem
constraint.

## 8. Hosts & deploy

| Host | Roda | 
|---|---|
| **VPC Oracle** (always-on + on_demand) | `core` + `console` + `runner` (dispatch + agent) + LiteLLM container; **workers on_demand executam via `docker run --pull=always`** (distill, gate/sift/assay, extract/winnow/glean/scrub, coletores harvest/shelf/dial/feed/courier/clip, echo) |
| **Mac** (residente) | `caption` (rara-transcribe, launchd, IP residencial obrigatório) |
| **GCP Cloud Run** (fallback) | fallback ordenado para todos os workers VPC; `hone` (cron, sem placement VPC); LiteLLM Service (gateway para workers cloud) |
| **Neon** | estado · config · domínio (de tudo) |

Detalhes de deploy em [INFRASTRUCTURE.md](./INFRASTRUCTURE.md).

## 9. Anti-lock-in (3 camadas)

1. **Control plane** — seu, Go fino (troca por Restate/Temporal depois sem tocar nos workers).
2. **Modelos** — atrás do LiteLLM; Claude/Gemini/Ollama trocáveis.
3. **Integração** — padrões abertos: **MCP** (tools) + **A2A** (agente-a-agente). ACP deprecado.

## 10. KURA (adiado)

O `rara` fica **agnóstico** ao KURA. Contrato (em [KURA-CONTRACT](./KURA-CONTRACT.md)): KURA **lê a
tabela `distillations`** (pull, read-only); o sinal implícito volta pela tool `rara_feedback_distillation`
(`source=kura_implicit`). Integração de fato fica pra quando o KURA amadurecer.

## 11. Console visual

Painel de operador/curador, na VPC, no mesmo tailnet (detalhe em
[CONSOLE-PLAN](./CONSOLE-PLAN.pt-BR.md)): **visual Opção B (estilo ChatGPT), Clean por padrão**;
SvelteKit embutido num Go via `embed.FS`; consome a superfície do core; acesso por Tailscale. Telas:
visão geral, pipeline, quarentena, distillations (thumbs), curadoria (perfil + regras), fontes/flows,
providers/roteamento, auditoria.

## 12. Estado de deploy e execução

### VPC Oracle (always-on, systemd, arm64 nativo)

| Serviço | Comando | Papel |
|---|---|---|
| `rara-core` | `core-job reconcile --loop` | Reconciler + superfície HTTP/MCP no Tailnet |
| `rara-console` | `console` | Painel operador: SvelteKit SPA embarcado em Go, Tailnet |
| `rara-runner-dispatch` | `rara-runner dispatch` | Lê `item_steps` + coletores vencidos; aciona por transporte: `jobs:run` (Cloud Run) ou poke HTTP (VPC agent) |
| `rara-runner-agent` | `rara-runner agent` | Daemon HTTP Tailnet → `docker run` para workers VPC-local |

Binários nativos arm64, sem Docker. Deploy: rsync + SSH + systemd (`deploy-core.yml` e `deploy-console.yml` em push a `main`; `deploy-runner.yml` é `workflow_dispatch` deliberado). LiteLLM roda como container Docker na VPC (`groq-fast` para gates, `groq-llama` para distill).

### VPC Oracle — workers on_demand (arm64, docker run --pull=always)

O `rara-runner agent` executa cada worker como container Docker com o env base de
`/etc/rara-runner/worker.env` (DATABASE_URL, LITELLM_BASE_URL, LITELLM_API_KEY) + overrides do body
(`CURATE_ENGINE`, `LITELLM_MODEL`, etc.). O dispatcher rota para o `*-vpc` placement primeiro.

| Placement(s) VPC | App | Capability |
|---|---|---|
| `harvest-vpc`, `shelf-vpc`, `feed-vpc` | harvest / shelf / feed | coletar (YouTube API, Spotify, RSS) |
| `dial-vpc` | dial | coletar (podcasts) |
| `courier-vpc` | courier | coletar (email) |
| `clip-vpc` | clip | coletar (LinkedIn via Bright Data) |
| `sift-vpc`, `assay-vpc` | gate | gate_barato (sift) / gate_rico (assay) — `LITELLM_MODEL=groq-fast` |
| `distill-vpc` | distill | destilar — `CURATE_ENGINE=litellm`, `LITELLM_MODEL=groq-llama` |
| `echo-vpc` | transcribe | transcrever (áudio via URL direta, echo) |
| `winnow-vpc`, `glean-vpc`, `scrub-vpc` | extract | extrair |

### Cloud Run Jobs (fallback, amd64, acionados pelo dispatcher)

Fallback ordenado: o dispatcher rota para `*-cloud` quando o placement VPC não está disponível.
`hone` não tem placement VPC (ainda) — roda apenas via Cloud Run Job (Cloud Scheduler diário).

| Job(s) | App | Capability |
|---|---|---|
| `rara-harvest`, `rara-shelf`, `rara-feed` | harvest / shelf / feed | coletar (YouTube API, Spotify, RSS) |
| `rara-dial` | dial | coletar (podcasts) |
| `rara-courier` | courier | coletar (email) |
| `rara-clip` | clip | coletar (LinkedIn via Bright Data) |
| `rara-gate` | gate | gate_barato (sift) / gate_rico (assay) |
| `rara-distill` | distill | destilar |
| `rara-transcribe` | transcribe | transcrever (áudio via URL direta, echo) |
| `rara-extract` (winnow / scrub / glean) | extract | extrair |
| `rara-hone` | hone | revise (Cloud Scheduler diário) |

Cloud Run **Service** `litellm`: gateway de inferência para os workers cloud (escala a zero). Workers 2.0 (gate, distill, transcribe, extract) usam imagens multi-arch (`buildx`, amd64 + arm64) — um manifest list serve Cloud Run e VPC.

### Mac (native, launchd)

`caption` (rara-transcribe): launchd `com.rara.transcribe`, diário 02:00. yt-dlp + ffmpeg nativos. IP residencial é constraint hard — sem fallback para datacenter.

### Neon (Postgres)

Endpoint direto (sem `-pooler`), pgx simple protocol. Branch-por-PR via GitHub integration. Migrations aplicadas por `database-*.yml` (BEGIN/ROLLBACK em PR; aplica no merge a `main`). `core-job seed` é idempotente e não destrutivo — preserva `runner_url`, env, heartbeats e `last_collect_at`.
