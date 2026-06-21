-- 019_rename_providers.sql
-- P1b: rename all 18 provider placements to <worker>-<runtime> taxonomy.
--
-- Changes in one atomic transaction:
--   1. providers.description — new TEXT column for the human-readable UI label.
--   2. item_steps FK       — drop then re-add WITH ON UPDATE CASCADE so PK renames cascade.
--   3. Renames             — all 18 placements in one UPDATE … FROM VALUES.
--   4. Worker + description — all 18 placements in one UPDATE … FROM VALUES.
--   5. Env rewrite         — SIFT_PROVIDER / DISTILL_PROVIDER identity keys via to_jsonb(name).
--   6. Fallback rewrite    — rebuild routing_policies.fallback arrays with new names.
--
-- Idempotent: WHERE guards on old names mean a re-run finds nothing and is a no-op.
-- Pre-condition: no item_steps rows in status running/assigned (drain before applying).

BEGIN;

-- 1. Add description column (idempotent).
ALTER TABLE providers ADD COLUMN IF NOT EXISTS description TEXT;

-- 2. Re-add FK with ON UPDATE CASCADE.
--    NOT VALID skips the full-table scan so ADD CONSTRAINT holds ACCESS EXCLUSIVE only briefly;
--    VALIDATE CONSTRAINT then checks existing rows under the lighter SHARE UPDATE EXCLUSIVE lock.
ALTER TABLE item_steps DROP CONSTRAINT IF EXISTS item_steps_assigned_provider_fkey;
ALTER TABLE item_steps
    ADD CONSTRAINT item_steps_assigned_provider_fkey
    FOREIGN KEY (assigned_provider) REFERENCES providers(name)
    ON UPDATE CASCADE
    NOT VALID;
ALTER TABLE item_steps VALIDATE CONSTRAINT item_steps_assigned_provider_fkey;

-- 3. Renames — all 18 placements in one statement.
--    Each old name maps to exactly one new name; no intermediate collision possible
--    because PostgreSQL evaluates all WHERE conditions against the pre-update state.
UPDATE providers AS p
SET name = v.new_name
FROM (VALUES
    ('harvest',           'harvest-cloud'),
    ('shelf',             'shelf-cloud'),
    ('dial',              'dial-cloud'),
    ('feed',              'feed-cloud'),
    ('courier',           'courier-cloud'),
    ('clip',              'clip-cloud'),      -- was 'brightdata-linkedin' before migration 012
    ('manual-inbox',      'stash'),
    ('asr-youtube',       'caption-mac'),
    ('asr-direct-audio',  'echo-cloud'),
    ('extrair-news',      'glean-cloud'),
    ('extrair-email',     'winnow-cloud'),
    ('extrair-linkedin',  'scrub-cloud'),
    ('gate-barato',       'sift-cloud'),
    ('gate-barato-local', 'sift-vpc'),
    ('gate-rico',         'assay-cloud'),
    ('gate-rico-local',   'assay-vpc'),
    ('distill',           'distill-cloud'),
    ('distill-local',     'distill-vpc')
) AS v(old_name, new_name)
WHERE p.name = v.old_name;

-- 4. Set worker codenames and descriptions — all 18 placements in one statement.
UPDATE providers AS p
SET worker = v.worker, description = v.description
FROM (VALUES
    ('harvest-cloud', 'harvest', 'Coletor de canais (YouTube)'),
    ('shelf-cloud',   'shelf',   'Coletor de playlists (YouTube)'),
    ('dial-cloud',    'dial',    'Coletor de podcasts (RSS)'),
    ('feed-cloud',    'feed',    'Coletor de feeds (RSS/HN/HTML)'),
    ('courier-cloud', 'courier', 'Coletor de e-mail (Gmail)'),
    ('clip-cloud',    'clip',    'Coletor de posts (LinkedIn)'),
    ('stash',         'stash',   'Submissão manual (LinkedIn)'),
    ('caption-mac',   'caption', 'Transcritor — vídeo YouTube (Mac)'),
    ('echo-cloud',    'echo',    'Transcritor — áudio/podcast'),
    ('glean-cloud',   'glean',   'Normalizador — feed (artigo)'),
    ('winnow-cloud',  'winnow',  'Normalizador — e-mail'),
    ('scrub-cloud',   'scrub',   'Normalizador — post LinkedIn'),
    ('sift-cloud',    'sift',    'Filtro — metadados (barato)'),
    ('sift-vpc',      'sift',    'Filtro — metadados (barato)'),
    ('assay-cloud',   'assay',   'Filtro — texto completo (rico)'),
    ('assay-vpc',     'assay',   'Filtro — texto completo (rico)'),
    ('distill-cloud', 'distill', 'Destilador (LLM)'),
    ('distill-vpc',   'distill', 'Destilador (LLM)')
) AS v(name, worker, description)
WHERE p.name = v.name;

-- 5. Rewrite env JSONB identity keys — to_jsonb(name) produces the correct new placement name.
UPDATE providers
    SET env = jsonb_set(env, '{DISTILL_PROVIDER}', to_jsonb(name))
    WHERE name IN ('distill-cloud', 'distill-vpc') AND env ? 'DISTILL_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{SIFT_PROVIDER}', to_jsonb(name))
    WHERE name IN ('sift-cloud', 'sift-vpc', 'assay-cloud', 'assay-vpc') AND env ? 'SIFT_PROVIDER';

-- 6. Rewrite routing_policies.fallback arrays to new names.
--    Covers all 18 renamed providers so stale names in any fallback row are converted rather
--    than passed through unchanged (which would violate the FK or route to a missing provider).
UPDATE routing_policies
    SET fallback = (
        SELECT COALESCE(jsonb_agg(
            CASE elem #>> '{}'
                -- gates (cloud + VPC)
                WHEN 'gate-barato-local' THEN '"sift-vpc"'::jsonb
                WHEN 'gate-rico-local'   THEN '"assay-vpc"'::jsonb
                WHEN 'gate-barato'       THEN '"sift-cloud"'::jsonb
                WHEN 'gate-rico'         THEN '"assay-cloud"'::jsonb
                -- distill (cloud + VPC)
                WHEN 'distill-local'     THEN '"distill-vpc"'::jsonb
                WHEN 'distill'           THEN '"distill-cloud"'::jsonb
                -- transcribers
                WHEN 'asr-youtube'       THEN '"caption-mac"'::jsonb
                WHEN 'asr-direct-audio'  THEN '"echo-cloud"'::jsonb
                -- extractors
                WHEN 'extrair-news'      THEN '"glean-cloud"'::jsonb
                WHEN 'extrair-email'     THEN '"winnow-cloud"'::jsonb
                WHEN 'extrair-linkedin'  THEN '"scrub-cloud"'::jsonb
                -- collectors
                WHEN 'harvest'           THEN '"harvest-cloud"'::jsonb
                WHEN 'shelf'             THEN '"shelf-cloud"'::jsonb
                WHEN 'dial'             THEN '"dial-cloud"'::jsonb
                WHEN 'feed'              THEN '"feed-cloud"'::jsonb
                WHEN 'courier'           THEN '"courier-cloud"'::jsonb
                WHEN 'clip'              THEN '"clip-cloud"'::jsonb
                WHEN 'manual-inbox'      THEN '"stash"'::jsonb
                ELSE elem
            END
        ), '[]'::jsonb)
        FROM jsonb_array_elements(fallback) AS elem
    )
    WHERE fallback != '[]'::jsonb;

COMMIT;
