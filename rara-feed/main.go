package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source types (feed_sources.source_type / news_items.source_type).
const (
	sourceRSS  = "rss"
	sourceHN   = "hn"
	sourceHTML = "html"
)

// news_items.status values.
const (
	statusReady  = "ready"  // ready for the distill upstream
	statusFailed = "failed" // no usable text could be captured
)

// news_items.fetch_status values (full-text coverage observability).
const (
	fetchFull    = "full"    // body captured (inline or fetched)
	fetchExcerpt = "excerpt" // only the source-provided excerpt
	fetchFailed  = "failed"  // full-text was attempted and failed
)

const (
	defaultBatchSize      = 25
	defaultHNMinPoints    = 20
	defaultItemMaxAgeDays = 30
	defaultMaxRetries     = 4

	// feedUserAgent identifies the collector to upstream sites (some block empty UAs).
	feedUserAgent = "rara-feed/1.0 (+https://github.com/renatobardi/rara)"
)

// fetchRetryBase is the base backoff for transient (429/5xx) fetch retries when the
// response carries no Retry-After header. A var so tests can shrink it.
var fetchRetryBase = 2 * time.Second

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// FeedSource is one enabled row of feed_sources: a place to discover items from.
type FeedSource struct {
	ID            int
	Name          string
	SourceType    string // rss | html | hn
	Endpoint      string // feed/page url, or HN search term
	Cls           string // badge carried onto the item (e.g. b-openai)
	FetchStrategy string // http | unlocker (v1 honours only http)
	Parser        string // "" = generic extractor; name = bespoke (reserved)
}

// FeedEntry is one item discovered from a source, before persistence. Summary is
// the source-provided excerpt; Content is full body when the source ships it inline
// (RSS content:encoded, Atom content, or an HTML article body).
type FeedEntry struct {
	Title     string
	Link      string
	Published time.Time // zero when the source gives no parseable date
	Summary   string
	Content   string
}

// NewsItem is one row of the news_items table.
type NewsItem struct {
	Source        string
	Cls           string
	SourceType    string
	URL           string
	Title         string
	PublishedAt   time.Time
	Excerpt       string
	Body          string
	FetchStatus   string
	ContentSHA256 string
	Status        string
	Error         string
}

// Config is the runtime configuration, sourced from environment variables.
type Config struct {
	DatabaseURL    string
	BatchSize      int      // max items taken per source
	FullText       bool     // best-effort full-text fetch when the feed has no inline body
	SourcesFilter  []string // subset of source names (empty = all)
	HNMinPoints    int
	ItemMaxAgeDays int
	Now            func() time.Time // injectable clock (tests); nil = time.Now
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// Interfaces (the seams that make the pipeline unit-testable with zero I/O)
// ---------------------------------------------------------------------------

// Database is the persistence seam. The real implementation talks to Neon; the
// tests use an in-memory mock that mirrors the SQL uniqueness/staleness contract.
type Database interface {
	EnabledSources(ctx context.Context) ([]FeedSource, error)
	GetItem(ctx context.Context, url string) (NewsItem, bool, error)
	SaveItem(ctx context.Context, it NewsItem) error
}

// Fetcher is the HTTP seam — the single point where the cheap HTTP tier and (later)
// the Bright Data unlocker tier are swapped by fetch_strategy. v1 ships only HTTP.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// ---------------------------------------------------------------------------
// Pure helpers (directly unit-tested)
// ---------------------------------------------------------------------------

// contentSHA256 hashes title + text into the staleness key for an item.
func contentSHA256(title, text string) string {
	h := sha256.Sum256([]byte(title + "\n" + text))
	return hex.EncodeToString(h[:])
}

// withinAge reports whether a publish date falls inside the age window. An unknown
// (zero) date is kept — we cannot judge it. A future date (clock skew) is kept too.
func withinAge(published time.Time, maxAgeDays int, now time.Time) bool {
	if published.IsZero() {
		return true
	}
	age := now.Sub(published)
	if age < 0 {
		return true
	}
	return age <= time.Duration(maxAgeDays)*24*time.Hour
}

var timeLayouts = []string{
	time.RFC1123Z,                    // Wed, 04 Jun 2025 12:00:00 +0000
	time.RFC1123,                     // Wed, 04 Jun 2025 12:00:00 GMT
	time.RFC3339,                     // 2025-06-03T09:00:00Z
	"Mon, 2 Jan 2006 15:04:05 -0700", // single-digit day variant
	time.RFC822Z,
	time.RFC822,
	"2006-01-02",
}

// parseTime parses the common feed date formats, returning ok=false when none match.
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// hnSearchURL builds the HN Algolia search-by-date URL for a term, filtering by a
// minimum points threshold server-side.
func hnSearchURL(term string, minPoints int) string {
	v := url.Values{}
	v.Set("query", term)
	v.Set("tags", "story")
	v.Set("numericFilters", fmt.Sprintf("points>%d", minPoints))
	v.Set("hitsPerPage", "10")
	return "https://hn.algolia.com/api/v1/search_by_date?" + v.Encode()
}

// hnPermalink is the canonical HN item URL, used as the natural key when an HN story
// has no external url (Ask HN / text posts).
func hnPermalink(objectID string) string {
	return "https://news.ycombinator.com/item?id=" + objectID
}

// --- Feed parsing (RSS 2.0 + Atom) ---

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
	Encoded     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"` // content:encoded
}

