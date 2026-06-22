// rara-clip is the 2.0 LinkedIn lane collector: a new, isolated agent that collects posts from
// LinkedIn via Bright Data and catalogs each into its own domain table, linkedin_posts. Like
// every rara agent it shares nothing but the Neon database and never calls another agent — the
// control plane (rara-core) reads linkedin_posts to build the items spine (lane=linkedin,
// source_ref=url, sensitivity=public) and the scrub-cloud worker reads body to clean it.
// Idempotent: every run upserts on the canonical post URL, so re-collecting a post converges.
//
// linkedin_posts is a CONTRACT table with TWO producers: this AUTOMATED Bright Data crawl, and
// rara-core's MANUAL inbox (a person pastes a post's URL + text through the surface, kept as a
// fallback for posts the crawl misses). Both write the SAME table behind the SAME URL-idempotent
// contract — multiple producers are fine. rara-clip writes ONLY its domain table; it never
// touches the items spine. Turning linkedin_posts into spine items is rara-core's ingest bridge
// (it reads linkedin_posts the same way it reads emails/podcast_episodes), unchanged by this app.
//
// The Bright Data fetch (a shell-out to the `bdata` CLI, the Bright Data agent skill's tool) lives
// behind a LinkedInCollector seam and the Neon write behind a Database seam; the JSON
// normalization (decodeBrightDataPosts) and the collector loop are pure over those two seams, so
// the whole logic is unit-tested with zero I/O (main_test.go). LinkedIn content is public; this
// agent only stores it, rara-core's router enforces where it may be processed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Environment variable names the collector reads — the single source of truth, shared with the
// tests (which live in package main). The Bright Data API key itself is read by the bdata CLI from
// its own env, so rara-clip never names or handles the credential.
const (
	envDatabaseURL    = "DATABASE_URL"
	envBdataBin       = "BDATA_BIN"
	envBrightDataArgs = "BRIGHTDATA_LINKEDIN_ARGS"
	envBrightDataURLs = "BRIGHTDATA_LINKEDIN_URLS"
)

// LinkedInPost is one collected post: its canonical URL (the spine's natural key), the post text,
// and the author (optional, carried downstream as the gate's "channel" signal).
type LinkedInPost struct {
	URL    string
	Author string
	Text   string
}

// LinkedInCollector is the fetch seam for the automated Bright Data crawl: it yields the current
// batch of posts to catalog. The production implementation (brightDataLinkedInSource) shells out
// to the `bdata` CLI; tests inject a fake so the collector loop runs with zero network I/O. It
// mirrors rara-dial's Fetcher / rara-courier's GmailAPI — the read side of the lane.
type LinkedInCollector interface {
	FetchPosts(ctx context.Context) ([]LinkedInPost, error)
}

// Database is the persistence seam: the idempotent linkedin_posts upsert and the provider stamp.
// The pgx implementation talks to Neon; tests use an in-memory mock.
type Database interface {
	UpsertLinkedInPost(ctx context.Context, p LinkedInPost) error
	// StampProviderCollected records the moment the named provider finished a collection run,
	// keeping rara-core's providers table in sync for scheduling decisions.
	StampProviderCollected(ctx context.Context, name string) error
}

func main() {
	databaseURL := os.Getenv(envDatabaseURL)
	if databaseURL == "" {
		log.Fatalf("%s environment variable is required", envDatabaseURL)
	}
	ctx := context.Background()

	conn := mustConnect(ctx, databaseURL)
	defer conn.Close(ctx)

	n, err := run(ctx, &pgxDatabase{conn: conn}, newBrightDataLinkedInSource())
	if err != nil {
		log.Fatalf("linkedin collector: %v", err)
	}
	log.Printf("LinkedIn job completed: %d posts catalogued", n)
}

// mustConnect opens the Neon connection (bounded by a startup timeout) or exits — the only
// startup I/O before the pure collector loop runs.
func mustConnect(ctx context.Context, databaseURL string) *pgx.Conn {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connectCtx, databaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	log.Println("Connected to database successfully")
	return conn
}

