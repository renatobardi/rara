package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures (embedded; zero I/O)
// ---------------------------------------------------------------------------

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>OpenAI</title>
    <item>
      <title>GPT-5 released</title>
      <link>https://openai.com/blog/gpt-5</link>
      <pubDate>Wed, 04 Jun 2025 12:00:00 +0000</pubDate>
      <description>Short excerpt.</description>
      <content:encoded>Full article body here.</content:encoded>
    </item>
  </channel>
</rss>`

const atomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Google DeepMind</title>
  <entry>
    <title>AlphaProof</title>
    <link rel="alternate" href="https://deepmind.google/blog/alphaproof"/>
    <published>2025-06-03T09:00:00Z</published>
    <summary>Atom excerpt.</summary>
    <content>Atom full content.</content>
  </entry>
</feed>`

// HN Algolia hits: a normal story, an Ask-HN/text post with null url (must fall
// back to the item permalink), and a below-threshold story (must be filtered).
const hnFixture = `{"hits":[
  {"title":"Anthropic raises a round","url":"https://techcrunch.com/anthropic","objectID":"111","created_at_i":1749045600,"points":150},
  {"title":"Ask HN: thoughts on Claude?","url":null,"objectID":"222","created_at_i":1749045600,"points":80},
  {"title":"Barely mentioned","url":"https://example.com/low","objectID":"333","created_at_i":1749045600,"points":5}
]}`

const jsonLDFixture = `<!doctype html><html><head>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"NewsArticle","headline":"Claude 4 launch","datePublished":"2025-06-05T10:00:00Z","articleBody":"The full article body.","url":"https://www.anthropic.com/news/claude-4"}
</script>
</head><body><p>ignored</p></body></html>`

const cssOnlyFixture = `<!doctype html><html><head><title>No structured data</title></head>
<body><article class="post"><h1>Some Title</h1><p>Some body.</p></article></body></html>`