type rssFeed struct {
	XMLName xml.Name  `xml:"rss"`
	Items   []rssItem `xml:"channel>item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Links     []atomLink `xml:"link"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

// parseFeed parses an RSS 2.0 or Atom feed into entries, auto-detecting the format
// by root element.
func parseFeed(data []byte) ([]FeedEntry, error) {
	var rss rssFeed
	if err := xml.Unmarshal(data, &rss); err == nil && rss.XMLName.Local == "rss" {
		out := make([]FeedEntry, 0, len(rss.Items))
		for _, it := range rss.Items {
			pub, _ := parseTime(it.PubDate)
			out = append(out, FeedEntry{
				Title:     strings.TrimSpace(it.Title),
				Link:      strings.TrimSpace(it.Link),
				Published: pub,
				Summary:   strings.TrimSpace(it.Description),
				Content:   strings.TrimSpace(it.Encoded),
			})
		}
		return out, nil
	}

	var atom atomFeed
	if err := xml.Unmarshal(data, &atom); err == nil && atom.XMLName.Local == "feed" {
		out := make([]FeedEntry, 0, len(atom.Entries))
		for _, e := range atom.Entries {
			pub, _ := parseTime(e.Published)
			if pub.IsZero() {
				pub, _ = parseTime(e.Updated)
			}
			out = append(out, FeedEntry{
				Title:     strings.TrimSpace(e.Title),
				Link:      pickAtomLink(e.Links),
				Published: pub,
				Summary:   strings.TrimSpace(e.Summary),
				Content:   strings.TrimSpace(e.Content),
			})
		}
		return out, nil
	}

	return nil, fmt.Errorf("unrecognized feed format (neither RSS nor Atom)")
}

// pickAtomLink prefers the alternate (or relation-less) link, falling back to the
// first link present.
func pickAtomLink(links []atomLink) string {
	var first, alt string
	for _, l := range links {
		if first == "" {
			first = strings.TrimSpace(l.Href)
		}
		if (l.Rel == "alternate" || l.Rel == "") && alt == "" {
			alt = strings.TrimSpace(l.Href)
		}
	}
	if alt != "" {
		return alt
	}
	return first
}

// --- Hacker News (Algolia) parsing ---

type hnHit struct {
	Title      string  `json:"title"`
	URL        *string `json:"url"` // null for Ask HN / text posts
	ObjectID   string  `json:"objectID"`
	CreatedAtI int64   `json:"created_at_i"`
	Points     int     `json:"points"`
}

type hnResponse struct {
	Hits []hnHit `json:"hits"`
}

// parseHN parses an Algolia search response into entries, filtering below-threshold
// stories and falling back to the HN permalink when a story has no external url.
func parseHN(data []byte, minPoints int) ([]FeedEntry, error) {
	var r hnResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	out := make([]FeedEntry, 0, len(r.Hits))
	for _, h := range r.Hits {
		if h.Points < minPoints {
			continue
		}
		link := ""
		if h.URL != nil {
			link = strings.TrimSpace(*h.URL)
		}
		if link == "" {
			link = hnPermalink(h.ObjectID)
		}
		var pub time.Time
		if h.CreatedAtI > 0 {
			pub = time.Unix(h.CreatedAtI, 0)
		}
		out = append(out, FeedEntry{
			Title:     strings.TrimSpace(h.Title),
			Link:      link,
			Published: pub,
		})
	}
	return out, nil
}

// --- Generic HTML extractor (JSON-LD only in v1) ---

// JSONLDArticle is the subset of a schema.org Article we care about.
type JSONLDArticle struct {
	Title     string
	Body      string
	URL       string
	Published time.Time
}

type jsonLDRaw struct {
	Headline      string `json:"headline"`
	Name          string `json:"name"`
	ArticleBody   string `json:"articleBody"`
	DatePublished string `json:"datePublished"`
	URL           string `json:"url"`
}

// extractJSONLD scans an HTML page for the first usable schema.org Article embedded
// as <script type="application/ld+json">. Returns ok=false when none is found — the
// honest v1 behaviour for pages that hide their content behind JS/CSS.
func extractJSONLD(html []byte) (JSONLDArticle, bool) {
	s := string(html)
	const marker = "application/ld+json"
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			break
		}
		gt := strings.IndexByte(s[i:], '>')
		if gt < 0 {
			break
		}
		start := i + gt + 1
		end := strings.Index(s[start:], "</script>")
		if end < 0 {
			break
		}
		block := strings.TrimSpace(s[start : start+end])
		s = s[start+end+len("</script>"):]

		if art, ok := parseJSONLDBlock(block); ok {
			return art, true
		}
	}
	return JSONLDArticle{}, false
}

