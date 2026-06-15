package main

import (
	"context"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// parseRSS — pure parsing, zero I/O.
// ---------------------------------------------------------------------------

const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>The Example Cast</title>
    <item>
      <title>Episode One</title>
      <guid>ep-0001</guid>
      <pubDate>Tue, 10 Jun 2025 08:00:00 +0000</pubDate>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
    </item>
    <item>
      <title>A blog-only post (no audio)</title>
      <guid>post-99</guid>
      <pubDate>Wed, 11 Jun 2025 08:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Episode Two (no guid)</title>
      <pubDate>Thu, 12 Jun 2025 08:00:00 +0000</pubDate>
      <enclosure url="https://cdn.example.com/ep2.mp3" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

// sampleFeedWithDescription has <description> and itunes:summary; tests that itunes:summary wins.
const sampleFeedWithDescription = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>Desc Cast</title>
    <item>
      <title>With itunes summary</title>
      <guid>ep-a</guid>
      <description>plain description</description>
      <itunes:summary>itunes summary</itunes:summary>
      <enclosure url="https://cdn.example.com/a.mp3" type="audio/mpeg"/>
    </item>
    <item>
      <title>With description only</title>
      <guid>ep-b</guid>
      <description>only description</description>
      <enclosure url="https://cdn.example.com/b.mp3" type="audio/mpeg"/>
    </item>
    <item>
      <title>No description</title>
      <guid>ep-c</guid>
      <enclosure url="https://cdn.example.com/c.mp3" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

func TestParseRSS(t *testing.T) {
	title, eps, err := parseRSS([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("parseRSS: %v", err)
	}
	if title != "The Example Cast" {
		t.Errorf("channel title = %q, want The Example Cast", title)
	}
	// Only the two audio items; the blog-only post is dropped.
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2 (audio only)", len(eps))
	}
	if eps[0].GUID != "ep-0001" || eps[0].EnclosureURL != "https://cdn.example.com/ep1.mp3" {
		t.Errorf("episode 1 = %+v", eps[0])
	}
	if eps[0].PublishedAt == nil || eps[0].PublishedAt.UTC().Format("2006-01-02") != "2025-06-10" {
		t.Errorf("episode 1 pubDate not parsed: %v", eps[0].PublishedAt)
	}
	// The second audio item has no guid -> falls back to the enclosure URL.
	if eps[1].GUID != "https://cdn.example.com/ep2.mp3" {
		t.Errorf("episode 2 guid fallback = %q, want the enclosure URL", eps[1].GUID)
	}
}

func TestParseRSSMalformed(t *testing.T) {
	if _, _, err := parseRSS([]byte("not xml at all <<<")); err == nil {
		t.Error("malformed feed should error")
	}
}

func TestIsAudioEnclosure(t *testing.T) {
	for _, mime := range []string{"audio/mpeg", "audio/mp4", "AUDIO/MPEG", ""} {
		if !isAudioEnclosure(mime) {
			t.Errorf("%q should count as audio", mime)
		}
	}
	for _, mime := range []string{"video/mp4", "text/html", "application/pdf"} {
		if isAudioEnclosure(mime) {
			t.Errorf("%q should NOT count as audio", mime)
		}
	}
}

func TestParsePubDate(t *testing.T) {
	if parsePubDate("") != nil {
		t.Error("empty pubDate should be nil")
	}
	if parsePubDate("garbage") != nil {
		t.Error("unparseable pubDate should be nil, not an error")
	}
	if got := parsePubDate("Tue, 10 Jun 2025 08:00:00 GMT"); got == nil {
		t.Error("RFC1123 (named zone) should parse")
	}
}

// ---------------------------------------------------------------------------
// MockDatabase — in-memory, mirrors the SQL contract (UNIQUE(guid) upsert, active feeds).
// Zero I/O.
// ---------------------------------------------------------------------------

type MockDatabase struct {
	feeds    []Feed
	episodes map[string]Episode // keyed by guid (UNIQUE)
	feedOf   map[string]int     // guid -> feed_id (FK)
	err      error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{episodes: map[string]Episode{}, feedOf: map[string]int{}}
}

func (m *MockDatabase) ActiveFeeds(_ context.Context) ([]Feed, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.feeds, nil
}

func (m *MockDatabase) UpsertEpisode(_ context.Context, feedID int, e Episode) error {
	if m.err != nil {
		return m.err
	}
	m.episodes[e.GUID] = e // ON CONFLICT (guid) DO UPDATE — stores Description too
	m.feedOf[e.GUID] = feedID
	return nil
}

func (m *MockDatabase) SetFeedTitle(_ context.Context, feedID int, title string) error {
	if m.err != nil {
		return m.err
	}
	for i := range m.feeds {
		if m.feeds[i].ID == feedID {
			m.feeds[i].Title = title
		}
	}
	return nil
}

var _ Database = (*MockDatabase)(nil)

// staticFetcher serves a fixed body for any URL.
func staticFetcher(body string) Fetcher {
	return func(_ context.Context, _ string) ([]byte, error) { return []byte(body), nil }
}

// TestRunCollectsEpisodes: the loop fetches each active feed, parses it, refreshes the title,
// and upserts every audio episode.
func TestRunCollectsEpisodes(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.feeds = []Feed{{ID: 1, FeedURL: "https://example.com/feed.xml"}}

	n, err := run(ctx, db, staticFetcher(sampleFeed))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Errorf("catalogued %d, want 2", n)
	}
	if len(db.episodes) != 2 {
		t.Errorf("stored %d episodes, want 2", len(db.episodes))
	}
	if db.feedOf["ep-0001"] != 1 {
		t.Errorf("episode not linked to its feed: %v", db.feedOf)
	}
	if db.feeds[0].Title != "The Example Cast" {
		t.Errorf("feed title not refreshed: %q", db.feeds[0].Title)
	}
}

