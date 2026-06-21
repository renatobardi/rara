-- 019_rename_providers.sql
-- P1b: rename all 18 provider placements to <worker>-<runtime> taxonomy.
--
-- Changes in one atomic transaction:
--   1. providers.description — new TEXT column for the human-readable UI label.
--   2. item_steps FK       — drop then re-add WITH ON UPDATE CASCADE so PK renames cascade.
--   3. Renames             — UPDATE providers.name for all 18 placements.
--   4. Worker + description — set codename and label on each placement.
--   5. Env rewrite         — update SIFT_PROVIDER / DISTILL_PROVIDER identity keys.
--   6. Fallback rewrite    — rebuild routing_policies.fallback arrays with new names.
--
-- Idempotent: WHERE name = '<old>' guards mean a re-run finds nothing and is a no-op.
-- Pre-condition: no item_steps rows in status running/assigned (drain before applying).

BEGIN;

-- 1. Add description column (idempotent).
ALTER TABLE providers ADD COLUMN IF NOT EXISTS description TEXT;

-- 2. Re-add FK with ON UPDATE CASCADE.
ALTER TABLE item_steps DROP CONSTRAINT IF EXISTS item_steps_assigned_provider_fkey;
ALTER TABLE item_steps
    ADD CONSTRAINT item_steps_assigned_provider_fkey
    FOREIGN KEY (assigned_provider) REFERENCES providers(name)
    ON UPDATE CASCADE;

-- 3. Renames.

-- collectors: <name> → <name>-cloud
UPDATE providers SET name = 'harvest-cloud' WHERE name = 'harvest';
UPDATE providers SET name = 'shelf-cloud'   WHERE name = 'shelf';
UPDATE providers SET name = 'dial-cloud'    WHERE name = 'dial';
UPDATE providers SET name = 'feed-cloud'    WHERE name = 'feed';
UPDATE providers SET name = 'courier-cloud' WHERE name = 'courier';
UPDATE providers SET name = 'clip-cloud'    WHERE name = 'clip';     -- was 'brightdata-linkedin' before migration 012

-- LinkedIn manual surface
UPDATE providers SET name = 'stash'         WHERE name = 'manual-inbox';

-- transcribers
UPDATE providers SET name = 'caption-mac'   WHERE name = 'asr-youtube';
UPDATE providers SET name = 'echo-cloud'    WHERE name = 'asr-direct-audio';

-- extractors
UPDATE providers SET name = 'glean-cloud'   WHERE name = 'extrair-news';
UPDATE providers SET name = 'winnow-cloud'  WHERE name = 'extrair-email';
UPDATE providers SET name = 'scrub-cloud'   WHERE name = 'extrair-linkedin';

-- gates (cloud + VPC)
UPDATE providers SET name = 'sift-cloud'    WHERE name = 'gate-barato';
UPDATE providers SET name = 'sift-vpc'      WHERE name = 'gate-barato-local';
UPDATE providers SET name = 'assay-cloud'   WHERE name = 'gate-rico';
UPDATE providers SET name = 'assay-vpc'     WHERE name = 'gate-rico-local';

-- distill (cloud + VPC)
UPDATE providers SET name = 'distill-cloud' WHERE name = 'distill';
UPDATE providers SET name = 'distill-vpc'   WHERE name = 'distill-local';

