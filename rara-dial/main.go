// rara-dial is the 2.0 Podcast lane collector: a new, isolated agent that polls
// operator-curated RSS feeds and catalogs each episode (an <item> with an audio enclosure)
// into its own domain table, podcast_episodes. Like every rara agent it shares nothing but the
// Neon database and never calls another agent — the control plane (rara-core) reads
// podcast_episodes to build the items spine, and the asr-direct-audio worker reads
// enclosure_url to transcribe. Idempotent: every run upserts on the episode GUID, so
// re-polling a feed converges.
//
// The RSS parse and the collector loop are pure over two seams — a Fetcher (HTTP) and a
// Database (Neon) — so the whole logic is unit-tested with zero I/O (main_test.go).
package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// httpClient is shared across feed fetches to reuse TCP connections (keep-alive).
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Feed is an RSS feed to poll.
type Feed struct {
	ID      int
	FeedURL string
	Title   string
}

// Episode is one collected podcast episode (an RSS item with an audio enclosure).
type Episode struct {
	GUID         string
	Title        string
	EnclosureURL string
	PublishedAt  *time.Time
	Description  string // itunes:summary if present, else <description>; may be empty
}

// Fetcher retrieves the raw bytes of a feed URL. The HTTP implementation is httpFetch; tests
// inject a fake so the collector loop runs with zero network I/O.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// Database is the persistence seam: the active feeds to poll plus the idempotent episode
// upsert. The pgx implementation talks to Neon; tests use an in-memory mock.
type Database interface {
	ActiveFeeds(ctx context.Context) ([]Feed, error)
	UpsertEpisode(ctx context.Context, feedID int, e Episode) error
	SetFeedTitle(ctx context.Context, feedID int, title string) error
}

// parsePublishedFloor parses the PODCAST_MIN_PUBLISHED env var (ISO date like "2025-07-01").
// Returns nil if the env var is empty (no floor), or a time.Time pointer if valid.
// An invalid date string returns an error.
//
// The floor is midnight UTC of the given day. Episode pubDates carry their own zone, so the
// comparison is timezone-aware (compares instants). At the exact boundary an episode dated for
// the floor day but in a zone behind UTC can land just under it — immaterial for a coarse
// back-catalog cutoff, where day-granularity is the intent.
func parsePublishedFloor(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, fmt.Errorf("PODCAST_MIN_PUBLISHED must be ISO date (YYYY-MM-DD), got %q: %v", s, err)
	}
	return &t, nil
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	publishedFloor, err := parsePublishedFloor(os.Getenv("PODCAST_MIN_PUBLISHED"))
	if err != nil {
		log.Fatalf("invalid PODCAST_MIN_PUBLISHED: %v", err)
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := pgx.Connect(connectCtx, databaseURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())
	log.Println("Connected to database successfully")

	n, err := runWithFloor(context.Background(), &pgxDatabase{conn: conn}, httpFetch, publishedFloor)
	if err != nil {
		log.Fatalf("podcast collector: %v", err)
	}
	log.Printf("Podcast job completed: %d episodes catalogued", n)
}

