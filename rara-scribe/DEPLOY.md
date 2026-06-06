# Deploy — rara-scribe (Cloud Run via GitHub Actions)

O [`deploy-scribe.yml`](../.github/workflows/deploy-scribe.yml) builda a imagem com
**Cloud Build** e faz deploy de um **Cloud Run Job** `rara-scribe`, autenticando no GCP
via **Workload Identity Federation** (sem chaves de service account).

Reaproveita toda a infra dos outros agentes (projeto `${PROJECT_ID}`, Artifact Registry
`rara`, WIF, service account `rara-deployer`, bucket `${PROJECT_ID}_cloudbuild`, base Neon).
A única coisa nova é **a chave da API de transcrição** — 1 secret no Secret Manager.

> Define `export PROJECT_ID=<o-teu-projeto>` antes de correr os comandos abaixo. O valor real
> vive na GitHub Variable `GCP_PROJECT_ID`, não neste ficheiro.

---

## 1. Criar o secret da Groq (uma vez)

Cria uma API key em https://console.groq.com → **API Keys**, depois (Cloud Shell):

```bash
printf '%s' 'gsk_a-tua-groq-key' | gcloud secrets create groq-api-key \
  --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
```

`database-url` já existe (reutilizado dos outros agentes). A service account `rara-deployer`
e a SA de runtime (compute default) já têm `secretmanager.secretAccessor` a nível de projeto,
portanto o secret novo fica acessível **sem alterações de IAM**.

---

## 2. Deploy

- **Automático**: merge de qualquer coisa em `rara-scribe/**` para `main` dispara o
  `deploy-scribe.yml`.
- **Manual**: Actions → **Deploy rara-scribe to Cloud Run** → *Run workflow*.

O workflow builda a imagem (Go + ffmpeg + yt-dlp), cria/atualiza o Cloud Run Job
`rara-scribe` (montando `database-url` + `groq-api-key`, com `TRANSCRIBE_ENGINE=groq` e
`BATCH_SIZE=25`) e executa uma vez. Recursos: `--memory 2Gi --cpu 2 --task-timeout 3600s`.

Cada execução transcreve até `BATCH_SIZE` vídeos ainda sem transcript. É idempotente: re-runs
continuam o backlog. Agenda execuções regulares (secção 5) para esgotar a fila ao longo do tempo.

---

## 3. Trocar para Gemini (opcional, mais barato)

```bash
printf '%s' 'a-tua-gemini-key' | gcloud secrets create gemini-api-key \
  --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"

gcloud run jobs update rara-scribe --region us-central1 --project "${PROJECT_ID}" \
  --set-env-vars "TRANSCRIBE_ENGINE=gemini,BATCH_SIZE=25" \
  --update-secrets "GEMINI_API_KEY=gemini-api-key:latest"
```

Tradeoff: Gemini 2.5 Flash é ~½ do custo, mas os timestamps dos segmentos são **aproximados**
(o Whisper alinha melhor). A coluna `engine` regista qual motor produziu cada linha.

---

## 4. Cookies do YouTube (opcional — contra bloqueio de bot)

O yt-dlp a partir de IPs de datacenter (Cloud Run) pode apanhar "Sign in to confirm you're not
a bot". Sem mitigação, vídeos bloqueados ficam `status='failed'` e são re-tentados no run
seguinte (o batch nunca pára por causa de um vídeo).

> **Nota de segurança:** um `cookies.txt` do YouTube é essencialmente a tua sessão Google —
> guardá-lo como secret é uma superfície de risco. Por isso **não** é montado por defeito.

Se mais tarde quiseres ativá-lo (de preferência com uma conta Google secundária), exporta o
`cookies.txt`, cria o secret e monta-o no job — e acrescenta `YT_DLP_COOKIES=yt-dlp-cookies:latest`
ao `--set-secrets` do `deploy-scribe.yml`:

```bash
gcloud secrets create yt-dlp-cookies --replication-policy=automatic \
  --data-file=cookies.txt --project "${PROJECT_ID}"

gcloud run jobs update rara-scribe --region us-central1 --project "${PROJECT_ID}" \
  --update-secrets "YT_DLP_COOKIES=yt-dlp-cookies:latest"
```

---

## 5. Verificar

```sql
SELECT source_type, engine, language, status, COUNT(*)
FROM transcripts
GROUP BY source_type, engine, language, status
ORDER BY COUNT(*) DESC;

-- Um transcript com os seus segmentos
SELECT t.youtube_video_id, t.language, COUNT(s.id) AS segments, t.duration_seconds
FROM transcripts t
LEFT JOIN transcript_segments s ON s.transcript_id = t.id
GROUP BY t.id
ORDER BY t.created_at DESC
LIMIT 10;
```

---

## 6. Agendamento (opcional)

```bash
gcloud scheduler jobs create http rara-scribe-hourly \
  --location=us-central1 --schedule="0 * * * *" \
  --uri="https://us-central1-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-scribe:run" \
  --http-method=POST \
  --oauth-service-account-email="rara-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```
