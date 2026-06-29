-- 032_llm_seed_catalog: cutover seed — the providers/models that currently live
-- in rara-distill/litellm/config.yaml, as placeholder rows for the operator.
--
-- Seeds vendor + alias→upstream mappings ONLY. No API keys are stored here (the
-- operator pastes each key, encrypted, in the console), and every row ships
-- disabled (enabled=false) so the reconciler ignores it until a key is added and
-- the row is enabled. Costs default to 0 — set the real per-token cost in the
-- console. See docs/INFERENCIA-CUTOVER.md for the full cutover runbook.
--
-- Idempotent: ON CONFLICT DO NOTHING preserves any operator-edited row (key,
-- enabled, cost) on re-apply, so this is safe to re-run on every merge.

INSERT INTO llm_providers (name, kind, enabled) VALUES
    ('groq',     'groq',     false),
    ('gemini',   'gemini',   false),
    ('deepseek', 'deepseek', false)
ON CONFLICT (owner_id, name) WHERE deleted_at IS NULL DO NOTHING;

INSERT INTO llm_models (provider_id, alias, upstream, enabled)
SELECT p.id, v.alias, v.upstream, false
FROM (VALUES
    ('groq',     'groq-llama',    'groq/llama-3.3-70b-versatile'),
    ('groq',     'groq-fast',     'groq/llama-3.1-8b-instant'),
    ('gemini',   'gemini-flash',  'gemini/gemini-2.5-flash-lite'),
    ('deepseek', 'deepseek-chat', 'deepseek/deepseek-v4-flash')
) AS v(provider_name, alias, upstream)
JOIN llm_providers p ON p.name = v.provider_name AND p.deleted_at IS NULL
ON CONFLICT (owner_id, alias) WHERE deleted_at IS NULL DO NOTHING;