// runWithFloor is the collector loop: for each active feed, fetch + parse the RSS, refresh
// the feed title, and upsert every audio episode. Episodes with published_at before the
// floor are skipped (if floor is set); episodes without a date are always kept.
// A per-feed error is logged and the loop continues — one bad feed must not stall the others.
// Returns the total episodes catalogued (not counting skipped).
func runWithFloor(ctx context.Context, db Database, fetch Fetcher, floor *time.Time) (int, error) {
	feeds, err := db.ActiveFeeds(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, f := range feeds {
		// Give each feed its own timeout so a slow feed cannot hang the whole run.
		feedCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		data, err := fetch(feedCtx, f.FeedURL)
		if err != nil {
			cancel()
			log.Printf("fetch feed %s: %v", f.FeedURL, err)
			continue
		}
		title, episodes, err := parseRSS(data)
		if err != nil {
			cancel()
			log.Printf("parse feed %s: %v", f.FeedURL, err)
			continue
		}
		if title != "" && title != f.Title {
			if err := db.SetFeedTitle(feedCtx, f.ID, title); err != nil {
				log.Printf("set feed title %s: %v", f.FeedURL, err)
			}
		}
		catalogued := 0
		skipped := 0
		for _, e := range episodes {
			// Skip episodes before the floor (if set) only if their published_at is known.
			if floor != nil && e.PublishedAt != nil && e.PublishedAt.Before(*floor) {
				skipped++
				continue
			}
			if err := db.UpsertEpisode(feedCtx, f.ID, e); err != nil {
				log.Printf("upsert episode %s (feed %s): %v", e.GUID, f.FeedURL, err)
				continue
			}
			catalogued++
		}
		cancel()
		if skipped > 0 {
			log.Printf("Feed %q: catalogued %d, skipped %d (before %s)", title, catalogued, skipped, floor.Format("2006-01-02"))
		} else {
			log.Printf("Feed %q: catalogued %d/%d episodes", title, catalogued, len(episodes))
		}
		total += catalogued
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// RSS parsing — pure (no I/O), so it is fully unit-tested.
// ---------------------------------------------------------------------------

// rssDoc is the minimal RSS 2.0 shape the collector needs.
type rssDoc struct {
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	GUID          string `xml:"guid"`
	Title         string `xml:"title"`
	PubDate       string `xml:"pubDate"`
	Description   string `xml:"description"`
	ItunesSummary string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	Enclosure     struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
}

// parseRSS extracts the channel title and the audio episodes from an RSS feed. Only items
// carrying an audio enclosure are kept (a podcast episode IS its audio). An item with no GUID
// falls back to its enclosure URL as the stable id (the spine needs a stable source_ref).
func parseRSS(data []byte) (title string, episodes []Episode, err error) {
	var doc rssDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil, err
	}
	for _, it := range doc.Channel.Items {
		url := strings.TrimSpace(it.Enclosure.URL)
		if url == "" || !isAudioEnclosure(it.Enclosure.Type) {
			continue // not an audio episode
		}
		guid := strings.TrimSpace(it.GUID)
		if guid == "" {
			guid = url // stable fallback so the episode still gets a source_ref
		}
		desc := strings.TrimSpace(it.ItunesSummary)
		if desc == "" {
			desc = strings.TrimSpace(it.Description)
		}
		episodes = append(episodes, Episode{
			GUID:         guid,
			Title:        strings.TrimSpace(it.Title),
			EnclosureURL: url,
			PublishedAt:  parsePubDate(it.PubDate),
			Description:  desc,
		})
	}
	return strings.TrimSpace(doc.Channel.Title), episodes, nil
}

// isAudioEnclosure reports whether an enclosure type denotes audio. An empty type is accepted
// (best-effort: many feeds carry the URL without a precise MIME type); a non-empty type must
// start with "audio".
func isAudioEnclosure(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	return mime == "" || strings.HasPrefix(mime, "audio")
}

// pubDateLayouts are the date formats podcast feeds use, tried in order. RSS prescribes
// RFC1123Z, but feeds in the wild also use RFC1123 (named zone) and a few variants.
var pubDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2006-01-02T15:04:05Z07:00", // RFC3339, some feeds use it
}

// parsePubDate parses an RSS pubDate, returning nil on an empty or unrecognized value (a
// missing date is not an error — the episode is still catalogued).
func parsePubDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// httpFetch is the production Fetcher: an HTTP GET of the feed URL.
func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rara-dial/1.0 (+https://github.com/RenatoBardi/rara)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed returned status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ---------------------------------------------------------------------------
// pgx Database — Neon implementation of the persistence seam.
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

func (d *pgxDatabase) ActiveFeeds(ctx context.Context) ([]Feed, error) {
	const q = `SELECT id, feed_url, COALESCE(title, '') FROM podcast_feeds WHERE active = true ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		var f Feed
		if err := rows.Scan(&f.ID, &f.FeedURL, &f.Title); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertEpisode inserts an episode, idempotent on guid. On conflict it refreshes the metadata
// (title/enclosure/published/description) so a feed edit propagates, but never the collected_at/status.
func (d *pgxDatabase) UpsertEpisode(ctx context.Context, feedID int, e Episode) error {
	const q = `
		INSERT INTO podcast_episodes (feed_id, guid, title, enclosure_url, published_at, description)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (guid) DO UPDATE
		SET title         = EXCLUDED.title,
		    enclosure_url = EXCLUDED.enclosure_url,
		    published_at  = EXCLUDED.published_at,
		    description   = EXCLUDED.description`
	var desc *string
	if e.Description != "" {
		desc = &e.Description
	}
	_, err := d.conn.Exec(ctx, q, feedID, e.GUID, e.Title, e.EnclosureURL, e.PublishedAt, desc)
	return err
}

func (d *pgxDatabase) SetFeedTitle(ctx context.Context, feedID int, title string) error {
	const q = `UPDATE podcast_feeds SET title = $2 WHERE id = $1`
	_, err := d.conn.Exec(ctx, q, feedID, title)
	return err
}
