# LiteLLM gateway (rara 2.0)

A self-hosted, OpenAI-compatible proxy in front of distill's curation models (and the
future LLM-judge curation gates). It is the **models** layer of the anti-lock-in posture in
[`ARCHITECTURE-2.0.md`](../../ARCHITECTURE-2.0.md): distill speaks one dialect (OpenAI chat
completions) to a gateway *we* own, and which concrete model answers — Claude, Gemini, Groq,
or a local model — is a gateway config value, not a distill code change.

## Why

Before 2.0 each engine in `rara-distill` carried a hardcoded vendor endpoint (Gemini,
Anthropic, Groq). The `litellm` engine collapses those into one OpenAI-compatible call:

```
distill  ──OpenAI chat/completions──▶  LiteLLM gateway  ──▶  Claude | Gemini | Groq | local
```

Swapping the upstream model is editing [`config.yaml`](./config.yaml) (or changing
`LITELLM_MODEL`), with **no distill change** and **no recipe re-hash** — the engine string is
stored as provenance but is intentionally not part of `recipe_sha256`, so a model swap never
invalidates the corpus.

## Run it

```sh
# VPC, resident (always-on). Keys live in the gateway's environment, never in distill.
export ANTHROPIC_API_KEY=...   # whichever upstreams config.yaml references
export LITELLM_MASTER_KEY=sk-... # optional; omit to run keyless on a private network
litellm --config rara-distill/litellm/config.yaml --port 4000
```

## Point distill at it

```sh
CURATE_ENGINE=litellm \
LITELLM_BASE_URL=http://localhost:4000/v1 \
LITELLM_MODEL=claude-sonnet-4-6 \
LITELLM_API_KEY=$LITELLM_MASTER_KEY   # omit if the gateway is keyless
```

`LITELLM_BASE_URL` is the gateway's OpenAI-compatible base; distill appends
`/chat/completions`. `LITELLM_MODEL` is one of the `model_name` aliases in `config.yaml`
(default `claude-sonnet-4-6`). The curation logic (patterns/contexts/strategies, the JSON
output schema, the session chain) is unchanged — only the model-call seam moved.
