-- 002_restamp_recipe_engine_independent.sql
--
-- One-time data migration. Run ONCE, MANUALLY, and BEFORE deploying the binary that
-- drops the engine from the recipe hash (PR: "Make distill recipe model-independent").
--
-- WHY: until that change, recipe_sha256 was hashed over (patterns + context + strategy
-- + engine). The new binary hashes only (patterns + context + strategy). Every existing
-- row therefore stores an engine-FUL hash that no longer matches what the new binary
-- computes, so on the first run the new binary would see the WHOLE corpus as stale and
-- re-distill it. This statement re-stamps the existing rows to the engine-LESS hash so
-- the deploy is a no-op for already-distilled docs.
--
-- The hashes below are the engine-less recipe hashes for the two production lanes with
-- NO context and NO strategy (the only recipes used in prod). They are derived as
-- sha256( <pattern system.md bytes> || 0x00 || "ctx:" || "strat:" ). If the embedded
-- pattern files change, these constants change too — recompute before reusing.
--
-- IDEMPOTENT: re-running it is safe (it just re-sets the same value).

-- transcripts lane: pattern = extract_wisdom
UPDATE distillations
SET    recipe_sha256 = 'c55cb2d0db09bbf61be5455943e90e2798ea05bbf6fcd963820451f2fd587eb7'
WHERE  pattern = 'extract_wisdom'
  AND  context IS NULL
  AND  strategy IS NULL
  AND  session_patterns IS NULL;

-- news lane: pattern = summarize_news
UPDATE distillations
SET    recipe_sha256 = 'be925779e480d2b7113552465d80b44f6dc0b514c701dc700706ec4bf31f8dfa'
WHERE  pattern = 'summarize_news'
  AND  context IS NULL
  AND  strategy IS NULL
  AND  session_patterns IS NULL;
