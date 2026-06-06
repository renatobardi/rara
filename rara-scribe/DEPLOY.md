# rara-scribe — Notas de operação

> **Cloud Run removido.** O rara-scribe corria como Cloud Run Job mas o YouTube bloqueia
> 100% dos downloads a partir de IPs de datacenter GCP ("Sign in to confirm you're not a bot").
> O agente passou a correr localmente no Mac via `launchd`.
> **Ver [README.md](README.md) para instalação e uso.**

---

## Queries de validação (Neon)

Úteis após qualquer run — local ou futuro.

```sql
-- Resumo por estado
SELECT source_type, engine, language, status, COUNT(*)
FROM transcripts
GROUP BY source_type, engine, language, status
ORDER BY COUNT(*) DESC;

-- Transcripts recentes com contagem de segmentos
SELECT t.youtube_video_id, t.language, COUNT(s.id) AS segments, t.duration_seconds, t.status
FROM transcripts t
LEFT JOIN transcript_segments s ON s.transcript_id = t.id
GROUP BY t.id
ORDER BY t.created_at DESC
LIMIT 10;

-- Taxa de falha nas últimas 24h (monitorização)
SELECT COUNT(*) FILTER (WHERE status = 'failed') AS falhados_recentes
FROM transcripts
WHERE updated_at > NOW() - INTERVAL '1 day';
```

## Limpeza GCP

```bash
export PROJECT_ID=oute-rara

# Eliminar o Cloud Run Job
gcloud run jobs delete rara-scribe --region us-central1 --project "${PROJECT_ID}"

# Opcional: eliminar secret de cookies (já não necessário localmente)
# gcloud secrets delete yt-dlp-cookies --project "${PROJECT_ID}"

# groq-api-key e database-url ficam — são usados pelos outros agentes
```

## Trocar motor para Gemini (futuro)

Quando/se migrares para Gemini, edita `~/.rara-scribe/.env`:

```bash
TRANSCRIBE_ENGINE=gemini
GEMINI_API_KEY=a-tua-gemini-key
```

Tradeoff: Gemini 2.5 Flash é ~½ do custo do Groq, mas os timestamps dos segmentos são
**aproximados** (o Whisper alinha melhor). A coluna `engine` regista qual motor produziu
cada linha.