// parseJSONLDBlock decodes one ld+json block (object, array, or {"@graph":[...]})
// and returns the first Article-like node.
func parseJSONLDBlock(block string) (JSONLDArticle, bool) {
	var candidates []jsonLDRaw
	switch {
	case strings.HasPrefix(block, "["):
		_ = json.Unmarshal([]byte(block), &candidates)
	default:
		var single jsonLDRaw
		if json.Unmarshal([]byte(block), &single) == nil {
			candidates = append(candidates, single)
		}
		var graph struct {
			Graph []jsonLDRaw `json:"@graph"`
		}
		if json.Unmarshal([]byte(block), &graph) == nil {
			candidates = append(candidates, graph.Graph...)
		}
	}
	for _, c := range candidates {
		title := c.Headline
		if title == "" {
			title = c.Name
		}
		if title == "" && c.ArticleBody == "" {
			continue
		}
		pub, _ := parseTime(c.DatePublished)
		return JSONLDArticle{
			Title:     strings.TrimSpace(title),
			Body:      strings.TrimSpace(c.ArticleBody),
			URL:       strings.TrimSpace(c.URL),
			Published: pub,
		}, true
	}
	return JSONLDArticle{}, false
}

// ---------------------------------------------------------------------------
// Orchestration (fetcher/database-agnostic; unit-tested via mocks)
// ---------------------------------------------------------------------------