// hnEpoch is the created_at_i used in hnFixture; tests anchor their clock to it so
// the HN items fall inside the age window deterministically.
var hnEpoch = time.Unix(1749045600, 0)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestParseFeedRSS(t *testing.T) {
	entries, err := parseFeed([]byte(rssFixture))
	if err != nil {
		t.Fatalf("parseFeed(rss) error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Title != "GPT-5 released" {
		t.Errorf("title = %q", e.Title)
	}
	if e.Link != "https://openai.com/blog/gpt-5" {
		t.Errorf("link = %q", e.Link)
	}
	if e.Summary != "Short excerpt." {
		t.Errorf("summary = %q", e.Summary)
	}
	if e.Content != "Full article body here." {
		t.Errorf("content = %q", e.Content)
	}
	if e.Published.IsZero() || e.Published.UTC().Format("2006-01-02") != "2025-06-04" {
		t.Errorf("published = %v, want 2025-06-04", e.Published)
	}
}

func TestParseFeedAtom(t *testing.T) {
	entries, err := parseFeed([]byte(atomFixture))
	if err != nil {
		t.Fatalf("parseFeed(atom) error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Title != "AlphaProof" {
		t.Errorf("title = %q", e.Title)
	}
	if e.Link != "https://deepmind.google/blog/alphaproof" {
		t.Errorf("link = %q", e.Link)
	}
	if e.Summary != "Atom excerpt." {
		t.Errorf("summary = %q", e.Summary)
	}
	if e.Content != "Atom full content." {
		t.Errorf("content = %q", e.Content)
	}
	if e.Published.UTC().Format("2006-01-02") != "2025-06-03" {
		t.Errorf("published = %v, want 2025-06-03", e.Published)
	}
}

func TestParseHN(t *testing.T) {
	entries, err := parseHN([]byte(hnFixture), 20)
	if err != nil {
		t.Fatalf("parseHN error: %v", err)
	}
	// 150 and 80 points survive; 5 points is below the threshold.
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (min points filter)", len(entries))
	}
	if entries[0].Link != "https://techcrunch.com/anthropic" {
		t.Errorf("entry0 link = %q", entries[0].Link)
	}
	// The null-url Ask HN post must fall back to the HN item permalink.
	if entries[1].Link != "https://news.ycombinator.com/item?id=222" {
		t.Errorf("entry1 link = %q, want HN permalink", entries[1].Link)
	}
	if entries[1].Title != "Ask HN: thoughts on Claude?" {
		t.Errorf("entry1 title = %q", entries[1].Title)
	}
	if entries[0].Published.Unix() != hnEpoch.Unix() {
		t.Errorf("entry0 published = %v, want %v", entries[0].Published, hnEpoch)
	}
}

func TestExtractJSONLD(t *testing.T) {
	art, ok := extractJSONLD([]byte(jsonLDFixture))
	if !ok {
		t.Fatal("expected JSON-LD article to be found")
	}
	if art.Title != "Claude 4 launch" {
		t.Errorf("title = %q", art.Title)
	}
	if art.Body != "The full article body." {
		t.Errorf("body = %q", art.Body)
	}
	if art.URL != "https://www.anthropic.com/news/claude-4" {
		t.Errorf("url = %q", art.URL)
	}
	if art.Published.UTC().Format("2006-01-02") != "2025-06-05" {
		t.Errorf("published = %v", art.Published)
	}

	// CSS-only page (no JSON-LD) must report not-found, not panic.
	if _, ok := extractJSONLD([]byte(cssOnlyFixture)); ok {
		t.Error("expected no JSON-LD article in CSS-only page")
	}
}

func TestContentSHA256(t *testing.T) {
	a := contentSHA256("Title", "body")
	if len(a) != 64 {
		t.Errorf("sha length = %d, want 64", len(a))
	}
	if a != contentSHA256("Title", "body") {
		t.Error("contentSHA256 not deterministic")
	}
	if a == contentSHA256("Title", "body changed") {
		t.Error("contentSHA256 must change when text changes")
	}
	if a == contentSHA256("Title changed", "body") {
		t.Error("contentSHA256 must change when title changes")
	}
}

func TestWithinAge(t *testing.T) {
	now := time.Date(2025, 6, 7, 0, 0, 0, 0, time.UTC)
	recent := now.AddDate(0, 0, -3)
	old := now.AddDate(0, 0, -40)
	if !withinAge(recent, 30, now) {
		t.Error("recent item should be within age")
	}
	if withinAge(old, 30, now) {
		t.Error("old item should be outside age")
	}
	// Unknown publish date (zero) is kept — we cannot judge its age.
	if !withinAge(time.Time{}, 30, now) {
		t.Error("zero publish date should be kept")
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]string{
		"Wed, 04 Jun 2025 12:00:00 +0000": "2025-06-04",
		"Wed, 04 Jun 2025 12:00:00 GMT":   "2025-06-04",
		"2025-06-03T09:00:00Z":            "2025-06-03",
	}
	for in, want := range cases {
		got, ok := parseTime(in)
		if !ok {
			t.Errorf("parseTime(%q) failed", in)
			continue
		}
		if got.UTC().Format("2006-01-02") != want {
			t.Errorf("parseTime(%q) = %v, want %s", in, got, want)
		}
	}
	if _, ok := parseTime(""); ok {
		t.Error("empty time should not parse")
	}
	if _, ok := parseTime("not a date"); ok {
		t.Error("garbage time should not parse")
	}
}

func TestHNSearchURL(t *testing.T) {
	got := hnSearchURL("Anthropic", 20)
	for _, want := range []string{"query=Anthropic", "tags=story", "points%3E20", "search_by_date"} {
		if !strings.Contains(got, want) {
			t.Errorf("hnSearchURL missing %q in %q", want, got)
		}
	}
}

func TestDistillable(t *testing.T) {
	cases := []struct {
		name string
		it   NewsItem
		want bool
	}{
		{"body", NewsItem{Status: "ready", Body: "full text"}, true},
		{"excerpt only", NewsItem{Status: "ready", Excerpt: "snippet"}, true},
		{"title only", NewsItem{Status: "ready", Title: "headline"}, false},
		{"blank text", NewsItem{Status: "ready", Body: "   "}, false},
		{"failed with body", NewsItem{Status: "failed", Body: "x"}, false},
	}
	for _, c := range cases {
		if got := distillable(c.it); got != c.want {
			t.Errorf("%s: distillable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidateFetchTarget(t *testing.T) {
	// SSRF guard: private/loopback/link-local targets and non-http(s) schemes are
	// rejected. Only IP literals are used so the test needs no DNS (zero I/O).
	bad := []string{
		"http://127.0.0.1/x",      // loopback
		"http://10.0.0.1/x",       // private
		"http://192.168.1.1/",     // private
		"http://169.254.169.254/", // link-local (cloud metadata)
		"http://[::1]/",           // IPv6 loopback
		"https://0.0.0.0/",        // unspecified
		"ftp://example.com/x",     // scheme
		"file:///etc/passwd",      // scheme
		"http:///nohost",          // no host
	}
	for _, u := range bad {
		if err := validateFetchTarget(u); err == nil {
			t.Errorf("validateFetchTarget(%q) = nil, want error", u)
		}
	}
	for _, u := range []string{"http://1.1.1.1/", "https://93.184.216.34/path"} {
		if err := validateFetchTarget(u); err != nil {
			t.Errorf("validateFetchTarget(%q) = %v, want nil", u, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// MockDatabase mirrors the SQL contract: idempotent on url (UNIQUE), and
// SaveItem upserts. saveCount counts actual writes so tests can assert that
// staleness skips re-writing unchanged items.
type MockDatabase struct {
	sources   []FeedSource
	items     map[string]NewsItem // keyed by url
	saveCount int
	saveErr   error
	srcErr    error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{items: make(map[string]NewsItem)}
}

func (m *MockDatabase) EnabledSources(ctx context.Context) ([]FeedSource, error) {
	if m.srcErr != nil {
		return nil, m.srcErr
	}
	return m.sources, nil
}

func (m *MockDatabase) GetItem(ctx context.Context, url string) (NewsItem, bool, error) {
	it, ok := m.items[url]
	return it, ok, nil
}

func (m *MockDatabase) SaveItem(ctx context.Context, it NewsItem) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.items[it.URL] = it // upsert: replaces, mirroring ON CONFLICT (url) DO UPDATE
	m.saveCount++
	return nil
}

// MockFetcher returns preconfigured bytes per URL, or a per-URL error. calls counts
// fetches per URL so tests can assert that a never-succeeding full-text URL is not
// re-fetched on every run.
type MockFetcher struct {
	bodies map[string][]byte
	errs   map[string]error
	calls  map[string]int
}

func newMockFetcher() *MockFetcher {
	return &MockFetcher{bodies: make(map[string][]byte), errs: make(map[string]error), calls: make(map[string]int)}
}

func (f *MockFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	f.calls[url]++
	if err, ok := f.errs[url]; ok {
		return nil, err
	}
	if b, ok := f.bodies[url]; ok {
		return b, nil
	}
	return nil, errors.New("mock: no body for " + url)
}

// ---------------------------------------------------------------------------
// Fluent harness
// ---------------------------------------------------------------------------

type FeedHarness struct {
	t     *testing.T
	db    *MockDatabase
	fetch *MockFetcher
	cfg   Config
}

func NewFeedHarness(t *testing.T) *FeedHarness {
	return &FeedHarness{
		t:     t,
		db:    newMockDatabase(),
		fetch: newMockFetcher(),
		cfg: Config{
			BatchSize:      25,
			FullText:       true,
			HNMinPoints:    20,
			ItemMaxAgeDays: 30,
			Now:            func() time.Time { return hnEpoch.Add(24 * time.Hour) },
		},
	}
}

func (h *FeedHarness) WithSource(s FeedSource) *FeedHarness {
	h.db.sources = append(h.db.sources, s)
	return h
}

func (h *FeedHarness) WithFetch(url, body string) *FeedHarness {
	h.fetch.bodies[url] = []byte(body)
	return h
}

func (h *FeedHarness) WithFetchError(url string, err error) *FeedHarness {
	h.fetch.errs[url] = err
	return h
}

func (h *FeedHarness) Execute() error {
	return runBatch(context.Background(), h.db, h.fetch, h.cfg)
}

func (h *FeedHarness) get(url string) (NewsItem, bool) {
	it, ok := h.db.items[url]
	return it, ok
}

func (h *FeedHarness) fetchCount(url string) int { return h.fetch.calls[url] }

func (h *FeedHarness) AssertItemCount(want int) {
	h.t.Helper()
	if len(h.db.items) != want {
		h.t.Errorf("item count = %d, want %d", len(h.db.items), want)
	}
}

func (h *FeedHarness) AssertItem(url string, check func(NewsItem)) {
	h.t.Helper()
	it, ok := h.get(url)
	if !ok {
		h.t.Errorf("item %q not found", url)
		return
	}
	check(it)
}

// ---------------------------------------------------------------------------
// Orchestration tests
// ---------------------------------------------------------------------------

func rssSource() FeedSource {
	return FeedSource{ID: 1, Name: "OpenAI", SourceType: "rss",
		Endpoint: "https://openai.com/rss.xml", Cls: "b-openai", FetchStrategy: "http"}
}

// A feed that ships full content (content:encoded) needs no extra full-text fetch.
func TestBatchRSSWithInlineContent(t *testing.T) {
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, rssFixture)
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(1)
	h.AssertItem("https://openai.com/blog/gpt-5", func(it NewsItem) {
		if it.Status != "ready" {
			t.Errorf("status = %q, want ready", it.Status)
		}
		if it.FetchStatus != "full" {
			t.Errorf("fetch_status = %q, want full (inline content:encoded)", it.FetchStatus)
		}
		if it.Body != "Full article body here." {
			t.Errorf("body = %q", it.Body)
		}
		if it.Source != "OpenAI" || it.Cls != "b-openai" || it.SourceType != "rss" {
			t.Errorf("provenance wrong: %+v", it)
		}
		if it.ContentSHA256 == "" {
			t.Error("content_sha256 empty")
		}
	})
}

// HN: the null-url Ask-HN post is stored under its permalink as the natural key.
func TestBatchHNPermalinkSaved(t *testing.T) {
	src := FeedSource{ID: 2, Name: "Hacker News", SourceType: "hn",
		Endpoint: "Anthropic", Cls: "b-hn", FetchStrategy: "http"}
	h := NewFeedHarness(t).WithSource(src).
		WithFetch(hnSearchURL("Anthropic", 20), hnFixture)
	// HN has no inline body; full-text fetch of each link is attempted but not
	// configured here, so items land as excerpt/failed — still saved.
	h.cfg.FullText = false
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(2) // 150 + 80 points; 5 filtered out
	h.AssertItem("https://news.ycombinator.com/item?id=222", func(it NewsItem) {
		if it.Status != "ready" {
			t.Errorf("status = %q", it.Status)
		}
		if it.Title != "Ask HN: thoughts on Claude?" {
			t.Errorf("title = %q", it.Title)
		}
	})
}

// Two sources surfacing the SAME url collapse to one row (UNIQUE(url) dedupe).
func TestBatchDedupeByURL(t *testing.T) {
	rss := rssSource()
	// A second RSS source whose single item points at the same canonical URL.
	dupFeed := strings.Replace(rssFixture, "OpenAI", "Mirror", 1)
	dup := FeedSource{ID: 9, Name: "Mirror", SourceType: "rss",
		Endpoint: "https://mirror.example/rss.xml", Cls: "b-mirror", FetchStrategy: "http"}
	h := NewFeedHarness(t).
		WithSource(rss).WithFetch(rss.Endpoint, rssFixture).
		WithSource(dup).WithFetch(dup.Endpoint, dupFeed)
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(1) // same url → one row
}

// Re-running with unchanged content does not re-write the row (staleness skip).
func TestBatchStalenessSkipsUnchanged(t *testing.T) {
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, rssFixture)
	if err := h.Execute(); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	first := h.db.saveCount
	if first == 0 {
		t.Fatal("expected at least one save on first run")
	}
	if err := h.Execute(); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if h.db.saveCount != first {
		t.Errorf("re-run wrote again: saveCount %d -> %d (should be stable)", first, h.db.saveCount)
	}
}

// A source that fails to fetch is skipped; the rest of the batch still runs.
func TestBatchSourceFailureContinues(t *testing.T) {
	bad := FeedSource{ID: 3, Name: "Broken", SourceType: "rss",
		Endpoint: "https://broken.example/rss.xml", Cls: "b-broken", FetchStrategy: "http"}
	good := rssSource()
	h := NewFeedHarness(t).
		WithSource(bad).WithFetchError(bad.Endpoint, errors.New("timeout")).
		WithSource(good).WithFetch(good.Endpoint, rssFixture)
	if err := h.Execute(); err != nil {
		t.Fatalf("execute must not fail the whole batch: %v", err)
	}
	h.AssertItemCount(1) // only the good source produced an item
}

// Full-text fetch failure degrades to excerpt with fetch_status='failed', but the
// item is still persisted as 'ready'.
func TestBatchFullTextFailKeepsExcerpt(t *testing.T) {
	// An RSS feed with only a description (no content:encoded), so full-text is
	// attempted against the article link — which errors here.
	feed := strings.Replace(rssFixture,
		"<content:encoded>Full article body here.</content:encoded>", "", 1)
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, feed).
		WithFetchError("https://openai.com/blog/gpt-5", errors.New("403 blocked"))
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItem("https://openai.com/blog/gpt-5", func(it NewsItem) {
		if it.FetchStatus != "failed" {
			t.Errorf("fetch_status = %q, want failed", it.FetchStatus)
		}
		if it.Status != "ready" {
			t.Errorf("status = %q, want ready (excerpt still usable)", it.Status)
		}
		if it.Excerpt != "Short excerpt." {
			t.Errorf("excerpt = %q", it.Excerpt)
		}
		if it.Body != "" {
			t.Errorf("body should be empty on full-text failure, got %q", it.Body)
		}
	})
}

// Full-text fetch success fills the body via JSON-LD and marks fetch_status='full'.
func TestBatchFullTextSuccess(t *testing.T) {
	feed := strings.Replace(rssFixture,
		"<content:encoded>Full article body here.</content:encoded>", "", 1)
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, feed).
		WithFetch("https://openai.com/blog/gpt-5", jsonLDFixture)
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItem("https://openai.com/blog/gpt-5", func(it NewsItem) {
		if it.FetchStatus != "full" {
			t.Errorf("fetch_status = %q, want full", it.FetchStatus)
		}
		if it.Body != "The full article body." {
			t.Errorf("body = %q", it.Body)
		}
	})
}

// Items older than the age window are discarded.
func TestBatchAgeWindowDiscardsOld(t *testing.T) {
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, rssFixture)
	// Move the clock far past the item so it falls outside the 30-day window.
	h.cfg.Now = func() time.Time { return time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC) }
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(0)
}

// An entry with no title, excerpt, or body is stored with status='failed'.
func TestBatchStatusFailedNoText(t *testing.T) {
	emptyFeed := `<?xml version="1.0"?>
<rss version="2.0"><channel><title>OpenAI</title>
<item><link>https://openai.com/blog/empty</link></item>
</channel></rss>`
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, emptyFeed)
	h.cfg.FullText = false
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItem("https://openai.com/blog/empty", func(it NewsItem) {
		if it.Status != "failed" {
			t.Errorf("status = %q, want failed (no usable text)", it.Status)
		}
	})
}