// run is the collector loop: fetch the current batch from Bright Data and catalog each post (see
// catalogPost for the per-post policy). A fetch error IS propagated — it is a real source fault, not
// a per-post quirk — so the whole run aborts. On success the provider stamp is updated so
// rara-core's scheduler sees that this lane finished a collection cycle; a stamp error is logged
// but does not fail the run (the posts are already safely stored). Returns the count of posts stored.
func run(ctx context.Context, db Database, collector LinkedInCollector) (int, error) {
	posts, err := collector.FetchPosts(ctx)
	if err != nil {
		return 0, err
	}
	stored := 0
	for _, p := range posts {
		if catalogPost(ctx, db, p) {
			stored++
		}
	}
	if err := db.StampProviderCollected(ctx, "clip"); err != nil {
		log.Printf("clip: stamp provider: %v", err)
	}
	return stored, nil
}

// catalogPost normalizes one fetched post and upserts it, reporting whether it was stored. A partial
// row (no URL or no real text) is skipped, and a per-post upsert error is logged and skipped — one
// bad row must not stall the crawl. The URL is trimmed before it becomes the idempotency key.
func catalogPost(ctx context.Context, db Database, p LinkedInPost) bool {
	url := strings.TrimSpace(p.URL)
	if url == "" || !postHasContent(p.Text) {
		log.Printf("clip: skipping partial post (url=%q)", url)
		return false
	}
	if err := db.UpsertLinkedInPost(ctx, LinkedInPost{
		URL: url, Author: strings.TrimSpace(p.Author), Text: strings.TrimSpace(p.Text),
	}); err != nil {
		log.Printf("clip: upsert post %s: %v", url, err)
		return false
	}
	return true
}

// reTag matches any HTML tag — the only regex the partial-row check needs.
var reTag = regexp.MustCompile(`(?s)<[^>]+>`)

// postHasContent reports whether a post carries any real text — the collector's storage gate. It
// strips tags and unescapes entities (so a pure-markup body like "<div></div>" or a lone "&nbsp;"
// counts as empty) and checks for any non-whitespace remainder. It is deliberately NOT the
// extractor: rara-clip stores the RAW post and drops only empty rows; the actual to-text
// normalization is the scrub-cloud worker's job (rara-extract), exactly as the email lane stores
// raw bodies. This is rara-clip's own copy — it shares no code with rara-core.
func postHasContent(raw string) bool {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(raw, ""))) != ""
}

// splitList splits a comma- or newline-separated env value into trimmed, non-empty entries.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// firstNonEmpty returns the first argument that is non-empty after trimming.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Bright Data LinkedIn collector — the production LinkedInCollector.
//
// It pulls posts from Bright Data via the `bdata` CLI (the Bright Data agent skill's tool) and
// normalizes them into []LinkedInPost. Like rara-dial's httpFetch this is the only real I/O; the
// decode below is pure, so the unit tests cover it with a fake collector.
//
// Integration contract (config, not code):
//   - BDATA_BIN                  the Bright Data CLI (default "bdata").
//   - BRIGHTDATA_LINKEDIN_ARGS   the pipeline subcommand + flags, space-separated
//                                (default "pipelines linkedin-posts --json"); the input URLs are
//                                appended as trailing args.
//   - BRIGHTDATA_LINKEDIN_URLS   the profile/post URLs to collect (comma- or newline-separated).
//
// The command must print a JSON array of post objects on stdout. Field names are matched flexibly
// (Bright Data's LinkedIn dataset keys vary): url|post_url, author|account|user_id,
// post_text|text|body|headline. The Bright Data API key is read by the CLI from its own env
// (BRIGHTDATA_API_KEY), so rara-clip never handles the credential.
// ---------------------------------------------------------------------------

type brightDataLinkedInSource struct {
	bin  string   // BDATA_BIN
	args []string // BRIGHTDATA_LINKEDIN_ARGS, split
	urls []string // BRIGHTDATA_LINKEDIN_URLS, split
}

