-- harvest-status.sql
-- Cole no SQL Editor do NeonDB para ver o status do rara-harvest.

-- ── 1. Totais gerais ─────────────────────────────────────────────────────────
SELECT
    COUNT(*)                                                        AS total_videos,
    COUNT(DISTINCT channel_id)                                      AS channels_with_videos,
    MIN(published_at)                                               AS oldest_video,
    MAX(published_at)                                               AS newest_video,
    MAX(collected_at)                                               AS last_run,
    COUNT(*) FILTER (WHERE collected_at >= NOW() - INTERVAL '24h') AS collected_last_24h,
    COUNT(*) FILTER (WHERE collected_at >= NOW() - INTERVAL '7d')  AS collected_last_7d
FROM channel_videos;

-- ── 2. Por canal ─────────────────────────────────────────────────────────────
SELECT
    tc.channel_name,
    tc.youtube_channel_id,
    tc.active,
    COUNT(cv.id)        AS video_count,
    MIN(cv.published_at) AS oldest_video,
    MAX(cv.published_at) AS newest_video,
    MAX(cv.collected_at) AS last_collected
FROM target_channels tc
LEFT JOIN channel_videos cv ON cv.channel_id = tc.id
GROUP BY tc.id
ORDER BY video_count DESC;

-- ── 3. Funil harvest → scribe → distill ──────────────────────────────────────
SELECT
    COUNT(cv.id)                                                        AS harvest_total,
    COUNT(t.id)                                                         AS transcribed,
    COUNT(d.id)                                                         AS distilled,
    COUNT(cv.id) FILTER (WHERE t.id IS NULL)                            AS pending_transcript,
    COUNT(cv.id) FILTER (WHERE t.id IS NOT NULL AND d.id IS NULL)       AS pending_distill,
    COUNT(t.id)  FILTER (WHERE t.status = 'error')                      AS transcript_errors,
    COUNT(d.id)  FILTER (WHERE d.status = 'error')                      AS distill_errors
FROM channel_videos cv
LEFT JOIN transcripts   t ON t.youtube_video_id = cv.youtube_video_id
LEFT JOIN distillations d ON d.youtube_video_id = cv.youtube_video_id;

-- ── 4. Últimos 10 vídeos coletados ───────────────────────────────────────────
SELECT
    cv.youtube_video_id,
    cv.title,
    cv.published_at,
    cv.collected_at,
    t.status  AS transcript_status,
    d.status  AS distill_status
FROM channel_videos cv
LEFT JOIN transcripts   t ON t.youtube_video_id = cv.youtube_video_id
LEFT JOIN distillations d ON d.youtube_video_id = cv.youtube_video_id
ORDER BY cv.collected_at DESC
LIMIT 10;