// FEED_SOURCES_FILTER restricts the run to the named sources.
func TestBatchSourcesFilter(t *testing.T) {
	a := rssSource()
	b := FeedSource{ID: 4, Name: "DeepMind", SourceType: "rss",
		Endpoint: "https://deepmind.google/rss.xml", Cls: "b-deepmind", FetchStrategy: "http"}
	h := NewFeedHarness(t).
		WithSource(a).WithFetch(a.Endpoint, rssFixture).
		WithSource(b).WithFetch(b.Endpoint, atomFixture)
	h.cfg.SourcesFilter = []string{"DeepMind"}
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(1)
	if _, ok := h.get("https://deepmind.google/blog/alphaproof"); !ok {
		t.Error("expected only the DeepMind item")
	}
}

// BatchSize caps how many items are taken from a single source.
func TestBatchSizeLimit(t *testing.T) {
	multi := `<?xml version="1.0"?>
<rss version="2.0"><channel><title>OpenAI</title>
<item><title>One</title><link>https://openai.com/1</link><description>a</description></item>
<item><title>Two</title><link>https://openai.com/2</link><description>b</description></item>
<item><title>Three</title><link>https://openai.com/3</link><description>c</description></item>
</channel></rss>`
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, multi)
	h.cfg.FullText = false
	h.cfg.BatchSize = 2
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(2)
}

