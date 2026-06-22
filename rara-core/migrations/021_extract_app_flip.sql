-- Flip extract providers from legacy apps (extrair-news, extrair-email, extrair-linkedin) to the consolidated rara-extract
-- job/image. Workers 'glean', 'winnow', and 'scrub' cover the 3 placements:
--   glean-news, winnow-email, scrub-linkedin.
-- Idempotent: re-running is a no-op (UPDATE on already-'extract' rows changes nothing).
UPDATE providers SET app = 'extract' WHERE worker IN ('glean', 'winnow', 'scrub') AND app != 'extract';
