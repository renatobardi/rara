// runners.go — the I/O edge of the worker shims (Phase 1 deliverable #5).
//
// These concrete StepRunners are the thin adapters that actually invoke the existing
// scribe/distill binaries and read back the domain row they wrote. They are deliberately
// minimal glue: exec + one SELECT. Like the pgx writes in main.go, they are exercised by
// real deploys/integration, not unit tests — the claim/advance orchestration in worker.go
// is what the pure tests cover (via a fake StepRunner).
//
// Binary paths and engines are environment-configured so a deploy points the shim at the
// right artifact (SCRIBE_BIN on the Mac, DISTILL_BIN in the Cloud Run image) without code
// changes. None of this touches scribe/distill domain logic.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// errNoOutputRow is a HARD failure: the worker ran but produced no usable domain row
// (e.g. scribe could not transcribe at all). errRetryable is a TRANSIENT miss: the row is
// expected to appear on a later attempt (e.g. distill's batch hasn't reached it yet), so
// the worker re-queues the step instead of failing the item. worker.go branches on these.
var (
	errNoOutputRow = errors.New("worker produced no output row")
	errRetryable   = errors.New("retryable: output not yet available")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// scribe shim (transcrever) — per-item entry: `--source <watch-url> --limit 1`.
// ---------------------------------------------------------------------------

type scribeRunner struct {
	conn *pgx.Conn
	bin  string // SCRIBE_BIN
}

func newScribeRunner(conn *pgx.Conn) *scribeRunner {
	return &scribeRunner{conn: conn, bin: envOr("SCRIBE_BIN", "scribe-job")}
}

func (r *scribeRunner) Run(ctx context.Context, item Item, _ ItemStep) (RunResult, error) {
	// Translate the spine's natural key into scribe's current single-source entrypoint.
	url := "https://www.youtube.com/watch?v=" + item.SourceRef
	cmd := exec.CommandContext(ctx, r.bin, "--source", url, "--limit", "1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return RunResult{}, fmt.Errorf("scribe %s: %w", url, err)
	}
	// Capture the transcript scribe wrote. 'empty' (no speech) is a benign no-content
	// outcome: the row exists but there is nothing to distill, so the item is filtered
	// rather than driven into a distill that must fail. No row at all is a hard failure.
	const q = `SELECT id, status FROM transcripts
	           WHERE youtube_video_id = $1 AND status IN ('done', 'empty')`
	var id int
	var status string
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id, &status); {
	case errors.Is(err, pgx.ErrNoRows):
		return RunResult{}, fmt.Errorf("transcrever %s: %w", item.SourceRef, errNoOutputRow)
	case err != nil:
		return RunResult{}, err
	}
	return RunResult{OutputRef: strconv.Itoa(id), Filtered: status == "empty"}, nil
}

// ---------------------------------------------------------------------------
// distill shim (destilar) — no per-item entry; trigger an idempotent batch drain.
// ---------------------------------------------------------------------------

type distillRunner struct {
	conn      *pgx.Conn
	bin       string // DISTILL_BIN
	batchSize string // forced high so one run drains the pending queue incl. this item
}

func newDistillRunner(conn *pgx.Conn) *distillRunner {
	return &distillRunner{
		conn:      conn,
		bin:       envOr("DISTILL_BIN", "etl-job"),
		batchSize: envOr("DISTILL_DRAIN_BATCH", "100"),
	}
}

func (r *distillRunner) Run(ctx context.Context, item Item, _ ItemStep) (RunResult, error) {
	// distill batch-pulls its own queue; force a large batch so this transcript is
	// included in the single run. Idempotent — re-distilling already-done rows is a no-op.
	cmd := exec.CommandContext(ctx, r.bin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DISTILL_BATCH_SIZE="+r.batchSize)
	if err := cmd.Run(); err != nil {
		return RunResult{}, fmt.Errorf("distill: %w", err)
	}
	// For a YouTube source, distillations.source_key is the youtube_video_id. A missing
	// row is TRANSIENT, not fatal: with a large backlog one drain may not have reached
	// this transcript yet, so re-queue (capped) rather than failing the item.
	const q = `SELECT id FROM distillations WHERE source_key = $1 AND status = 'done'`
	var id int
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return RunResult{}, fmt.Errorf("destilar %s: %w", item.SourceRef, errRetryable)
	case err != nil:
		return RunResult{}, err
	}
	return RunResult{OutputRef: strconv.Itoa(id)}, nil
}

// ---------------------------------------------------------------------------
// pgx SpineSource — the read side of ingest (channel_videos ∪ playlist_videos).
// ---------------------------------------------------------------------------

type pgxSpineSource struct{ conn *pgx.Conn }

// YouTubeVideos returns the deduped union of harvested channel videos and shelved playlist
// videos. A video present in both (or in many playlists) collapses to one row — the spine
// is globally keyed on youtube_video_id.
func (s *pgxSpineSource) YouTubeVideos(ctx context.Context) ([]YouTubeVideo, error) {
	const q = `
		SELECT youtube_video_id, MAX(title) AS title
		FROM (
			SELECT youtube_video_id, title FROM channel_videos
			UNION ALL
			SELECT youtube_video_id, title FROM playlist_videos
		) v
		WHERE youtube_video_id IS NOT NULL AND youtube_video_id <> ''
		GROUP BY youtube_video_id`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YouTubeVideo
	for rows.Next() {
		var v YouTubeVideo
		if err := rows.Scan(&v.VideoID, &v.Title); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