// HTML source with JSON-LD yields one article; a CSS-only page yields nothing
// (honest v1 behaviour until the unlocker/bespoke follow-up).
func TestBatchHTMLJSONLD(t *testing.T) {
	src := FeedSource{ID: 5, Name: "Anthropic", SourceType: "html",
		Endpoint: "https://www.anthropic.com/news", Cls: "b-anthropic", FetchStrategy: "http"}
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, jsonLDFixture)
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h.AssertItemCount(1)
	h.AssertItem("https://www.anthropic.com/news/claude-4", func(it NewsItem) {
		if it.FetchStatus != "full" || it.Body != "The full article body." {
			t.Errorf("unexpected item: %+v", it)
		}
	})

	css := FeedSource{ID: 6, Name: "CssOnly", SourceType: "html",
		Endpoint: "https://cssonly.example", Cls: "b-css", FetchStrategy: "http"}
	h2 := NewFeedHarness(t).WithSource(css).WithFetch(css.Endpoint, cssOnlyFixture)
	if err := h2.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	h2.AssertItemCount(0)
}

// An excerpt-only item whose full-text fetch never succeeds must not be re-fetched on
// every run: once it has settled on the excerpt and the feed signal is unchanged, the
// expensive article fetch is skipped.
func TestBatchExcerptOnlyNotRefetched(t *testing.T) {
	feed := strings.Replace(rssFixture,
		"<content:encoded>Full article body here.</content:encoded>", "", 1)
	src := rssSource()
	article := "https://openai.com/blog/gpt-5"
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, feed).
		WithFetchError(article, errors.New("403 blocked"))
	if err := h.Execute(); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if h.fetchCount(article) != 1 {
		t.Fatalf("run 1 article fetches = %d, want 1", h.fetchCount(article))
	}
	if err := h.Execute(); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if h.fetchCount(article) != 1 {
		t.Errorf("article re-fetched on unchanged re-run: count = %d, want 1", h.fetchCount(article))
	}
	h.AssertItem(article, func(it NewsItem) {
		if it.FetchStatus != "failed" || it.Status != "ready" {
			t.Errorf("unexpected item: fetch_status=%q status=%q", it.FetchStatus, it.Status)
		}
	})
}

