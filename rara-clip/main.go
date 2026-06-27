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
// Target profile URLs come from target_linkedin_profiles in the DB (not an env var).
const (
	envDatabaseURL        = "DATABASE_URL"
	envBdataBin           = "BDATA_BIN"
	envBrightDataArgs     = "BRIGHTDATA_LINKEDIN_ARGS"
	envBrightDataCoArgs   = "BRIGHTDATA_COMPANY_ARGS"
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

// Database is the persistence seam: the idempotent linkedin_posts upsert, the provider stamp,
// and the operator-managed target profile list. The pgx implementation talks to Neon; tests use
// an in-memory mock.
type Database interface {
	// FetchTargetProfiles returns the active, non-deleted LinkedIn profile URLs from
	// target_linkedin_profiles — the operator-managed list configured via the Fontes UI.
	FetchTargetProfiles(ctx context.Context) ([]string, error)
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

	providerName := os.Getenv("CLIP_PROVIDER")
	if providerName == "" {
		providerName = "clip-cloud"
	}

	db := &pgxDatabase{conn: conn}
	n, err := collectLinkedIn(ctx, db, providerName)
	if err != nil {
		log.Fatalf("linkedin collector: %v", err)
	}
	log.Printf("LinkedIn job completed: %d posts catalogued", n)
}

// partitionURLs splits profile URLs by type: /company/ and /showcase/ go to companies; everything
// else (expected to be /in/ person profiles) goes to persons.
func partitionURLs(urls []string) (persons, companies []string) {
	for _, u := range urls {
		if strings.Contains(u, "/company/") || strings.Contains(u, "/showcase/") {
			companies = append(companies, u)
		} else {
			persons = append(persons, u)
		}
	}
	return
}

// collectLinkedIn fetches active profiles from the DB and collects posts from each. Person profiles
// (/in/) use linkedin_person_profile; company/showcase pages use linkedin_company_profile. Both are
// collected before the provider is stamped so a partial failure leaves the stamp unset.
func collectLinkedIn(ctx context.Context, db Database, providerName string) (int, error) {
	urls, err := db.FetchTargetProfiles(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetch target profiles: %w", err)
	}
	if len(urls) == 0 {
		log.Printf("clip: no active profiles in target_linkedin_profiles, skipping")
		return 0, nil
	}
	persons, companies := partitionURLs(urls)
	total := 0
	if len(persons) > 0 {
		n, err := store(ctx, db, newBrightDataLinkedInSource(persons))
		if err != nil {
			return total, fmt.Errorf("person profiles: %w", err)
		}
		total += n
	}
	if len(companies) > 0 {
		n, err := store(ctx, db, newBrightDataCompanySource(companies))
		if err != nil {
			return total, fmt.Errorf("company profiles: %w", err)
		}
		total += n
	}
	if err := db.StampProviderCollected(ctx, providerName); err != nil {
		log.Printf("clip: stamp provider: %v", err)
	}
	return total, nil
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

// store fetches posts from collector and catalogs each one. Separated from run so collectLinkedIn
// can call multiple collectors (person + company) and stamp once after all succeed.
func store(ctx context.Context, db Database, collector LinkedInCollector) (int, error) {
	posts, err := collector.FetchPosts(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range posts {
		if catalogPost(ctx, db, p) {
			n++
		}
	}
	return n, nil
}

// run fetches, stores, and stamps the provider — convenience wrapper for a single-collector path.
func run(ctx context.Context, db Database, collector LinkedInCollector, providerName string) (int, error) {
	n, err := store(ctx, db, collector)
	if err != nil {
		return 0, err
	}
	if err := db.StampProviderCollected(ctx, providerName); err != nil {
		log.Printf("clip: stamp provider: %v", err)
	}
	return n, nil
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
//                                (default "pipelines linkedin_person_profile --json"); the input
//                                profile URLs are appended as trailing args.
//
// The command must print a JSON array of profile objects, each containing a "posts" array with
// "title", "attribution" (excerpt), and "link" fields — the linkedin_person_profile dataset shape.
// The Bright Data API key is read by the CLI from its own env (BRIGHTDATA_API_KEY), so rara-clip
// never handles the credential.
// ---------------------------------------------------------------------------

type brightDataLinkedInSource struct {
	bin  string   // BDATA_BIN
	args []string // BRIGHTDATA_LINKEDIN_ARGS, split
	urls []string // BRIGHTDATA_LINKEDIN_URLS, split
}

// newBrightDataLinkedInSource builds the collector. urls come from the DB (target_linkedin_profiles)
// and are passed by the caller; BDATA_BIN and BRIGHTDATA_LINKEDIN_ARGS still read from env for
// tool configuration.
func newBrightDataLinkedInSource(urls []string) *brightDataLinkedInSource {
	bin := os.Getenv(envBdataBin)
	if bin == "" {
		bin = "bdata"
	}
	args := os.Getenv(envBrightDataArgs)
	if args == "" {
		args = "pipelines linkedin_person_profile --json"
	}
	return &brightDataLinkedInSource{
		bin:  bin,
		args: strings.Fields(args),
		urls: urls,
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

// decodeBrightDataPosts parses the linkedin_person_profile pipeline output — an array of profile
// objects each carrying a "posts" sub-array — into a flat []LinkedInPost. Posts with neither a
// link nor any text are dropped; the remaining filtering and idempotency are the run loop's job.
func decodeBrightDataPosts(raw []byte) ([]LinkedInPost, error) {
	var profiles []brightDataProfile
	if err := json.Unmarshal(raw, &profiles); err != nil {
		return nil, fmt.Errorf("brightdata linkedin: decode JSON: %w", err)
	}
	var out []LinkedInPost
	for _, prof := range profiles {
		for _, p := range prof.Posts {
			text := firstNonEmpty(p.Title, p.Attribution)
			if p.Title != "" && p.Attribution != "" {
				text = p.Title + "\n\n" + p.Attribution
			}
			if p.Link == "" && text == "" {
				continue
			}
			out = append(out, LinkedInPost{URL: p.Link, Author: prof.Name, Text: text})
		}
	}
	return out, nil
}

// brightDataProfile is the top-level object from the linkedin_person_profile pipeline:
// one entry per requested profile URL, each containing the profile's name and its recent posts.
type brightDataProfile struct {
	Name  string                  `json:"name"`
	Posts []brightDataProfilePost `json:"posts"`
}

// brightDataProfilePost is one post entry inside a brightDataProfile.
type brightDataProfilePost struct {
	Title       string `json:"title"`
	Attribution string `json:"attribution"` // excerpt / first paragraph
	Link        string `json:"link"`
}

// ---------------------------------------------------------------------------
// Bright Data company collector — linkedin_company_profile pipeline.
// ---------------------------------------------------------------------------

type brightDataCompanySource struct {
	bin  string
	args []string
	urls []string
}

func newBrightDataCompanySource(urls []string) *brightDataCompanySource {
	bin := os.Getenv(envBdataBin)
	if bin == "" {
		bin = "bdata"
	}
	args := os.Getenv(envBrightDataCoArgs)
	if args == "" {
		args = "pipelines linkedin_company_profile --json"
	}
	return &brightDataCompanySource{bin: bin, args: strings.Fields(args), urls: urls}
}

func (s *brightDataCompanySource) FetchPosts(ctx context.Context) ([]LinkedInPost, error) {
	if len(s.urls) == 0 {
		return nil, fmt.Errorf("brightdata company: no URLs to collect")
	}
	args := append(append([]string{}, s.args...), s.urls...)
	cmd := exec.CommandContext(ctx, s.bin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("brightdata company: %s: %w", s.bin, err)
	}
	return decodeCompanyProfiles(out)
}

// decodeCompanyProfiles parses linkedin_company_profile output — an array of company objects each
// carrying an "updates" sub-array — into a flat []LinkedInPost.
func decodeCompanyProfiles(raw []byte) ([]LinkedInPost, error) {
	var profiles []brightDataCompanyProfile
	if err := json.Unmarshal(raw, &profiles); err != nil {
		return nil, fmt.Errorf("brightdata company: decode JSON: %w", err)
	}
	var out []LinkedInPost
	for _, prof := range profiles {
		for _, u := range prof.Updates {
			text := firstNonEmpty(u.Text, u.Title)
			if u.PostURL == "" && text == "" {
				continue
			}
			out = append(out, LinkedInPost{URL: u.PostURL, Author: prof.Name, Text: text})
		}
	}
	return out, nil
}

type brightDataCompanyProfile struct {
	Name    string                    `json:"name"`
	Updates []brightDataCompanyUpdate `json:"updates"`
}

type brightDataCompanyUpdate struct {
	PostURL string `json:"post_url"`
	Text    string `json:"text"`
	Title   string `json:"title"`
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

// FetchTargetProfiles returns active, non-deleted LinkedIn profile URLs from
// target_linkedin_profiles — the operator list managed via the Fontes UI.
func (d *pgxDatabase) FetchTargetProfiles(ctx context.Context) ([]string, error) {
	const q = `SELECT profile_url FROM target_linkedin_profiles WHERE active = true AND deleted_at IS NULL`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query target_linkedin_profiles: %w", err)
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan profile_url: %w", err)
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate target_linkedin_profiles: %w", err)
	}
	return urls, nil
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
