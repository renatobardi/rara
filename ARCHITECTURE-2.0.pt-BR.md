# rara 2.0 — Solução (fonte única da verdade)

Documento mestre da arquitetura 2.0 do `rara`. Reflete **todas as decisões finais** — qualquer
contradição em docs antigos foi removida nesta consolidação. Os docs de detalhe (linkados abaixo)
aprofundam cada parte sem divergir deste.

> **Status:** o motor (`rara-core`) está **code-complete** (7 fases de build, validadas, no `main`;
> schema migrado no Neon). O que falta é a **reestruturação bridge-total + o deploy** — ver §12.

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

Docs do 1.0 (`ARCHITECTURE.md`, `DATABASE_SCHEMA.md`, `INFRASTRUCTURE.md`) ficam como histórico do
sistema legado.

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

> **Regra de host (não negociável):** **always-on → VPC Oracle (systemd); on_demand → Cloud Run Jobs.**
> O `rara-core` (reconciler + surface) é always-on → mora na VM (systemd, custo marginal ~zero, a VM já é
> paga). Os workers (coletores, gates, glean, scribe-direct, distill) são on_demand → Cloud Run Jobs
> (scale-to-zero, ~grátis ocioso). O `hone` é cron → Cloud Run Job + Scheduler.
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
  `dial`(podcast) `courier`(email) `clip`(linkedin) · workers `scribe`(transcrever) `glean`(extrair)
  `sift`(gates) `distill`(destila) `hone`(revise/aprendizado). Metáfora: colher → peneirar →
  destilar, e afiar o gosto. **Nomes de capability no banco não mudam** — só o nome do app é
  evocativo.

## 6. Curadoria: dois portões + aprendizado

- **Dois portões cost-aware:** `gate_barato` em metadata (antes de transcrever), `gate_rico` no
  texto (antes de destilar). Cada um é a capability do app **`sift`**.
- **Cascata barato→caro:** regras (allow/deny) → match de `interest_profile` → LLM-judge só no meio
  duvidoso. Resultado: keep → avança; drop → `filtered`; **defer → quarentena** (combate cold-start).
- **Aprendizado (`hone`):** reescrita **híbrida** do `interest_profile` — motor determinístico decide
  o estruturado (Wilson lower-bound, tetos por revisão); LLM escreve só a narrativa. Nova versão é
  `proposed` (append-only) e **só entra após aprovação humana** (versão ativa ≠ última). Gatilho:
  cadência + limiar. Sinais: thumbs nas distillations + revisão da quarentena (+ KURA implícito,
  adiado).

## 7. Inferência & custo

Duas camadas (detalhe em [INFERENCE-ROUTING](./INFERENCE-ROUTING.pt-BR.md)):

- **Router do rara = ONDE** (host): pros workers de LLM (`sift`, `distill`), a cadeia de custo é
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

- **`asr-youtube`** (`requires: residential`): yt-dlp baixa o áudio do YouTube — bloqueado em IPs
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
| **VPC Oracle** (always-on) | `core` (orquestra + superfície + console) + `hone` (cron) + Tailscale |
| **Mac** (residente) | `scribe`/asr-youtube, `sift`-local, `distill`-local, Ollama, CLI |
| **GCP Cloud Run** (on_demand) | todos os coletores + `scribe`/asr-direct + `glean` + `sift`/distill de terceiro + LiteLLM |
| **Neon** | estado · config · domínio (de tudo) |

Matriz com ondas em [DEPLOY-MATRIX](./DEPLOY-MATRIX.pt-BR.md); roteiro em
[DEPLOY-PHASES](./DEPLOY-PHASES.pt-BR.md). **Marco de "começar a usar": fim da onda 2** (podcast
público, 100% Cloud Run, sem depender do Mac).

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

## 12. Estado atual & próximos passos

- **Pronto (code):** 7 fases de build do `rara-core` no `main`, validadas; schema no Neon.
- **A fazer (a reestruturação bridge-total + deploy):**
  - **P1a** — extrair o SDK `rara-addon` + corrigir o claim por `(capability, assigned_provider)`.
  - **P1b** — Activators reais (Cloud Run `run` + poke no tailnet).
  - **P1c** — separar as capacidades em apps (nomes do §5), usando o SDK.
  - **P2–P7** — imagem/CI, VPC+Tailscale+seed, inferência (LiteLLM por host + shim CLI + Ollama),
    Cloud Run jobs, Mac, acender as raias. Depois → **console** (C0…).
