# rara-scribe

Third agent in the **rara** ecosystem. Produces **high-quality transcripts, in the audio's
native language**, for videos collected by `rara-harvest` (`channel_videos`) and cataloged
by `rara-shelf` (`playlist_videos`).

Replaces weak YouTube auto-captions with specialist ASR. **Source-agnostic**: YouTube, any
other video site (1800+ via yt-dlp), or a local file.

Own tables in the same Neon database (isolated from the other agents). **Runs locally on
your Mac** (scheduled by `launchd`), which avoids the YouTube bot-check that blocks
datacenter IPs.

## How it works

1. **Discovery** (batch mode): selects up to `BATCH_SIZE` videos present in
   `channel_videos` ∪ `playlist_videos` that do not yet have a `done` transcript (idempotent).
2. **Audio**: `yt-dlp` downloads the audio; `ffmpeg` converts to 16 kHz mono and splits into
   10-minute segments (each chunk < 25 MB, within the Groq API limit).
3. **Transcription**: each chunk is sent to the ASR engine; segment timestamps are re-indexed
   to the global timeline and the text is stitched together.
4. **Persistence**: header row (`transcripts`) + segments (`transcript_segments`) in a single
   transaction. A failure on one video → `status='failed'` and the batch continues.

## Local installation (once)

Prerequisites: `yt-dlp` and `ffmpeg` installed (most likely via Homebrew).

```bash
cd rara-scribe

# First run: creates ~/.rara-scribe/.env from template and exits with instructions
bash install-local.sh

# Fill in the values (DATABASE_URL and GROQ_API_KEY are required)
$EDITOR ~/.rara-scribe/.env

# Actual install (compiles binary + activates launchd agent)
bash install-local.sh
```

The agent is scheduled **daily at 02:00**. The Mac processes the backlog overnight.
To change the schedule, edit `~/Library/LaunchAgents/com.rara.scribe.plist`.

## Daily usage

```bash
# Force an immediate run
launchctl start com.rara.scribe

# Watch logs in real time
tail -f ~/Library/Logs/rara-scribe/output.log

# Single manual run (without launchd, from the repo)
cd rara-scribe && make run

# Ad-hoc single source
cd rara-scribe && source ~/.rara-scribe/.env && ./scribe-job --source "https://youtu.be/VIDEO_ID"

# Update after a new build
cd rara-scribe && make build && bash install-local.sh
```

## Transcription engine (pluggable)

Chosen by `TRANSCRIBE_ENGINE` in `.env`:

| Engine | Model | Approx. cost | Notes |
|--------|-------|--------------|-------|
| `groq` (default) | `whisper-large-v3` | ~$0.111/h | Best quality/cost; precise timestamps. |
| `gemini` | `gemini-2.5-flash` | ~$0.045/h (batch) | Cheaper; approximate timestamps. |

The `engine` column records which engine produced each row.

## Configuration (env)

| Var | Required | Default | Description |
|-----|----------|---------|-------------|
| `DATABASE_URL` | yes | — | Neon PostgreSQL (shared) |
| `TRANSCRIBE_ENGINE` | no | `groq` | `groq` or `gemini` |
| `GROQ_API_KEY` | if engine=groq | — | https://console.groq.com |
| `GEMINI_API_KEY` | if engine=gemini | — | https://aistudio.google.com |
| `BATCH_SIZE` | no | `25` | Videos per run (default; raise freely — e.g. 100, 1000 — to drain the backlog faster, no hard cap) |
| `YT_DLP_BIN` | yes (local) | — | Absolute path to `yt-dlp` |
| `FFMPEG_BIN` | yes (local) | — | Absolute path to `ffmpeg` |
| `YT_DLP_COOKIES` | no | — | cookies.txt (rarely needed from a residential IP) |

## Development

```bash
make test          # tests (TDD)
make test-race     # tests with race detector
make lint          # go vet + staticcheck
make build         # local binary (scribe-job)
make run           # build + run one batch (requires .env in this directory)
```

## GCP infrastructure cleanup (Cloud Run removed)

The Cloud Run Job `rara-scribe` is no longer in use. To clean up:

```bash
# Delete the Cloud Run Job
gcloud run jobs delete rara-scribe --region us-central1 --project oute-rara

# Optional: delete the cookies secret (no longer needed locally)
# gcloud secrets delete yt-dlp-cookies --project oute-rara
# groq-api-key and database-url remain (used by the other agents)
```

## Migrations

- `migrations/001_initial_schema.sql` creates `transcripts` + `transcript_segments`.
- `migrations/002_widen_language.sql` widens `transcripts.language` to `TEXT` (Whisper returns
  full language names like `azerbaijani` that overflow the original `VARCHAR(10)`).

Applied by the `database-scribe.yml` workflow. See [DEPLOY.md](DEPLOY.md).
