# rara-scribe — Operations notes

> **Cloud Run removed.** rara-scribe previously ran as a Cloud Run Job, but YouTube blocks
> 100% of downloads from GCP datacenter IPs ("Sign in to confirm you're not a bot").
> The agent now runs locally on the Mac via `launchd`.
> **See [README.md](README.md) for installation and usage.**

---

## Validation queries (Neon)

Useful after any run — local or future.

```sql
-- Summary by status
SELECT source_type, engine, language, status, COUNT(*)
FROM transcripts
GROUP BY source_type, engine, language, status
ORDER BY COUNT(*) DESC;

-- Recent transcripts with segment count
SELECT t.youtube_video_id, t.language, COUNT(s.id) AS segments, t.duration_seconds, t.status
FROM transcripts t
LEFT JOIN transcript_segments s ON s.transcript_id = t.id
GROUP BY t.id
ORDER BY t.created_at DESC
LIMIT 10;

-- Failure rate in the last 24 hours (monitoring)
SELECT COUNT(*) FILTER (WHERE status = 'failed') AS recent_failures
FROM transcripts
WHERE updated_at > NOW() - INTERVAL '1 day';
```

## GCP cleanup

```bash
export PROJECT_ID=oute-rara

# Delete the Cloud Run Job
gcloud run jobs delete rara-scribe --region us-central1 --project "${PROJECT_ID}"

# Optional: delete the cookies secret (no longer needed locally)
# gcloud secrets delete yt-dlp-cookies --project "${PROJECT_ID}"

# groq-api-key and database-url remain — used by the other agents
```

## Switching to Gemini (future)

If/when you migrate to Gemini, edit `~/.rara-scribe/.env`:

```bash
TRANSCRIBE_ENGINE=gemini
GEMINI_API_KEY=your-gemini-api-key
```

Trade-off: Gemini 2.5 Flash costs ~½ of Groq, but segment timestamps are **approximate**
(Whisper alignment is more precise). The `engine` column records which engine produced each row.
