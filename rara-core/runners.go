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

// errNoOutputRow signals the worker ran but its domain row is not (yet) present — distinct
// from a hard failure, useful for diagnosis in the logs.
var errNoOutputRow = errors.New("worker produced no output row")

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

func (r *scribeRunner) Run(ctx context.Context, item Item, _ ItemStep) (string, error) {
	// Translate the spine's natural key into scribe's current single-source entrypoint.
	url := "https://www.youtube.com/watch?v=" + item.SourceRef
	cmd := exec.CommandContext(ctx, r.bin, "--source", url, "--limit", "1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("scribe %s: %w", url, err)
	}
	// Capture the transcript row scribe wrote. 'empty' (no speech) still counts as a
	// produced row for the step; the downstream gate/distill handles thin text.
	const q = `SELECT id FROM transcripts
	           WHERE youtube_video_id = $1 AND status IN ('done', 'empty')`
	var id int
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("transcrever %s: %w", item.SourceRef, errNoOutputRow)
	case err != nil:
		return "", err
	}
	return strconv.Itoa(id), nil
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

func (r *distillRunner) Run(ctx context.Context, item Item, _ ItemStep) (string, error) {
	// distill batch-pulls its own queue; force a large batch so this transcript is
	// included in the single run. Idempotent — re-distilling already-done rows is a no-op.
	cmd := exec.CommandContext(ctx, r.bin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DISTILL_BATCH_SIZE="+r.batchSize)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("distill: %w", err)
	}
	// For a YouTube source, distillations.source_key is the youtube_video_id.
	const q = `SELECT id FROM distillations WHERE source_key = $1 AND status = 'done'`
	var id int
	switch err := r.conn.QueryRow(ctx, q, item.SourceRef).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("destilar %s: %w", item.SourceRef, errNoOutputRow)
	case err != nil:
		return "", err
	}
	return strconv.Itoa(id), nil
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