// runBatch walks every enabled source, discovers its items, and upserts each into
// news_items. A source that fails to fetch is logged and skipped so it cannot bring
// down the rest of the batch.
func runBatch(ctx context.Context, db Database, fetch Fetcher, cfg Config) error {
	sources, err := db.EnabledSources(ctx)
	if err != nil {
		return fmt.Errorf("failed to load feed sources: %w", err)
	}
	sources = filterSources(sources, cfg.SourcesFilter)
	if len(sources) == 0 {
		log.Println("No enabled feed sources to process")
		return nil
	}
	now := cfg.now()
	log.Printf("Processing %d feed source(s)\n", len(sources))

	saved, skippedSources := 0, 0
	// Per-source-type yield: items upserted and how many are distillable. html index
	// pages and HN permalink posts often yield title-only rows in v1, so this makes the
	// unlocker follow-up data-driven instead of a guess.
	type yield struct{ upserted, distillable int }
	byType := map[string]*yield{}

	for _, src := range sources {
		entries, err := discover(ctx, fetch, src, cfg)
		if err != nil {
			// A blocked/JS/timeout source is recorded and skipped — the batch goes on.
			log.Printf("Source %q failed: %v (continuing)\n", src.Name, err)
			skippedSources++
			continue
		}

		// Drop unusable (no url) and out-of-window entries BEFORE the per-source cap,
		// so the cap counts items we'd actually store — not stale ones we'd discard.
		fresh := make([]FeedEntry, 0, len(entries))
		for _, e := range entries {
			if e.Link == "" {
				continue // no natural key — cannot dedupe/store
			}
			if !withinAge(e.Published, cfg.ItemMaxAgeDays, now) {
				continue
			}
			fresh = append(fresh, e)
		}
		if cfg.BatchSize > 0 && len(fresh) > cfg.BatchSize {
			fresh = fresh[:cfg.BatchSize]
		}

		yt := byType[src.SourceType]
		if yt == nil {
			yt = &yield{}
			byType[src.SourceType] = yt
		}
		for _, e := range fresh {
			item, wrote, err := processEntry(ctx, db, fetch, src, e, cfg)
			if err != nil {
				log.Printf("Warning: failed to save %q: %v\n", e.Link, err)
				continue
			}
			if !wrote {
				continue
			}
			saved++
			yt.upserted++
			if distillable(item) {
				yt.distillable++
			}
		}
	}

	for _, st := range []string{sourceRSS, sourceHN, sourceHTML} {
		if yt := byType[st]; yt != nil {
			log.Printf("Yield %-4s: %d upserted, %d distillable\n", st, yt.upserted, yt.distillable)
		}
	}
	log.Printf("Batch complete: %d item(s) upserted, %d source(s) skipped\n", saved, skippedSources)
	return nil
}

// filterSources keeps only sources whose name is in the (case-insensitive) filter;
// an empty filter keeps everything.
func filterSources(sources []FeedSource, filter []string) []FeedSource {
	if len(filter) == 0 {
		return sources
	}
	want := make(map[string]bool, len(filter))
	for _, f := range filter {
		want[strings.ToLower(strings.TrimSpace(f))] = true
	}
	out := make([]FeedSource, 0, len(sources))
	for _, s := range sources {
		if want[strings.ToLower(s.Name)] {
			out = append(out, s)
		}
	}
	return out
}

// discover fetches a source and parses it into entries, per source_type.
func discover(ctx context.Context, fetch Fetcher, src FeedSource, cfg Config) ([]FeedEntry, error) {
	switch src.SourceType {
	case sourceRSS:
		raw, err := fetch.Fetch(ctx, src.Endpoint)
		if err != nil {
			return nil, err
		}
		return parseFeed(raw)
	case sourceHN:
		raw, err := fetch.Fetch(ctx, hnSearchURL(src.Endpoint, cfg.HNMinPoints))
		if err != nil {
			return nil, err
		}
		return parseHN(raw, cfg.HNMinPoints)
	case sourceHTML:
		raw, err := fetch.Fetch(ctx, src.Endpoint)
		if err != nil {
			return nil, err
		}
		art, ok := extractJSONLD(raw)
		if !ok {
			return nil, nil // no structured data — nothing to store (until unlocker/bespoke)
		}
		link := art.URL
		if link == "" {
			link = src.Endpoint
		}
		return []FeedEntry{{Title: art.Title, Link: link, Published: art.Published, Content: art.Body}}, nil
	default:
		return nil, fmt.Errorf("unknown source_type %q", src.SourceType)
	}
}