// The per-source BatchSize cap is applied AFTER the age filter, so a stale item at the
// head of the feed cannot consume a slot that a fresh item below it would have used.
func TestBatchAgeFilterBeforeCap(t *testing.T) {
	// Clock is hnEpoch+24h (~2025-06-05); the first item is far outside the 30-day
	// window, the next two are inside it.
	feed := `<?xml version="1.0"?>
<rss version="2.0"><channel><title>OpenAI</title>
<item><title>Old</title><link>https://openai.com/old</link><description>x</description><pubDate>Wed, 01 Jan 2025 12:00:00 +0000</pubDate></item>
<item><title>FreshA</title><link>https://openai.com/a</link><description>a</description><pubDate>Sun, 01 Jun 2025 12:00:00 +0000</pubDate></item>
<item><title>FreshB</title><link>https://openai.com/b</link><description>b</description><pubDate>Mon, 02 Jun 2025 12:00:00 +0000</pubDate></item>
</channel></rss>`
	src := rssSource()
	h := NewFeedHarness(t).WithSource(src).WithFetch(src.Endpoint, feed)
	h.cfg.FullText = false
	h.cfg.BatchSize = 2
	if err := h.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Old-first + cap-first would have yielded only 1 fresh item; age-first yields 2.
	h.AssertItemCount(2)
	if _, ok := h.get("https://openai.com/old"); ok {
		t.Error("stale item should have been discarded")
	}
	for _, u := range []string{"https://openai.com/a", "https://openai.com/b"} {
		if _, ok := h.get(u); !ok {
			t.Errorf("fresh item %q missing", u)
		}
	}
}
