// runners.go — the I/O edge (pgx + CLI) of the roles the core still runs in-process.
//
// rara-core no longer runs a `work` role: every domain worker — transcrever (rara-transcribe), destilar
// (rara-distill), the curation gates (rara-gate) and the to-text extractor (rara-extract) — is its own
// sovereign app on the rara-addon SDK, claiming its steps through the Neon contract. The
// interest_profile reviser is likewise gone — it moved out to rara-hone, a periodic systemd-timer
// job that PROPOSES profile revisions; the core keeps only the human APPROVAL (the surface). What
// remains here is the orchestrator's own I/O edges: the read sides of ingest (channel_videos /
// podcast / email) and the LinkedIn stash write. They are deliberately minimal glue,
// exercised by real deploys/integration, not unit tests — the pure logic each backs is what the
// unit tests cover. The AUTOMATED LinkedIn collector (Bright Data) is no longer here either: it is
// its own producer app, rara-clip.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// transcrever, destilar, the curation gates and extrair have NO runner here: each is its own
// independent app on the rara-addon SDK (rara-transcribe, rara-distill, rara-gate, rara-extract), claiming
// its steps through the Neon contract. The orchestrator still ROUTES every capability and ACTIVATES
// the assigned provider (Cloud Run `run` / tailnet poke); it never executes the work itself.

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

// ---------------------------------------------------------------------------
// pgx PodcastSource — the read side of podcast ingest (podcast_episodes).
// ---------------------------------------------------------------------------

type pgxPodcastSource struct{ conn *pgx.Conn }

// PodcastEpisodes returns every collected episode that carries a stable GUID. The spine is
// keyed on (lane=podcast, source_ref=guid); the collector (rara-dial) owns the table.
func (s *pgxPodcastSource) PodcastEpisodes(ctx context.Context) ([]PodcastEpisode, error) {
	const q = `
		SELECT guid, COALESCE(title, '')
		FROM podcast_episodes
		WHERE guid IS NOT NULL AND guid <> ''`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PodcastEpisode
	for rows.Next() {
		var e PodcastEpisode
		if err := rows.Scan(&e.GUID, &e.Title); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// pgx EmailSource — the read side of email ingest (emails).
// ---------------------------------------------------------------------------

type pgxEmailSource struct{ conn *pgx.Conn }

// Emails returns every collected email that carries a message id. The spine is keyed on
// (lane=email, source_ref=message_id); the collector (rara-courier) owns the table.
func (s *pgxEmailSource) Emails(ctx context.Context) ([]EmailItem, error) {
	const q = `
		SELECT message_id, COALESCE(subject, '')
		FROM emails
		WHERE message_id IS NOT NULL AND message_id <> ''`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailItem
	for rows.Next() {
		var e EmailItem
		if err := rows.Scan(&e.MessageID, &e.Subject); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// pgx NewsSource — the read side of news ingest (news_items).
// ---------------------------------------------------------------------------

type pgxNewsSource struct{ conn *pgx.Conn }

// News returns every collected feed article that carries a url. The spine is keyed on
// (lane=news, source_ref=url); the collector (rara-feed) owns the table (HN/RSS/html).
func (s *pgxNewsSource) News(ctx context.Context) ([]NewsItem, error) {
	const q = `
		SELECT url, COALESCE(title, '')
		FROM news_items
		WHERE url IS NOT NULL AND url <> ''`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query news_items: %w", err)
	}
	defer rows.Close()
	var out []NewsItem
	for rows.Next() {
		var a NewsItem
		if err := rows.Scan(&a.URL, &a.Title); err != nil {
			return nil, fmt.Errorf("scan news_items row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// pgx LinkedInPostStore — the write side of the stash collector (linkedin_posts).
//
// The manual inbox lives inside the surface (a person pastes a post through an MCP tool / HTTP
// endpoint), so rara-core writes linkedin_posts directly here. It is a CONTRACT table: the
// AUTOMATED Bright Data collector is its own app, rara-clip, which writes the SAME table behind
// the SAME url-idempotent contract — multiple producers, one table. The flow and extractor never
// change regardless of which producer filled a row.
// ---------------------------------------------------------------------------

type pgxLinkedInInbox struct{ conn pgConn }

func newPgxLinkedInInbox(conn pgConn) *pgxLinkedInInbox { return &pgxLinkedInInbox{conn: conn} }

// UpsertLinkedInPost writes the submitted post, idempotent on the canonical URL (a
// resubmission refreshes the author/body in place).
func (s *pgxLinkedInInbox) UpsertLinkedInPost(ctx context.Context, p LinkedInPost) error {
	const q = `
		INSERT INTO linkedin_posts (url, author, body)
		VALUES ($1, $2, $3)
		ON CONFLICT (url) DO UPDATE SET
			author = EXCLUDED.author,
			body   = EXCLUDED.body`
	_, err := s.conn.Exec(ctx, q, p.URL, nullStr(p.Author), p.Text)
	return err
}

// The AUTOMATED Bright Data LinkedIn collector is no longer here: it is its own producer app,
// rara-clip, which shells out to the `bdata` CLI, normalizes the dataset's varying keys, and writes
// the SAME linkedin_posts contract table behind the SAME url-idempotent contract. rara-core keeps
// only the stash write (above) and the linkedin_posts -> spine bridge (SubmitLinkedInPost's
// DiscoverItem), both unchanged. rara-clip writes ONLY the domain table; it never touches the spine.

// ---------------------------------------------------------------------------
// pgx LinkedInSource — the read side of bulk LinkedIn ingest (linkedin_posts).
//
// Both producers (stash and rara-clip) write the same table behind the same
// url-idempotent contract; this source covers all of them in one SELECT.
// ---------------------------------------------------------------------------

type pgxLinkedInSource struct{ conn *pgx.Conn }

// LinkedInPosts returns every row in linkedin_posts that carries a url. The spine is keyed on
// (lane=linkedin, source_ref=url); both producers (stash and rara-clip) write the same table.
func (s *pgxLinkedInSource) LinkedInPosts(ctx context.Context) ([]LinkedInPost, error) {
	const q = `
		SELECT url, COALESCE(author, '')
		FROM linkedin_posts
		WHERE url IS NOT NULL AND url <> ''`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query linkedin_posts: %w", err)
	}
	defer rows.Close()
	var out []LinkedInPost
	for rows.Next() {
		var p LinkedInPost
		if err := rows.Scan(&p.URL, &p.Author); err != nil {
			return nil, fmt.Errorf("scan linkedin_posts row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