// processEntry builds and upserts one news item, attempting best-effort full-text
// when the source shipped no inline body. Returns the resolved item and whether a
// write happened (false when the item is unchanged from a previous run — staleness
// skip).
func processEntry(ctx context.Context, db Database, fetch Fetcher, src FeedSource, e FeedEntry, cfg Config) (NewsItem, bool, error) {
	body := e.Content
	fetchStatus := fetchExcerpt
	if body != "" {
		fetchStatus = fetchFull
	}

	existing, found, err := db.GetItem(ctx, e.Link)
	if err != nil {
		return NewsItem{}, false, err
	}

	// Best-effort full-text only when we have no inline body yet.
	if body == "" && cfg.FullText {
		switch {
		case found && existing.FetchStatus == fetchFull && existing.Body != "":
			body, fetchStatus = existing.Body, fetchFull // reuse prior success; don't refetch
		case found && existing.Status == statusReady &&
			existing.ContentSHA256 == contentSHA256(e.Title, e.Summary):
			// Already settled on the excerpt and the feed's signal is unchanged:
			// skip the otherwise-every-run, never-succeeding full-text fetch.
			return existing, false, nil
		default:
			if raw, ferr := fetch.Fetch(ctx, e.Link); ferr == nil {
				if art, ok := extractJSONLD(raw); ok && art.Body != "" {
					body, fetchStatus = art.Body, fetchFull
				} else {
					fetchStatus = fetchFailed
				}
			} else {
				fetchStatus = fetchFailed
			}
		}
	}

	text := body
	if text == "" {
		text = e.Summary
	}
	status := statusReady
	if e.Title == "" && e.Summary == "" && body == "" {
		status = statusFailed // nothing for the distill to curate
	}

	item := NewsItem{
		Source:        src.Name,
		Cls:           src.Cls,
		SourceType:    src.SourceType,
		URL:           e.Link,
		Title:         e.Title,
		PublishedAt:   e.Published,
		Excerpt:       e.Summary,
		Body:          body,
		FetchStatus:   fetchStatus,
		ContentSHA256: contentSHA256(e.Title, text),
		Status:        status,
	}

	// Idempotency: an unchanged, already-ready item is left untouched.
	if found && existing.ContentSHA256 == item.ContentSHA256 && existing.Status == statusReady {
		return item, false, nil
	}
	if err := db.SaveItem(ctx, item); err != nil {
		return NewsItem{}, false, err
	}
	return item, true, nil
}

// distillable reports whether a stored item carries text the distill news lane will
// actually pick up (a non-empty body or excerpt). Title-only rows — common for html
// index pages and HN permalink/text posts in v1 — are stored but not yet distillable;
// runBatch tallies this so the unlocker follow-up is driven by real coverage numbers.
func distillable(it NewsItem) bool {
	return it.Status == statusReady &&
		(strings.TrimSpace(it.Body) != "" || strings.TrimSpace(it.Excerpt) != "")
}

// ---------------------------------------------------------------------------
// Real fetcher: HTTP with transient-error retry
// ---------------------------------------------------------------------------

// HTTPFetcher is the cheap tier: a direct GET with retry on transient errors.
type HTTPFetcher struct {
	client     *http.Client
	maxRetries int
}

func newHTTPFetcher(timeout time.Duration, maxRetries int) *HTTPFetcher {
	return &HTTPFetcher{
		client: &http.Client{
			Timeout: timeout,
			// A public URL can 30x to an internal address (SSRF): re-validate every
			// redirect hop and cap the chain.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return validateFetchTarget(req.URL.String())
			},
		},
		maxRetries: maxRetries,
	}
}

func (f *HTTPFetcher) Fetch(ctx context.Context, target string) ([]byte, error) {
	if err := validateFetchTarget(target); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", feedUserAgent)

		resp, err := f.client.Do(req)
		if err != nil {
			return nil, err // transport error — not retried (matches the other agents)
		}

		if resp.StatusCode == http.StatusOK {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			return body, rerr
		}

		body, _ := io.ReadAll(resp.Body)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		lastErr = fmt.Errorf("fetch %s: status %d: %s", target, resp.StatusCode, truncate(string(body), 300))

		// Retry only transient failures (429 rate limit, 5xx). Other 4xx are permanent.
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !transient || attempt >= f.maxRetries {
			return nil, lastErr
		}
		wait := retryAfter
		if wait <= 0 {
			wait = fetchRetryBase << attempt // 2s, 4s, 8s, 16s
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// parseRetryAfter reads a Retry-After header in delta-seconds form. Returns 0 when
// absent/unparseable, so the caller falls back to exponential backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

// validateFetchTarget is the SSRF guard. The fetcher follows links that come from
// feed *content* (RSS item links, HN external urls, JSON-LD url) — attacker-influenceable
// — so we reject non-http(s) schemes and any host that is (or resolves to) a private,
// loopback, link-local or unspecified address before issuing the request.
func validateFetchTarget(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", target, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("disallowed url scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url %q has no host", target)
	}
	// IP literal: check directly (no DNS). Hostname: resolve and check every result.
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("disallowed (non-public) address %s", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("%s resolves to non-public address %s", host, ip)
		}
	}
	return nil
}

