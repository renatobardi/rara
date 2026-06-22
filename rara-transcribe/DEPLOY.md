# rara-transcribe — Operations notes

> The `caption` worker runs locally on the Mac via `launchd` — YouTube blocks
> datacenter IPs. The `echo` worker runs as Cloud Run Job `rara-transcribe`.
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
export PROJECT_ID=YOUR_PROJECT_ID  # replace with your GCP project ID

# Delete the Cloud Run Job (old echo job, after P2b-transcribe-B cutover)
gcloud run jobs delete rara-transcribe --region us-central1 --project "${PROJECT_ID}"

# groq-api-key and database-url remain — used by the other agents
```

## Switching to Gemini (future)

If/when you migrate to Gemini, edit `~/.rara-transcribe/.env`:

```bash
TRANSCRIBE_ENGINE=gemini
GEMINI_API_KEY=your-gemini-api-key
```

Trade-off: Gemini 2.5 Flash costs ~½ of Groq, but segment timestamps are **approximate**
(Whisper alignment is more precise). The `engine` column records which engine produced each row.
