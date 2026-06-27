package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// Core operations — source registry + unified source listing (fatia #1).
// ---------------------------------------------------------------------------

func TestCoreSourceKindsRegistryIntegrity(t *testing.T) {
	core, _, _ := newTestCore(t)
	kinds := core.SourceKinds()

	if len(kinds) != 8 {
		t.Fatalf("want 8 source kinds, got %d", len(kinds))
	}
	knownKinds := map[string]bool{
		"youtube_channel": false, "youtube_playlist": false, "podcast": false,
		"rss": false, "html": false, "hn": false, "email": false,
		"linkedin_profile": false,
	}
	for _, k := range kinds {
		if k.Kind == "" || k.Label == "" || k.Lane == "" || k.TargetApp == "" {
			t.Errorf("kind %q: missing required field (label=%q lane=%q target_app=%q)",
				k.Kind, k.Label, k.Lane, k.TargetApp)
		}
		for _, f := range k.Fields {
			if f.Name == "" || f.Label == "" || f.Type == "" {
				t.Errorf("kind %q field %q: missing name/label/type", k.Kind, f.Name)
			}
		}
		if _, known := knownKinds[k.Kind]; !known {
			t.Errorf("unexpected kind %q", k.Kind)
		}
		knownKinds[k.Kind] = true
	}
	for k, seen := range knownKinds {
		if !seen {
			t.Errorf("missing kind %q", k)
		}
	}
}

func TestCoreSourcesListsAll(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Lane: "podcast", DisplayName: "Feed A", Tags: []string{}, Status: "active"},
		{ApiID: "youtube_channel:1", Kind: "youtube_channel", Lane: "youtube", DisplayName: "Chan A", Tags: []string{}, Status: "active"},
		{ApiID: "rss:1", Kind: "rss", Lane: "news", DisplayName: "RSS A", Tags: []string{}, Status: "paused"},
	}

	result, err := core.Sources(ctx, SourceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || len(result.Items) != 3 {
		t.Errorf("want 3 sources, got total=%d items=%d", result.Total, len(result.Items))
	}
}

// TestCoreSourcesFilters covers all single-filter cases (kind/status/tag/q) in a table
// to avoid repeating the same 10-line setup-filter-assert pattern four times.
func TestCoreSourcesFilters(t *testing.T) {
	twoSources := func(a, b SourceItem) []SourceItem { return []SourceItem{a, b} }
	podcastActive := SourceItem{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}}
	rssActive := SourceItem{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}}

	cases := []struct {
		name    string
		sources []SourceItem
		filter  SourceFilter
		wantID  string
	}{
		{
			name:    "by kind",
			sources: twoSources(podcastActive, rssActive),
			filter:  SourceFilter{Kind: "podcast"},
			wantID:  "podcast:1",
		},
		{
			name:    "by status",
			sources: twoSources(podcastActive, SourceItem{ApiID: "rss:1", Kind: "rss", Status: "paused", Tags: []string{}}),
			filter:  SourceFilter{Status: "active"},
			wantID:  "podcast:1",
		},
		{
			name: "by tag",
			sources: twoSources(
				SourceItem{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{"tech", "ai"}},
				SourceItem{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{"sports"}},
			),
			filter: SourceFilter{Tag: "tech"},
			wantID: "podcast:1",
		},
		{
			name: "by q (display_name)",
			sources: twoSources(
				SourceItem{ApiID: "podcast:1", Kind: "podcast", DisplayName: "AI Weekly", Status: "active", Tags: []string{}},
				SourceItem{ApiID: "rss:1", Kind: "rss", DisplayName: "Sports Daily", Status: "active", Tags: []string{}},
			),
			filter: SourceFilter{Q: "weekly"},
			wantID: "podcast:1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			core, db, _ := newTestCore(t)
			db.sources = tc.sources
			result, err := core.Sources(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if result.Total != 1 || result.Items[0].ApiID != tc.wantID {
				t.Errorf("filter %+v: got total=%d items=%v", tc.filter, result.Total, result.Items)
			}
		})
	}
}

