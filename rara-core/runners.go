// runners.go — the I/O edge (pgx + CLI) of the roles the core still runs in-process.
//
// rara-core no longer runs a `work` role: every domain worker — transcrever (rara-scribe), destilar
// (rara-distill), the curation gates (rara-sift) and the to-text extractor (rara-glean) — is its own
// sovereign app on the rara-addon SDK, claiming its steps through the Neon contract. What remains
// here is the orchestrator's own I/O edges: the read sides of ingest (channel_videos / podcast /
// email), the LinkedIn manual-inbox write, and the reviser's distillation read + LiteLLM narrator.
// They are deliberately minimal glue, exercised by real deploys/integration, not unit tests — the
// pure logic each backs is what the unit tests cover. The AUTOMATED LinkedIn collector (Bright
// Data) is no longer here: it is its own producer app, rara-clip.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// transcrever, destilar, the curation gates and extrair have NO runner here: each is its own
// independent app on the rara-addon SDK (rara-scribe, rara-distill, rara-sift, rara-glean), claiming
// its steps through the Neon contract. The orchestrator still ROUTES every capability and ACTIVATES
// the assigned provider (Cloud Run `run` / tailnet poke); it never executes the work itself.

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
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
// pgx LinkedInPostStore — the write side of the manual-inbox collector (linkedin_posts).
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
// only the manual-inbox write (above) and the linkedin_posts -> spine bridge (SubmitLinkedInPost's
// DiscoverItem), both unchanged. rara-clip writes ONLY the domain table; it never touches the spine.

// ---------------------------------------------------------------------------
// pgx DistillationResolver — the read side of the reviser's attribution (Phase 6).
//
// Reads a distillation's `structured` JSONB by id (cross-agent SELECT, no FK — the 1.0 isolation
// convention) and parses out its concepts/entities/author via the pure parseDistillationStructured.
// Integration-only glue; the parser is what the unit tests cover.
// ---------------------------------------------------------------------------

type pgxDistillationResolver struct{ conn *pgx.Conn }

func newPgxDistillationResolver(conn *pgx.Conn) *pgxDistillationResolver {
	return &pgxDistillationResolver{conn: conn}
}

func (r *pgxDistillationResolver) Resolve(ctx context.Context, distillationID string) (DistillationStructured, bool, error) {
	const q = `SELECT COALESCE(structured::text, '{}') FROM distillations WHERE id = $1`
	var raw string
	switch err := r.conn.QueryRow(ctx, q, distillationID).Scan(&raw); {
	case errors.Is(err, pgx.ErrNoRows):
		return DistillationStructured{}, false, nil
	case err != nil:
		return DistillationStructured{}, false, err
	}
	return parseDistillationStructured([]byte(raw)), true, nil
}

// ---------------------------------------------------------------------------
// LiteLLM narrator — the prose half of the hybrid reviser (Phase 6).
//
// Writes ONLY the natural-language narrative of a proposed profile (the gate LLM-judge's
// context); it never touches a structured field — the deterministic engine owns those. Same
// gateway/dialect as the gate judge (LITELLM_*), so the model is a swappable config value.
// Integration-only; a failure is non-fatal (ReviseProfile falls back to a template narrative).
// ---------------------------------------------------------------------------

type liteLLMNarrator struct {
	apiKey   string
	model    string
	endpoint string
	client   *http.Client
}

func newLiteLLMNarrator() (*liteLLMNarrator, error) {
	base := os.Getenv("LITELLM_BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("LITELLM_BASE_URL is required for the profile narrator")
	}
	return &liteLLMNarrator{
		apiKey:   os.Getenv("LITELLM_API_KEY"),
		model:    envOr("LITELLM_MODEL", "claude-sonnet-4-6"),
		endpoint: strings.TrimRight(base, "/") + "/chat/completions",
		client:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (n *liteLLMNarrator) Narrate(ctx context.Context, old, proposed revisedStructured) (string, error) {
	reqBody := map[string]any{
		"model": n.model,
		"messages": []any{
			map[string]any{"role": "system", "content": narratorSystemPrompt()},
			map[string]any{"role": "user", "content": narratorUserPrompt(old, proposed)},
		},
		"temperature": 0.3,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+n.apiKey)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("litellm narrator: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var lr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", err
	}
	if len(lr.Choices) == 0 {
		return "", fmt.Errorf("litellm narrator returned no choices")
	}
	return lr.Choices[0].Message.Content, nil
}

// narratorSystemPrompt frames the narrative task and FORBIDS inventing structure — the LLM
// describes only the terms it is given; the deterministic engine already decided them.
func narratorSystemPrompt() string {
	return "You write a short natural-language summary of a personal content-curation interest " +
		"profile, to be used as CONTEXT for a curation judge. Describe, in 2-4 sentences, what the " +
		"reader is interested in (topics, authors) and what they want to avoid (anti-topics). " +
		"Use ONLY the lists you are given — never add, infer, or invent a topic, author, or " +
		"interest that is not listed. Output prose only, no JSON, no lists."
}

// narratorUserPrompt presents the proposed structured profile (and the prior one for contrast).
// These lists are the deterministic engine's output — the narrator describes, never edits, them.
func narratorUserPrompt(old, proposed revisedStructured) string {
	var b strings.Builder
	b.WriteString("Proposed profile to summarize:\n")
	b.WriteString("- Topics: " + joinOrNone(proposed.Topics) + "\n")
	b.WriteString("- Authors: " + joinOrNone(proposed.Authors) + "\n")
	b.WriteString("- Anti-topics: " + joinOrNone(proposed.AntiTopics) + "\n")
	b.WriteString(fmt.Sprintf("- keep_threshold: %.2f\n\n", proposed.KeepThreshold))
	b.WriteString("Previous profile (for contrast only):\n")
	b.WriteString("- Topics: " + joinOrNone(old.Topics) + "\n")
	b.WriteString("- Authors: " + joinOrNone(old.Authors) + "\n")
	b.WriteString("- Anti-topics: " + joinOrNone(old.AntiTopics) + "\n")
	return b.String()
}
