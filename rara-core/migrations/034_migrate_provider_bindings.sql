-- 034_migrate_provider_bindings: rewrite bare-alias LITELLM_MODEL values in providers.env
-- to concrete kind/model strings. Workers must send the full upstream to the gateway (CORR-INFER #5).
-- Idempotent: WHERE only matches the 4 legacy aliases; kind/model values contain '/' so they never match.
-- Alias→upstream map from SPIKE CORR-INFER #0 §Q6.

UPDATE providers
SET env = env || jsonb_build_object('LITELLM_MODEL',
    CASE env->>'LITELLM_MODEL'
        WHEN 'groq-llama'    THEN 'groq/llama-3.3-70b-versatile'
        WHEN 'groq-fast'     THEN 'groq/llama-3.1-8b-instant'
        WHEN 'gemini-flash'  THEN 'gemini/gemini-2.5-flash-lite'
        WHEN 'deepseek-chat' THEN 'deepseek/deepseek-v4-flash'
    END
)
WHERE env->>'LITELLM_MODEL' IN ('groq-llama', 'groq-fast', 'gemini-flash', 'deepseek-chat');
