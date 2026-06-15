# rara 2.0 — Roteamento de inferência (onde × qual modelo × custo)

Como o trabalho de LLM é roteado pra ficar **barato por padrão**, com resiliência. Companheiro de
[DEPLOY-MATRIX.pt-BR.md](./DEPLOY-MATRIX.pt-BR.md) e [ADDON-CONTRACT.pt-BR.md](./ADDON-CONTRACT.pt-BR.md).

## Duas camadas que compõem

- **Router do rara — ONDE roda** (qual host/provider). Ordena por saúde/custo/constraint/fallback:
  **Mac → VPC → Cloud Run**. É o que dá "Mac primeiro; se dormir, cai pro próximo".
- **LiteLLM — QUAL modelo** a chamada usa (por host). Cada worker aponta pra um endpoint LiteLLM e
  escolhe o modelo (e uma cadeia de modelos) ali. É o "LiteLLM em todos, escolho a LLM que quiser".

O rara escolhe o host; dentro do host, o LiteLLM escolhe o modelo.

## A cadeia de custo (capacidades de LLM: sift, distill)

| Tier | Host | Modelo (via LiteLLM) | Custo | Qualidade |
|---|---|---|---|---|
| 1 (preferido) | **Mac** | `claude-cli`/`gemini-cli` (assinatura) | ~grátis | boa |
| — | **Mac** | Ollama local (se quiser truly-local) | grátis | média |
| 2 | **VPC** | Ollama-VPC | grátis | fraca (CPU ARM) |
| 3 (fallback) | **Cloud Run** | API paga (Anthropic/Gemini) | $$ | melhor/rápida |

Um app (`sift`, `distill`) tem **um provider por host** (`sift-mac`, `sift-vpc`, `sift-cloud`), e a
`routing_policy` ordena Mac → VPC → Cloud Run. **Mac acordado → Mac (barato); Mac dormindo → o
router pula pro próximo healthy.** Tudo editável no console (providers, custos, fallback, e o modelo
no LiteLLM).

## A assinatura CLI (best-effort)

A assinatura (Claude Pro/Max, Gemini) **só funciona no Mac** (login de consumidor, não API key) e o
LiteLLM não fala com ela nativamente. Encaixa assim: um **shim** expõe a CLI como endpoint local
OpenAI-compatible; o LiteLLM-do-Mac trata `claude-cli` como mais um modelo.

**Postura (decidida): best-effort + API fallback.** A CLI é um **tier barato**, não o caminho que
sustenta o sistema. Riscos reais: rate-limit (escala interativa, não de lote), instabilidade de
auth, e zona cinza de ToS (planos de consumidor são pra uso interativo; automação é via API). O
router **cai pra Ollama/API** quando a CLI limita — então nunca se perde correção; no pior caso paga
a API no excedente. Não arquitetar contando que a assinatura aguente volume.

## Privacidade do email (relaxada)

Decisão: **email pode usar a CLI/API** (você confia na Claude/Gemini com ele). Então a constraint
dura de sensibilidade **não força mais self-host pro email** — ele é custo-roteado como o resto. O
**mecanismo** de sensibilidade continua disponível (marca `sensitivity` num item/raia e o router
volta a barrar terceiro) pra quando você quiser blindar algo específico. Consequência: os providers
`*-local` deixam de ser obrigação de privacidade e viram só **tier de custo** (grátis quando o Mac
está acordado).

## Impacto no deploy

- **Matriz/placement não muda** — `sift`/`distill` já deployam em Mac + Cloud Run (+ VPC opcional). O
  que muda é a **routing_policy** (Mac-first), que é **config**, não código.
- **Bring-up vs steady-state:** na onda 2 (sem Mac) o smoke-test usa Cloud Run/API (prova o
  pipeline). Quando o Mac entra (onda 3), a policy flipa pra Mac-first e o grosso do LLM vai pro
  barato; Cloud Run vira fallback.
- **Fase de inferência (P4) ganha:** o shim CLI→endpoint local no Mac, um LiteLLM por host
  (Mac/VPC/Cloud Run) com os modelos configurados, e os provider rows `*-mac`/`*-vpc`/`*-cloud` com
  custo/qualidade/fallback semeados.
- **`hone`** (revise, na VPC) usa LLM de forma mínima (só a narrativa) — vai por LiteLLM-VPC
  (Ollama ou API); custo desprezível, não vale roteá-lo pro Mac.
