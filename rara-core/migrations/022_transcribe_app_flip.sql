-- Flip transcribe providers from legacy apps (asr-youtube, asr-direct-audio) to the consolidated
-- rara-transcribe job/image. Workers 'caption' and 'echo' cover the 2 placements:
--   caption-mac (Mac resident), echo-cloud (Cloud Run on-demand).
-- Idempotent: re-running is a no-op (UPDATE on already-'transcribe' rows changes nothing).
UPDATE providers SET app = 'transcribe' WHERE worker IN ('caption', 'echo') AND app != 'transcribe';
