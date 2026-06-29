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

-- owner_id IS NULL = system-owned. Names are unique only per owner, so the seed
-- pins owner_id NULL explicitly and the model JOIN matches only the system row —
-- a future tenant provider sharing a name never captures these aliases.
INSERT INTO llm_providers (owner_id, name, kind, enabled) VALUES
    (NULL, 'groq',     'groq',     false),
    (NULL, 'gemini',   'gemini',   false),
    (NULL, 'deepseek', 'deepseek', false)
ON CONFLICT (owner_id, name) WHERE deleted_at IS NULL DO NOTHING;

-- llm_models was dropped in migration 035. This seed predates that, so guard the insert behind a
-- to_regclass() existence check: on a fresh replay (031 creates the table → 032 seeds it → 035
-- drops it) the branch runs; against a DB where 035 already applied (table gone) it is a no-op.
-- PL/pgSQL only parses the statement when the IF is true, so referencing a now-absent table never
-- errors — keeping this migration safe to re-run, per the repo's idempotency rule.
DO $$
BEGIN
    IF to_regclass('public.llm_models') IS NOT NULL THEN
        INSERT INTO llm_models (provider_id, alias, upstream, enabled)
        SELECT p.id, v.alias, v.upstream, false
        FROM (VALUES
            ('groq',     'groq-llama',    'groq/llama-3.3-70b-versatile'),
            ('groq',     'groq-fast',     'groq/llama-3.1-8b-instant'),
            ('gemini',   'gemini-flash',  'gemini/gemini-2.5-flash-lite'),
            ('deepseek', 'deepseek-chat', 'deepseek/deepseek-v4-flash')
        ) AS v(provider_name, alias, upstream)
        JOIN llm_providers p
          ON p.name = v.provider_name
         AND p.owner_id IS NULL
         AND p.deleted_at IS NULL
        ON CONFLICT (owner_id, alias) WHERE deleted_at IS NULL DO NOTHING;
    END IF;
END $$;