-- 4. Set worker codenames and descriptions (idempotent: uses new names as WHERE key).
UPDATE providers SET worker = 'harvest', description = 'Coletor de canais (YouTube)'      WHERE name = 'harvest-cloud';
UPDATE providers SET worker = 'shelf',   description = 'Coletor de playlists (YouTube)'   WHERE name = 'shelf-cloud';
UPDATE providers SET worker = 'dial',    description = 'Coletor de podcasts (RSS)'        WHERE name = 'dial-cloud';
UPDATE providers SET worker = 'feed',    description = 'Coletor de feeds (RSS/HN/HTML)'   WHERE name = 'feed-cloud';
UPDATE providers SET worker = 'courier', description = 'Coletor de e-mail (Gmail)'        WHERE name = 'courier-cloud';
UPDATE providers SET worker = 'clip',    description = 'Coletor de posts (LinkedIn)'      WHERE name = 'clip-cloud';
UPDATE providers SET worker = 'stash',   description = 'Submissão manual (LinkedIn)'      WHERE name = 'stash';
UPDATE providers SET worker = 'caption', description = 'Transcritor — vídeo YouTube (Mac)' WHERE name = 'caption-mac';
UPDATE providers SET worker = 'echo',    description = 'Transcritor — áudio/podcast'      WHERE name = 'echo-cloud';
UPDATE providers SET worker = 'glean',   description = 'Normalizador — feed (artigo)'     WHERE name = 'glean-cloud';
UPDATE providers SET worker = 'winnow',  description = 'Normalizador — e-mail'            WHERE name = 'winnow-cloud';
UPDATE providers SET worker = 'scrub',   description = 'Normalizador — post LinkedIn'     WHERE name = 'scrub-cloud';
UPDATE providers SET worker = 'sift',    description = 'Filtro — metadados (barato)'      WHERE name = 'sift-cloud';
UPDATE providers SET worker = 'sift',    description = 'Filtro — metadados (barato)'      WHERE name = 'sift-vpc';
UPDATE providers SET worker = 'assay',   description = 'Filtro — texto completo (rico)'   WHERE name = 'assay-cloud';
UPDATE providers SET worker = 'assay',   description = 'Filtro — texto completo (rico)'   WHERE name = 'assay-vpc';
UPDATE providers SET worker = 'distill', description = 'Destilador (LLM)'                 WHERE name = 'distill-cloud';
UPDATE providers SET worker = 'distill', description = 'Destilador (LLM)'                 WHERE name = 'distill-vpc';

-- 5. Rewrite env JSONB identity keys to new placement names.
UPDATE providers
    SET env = jsonb_set(env, '{DISTILL_PROVIDER}', '"distill-cloud"')
    WHERE name = 'distill-cloud' AND env ? 'DISTILL_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{DISTILL_PROVIDER}', '"distill-vpc"')
    WHERE name = 'distill-vpc' AND env ? 'DISTILL_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{SIFT_PROVIDER}', '"sift-cloud"')
    WHERE name = 'sift-cloud' AND env ? 'SIFT_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{SIFT_PROVIDER}', '"sift-vpc"')
    WHERE name = 'sift-vpc' AND env ? 'SIFT_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{SIFT_PROVIDER}', '"assay-cloud"')
    WHERE name = 'assay-cloud' AND env ? 'SIFT_PROVIDER';

UPDATE providers
    SET env = jsonb_set(env, '{SIFT_PROVIDER}', '"assay-vpc"')
    WHERE name = 'assay-vpc' AND env ? 'SIFT_PROVIDER';

-- 6. Rewrite routing_policies.fallback arrays to new names.
--    Rebuilds each array element-by-element; unknown names pass through unchanged.
UPDATE routing_policies
    SET fallback = (
        SELECT COALESCE(jsonb_agg(
            CASE elem #>> '{}'
                WHEN 'gate-barato-local' THEN '"sift-vpc"'::jsonb
                WHEN 'gate-rico-local'   THEN '"assay-vpc"'::jsonb
                WHEN 'gate-barato'       THEN '"sift-cloud"'::jsonb
                WHEN 'gate-rico'         THEN '"assay-cloud"'::jsonb
                WHEN 'distill-local'     THEN '"distill-vpc"'::jsonb
                WHEN 'distill'           THEN '"distill-cloud"'::jsonb
                ELSE elem
            END
        ), '[]'::jsonb)
        FROM jsonb_array_elements(fallback) AS elem
    )
    WHERE fallback != '[]'::jsonb;

COMMIT;
