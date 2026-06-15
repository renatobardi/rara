# rara 2.0 — Matriz de Deploy (app × host × onda)

Onde cada app roda e em que **onda** (prioridade de execução prevista). O número na célula é a
ordem de deploy; célula vazia = a app não roda ali. Quando uma app roda em dois hosts, cada um tem
sua onda. Companheiro de [DEPLOY-PHASES.pt-BR.md](./DEPLOY-PHASES.pt-BR.md) e
[ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md).

| App | VPC Oracle | GCP Cloud Run | Macbook |
|---|:---:|:---:|:---:|
| **core** — orquestra + surface | **1** | — | — |
| **dial** — podcast (RSS de áudio) | — | **2** | — |
| **scribe** — transcrever | — | **2** · asr-direct-audio | **3** · asr-youtube |
| **sift** — curadoria (gate_barato/rico) | — | **2** · terceiro | **3** · `*-local` |
| **distill** — destila | — | **2** · terceiro | **3** · distill-local |
| **harvest** — canais YouTube | — | **3** | — |
| **shelf** — playlists YouTube | — | **3** | — |
| **courier** — email (Gmail) | — | **4** | — |
| **glean** — extrair (email/linkedin) | — | **4** | — |
| **feed** — news | — | **4** | — |
| **clip** — linkedin | — | **4** | — |
| **hone** — aprendizado (revise) | **5** | — | — |
| *infra — LiteLLM* | — | **2** | — |
| *infra — Ollama* | — | — | **3** |
| *infra — Tailscale* | **1** | — | **1** (Mac entra na onda 3) |

## Ondas — caminho mais curto pra usar

| Onda | O que sobe | Marco |
|---|---|---|
| **1 — cérebro** | `core` (VPC) + Tailscale | orquestrador + superfície vivos; nada processa ainda |
| **2 — 1ª raia usável** | LiteLLM + `dial` + `scribe`(nuvem) + `sift`(nuvem) + `distill`(nuvem) | **podcast público ponta a ponta — já dá pra usar**, sem depender do Mac |
| **3 — Mac entra** | Ollama + `scribe`(Mac) + `sift`(local) + `distill`(local) + `harvest` + `shelf` | YouTube ponta a ponta + base do conteúdo privado |
| **4 — raias restantes** | `courier` + `glean` + `feed` + `clip` | email, extração, news e linkedin no ar |
| **5 — aprendizado** | `hone` (cron na VPC) | o loop de feedback fecha (só faz sentido com feedback acumulado) |

## Roteamento de LLM (custo): Mac primeiro

A matriz acima é **placement** (onde a app *pode* rodar). O **custo** do LLM é decidido por uma
camada à parte — a `routing_policy` (ver [INFERENCE-ROUTING.pt-BR.md](./INFERENCE-ROUTING.pt-BR.md)):
para `sift` e `distill`, o router prefere **Mac (assinatura CLI, ~grátis) → VPC (Ollama) → Cloud Run
(API paga)**. Mac acordado → barato; Mac dormindo → fallback. É config no console, não mexe no
placement. No bring-up (onda 2, sem Mac) o smoke usa Cloud Run/API; quando o Mac entra (onda 3) a
policy flipa pra Mac-first.

## Por que essa ordem

- **A raia toda-nuvem (podcast público) vem primeiro** porque não depende do Mac estar acordado —
  é o jeito mais rápido de ver o pipeline inteiro funcionando de verdade.
- **O Mac (onda 3) destrava duas coisas de uma vez:** o áudio do YouTube (que exige IP residencial)
  e o processamento **privado** (gates/distill local com Ollama, pro email não tocar terceiro).
- **`hone` é o último** de propósito: reescrever o perfil sem feedback acumulado seria um no-op.
- Regra geral: **`core` antes de tudo** (sem orquestrador ninguém é atribuído/acordado); **LiteLLM
  antes dos workers de LLM** (sift/distill de terceiro dependem dele); **Ollama antes dos `*-local`**.