// newBrightDataLinkedInSource builds the collector from the environment, applying the defaults for
// the CLI binary and the pipeline args inline.
func newBrightDataLinkedInSource() *brightDataLinkedInSource {
	bin := os.Getenv(envBdataBin)
	if bin == "" {
		bin = "bdata"
	}
	args := os.Getenv(envBrightDataArgs)
	if args == "" {
		args = "pipelines linkedin-posts --json"
	}
	return &brightDataLinkedInSource{
		bin:  bin,
		args: strings.Fields(args),
		urls: splitList(os.Getenv(envBrightDataURLs)),
	}
}

// FetchPosts runs the Bright Data CLI over the configured input URLs and decodes the result.
func (s *brightDataLinkedInSource) FetchPosts(ctx context.Context) ([]LinkedInPost, error) {
	if len(s.urls) == 0 {
		return nil, fmt.Errorf("brightdata linkedin: BRIGHTDATA_LINKEDIN_URLS is empty (nothing to collect)")
	}
	args := append(append([]string{}, s.args...), s.urls...)
	cmd := exec.CommandContext(ctx, s.bin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("brightdata linkedin: %s: %w", s.bin, err)
	}
	return decodeBrightDataPosts(out)
}

// decodeBrightDataPosts parses the CLI's JSON array into normalized posts, matching the dataset's
// varying key names flexibly. A row with neither a URL nor any text is dropped here (so the pure
// run loop never has to); the remaining filtering/idempotency is the loop's.
func decodeBrightDataPosts(raw []byte) ([]LinkedInPost, error) {
	var rows []brightDataPost
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("brightdata linkedin: decode JSON: %w", err)
	}
	out := make([]LinkedInPost, 0, len(rows))
	for _, r := range rows {
		p := LinkedInPost{
			URL:    firstNonEmpty(r.URL, r.PostURL),
			Author: firstNonEmpty(r.Author, r.Account, r.UserID),
			Text:   firstNonEmpty(r.PostText, r.Text, r.Body, r.Headline),
		}
		if p.URL == "" && p.Text == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// brightDataPost mirrors the candidate keys of Bright Data's LinkedIn-post dataset; the normalizer
// above picks the first populated alias for each field.
type brightDataPost struct {
	URL      string `json:"url"`
	PostURL  string `json:"post_url"`
	Author   string `json:"author"`
	Account  string `json:"account"`
	UserID   string `json:"user_id"`
	PostText string `json:"post_text"`
	Text     string `json:"text"`
	Body     string `json:"body"`
	Headline string `json:"headline"`
}

// ---------------------------------------------------------------------------
// pgx Database — Neon implementation of the persistence seam.
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

// UpsertLinkedInPost writes a collected post, idempotent on the canonical URL (a re-collect
// refreshes the author/body in place). This is rara-clip's OWN write — the same linkedin_posts
// contract rara-core's manual inbox upholds. The optional author maps to SQL NULL in-query
// (NULLIF), so an authorless post stores NULL rather than an empty string.
func (d *pgxDatabase) UpsertLinkedInPost(ctx context.Context, p LinkedInPost) error {
	const q = `
		INSERT INTO linkedin_posts (url, author, body)
		VALUES ($1, NULLIF($2, ''), $3)
		ON CONFLICT (url) DO UPDATE SET
			author = EXCLUDED.author,
			body   = EXCLUDED.body`
	_, err := d.conn.Exec(ctx, q, p.URL, p.Author, p.Text)
	return err
}

// StampProviderCollected updates the last_collect_at timestamp for the named provider row so
// rara-core's scheduler sees that this lane finished a collection cycle.
func (d *pgxDatabase) StampProviderCollected(ctx context.Context, name string) error {
	const q = `UPDATE providers SET last_collect_at = now() WHERE name = $1`
	tag, err := d.conn.Exec(ctx, q, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("provider %q not found in providers table", name)
	}
	return nil
}
