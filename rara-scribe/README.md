# rara-scribe

Terceiro agente do ecossistema **rara**. Produz **transcripts de excelente qualidade, na
língua nativa do áudio**, para os vídeos colhidos pelo `rara-harvest` (`channel_videos`) e
catalogados pelo `rara-shelf` (`playlist_videos`).

Substitui as legendas automáticas (fracas) do YouTube por ASR especialista. **Agnóstico à
fonte**: YouTube, outro site de vídeo (1800+ via yt-dlp) ou um ficheiro local.

Tabelas próprias no mesmo Neon (isolado dos outros agentes). **Corre localmente no Mac**
(agendado por `launchd`), o que evita o bot-check do YouTube que bloqueia IPs de datacenter.

## Como funciona

1. **Descoberta** (modo batch): selecciona até `BATCH_SIZE` vídeos presentes em
   `channel_videos` ∪ `playlist_videos` que ainda não têm transcript `done` (idempotente).
2. **Áudio**: `yt-dlp` descarrega o áudio; `ffmpeg` converte para 16 kHz mono e fatia em
   segmentos de 10 min (cada chunk < 25 MB, dentro do limite da API Groq).
3. **Transcrição**: cada chunk vai para o motor ASR; os timestamps dos segmentos são
   reindexados para a timeline global e o texto é costurado.
4. **Persistência**: cabeçalho (`transcripts`) + segmentos (`transcript_segments`) numa
   transacção. Falha num vídeo → `status='failed'` e o batch continua.

## Instalação local (uma vez)

Pré-requisito: `yt-dlp` e `ffmpeg` instalados (provavelmente já tens via Homebrew).

```bash
cd rara-scribe

# 1ª vez: cria ~/.rara-scribe/.env a partir do template e termina com instruções
bash install-local.sh

# Preenche os valores (DATABASE_URL e GROQ_API_KEY obrigatórios)
$EDITOR ~/.rara-scribe/.env

# Instalar de facto (compila binário + activa agente launchd)
bash install-local.sh
```

O agente fica agendado **diariamente às 02:00**. O Mac processa o backlog durante a noite.
Para alterar o horário, edita `~/Library/LaunchAgents/com.rara.scribe.plist`.

## Uso diário

```bash
# Forçar um run imediato
launchctl start com.rara.scribe

# Ver logs em tempo real
tail -f ~/Library/Logs/rara-scribe/output.log

# Run manual único (sem launchd, a partir do repo)
cd rara-scribe && make run

# Fonte única ad-hoc
cd rara-scribe && source ~/.rara-scribe/.env && ./scribe-job --source "https://youtu.be/VIDEO_ID"

# Actualizar após novo build
cd rara-scribe && make build && bash install-local.sh
```

## Motor de transcrição (plugável)

Escolhido por `TRANSCRIBE_ENGINE` no `.env`:

| Engine | Modelo | Custo aprox. | Notas |
|--------|--------|--------------|-------|
| `groq` (default) | `whisper-large-v3` | ~$0.111/h | Melhor qualidade/custo; timestamps precisos. |
| `gemini` | `gemini-2.5-flash` | ~$0.045/h (batch) | Mais barato; timestamps aproximados. |

A coluna `engine` regista qual motor produziu cada linha.

## Configuração (env)

| Var | Obrigatória | Default | Descrição |
|-----|-------------|---------|-----------|
| `DATABASE_URL` | sim | — | Neon PostgreSQL (partilhado) |
| `TRANSCRIBE_ENGINE` | não | `groq` | `groq` ou `gemini` |
| `GROQ_API_KEY` | se engine groq | — | https://console.groq.com |
| `GEMINI_API_KEY` | se engine gemini | — | https://aistudio.google.com |
| `BATCH_SIZE` | não | `25` | Vídeos por execução |
| `YT_DLP_BIN` | sim (local) | — | Caminho absoluto para `yt-dlp` |
| `FFMPEG_BIN` | sim (local) | — | Caminho absoluto para `ffmpeg` |
| `YT_DLP_COOKIES` | não | — | cookies.txt (raramente necessário de IP residencial) |

## Desenvolvimento

```bash
make test          # testes (TDD)
make test-race     # testes com race detector
make lint          # go vet + staticcheck
make build         # binário local (scribe-job)
make run           # compilar + correr um batch (requer .env nesta directoria)
```

## Limpeza da infra GCP (Cloud Run removido)

O Cloud Run Job `rara-scribe` já não é usado. Para limpar:

```bash
# Eliminar o Cloud Run Job
gcloud run jobs delete rara-scribe --region us-central1 --project oute-rara

# Opcional: eliminar o secret de cookies (já não necessário localmente)
# gcloud secrets delete yt-dlp-cookies --project oute-rara
# groq-api-key e database-url ficam (usados por outros agentes)
```

## Migrações

`migrations/001_initial_schema.sql` cria `transcripts` + `transcript_segments`. Aplicadas pelo
workflow `database-scribe.yml`. Ver [DEPLOY.md](DEPLOY.md).
