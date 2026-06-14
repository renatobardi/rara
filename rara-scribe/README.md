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

Since P1c, rara-scribe is a 2.0 **bridge-total claim-worker**: it attaches to `rara-core` only
through the Neon contract (a `providers` row + the `item_steps` protocol) via the
[rara-addon](../rara-addon) SDK. The SDK owns claim/heartbeat/result/requeue/poke; this app supplies
only the `transcrever` domain (`transcribeHandler`). The reconciler routes and activates it; it
never decides *what* to transcribe.

One app serves **two providers**, selected by `SCRIBE_PROVIDER`; the handler picks the fetch
strategy by provider/lane:

- **`asr-youtube`** (residential-IP Mac): builds the watch URL from the item's video id.
- **`asr-direct-audio`** (anywhere): resolves the episode's enclosure URL from `podcast_episodes`
  and re-keys the transcript to the spine's GUID + `source_type=podcast`.

Per claimed item: 1) resolve the fetch target; 2) `yt-dlp` downloads the audio and `ffmpeg`
converts to 16 kHz mono in ~10-minute chunks; 3) each chunk goes to the ASR engine, segment
timestamps re-indexed to the global timeline and text stitched; 4) the `transcripts` row +
`transcript_segments` are written in one transaction and the row id is returned as the step's
`output_ref`. A no-speech result is `empty` (the item is curated out); a download/ASR failure is
persisted and surfaced as **retryable** so the SDK requeues up to the cap.

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
# Claim & drain the transcrever queue for SCRIBE_PROVIDER, then exit (uses .env)
cd rara-scribe && make run

# Watch logs in real time (Go logs to stderr → error.log; output.log stays empty)
tail -f ~/Library/Logs/rara-scribe/error.log
```

The binary is now configured entirely by env (no CLI flags). `SCRIBE_PROVIDER` selects the
provider; `TRANSCRIBE_ENGINE` the ASR engine; set `WORK_POLL_INTERVAL` and/or `POKE_ADDR`/
`POKE_TOKEN` to run resident with symmetric activation (otherwise it drains once and exits).

> **launchd deploy is P2.** The local-Mac launchd wiring (`install-local.sh`, `run.sh`,
> `com.rara.scribe.plist`) still reflects the 1.0 batch model and the removed `--engine`/`--limit`/
> `--source` flags. Re-wiring it for the resident claim-worker (set `SCRIBE_PROVIDER=asr-youtube`,
> a `WORK_POLL_INTERVAL`/poke, drop the flags) lands in the P2 deploy phase alongside the
> reconciler/VPC.

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
| `SCRIBE_PROVIDER` | yes | — | the provider this worker serves: `asr-youtube` \| `asr-direct-audio`; the SDK claims its steps by `(transcrever, this provider)` |
| `WORK_POLL_INTERVAL` | no | (unset → on_demand) | resident safety-net poll cadence (Go duration or bare seconds) |
| `POKE_ADDR` / `POKE_TOKEN` | no | (unset) | tailnet poke listener (`POST /poke`, Bearer) for symmetric activation |
| `TRANSCRIBE_ENGINE` | no | `groq` | `groq`, `gemini` or `local` |
| `GROQ_API_KEY` | if engine=groq | — | https://console.groq.com (also enables the Groq fallback under `local`) |
| `GEMINI_API_KEY` | if engine=gemini | — | https://aistudio.google.com |
| `WHISPER_CPP_BIN` | if engine=local | `/opt/homebrew/bin/whisper-cli` | whisper.cpp CLI binary |
| `WHISPER_CPP_MODEL` | if engine=local | — | Absolute path to the ggml `large-v3` model |
| `WHISPER_CPP_VAD_MODEL` | no | — | Optional silero VAD model (trims silence/music, cuts hallucinations) |
| `SOURCE_TIMEOUT_MINUTES` | no | `20` (`60` for local) | Per-item timeout (download + transcribe); raise for very long local videos |
| `YT_DLP_BIN` | yes (local) | — | Absolute path to `yt-dlp` |
| `FFMPEG_BIN` | yes (local) | — | Absolute path to `ffmpeg` |
| `YT_DLP_COOKIES` | no | — | cookies.txt (rarely needed from a residential IP) |

## Development

```bash
make test          # tests (TDD)
make test-race     # tests with race detector
make lint          # go vet + staticcheck
make build         # local binary (scribe-job)
make run           # build + claim/drain the queue once for SCRIBE_PROVIDER (requires .env)

> Module wiring: rara-scribe couples to the SDK via `replace rara-addon => ../rara-addon` in
> `go.mod` (no committed `go.work`). The launchd/Docker (multi-module) build is wired in P2.
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
- `migrations/003_add_attempt_count.sql` adds `transcripts.attempt_count` so permanently-failing
  videos (deleted/private) stop being retried after a cap instead of every run.

Applied by the `database-scribe.yml` workflow. See [DEPLOY.md](DEPLOY.md).
