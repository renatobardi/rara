# Inferência cutover runbook (config.yaml → registry)

How to move the LLM catalog from `rara-distill/litellm/config.yaml` to the Neon
registry (`llm_providers` / `llm_models`) the console manages, and prove the
workers still run through the gateway. Companion to
[CONSOLE-INFERENCIA.pt-BR.md](../CONSOLE-INFERENCIA.pt-BR.md) (§1, §5, §6).

> **Keys never live in the repo.** The operator pastes each API key in the
> console; it is encrypted (AES-256-GCM) and only ever decrypted server-side by
> the reconciler. No key value goes in a migration, seed, log, or commit.

## What's already in place

- `store_model_in_db: true` on the gateway (#4a) — Admin API live.
- Reconciler (#4b): the **only** writer of the gateway's DB-backed model set.
  It full-syncs the *enabled* registry rows into the gateway and deletes orphans.
  `config.yaml` models are read-only to it (`db_model == false`) and never touched.
- Console `/inferencia` (#6): CRUD for providers + models.
- Seed migration `rara-core/migrations/032_llm_seed_catalog.sql` (#7) — inserts
  the three vendors and four aliases below as **disabled, keyless placeholders**.
  Because they're disabled, the reconciler ignores them until the operator acts.

## The catalog (from config.yaml)

| Provider (`kind`) | Model alias = `LITELLM_MODEL` | Upstream = `litellm_params.model` |
|---|---|---|
| `groq`     | `groq-llama`    | `groq/llama-3.3-70b-versatile` |
| `groq`     | `groq-fast`     | `groq/llama-3.1-8b-instant`    |
| `gemini`   | `gemini-flash`  | `gemini/gemini-2.5-flash-lite` |
| `deepseek` | `deepseek-chat` | `deepseek/deepseek-v4-flash`   |

Costs seed to `0`. Set the real per-token price in the console (vendor list
price at cutover, for reference — verify against current vendor pricing):

| Alias | input $/tok | output $/tok |
|---|---|---|
| `groq-llama`    | _set in UI_ | _set in UI_ |
| `groq-fast`     | _set in UI_ | _set in UI_ |
| `gemini-flash`  | _set in UI_ | _set in UI_ |
| `deepseek-chat` | _set in UI_ | _set in UI_ |

Cost is used only for spend tracking (#9), never for routing, so `0` is safe
until filled in.

## Step-by-step (operator, after #7 merges and migration 032 applies)

1. **Add keys to providers.** In `/inferencia`, open each provider (`groq`,
   `gemini`, `deepseek`), paste the vendor API key, set **enabled**. The key is
   write-only — the UI shows only `•••• last4` afterwards.
2. **Set model cost + enable.** For each of the four models, set input/output
   cost (table above) and set **enabled**. A model only syncs when **both** the
   model and its provider are enabled.
3. **Reconcile.** The always-on `core-job reconcile --loop` picks it up on the
   next pass; to force it now run `core-job reconcile-llm` once.
4. **Verify the gateway.** Confirm all four aliases are served:
   ```bash
   curl -s "$LITELLM_BASE_URL/model/info" \
     -H "Authorization: Bearer $LITELLM_MASTER_KEY" \
     | jq -r '.data[] | "\(.model_name)\t\(.model_info.db_model)"'
   ```
   Expect each alias once with `db_model = true`. (During the transition window
   you'll also see the `config.yaml` copies with `db_model = false` — see below.)
5. **Smoke a real distill.** Run `distill` against one real item through the
   gateway alias and confirm a normal completion (no auth/model-not-found error).
6. **Pause one model = remove only the registry copy.** Disable a model in the
   UI; after the next reconcile pass its `db_model = true` entry is gone from
   `/model/info`. While `config.yaml` is still loaded as the safety-net the alias
   keeps being served by the static (`db_model = false`) copy until the follow-up
   deploy — so this proves the reconciler owns the registry copy, not that the
   alias is gone. Re-enable to bring the registry copy back.

## config.yaml decision — kept as legacy safety-net, emptied in a follow-up

`config.yaml` is **not** emptied in #7. It stays as the bootstrap/safety-net so
the gateway keeps serving the four aliases through the whole cutover, including
the window before the operator has pasted keys.

The transition produces, briefly, **two** entries per alias — the `config.yaml`
static one (`db_model = false`) and the reconciled registry one (`db_model =
true`). This is **harmless**: both map the same alias to the same upstream with
the same key, so whichever LiteLLM picks behaves identically.

Once step 4–5 confirm the registry serves all four aliases, retire the duplicates
in a **small follow-up PR**: reduce `config.yaml`'s `model_list` to empty (keep
`general_settings`) and let `deploy-litellm.yml` redeploy. The gateway then boots
"empty" and the reconciler is the sole source of models. Doing this as a separate,
deliberate deploy — rather than in #7 — keeps the catalog continuously served and
makes the "registry is now authoritative" cutover a single reviewable change.