// TestRunIdempotent: polling the same feed twice converges (UNIQUE guid upsert), no duplicates.
func TestRunIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.feeds = []Feed{{ID: 1, FeedURL: "https://example.com/feed.xml"}}
	fetch := staticFetcher(sampleFeed)

	if _, err := run(ctx, db, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, fetch); err != nil {
		t.Fatal(err)
	}
	if len(db.episodes) != 2 {
		t.Errorf("re-poll duplicated episodes: %d, want 2", len(db.episodes))
	}
}

// TestRunSkipsBadFeed: a feed that fails to fetch or parse is logged and skipped; the others
// still process (one bad feed must not stall the run).
func TestRunSkipsBadFeed(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.feeds = []Feed{
		{ID: 1, FeedURL: "https://bad.example.com/feed.xml"},
		{ID: 2, FeedURL: "https://good.example.com/feed.xml"},
	}
	fetch := func(_ context.Context, url string) ([]byte, error) {
		if url == "https://bad.example.com/feed.xml" {
			return nil, errors.New("boom")
		}
		return []byte(sampleFeed), nil
	}

	n, err := run(ctx, db, fetch)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Errorf("catalogued %d, want 2 (good feed only)", n)
	}
}

// TestRunNoFeeds: an empty active-feed set is a clean no-op.
func TestRunNoFeeds(t *testing.T) {
	n, err := run(context.Background(), newMockDatabase(), staticFetcher(sampleFeed))
	if err != nil || n != 0 {
		t.Errorf("no feeds: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestRunSurfacesFeedListError: an error listing feeds aborts the run.
func TestRunSurfacesFeedListError(t *testing.T) {
	db := newMockDatabase()
	db.err = errors.New("db down")
	if _, err := run(context.Background(), db, staticFetcher(sampleFeed)); err == nil {
		t.Error("a feed-list error should abort the run")
	}
}

// TestParseRSSPrefersItunesSummary: itunes:summary wins over <description> when both present.
func TestParseRSSPrefersItunesSummary(t *testing.T) {
	_, eps, err := parseRSS([]byte(sampleFeedWithDescription))
	if err != nil {
		t.Fatalf("parseRSS: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3", len(eps))
	}
	if eps[0].Description != "itunes summary" {
		t.Errorf("ep-a: Description = %q, want itunes:summary value", eps[0].Description)
	}
	if eps[1].Description != "only description" {
		t.Errorf("ep-b: Description = %q, want <description> fallback", eps[1].Description)
	}
	if eps[2].Description != "" {
		t.Errorf("ep-c: Description = %q, want empty", eps[2].Description)
	}
}

// TestRunStoresEpisodeDescription: the collector loop passes description through to UpsertEpisode.
func TestRunStoresEpisodeDescription(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	db.feeds = []Feed{{ID: 1, FeedURL: "https://example.com/feed.xml"}}

	_, err := run(ctx, db, staticFetcher(sampleFeedWithDescription))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	ep, ok := db.episodes["ep-a"]
	if !ok {
		t.Fatal("ep-a not stored")
	}
	if ep.Description != "itunes summary" {
		t.Errorf("stored description = %q, want itunes summary", ep.Description)
	}
}
