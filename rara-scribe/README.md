# rara-scribe

Terceiro agente do ecossistema **rara**. Produz **transcripts de excelente qualidade, na
língua nativa do áudio**, para os vídeos colhidos pelo `rara-harvest` (`channel_videos`) e
catalogados pelo `rara-shelf` (`playlist_videos`).

Substitui as legendas automáticas (fracas) do YouTube por ASR especialista. **Agnóstico à
fonte**: YouTube, outro site de vídeo (1800+ via yt-dlp) ou um ficheiro local.

Tabelas próprias no mesmo Neon (isolado dos outros agentes). Corre como **Cloud Run Job**.

## Como funciona

1. **Descoberta** (modo batch): selecciona até `BATCH_SIZE` vídeos presentes em
   `channel_videos` ∪ `playlist_videos` que ainda não têm transcript `done` (idempotente).
2. **Áudio**: `yt-dlp` descarrega o áudio (ou lê o ficheiro local); `ffmpeg` converte para
   16 kHz mono e fatia em segmentos de 10 min (cada chunk < 25 MB).
3. **Transcrição**: cada chunk vai para o motor ASR; os timestamps dos segmentos são
   reindexados para a timeline global e o texto é costurado.
4. **Persistência**: cabeçalho (`transcripts`) + segmentos (`transcript_segments`) numa
   transacção. Falha num vídeo → `status='failed'` e o batch continua.

## Motor de transcrição (plugável)

Escolhido por `TRANSCRIBE_ENGINE`:

| Engine | Modelo | Custo aprox. | Notas |
|--------|--------|--------------|-------|
| `groq` (default) | `whisper-large-v3` | ~$0.111/h | Melhor qualidade/custo; timestamps precisos. |
| `gemini` | `gemini-2.5-flash` | ~$0.045/h (batch) | Mais barato; timestamps aproximados. |

A coluna `engine` regista qual motor produziu cada linha.

## Modos

```bash
# Batch (default): a partir das tabelas dos colectores
./scribe-job

# Fonte única (ad-hoc): YouTube, outro site, ou ficheiro local
./scribe-job --source "https://www.youtube.com/watch?v=VIDEO_ID"
./scribe-job --source "/caminho/para/video.mp4"
```

## Configuração (env)

| Var | Obrigatória | Default | Descrição |
|-----|-------------|---------|-----------|
| `DATABASE_URL` | sim | — | Neon PostgreSQL (partilhado) |
| `TRANSCRIBE_ENGINE` | não | `groq` | `groq` ou `gemini` |
| `GROQ_API_KEY` | se engine groq | — | https://console.groq.com |
| `GEMINI_API_KEY` | se engine gemini | — | https://aistudio.google.com |
| `BATCH_SIZE` | não | `25` | Vídeos por execução |
| `YT_DLP_COOKIES` | não | — | cookies.txt para contornar bot-check do YouTube |

## Desenvolvimento

```bash
make test          # testes (TDD)
make test-race     # testes com race detector
make lint          # go vet + staticcheck
make build         # binário local (scribe-job)
```

Requer `yt-dlp` e `ffmpeg` no PATH para correr de verdade (os testes não precisam — usam mocks).

## Migrações

`migrations/001_initial_schema.sql` cria `transcripts` + `transcript_segments`. Aplicadas pelo
workflow `database-scribe.yml`. Ver [DEPLOY.md](DEPLOY.md).