// isPublicIP reports whether an IP is routable on the public internet (not loopback,
// private, link-local, multicast or unspecified).
func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified())
}

// truncate caps a string to max runes for logging, appending an ellipsis when cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ---------------------------------------------------------------------------
// Real database: Neon PostgreSQL via pgx
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

func (d *pgxDatabase) EnabledSources(ctx context.Context) ([]FeedSource, error) {
	const q = `
		SELECT id, name, source_type, endpoint, cls, fetch_strategy, COALESCE(parser, '')
		FROM feed_sources
		WHERE enabled = true
		ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FeedSource
	for rows.Next() {
		var s FeedSource
		if err := rows.Scan(&s.ID, &s.Name, &s.SourceType, &s.Endpoint, &s.Cls, &s.FetchStrategy, &s.Parser); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *pgxDatabase) GetItem(ctx context.Context, target string) (NewsItem, bool, error) {
	const q = `SELECT content_sha256, status, fetch_status, COALESCE(body, '') FROM news_items WHERE url = $1`
	it := NewsItem{URL: target}
	err := d.conn.QueryRow(ctx, q, target).Scan(&it.ContentSHA256, &it.Status, &it.FetchStatus, &it.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return NewsItem{}, false, nil
	}
	if err != nil {
		return NewsItem{}, false, err
	}
	return it, true, nil
}

// SaveItem upserts a news item. Idempotent on url: a re-run replaces the row and
// (on a failed item) increments attempt_count, resetting it on a ready save.
func (d *pgxDatabase) SaveItem(ctx context.Context, it NewsItem) error {
	initialAttempt := 0
	if it.Status == statusFailed {
		initialAttempt = 1
	}
	const upsert = `
		INSERT INTO news_items
			(source, cls, source_type, url, title, published_at, excerpt, body,
			 fetch_status, content_sha256, status, error, attempt_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (url) DO UPDATE SET
			source         = EXCLUDED.source,
			cls            = EXCLUDED.cls,
			source_type    = EXCLUDED.source_type,
			title          = EXCLUDED.title,
			published_at   = EXCLUDED.published_at,
			excerpt        = EXCLUDED.excerpt,
			body           = EXCLUDED.body,
			fetch_status   = EXCLUDED.fetch_status,
			content_sha256 = EXCLUDED.content_sha256,
			status         = EXCLUDED.status,
			error          = EXCLUDED.error,
			attempt_count  = CASE WHEN EXCLUDED.status = 'failed'
			                      THEN news_items.attempt_count + 1
			                      ELSE 0 END,
			updated_at     = CURRENT_TIMESTAMP`
	_, err := d.conn.Exec(ctx, upsert,
		it.Source,
		it.Cls,
		it.SourceType,
		it.URL,
		nullStr(it.Title),
		nullTime(it.PublishedAt),
		nullStr(it.Excerpt),
		nullStr(it.Body),
		it.FetchStatus,
		it.ContentSHA256,
		it.Status,
		nullStr(it.Error),
		initialAttempt,
	)
	return err
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------------------
// Config & entrypoint
// ---------------------------------------------------------------------------

func loadConfig() Config {
	return Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		BatchSize:      envInt("FEED_BATCH_SIZE", defaultBatchSize),
		FullText:       envBool("FEED_FULLTEXT", true),
		SourcesFilter:  splitCSV(os.Getenv("FEED_SOURCES_FILTER")),
		HNMinPoints:    envInt("HN_MIN_POINTS", defaultHNMinPoints),
		ItemMaxAgeDays: envInt("ITEM_MAX_AGE_DAYS", defaultItemMaxAgeDays),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return def
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	cfg := loadConfig()
	if cfg.DatabaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := pgx.Connect(connectCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())
	log.Println("Connected to database successfully")

	db := &pgxDatabase{conn: conn}
	timeout := time.Duration(envInt("FEED_HTTP_TIMEOUT", 30)) * time.Second
	fetch := newHTTPFetcher(timeout, envInt("FEED_MAX_RETRIES", defaultMaxRetries))

	if err := runBatch(context.Background(), db, fetch, cfg); err != nil {
		log.Fatalf("Batch failed: %v", err)
	}
	log.Println("Feed job completed successfully")
}