func TestCoreSourcesPagination(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}},
		{ApiID: "rss:2", Kind: "rss", Status: "active", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 {
		t.Errorf("total = %d, want 3", result.Total)
	}
	if len(result.Items) != 1 {
		t.Errorf("page 2 of 2: want 1 item, got %d", len(result.Items))
	}
	if result.Page != 2 || result.PageSize != 2 {
		t.Errorf("page=%d page_size=%d, want 2/2", result.Page, result.PageSize)
	}
}

func TestCoreSourcesPageSizeCapped(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{PageSize: 99999})
	if err != nil {
		t.Fatal(err)
	}
	if result.PageSize > maxSourcePageSize {
		t.Errorf("pageSize should be capped at %d, got %d", maxSourcePageSize, result.PageSize)
	}
}

func TestCoreSourcesCountsMatchItems(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "podcast:2", Kind: "podcast", Status: "paused", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts.ByStatus["active"] != 2 || result.Counts.ByStatus["paused"] != 1 {
		t.Errorf("by_status: %v", result.Counts.ByStatus)
	}
	if result.Counts.ByKind["podcast"] != 2 || result.Counts.ByKind["rss"] != 1 {
		t.Errorf("by_kind: %v", result.Counts.ByKind)
	}
}

func TestCoreGetSourceFound(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", DisplayName: "Feed A", Status: "active", Tags: []string{}},
	}

	src, found, err := core.Source(ctx, "podcast:1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || src.ApiID != "podcast:1" || src.DisplayName != "Feed A" {
		t.Errorf("Source(podcast:1) = %+v found=%v", src, found)
	}
}

func TestCoreGetSourceNotFound(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	_, found, err := core.Source(ctx, "podcast:999")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("missing source should return found=false")
	}
}

// ---------------------------------------------------------------------------
// HTTP surface — /v1/source-kinds, /v1/sources, /v1/sources/{source_id}
// ---------------------------------------------------------------------------

func TestHTTPListSourceKindsReturnsAllKinds(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/source-kinds", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var kinds []SourceKind
	if err := json.Unmarshal(rec.Body.Bytes(), &kinds); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 8 {
		t.Errorf("want 8 kinds, got %d", len(kinds))
	}
}

func TestHTTPListSources(t *testing.T) {
	twoSources := []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Lane: "podcast", DisplayName: "F1", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Lane: "news", DisplayName: "R1", Status: "paused", Tags: []string{}},
	}

	cases := []struct {
		name      string
		path      string
		wantTotal int
		wantKind  string // if set, assert Items[0].Kind
	}{
		{name: "no filter", path: "/v1/sources", wantTotal: 2},
		{name: "filter kind=podcast", path: "/v1/sources?kind=podcast", wantTotal: 1, wantKind: "podcast"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, db, _ := newTestCore(t)
			db.sources = twoSources
			h := NewSurfaceMux(core, testToken)

			rec := do(t, h, http.MethodGet, tc.path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			var result SourcesResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total != tc.wantTotal {
				t.Errorf("total=%d, want %d", result.Total, tc.wantTotal)
			}
			if tc.wantKind != "" && (len(result.Items) == 0 || result.Items[0].Kind != tc.wantKind) {
				t.Errorf("items[0].kind=%q, want %q", result.Items[0].Kind, tc.wantKind)
			}
		})
	}
}

func TestHTTPGetSourceFound(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", DisplayName: "Feed A", Status: "active", Tags: []string{"news"}},
	}
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources/podcast:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var src SourceItem
	if err := json.Unmarshal(rec.Body.Bytes(), &src); err != nil {
		t.Fatal(err)
	}
	if src.ApiID != "podcast:1" || src.DisplayName != "Feed A" {
		t.Errorf("source = %+v", src)
	}
}

