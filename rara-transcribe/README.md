# rara-transcribe

Transcription agent in the **rara** ecosystem. Produces **high-quality transcripts, in the audio's
native language**, for videos collected by `rara-harvest` and podcasts collected by `rara-dial`.

Replaces weak YouTube auto-captions with specialist ASR. **Two workers, one app:**

| Worker | Provider | Placement | How |
|--------|----------|-----------|-----|
| **caption** | `caption` | Mac only (residential IP) | yt-dlp downloads video audio |
| **echo** | `echo` | cloud / VPC / Mac | Fetches enclosure URL over HTTP |

Own tables in the same Neon database (isolated from the other agents).

## How it works

Since P1c, rara-transcribe is a 2.0 **bridge-total claim-worker**: it attaches to `rara-core` only
through the Neon contract (a `providers` row + the `item_steps` protocol) via the
[rara-addon](../rara-addon) SDK. The SDK owns claim/heartbeat/result/requeue/poke; this app supplies
only the `transcrever` domain (`transcribeHandler`). The reconciler routes and activates it; it
never decides *what* to transcribe.

One binary serves **two workers**, selected by `SCRIBE_PROVIDER`; the handler picks the fetch
strategy by provider/lane:

- **`caption`** (residential-IP Mac): builds the watch URL from the item's video id.
- **`echo`** (anywhere): resolves the episode's enclosure URL from
  `podcast_episodes` and re-keys the transcript to the spine's GUID + `source_type=podcast`.

Per claimed item: 1) resolve the fetch target; 2) `yt-dlp` downloads the audio and `ffmpeg`
converts to 16 kHz mono in ~10-minute chunks; 3) each chunk goes to the ASR engine, segment
timestamps re-indexed to the global timeline and text stitched; 4) the `transcripts` row +
`transcript_segments` are written in one transaction and the row id is returned as the step's
`output_ref`. A no-speech result is `empty` (the item is curated out); a download/ASR failure is
persisted and surfaced as **retryable** so the SDK requeues up to the cap.

## Local installation (caption-mac, once)

Prerequisites: `yt-dlp` and `ffmpeg` installed (most likely via Homebrew).

```bash
cd rara-transcribe

# First run: creates ~/.rara-transcribe/.env from template and exits with instructions
bash install-local.sh

# Fill in the values (DATABASE_URL and GROQ_API_KEY are required)
$EDITOR ~/.rara-transcribe/.env

# Actual install (compiles binary + activates launchd agent)
bash install-local.sh
```

The agent is scheduled **daily at 02:00**. The Mac processes the backlog overnight.
To change the schedule, edit `~/Library/LaunchAgents/com.rara.transcribe.plist`.

## Daily usage

```bash
# Claim & drain the transcrever queue for SCRIBE_PROVIDER, then exit (uses .env)
cd rara-transcribe && make run

# Watch logs in real time (Go logs to stderr → error.log; output.log stays empty)
tail -f ~/Library/Logs/rara-transcribe/error.log
```

The binary is configured entirely by env (no CLI flags). `SCRIBE_PROVIDER` selects the
provider; `TRANSCRIBE_ENGINE` the ASR engine; set `WORK_POLL_INTERVAL` and/or `POKE_ADDR`/
`POKE_TOKEN` to run resident with symmetric activation (otherwise it drains once and exits).

## Transcription engine (pluggable)

Chosen by `TRANSCRIBE_ENGINE` in `.env`:

| Engine | Model | Approx. cost | Notes |
|--------|-------|--------------|-------|
| `groq` (default) | `whisper-large-v3` | ~$0.111/h | Best quality/cost; precise timestamps. Free tier caps at 2000 req/day. |
| `gemini` | `gemini-2.5-flash` | ~$0.045/h (batch) | Cheaper; approximate timestamps. |
| `local` | `whisper.cpp` `large-v3` | $0 (compute) | Offline, no API quota — same large-v3 weights as Groq (beam search 5). Hybrid: Groq fallback per chunk when `GROQ_API_KEY` is set. |

The `engine` column records which engine produced each row (for `local`, the stored
name is `whispercpp/whisper-large-v3` even when a chunk fell back to Groq).

## Configuration (env)

| Var | Required | Default | Description |
|-----|----------|---------|-------------|
| `DATABASE_URL` | yes | — | Neon PostgreSQL (shared) |
| `SCRIBE_PROVIDER` | yes | — | provider this worker serves: `caption` \| `echo`; the SDK claims its steps by `(transcrever, this provider)` |
| `WORK_POLL_INTERVAL` | no | (unset → on_demand) | resident safety-net poll cadence (Go duration or bare seconds) |
| `POKE_ADDR` / `POKE_TOKEN` | no | (unset) | tailnet poke listener (`POST /poke`, Bearer) for symmetric activation |
| `TRANSCRIBE_ENGINE` | no | `groq` | `groq`, `gemini` or `local` |
| `GROQ_API_KEY` | if engine=groq | — | https://console.groq.com (also enables the Groq fallback under `local`) |
| `GEMINI_API_KEY` | if engine=gemini | — | https://aistudio.google.com |
| `WHISPER_CPP_BIN` | if engine=local | `whisper-cli` (PATH) | whisper.cpp CLI binary; set to absolute path on Mac/launchd |
| `WHISPER_CPP_MODEL` | if engine=local | — | Absolute path to the ggml `large-v3` model |
| `WHISPER_CPP_VAD_MODEL` | no | — | Optional silero VAD model (trims silence/music, cuts hallucinations) |
| `SOURCE_TIMEOUT_MINUTES` | no | `20` (`60` for local) | Per-item timeout (download + transcribe); raise for very long local videos |
| `YT_DLP_BIN` | no | `yt-dlp` (PATH) | Path to `yt-dlp`; set to absolute path on Mac/launchd |
| `FFMPEG_BIN` | no | `ffmpeg` (PATH) | Path to `ffmpeg`; set to absolute path on Mac/launchd |
| `YT_DLP_COOKIES` | no | — | cookies.txt (rarely needed from a residential IP) |

## Development

```bash
make test          # tests (TDD)
make test-race     # tests with race detector
make lint          # go vet + staticcheck
make build         # local binary (transcribe-job)
make run           # build + claim/drain the queue once for SCRIBE_PROVIDER (requires .env)
```

> Module wiring: rara-transcribe couples to the SDK via `replace rara-addon => ../rara-addon` in
> `go.mod` (no committed `go.work`).

## Cloud Run Job

The `echo` worker runs as Cloud Run Job `rara-transcribe` (deployed by
`deploy-transcribe.yml`). The `caption` worker runs natively on the Mac via
launchd — a rebuild of the binary there is required after any rename (manual step, P2b-transcribe-B).

## Migrations

- `migrations/001_initial_schema.sql` creates `transcripts` + `transcript_segments`.
- `migrations/002_widen_language.sql` widens `transcripts.language` to `TEXT` (Whisper returns
  full language names like `azerbaijani` that overflow the original `VARCHAR(10)`).
- `migrations/003_add_attempt_count.sql` adds `transcripts.attempt_count` so permanently-failing
  videos (deleted/private) stop being retried after a cap instead of every run.

Applied by the `database-transcribe.yml` workflow. See [DEPLOY.md](DEPLOY.md).
