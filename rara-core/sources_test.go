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

	if len(kinds) != 7 {
		t.Fatalf("want 7 source kinds, got %d", len(kinds))
	}
	knownKinds := map[string]bool{
		"youtube_channel": false, "youtube_playlist": false, "podcast": false,
		"rss": false, "html": false, "hn": false, "email": false,
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
		{ApiID: "podcast:1", Kind: "podcast", Lane: "podcast", DisplayName: "Feed A", Tags: []string{}, Status: "active", ConfigSummary: "https://a.example/rss"},
		{ApiID: "youtube_channel:1", Kind: "youtube_channel", Lane: "youtube", DisplayName: "Chan A", Tags: []string{}, Status: "active", ConfigSummary: "chan-a"},
		{ApiID: "rss:1", Kind: "rss", Lane: "news", DisplayName: "RSS A", Tags: []string{}, Status: "paused", ConfigSummary: "https://b.example/rss"},
	}

	result, err := core.Sources(ctx, SourceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 {
		t.Errorf("total = %d, want 3", result.Total)
	}
	if len(result.Items) != 3 {
		t.Errorf("len(items) = %d, want 3", len(result.Items))
	}
}

func TestCoreSourcesFiltersByKind(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{Kind: "podcast"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Items) != 1 || result.Items[0].ApiID != "podcast:1" {
		t.Errorf("filter kind=podcast: got total=%d items=%v", result.Total, result.Items)
	}
}

func TestCoreSourcesFiltersByStatus(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "paused", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Items[0].ApiID != "podcast:1" {
		t.Errorf("filter status=active: got %+v", result)
	}
}

func TestCoreSourcesFiltersByTag(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{"tech", "ai"}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{"sports"}},
	}

	result, err := core.Sources(ctx, SourceFilter{Tag: "tech"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Items[0].ApiID != "podcast:1" {
		t.Errorf("filter tag=tech: got %+v", result)
	}
}

func TestCoreSourcesSearchByQ(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", DisplayName: "AI Weekly", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", DisplayName: "Sports Daily", Status: "active", Tags: []string{}},
	}

	result, err := core.Sources(ctx, SourceFilter{Q: "weekly"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Items[0].ApiID != "podcast:1" {
		t.Errorf("q=weekly: got %+v", result)
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
	if len(kinds) != 7 {
		t.Errorf("want 7 kinds, got %d", len(kinds))
	}
}

func TestHTTPListSourcesNoFilter(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Lane: "podcast", DisplayName: "F1", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Lane: "news", DisplayName: "R1", Status: "paused", Tags: []string{}},
	}
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var result SourcesResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Items) != 2 {
		t.Errorf("want 2 sources, got total=%d items=%d", result.Total, len(result.Items))
	}
}

func TestHTTPListSourcesFilterKind(t *testing.T) {
	core, db, _ := newTestCore(t)
	db.sources = []SourceItem{
		{ApiID: "podcast:1", Kind: "podcast", Status: "active", Tags: []string{}},
		{ApiID: "rss:1", Kind: "rss", Status: "active", Tags: []string{}},
	}
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodGet, "/v1/sources?kind=podcast", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var result SourcesResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.Total != 1 || result.Items[0].Kind != "podcast" {
		t.Errorf("filter kind=podcast: %+v", result)
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