func TestHTTPGetSourceNotFound(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources/podcast:999", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPSourcesRequireAuth(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/v1/source-kinds"},
		{http.MethodGet, "/v1/sources"},
		{http.MethodGet, "/v1/sources/podcast:1"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with wrong token: got %d, want 401", tc.method, tc.target, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Core — GetSourceConfig (Edit modal pre-fill)
// ---------------------------------------------------------------------------

func TestGetSourceConfig_YouTubePlaylist(t *testing.T) {
	core, db, _ := newTestCore(t)
	// Seed a playlist row id=7 with a known youtube_playlist_id.
	db.SeedYouTubePlaylist(7, "PLabc123", "My List", "")

	cfg, found, err := core.SourceConfig(context.Background(), "youtube_playlist:7")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := cfg["playlist_url"]; got != "https://www.youtube.com/playlist?list=PLabc123" {
		t.Fatalf("playlist_url = %q", got)
	}
}

func TestGetSourceConfig_NotFound(t *testing.T) {
	core, _, _ := newTestCore(t)
	_, found, err := core.SourceConfig(context.Background(), "podcast:999")
	if err != nil || found {
		t.Fatalf("want found=false err=nil, got found=%v err=%v", found, err)
	}
}

func TestGetSourceConfig_YouTubeChannel(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedYouTubeChannel(3, "UCabc", "Test Channel", "")

	cfg, found, err := core.SourceConfig(context.Background(), "youtube_channel:3")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg["channel_id"] != "UCabc" || cfg["channel_name"] != "Test Channel" {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestGetSourceConfig_Podcast(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedPodcastFeed(5, "https://example.com/feed.xml", "My Pod", "")

	cfg, found, err := core.SourceConfig(context.Background(), "podcast:5")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg["feed_url"] != "https://example.com/feed.xml" || cfg["title"] != "My Pod" {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestGetSourceConfig_RSS(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedFeedSource(2, "My RSS", "rss", "https://example.com/rss", "")

	cfg, found, err := core.SourceConfig(context.Background(), "rss:2")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg["name"] != "My RSS" || cfg["endpoint"] != "https://example.com/rss" {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestGetSourceConfig_HN(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedFeedSource(4, "HN Top", "hn", "", "")

	cfg, found, err := core.SourceConfig(context.Background(), "hn:4")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg["name"] != "HN Top" {
		t.Fatalf("cfg = %v", cfg)
	}
	if _, hasEndpoint := cfg["endpoint"]; hasEndpoint {
		t.Fatalf("hn kind must not include endpoint field, cfg = %v", cfg)
	}
}

func TestGetSourceConfig_LinkedInProfile(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedLinkedInProfile(9, "https://linkedin.com/in/johndoe", "")

	cfg, found, err := core.SourceConfig(context.Background(), "linkedin_profile:9")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg["profile_url"] != "https://linkedin.com/in/johndoe" {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestGetSourceConfig_Email(t *testing.T) {
	core, _, _ := newTestCore(t)
	cfg, found, err := core.SourceConfig(context.Background(), "email:1")
	if err != nil || !found {
		t.Fatalf("email kind: found=%v err=%v", found, err)
	}
	if len(cfg) != 0 {
		t.Fatalf("email kind must return empty map, got %v", cfg)
	}
}

func TestHTTPGetSourceConfig(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.SeedYouTubePlaylist(7, "PLabc123", "My List", "")
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources/youtube_playlist:7/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["playlist_url"] != "https://www.youtube.com/playlist?list=PLabc123" {
		t.Errorf("playlist_url = %q", cfg["playlist_url"])
	}
}

func TestHTTPGetSourceConfigNotFound(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources/podcast:999/config", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// MCP — rara_list_sources
// ---------------------------------------------------------------------------

func TestMCPListSources(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}},
	}
	s := newMCPServer(core)

	res, rpcErr := callTool(t, s, "rara_list_sources", map[string]any{"kind": "podcast"})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	var result SourcesResult
	toolJSON(t, res, &result)
	if result.Total != 1 || result.Items[0].Kind != "podcast" {
		t.Errorf("rara_list_sources kind=podcast: %+v", result)
	}
}
