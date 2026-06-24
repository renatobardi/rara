package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Core — AddYouTubeChannel
// ---------------------------------------------------------------------------

func TestCoreAddYouTubeChannelCreates(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddYouTubeChannel(ctx, "UCabc123", "Test Channel", "")
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
	ch, ok := db.ytChannels[id]
	if !ok {
		t.Fatalf("channel not stored in mock, id=%d", id)
	}
	if ch.ChannelID != "UCabc123" || ch.ChannelName != "Test Channel" {
		t.Errorf("stored channel: %+v", ch)
	}
}

func TestCoreAddYouTubeChannelIdempotent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	id1, err := core.AddYouTubeChannel(ctx, "UCabc123", "Name A", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := core.AddYouTubeChannel(ctx, "UCabc123", "Name B", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("same channel_id should be idempotent: got %d vs %d", id1, id2)
	}
}

func TestCoreAddYouTubeChannelRejectsEmptyID(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddYouTubeChannel(ctx, "   ", "Name", ""); !isBadInput(err) {
		t.Fatalf("empty channel_id should be badInput, got %v", err)
	}
}

// AddYouTubeChannel resolves a handle to a UC id but keeps the handle as the
// operator-facing channel_name (the registry field is the handle/name, not the UC id).
func TestCoreAddYouTubeChannelResolvesHandle(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	core.resolveChannel = func(_ context.Context, ref string) (string, error) {
		if ref != "@MyHandle" {
			t.Fatalf("resolver got %q", ref)
		}
		return "UCresolved00000000000000", nil
	}

	id, err := core.AddYouTubeChannel(ctx, "@MyHandle", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ch := db.ytChannels[id]
	if ch.ChannelID != "UCresolved00000000000000" {
		t.Errorf("channel_id should be the resolved UC id, got %q", ch.ChannelID)
	}
	if ch.ChannelName != "@MyHandle" {
		t.Errorf("channel_name should keep the handle, got %q", ch.ChannelName)
	}
}

// A resolver failure (e.g. channel not found) propagates out of AddYouTubeChannel.
func TestCoreAddYouTubeChannelResolveFailure(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	core.resolveChannel = func(_ context.Context, _ string) (string, error) {
		return "", badInput("channel not found")
	}
	if _, err := core.AddYouTubeChannel(ctx, "Ghost Channel", "", ""); !isBadInput(err) {
		t.Fatalf("resolver failure should propagate as badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — AddYouTubePlaylist
// ---------------------------------------------------------------------------

func TestCoreAddYouTubePlaylistParsesURL(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddYouTubePlaylist(ctx, "https://www.youtube.com/playlist?list=PLabc123", "")
	if err != nil {
		t.Fatal(err)
	}
	pl, ok := db.ytPlaylists[id]
	if !ok {
		t.Fatalf("playlist not stored, id=%d", id)
	}
	if pl.PlaylistID != "PLabc123" {
		t.Errorf("playlist_id = %q, want PLabc123", pl.PlaylistID)
	}
}

func TestCoreAddYouTubePlaylistAcceptsRawID(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddYouTubePlaylist(ctx, "PLabc999", "")
	if err != nil {
		t.Fatal(err)
	}
	if pl := db.ytPlaylists[id]; pl.PlaylistID != "PLabc999" {
		t.Errorf("raw playlist_id = %q, want PLabc999", pl.PlaylistID)
	}
}

func TestCoreAddYouTubePlaylistRejectsMissingListParam(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	// URL with no list= param
	_, err := core.AddYouTubePlaylist(ctx, "https://www.youtube.com/watch?v=abc", "")
	if !isBadInput(err) {
		t.Fatalf("URL without list= should be badInput, got %v", err)
	}
}

func TestCoreAddYouTubePlaylistRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddYouTubePlaylist(ctx, "   ", ""); !isBadInput(err) {
		t.Fatalf("empty input should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — AddFeedSource (rss/html/hn)
// ---------------------------------------------------------------------------

func TestCoreAddFeedSourceRSS(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddFeedSource(ctx, "rss", "My Feed", "https://example.com/feed.rss", "")
	if err != nil {
		t.Fatal(err)
	}
	fs := db.feedSources[id]
	if fs.SourceType != "rss" || fs.Endpoint != "https://example.com/feed.rss" || fs.Name != "My Feed" {
		t.Errorf("feedSource = %+v", fs)
	}
}

func TestCoreAddFeedSourceHNDefaultsEndpoint(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddFeedSource(ctx, "hn", "Hacker News", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fs := db.feedSources[id]
	if fs.SourceType != "hn" || fs.Name != "Hacker News" {
		t.Errorf("hn feedSource = %+v", fs)
	}
	if fs.Endpoint != "" {
		t.Errorf("hn endpoint should be empty string by default, got %q", fs.Endpoint)
	}
}

func TestCoreAddFeedSourceRejectsUnknownKind(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "xml", "Bad", "http://x", ""); !isBadInput(err) {
		t.Fatalf("unknown kind should be badInput, got %v", err)
	}
}

func TestCoreAddFeedSourceRejectsEmptyName(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "rss", "   ", "http://x", ""); !isBadInput(err) {
		t.Fatalf("empty name should be badInput, got %v", err)
	}
}

func TestCoreAddFeedSourceRSSRejectsEmptyEndpoint(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "rss", "Name", "", ""); !isBadInput(err) {
		t.Fatalf("rss with empty endpoint should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — AddEmailSource
// ---------------------------------------------------------------------------

func TestCoreAddEmailSourceCreates(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddEmailSource(ctx, "from:news@example.com", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	es := db.emailSources[id]
	if es.GmailQuery != "from:news@example.com" {
		t.Errorf("emailSource = %+v", es)
	}
}

func TestCoreAddEmailSourceRejectsAllEmpty(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddEmailSource(ctx, "", "", "", ""); !isBadInput(err) {
		t.Fatalf("all empty fields should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — PatchSource
// ---------------------------------------------------------------------------

func TestCorePatchSourceDisplayName(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddYouTubeChannel(ctx, "UCpatch1", "Channel", "")
	if err != nil {
		t.Fatal(err)
	}
	apiID := fmt.Sprintf("youtube_channel:%d", id)
	name := "My Channel"
	if err := core.PatchSource(ctx, apiID, SourcePatch{DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	if db.ytChannels[id].DisplayName != "My Channel" {
		t.Errorf("display_name not patched: %+v", db.ytChannels[id])
	}
}

func TestCorePatchSourceTags(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddFeedSource(ctx, "rss", "Feed", "https://x.com/rss", "")
	if err != nil {
		t.Fatal(err)
	}
	apiID := fmt.Sprintf("rss:%d", id)
	if err := core.PatchSource(ctx, apiID, SourcePatch{Tags: []string{"tech", "ai"}}); err != nil {
		t.Fatal(err)
	}
	if tags := db.feedSources[id].Tags; len(tags) != 2 || tags[0] != "tech" {
		t.Errorf("tags not patched: %v", tags)
	}
}

func TestCorePatchSourceBadAPIID(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	name := "X"
	if err := core.PatchSource(ctx, "nope", SourcePatch{DisplayName: &name}); !isBadInput(err) {
		t.Fatalf("bad api_id should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — PauseSource / ResumeSource / DeleteSource
// ---------------------------------------------------------------------------

func TestCorePauseSourceReflectsInSourcesV(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddYouTubeChannel(ctx, "UCpause1", "Chan", "")
	if err != nil {
		t.Fatal(err)
	}
	apiID := fmt.Sprintf("youtube_channel:%d", id)
	db.sources = append(db.sources, SourceItem{ApiID: apiID, Kind: "youtube_channel", Status: "active", Tags: []string{}})

	if err := core.PauseSource(ctx, apiID); err != nil {
		t.Fatal(err)
	}

	src, found, err := core.Source(ctx, apiID)
	if err != nil || !found {
		t.Fatalf("source not found after pause: %v", err)
	}
	if src.Status != "paused" {
		t.Errorf("status after pause = %q, want paused", src.Status)
	}
}

func TestCoreResumeSourceRestoresActive(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddEmailSource(ctx, "from:x@y.com", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	apiID := fmt.Sprintf("email:%d", id)
	db.sources = append(db.sources, SourceItem{ApiID: apiID, Kind: "email", Status: "paused", Tags: []string{}})

	if err := core.ResumeSource(ctx, apiID); err != nil {
		t.Fatal(err)
	}
	src, _, _ := core.Source(ctx, apiID)
	if src.Status != "active" {
		t.Errorf("status after resume = %q, want active", src.Status)
	}
}

// DeleteSource hides the source from sources_v (it disappears from the list) while the
// backing row — and thus its collected content — is preserved.
func TestCoreDeleteSourceHidesFromList(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddFeedSource(ctx, "rss", "Feed", "https://x/rss", "")
	if err != nil {
		t.Fatal(err)
	}
	apiID := fmt.Sprintf("rss:%d", id)
	if _, found, _ := core.Source(ctx, apiID); !found {
		t.Fatal("source should be listed before delete")
	}

	if err := core.DeleteSource(ctx, apiID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := core.Source(ctx, apiID); found {
		t.Error("deleted source must disappear from sources_v")
	}
	// Backing row (content) is preserved — not destroyed.
	if _, ok := db.feedSources[id]; !ok {
		t.Error("backing row should be preserved on soft-delete")
	}
}

// DeleteSource is idempotent: deleting an already-deleted source is a no-op success.
func TestCoreDeleteSourceIdempotent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id, _ := core.AddFeedSource(ctx, "rss", "Feed", "https://x/rss", "")
	apiID := fmt.Sprintf("rss:%d", id)
	if err := core.DeleteSource(ctx, apiID); err != nil {
		t.Fatal(err)
	}
	if err := core.DeleteSource(ctx, apiID); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

// Pausing keeps the source listed (as paused) — distinct from deleting.
func TestCorePauseDoesNotHide(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id, _ := core.AddFeedSource(ctx, "rss", "Feed", "https://x/rss", "")
	apiID := fmt.Sprintf("rss:%d", id)
	if err := core.PauseSource(ctx, apiID); err != nil {
		t.Fatal(err)
	}
	src, found, _ := core.Source(ctx, apiID)
	if !found {
		t.Fatal("paused source must stay listed")
	}
	if src.Status != "paused" {
		t.Errorf("status = %q, want paused", src.Status)
	}
}

func TestCorePauseRejectsUnknownKind(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.PauseSource(ctx, "nope:1"); !isBadInput(err) {
		t.Fatalf("unknown kind should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — BulkSources
// ---------------------------------------------------------------------------

func TestCoreBulkSourcesPausesMultiple(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id1, _ := core.AddYouTubeChannel(ctx, "UCb1", "A", "")
	id2, _ := core.AddYouTubeChannel(ctx, "UCb2", "B", "")
	ids := []string{fmt.Sprintf("youtube_channel:%d", id1), fmt.Sprintf("youtube_channel:%d", id2)}
	db.sources = []SourceItem{
		{ApiID: ids[0], Kind: "youtube_channel", Status: "active", Tags: []string{}},
		{ApiID: ids[1], Kind: "youtube_channel", Status: "active", Tags: []string{}},
	}

	result, err := core.BulkSources(ctx, "pause", ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 || result.Failed != 0 {
		t.Errorf("bulk pause: applied=%d failed=%d", result.Applied, result.Failed)
	}
	if db.ytChannels[id1].Active || db.ytChannels[id2].Active {
		t.Error("both channels should be paused")
	}
}

func TestCoreBulkSourcesDeletesMultiple(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	id1, _ := core.AddYouTubeChannel(ctx, "UCbd1", "A", "")
	id2, _ := core.AddYouTubeChannel(ctx, "UCbd2", "B", "")
	ids := []string{fmt.Sprintf("youtube_channel:%d", id1), fmt.Sprintf("youtube_channel:%d", id2)}

	result, err := core.BulkSources(ctx, "delete", ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 || result.Failed != 0 {
		t.Errorf("bulk delete: applied=%d failed=%d", result.Applied, result.Failed)
	}
	for _, apiID := range ids {
		if _, found, _ := core.Source(ctx, apiID); found {
			t.Errorf("%s should be hidden after bulk delete", apiID)
		}
	}
}

func TestCoreBulkSourcesTagsMultiple(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id1, _ := core.AddFeedSource(ctx, "rss", "F1", "https://a/rss", "")
	id2, _ := core.AddFeedSource(ctx, "rss", "F2", "https://b/rss", "")
	ids := []string{fmt.Sprintf("rss:%d", id1), fmt.Sprintf("rss:%d", id2)}

	result, err := core.BulkSources(ctx, "tag", ids, "featured")
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 {
		t.Errorf("bulk tag: applied=%d", result.Applied)
	}
	if !sourceWriteContainsStr(db.feedSources[id1].Tags, "featured") || !sourceWriteContainsStr(db.feedSources[id2].Tags, "featured") {
		t.Error("tag not applied")
	}
}

func TestCoreBulkSourcesRejectsUnknownAction(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.BulkSources(ctx, "nuke", []string{"podcast:1"}, ""); !isBadInput(err) {
		t.Fatalf("unknown bulk action should be badInput, got %v", err)
	}
}

func TestCoreBulkSourcesPartialSuccess(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	// One valid source + one bad api_id to verify per-item error reporting.
	id, _ := core.AddYouTubeChannel(ctx, "UCbulkpart", "Chan", "")
	ids := []string{fmt.Sprintf("youtube_channel:%d", id), "nope"}
	result, err := core.BulkSources(ctx, "pause", ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 1 || result.Failed != 1 {
		t.Errorf("partial: applied=%d failed=%d want 1/1", result.Applied, result.Failed)
	}
}

// ---------------------------------------------------------------------------
// HTTP — create endpoints
// ---------------------------------------------------------------------------

func TestHTTPAddYouTubeChannel(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/youtube_channel",
		`{"channel_id":"UCabc","channel_name":"Test","display_name":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ID <= 0 {
		t.Errorf("id = %d, want >0", res.ID)
	}
}

func TestHTTPAddYouTubeChannelMissingIDIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/youtube_channel", `{"channel_id":"","channel_name":"Test"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAddYouTubePlaylistBadURLIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/youtube_playlist", `{"playlist_url":"https://youtube.com/watch?v=abc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAddFeedSourceRSS(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/rss", `{"name":"My RSS","endpoint":"https://x.com/feed.rss"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAddEmailSource(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/email", `{"gmail_query":"from:news@x.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAddSourceUnknownKindIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/unknown_kind", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: want 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// HTTP — patch + delete
// ---------------------------------------------------------------------------

func TestHTTPPatchSource(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	id, _ := core.AddYouTubeChannel(ctx, "UCpatch2", "Chan", "")
	apiID := fmt.Sprintf("youtube_channel:%d", id)

	rec := do(t, h, http.MethodPatch, "/v1/sources/"+apiID, `{"display_name":"Patched","tags":["a","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	ch := db.ytChannels[id]
	if ch.DisplayName != "Patched" || len(ch.Tags) != 2 {
		t.Errorf("patch not applied: %+v", ch)
	}
}

func TestHTTPDeleteSourceHides(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	id, _ := core.AddYouTubeChannel(ctx, "UCdel1", "Chan", "")
	apiID := fmt.Sprintf("youtube_channel:%d", id)

	rec := do(t, h, http.MethodDelete, "/v1/sources/"+apiID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if _, found, _ := core.Source(ctx, apiID); found {
		t.Error("source should disappear from sources_v after DELETE")
	}
	// Content preserved — the backing row is not destroyed.
	if _, ok := db.ytChannels[id]; !ok {
		t.Error("backing row should be preserved after soft-delete")
	}
}

// ---------------------------------------------------------------------------
// HTTP — pause / resume
// ---------------------------------------------------------------------------

func TestHTTPPauseSource(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	id, _ := core.AddFeedSource(ctx, "rss", "Feed", "https://a/rss", "")
	apiID := fmt.Sprintf("rss:%d", id)

	rec := do(t, h, http.MethodPost, "/v1/sources/"+apiID+"/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d: %s", rec.Code, rec.Body.String())
	}
	if db.feedSources[id].Enabled {
		t.Error("feed should be disabled after pause")
	}
}

func TestHTTPResumeSource(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	id, _ := core.AddEmailSource(ctx, "from:x@y.com", "", "", "")
	apiID := fmt.Sprintf("email:%d", id)
	_ = core.PauseSource(ctx, apiID)

	rec := do(t, h, http.MethodPost, "/v1/sources/"+apiID+"/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d: %s", rec.Code, rec.Body.String())
	}
	if !db.emailSources[id].Enabled {
		t.Error("email source should be enabled after resume")
	}
}

// ---------------------------------------------------------------------------
// HTTP — bulk
// ---------------------------------------------------------------------------

func TestHTTPBulkSources(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	id1, _ := core.AddYouTubeChannel(ctx, "UCbulk1", "A", "")
	id2, _ := core.AddYouTubeChannel(ctx, "UCbulk2", "B", "")
	apiID1 := fmt.Sprintf("youtube_channel:%d", id1)
	apiID2 := fmt.Sprintf("youtube_channel:%d", id2)
	db.sources = []SourceItem{
		{ApiID: apiID1, Kind: "youtube_channel", Status: "active", Tags: []string{}},
		{ApiID: apiID2, Kind: "youtube_channel", Status: "active", Tags: []string{}},
	}

	body := fmt.Sprintf(`{"action":"pause","ids":[%q,%q]}`, apiID1, apiID2)
	rec := do(t, h, http.MethodPost, "/v1/sources/bulk", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk: got %d: %s", rec.Code, rec.Body.String())
	}
	var result BulkSourcesResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 {
		t.Errorf("applied=%d, want 2", result.Applied)
	}
}

// ---------------------------------------------------------------------------
// HTTP — auth required on all write endpoints
// ---------------------------------------------------------------------------

func TestHTTPSourceWritesRequireAuth(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/sources/youtube_channel", `{"channel_id":"UC1"}`},
		{http.MethodPost, "/v1/sources/youtube_playlist", `{"playlist_url":"PLabc"}`},
		{http.MethodPost, "/v1/sources/rss", `{"name":"x","endpoint":"http://x"}`},
		{http.MethodPost, "/v1/sources/email", `{"gmail_query":"from:x"}`},
		{http.MethodPatch, "/v1/sources/podcast:1", `{"display_name":"x"}`},
		{http.MethodDelete, "/v1/sources/podcast:1", ""},
		{http.MethodPost, "/v1/sources/podcast:1/pause", ""},
		{http.MethodPost, "/v1/sources/podcast:1/resume", ""},
		{http.MethodPost, "/v1/sources/bulk", `{"action":"pause","ids":["podcast:1"]}`},
	}
	for _, tc := range routes {
		t.Run("wrong_token/"+tc.method+" "+tc.path, func(t *testing.T) {
			var r *http.Request
			if tc.body == "" {
				r = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				r = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			}
			r.Header.Set("Authorization", "Bearer wrong-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("want 401, got %d", rec.Code)
			}
			if strings.Contains(rec.Body.String(), testToken) {
				t.Error("leaked service token")
			}
		})
		t.Run("no_header/"+tc.method+" "+tc.path, func(t *testing.T) {
			var r *http.Request
			if tc.body == "" {
				r = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				r = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			}
			// no Authorization header at all
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("no header: want 401, got %d", rec.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MCP — rara_pause_source, rara_resume_source
// ---------------------------------------------------------------------------

func TestMCPPauseSource(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, _ := core.AddYouTubeChannel(ctx, "UCmcp1", "Chan", "")
	apiID := fmt.Sprintf("youtube_channel:%d", id)
	db.sources = []SourceItem{{ApiID: apiID, Kind: "youtube_channel", Status: "active", Tags: []string{}}}

	s := newMCPServer(core)
	res, rpcErr := callTool(t, s, "rara_pause_source", map[string]any{"source_id": apiID})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	var ok okResult
	toolJSON(t, res, &ok)
	if !ok.OK {
		t.Error("expected ok=true")
	}
	if db.ytChannels[id].Active {
		t.Error("channel should be paused after MCP call")
	}
}

func TestMCPResumeSource(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, _ := core.AddEmailSource(ctx, "from:x@y.com", "", "", "")
	apiID := fmt.Sprintf("email:%d", id)
	_ = core.PauseSource(ctx, apiID)

	s := newMCPServer(core)
	res, rpcErr := callTool(t, s, "rara_resume_source", map[string]any{"source_id": apiID})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	var ok okResult
	toolJSON(t, res, &ok)
	if !ok.OK {
		t.Error("expected ok=true")
	}
	if !db.emailSources[id].Enabled {
		t.Error("email source should be enabled after MCP resume")
	}
}

// ---------------------------------------------------------------------------
// Core — display_name persistence (CodeRabbit: AddFeedSource/AddEmailSource dropped it)
// ---------------------------------------------------------------------------

func TestCoreAddFeedSourcePersistsDisplayName(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddFeedSource(ctx, "rss", "My Feed", "https://example.com/feed.rss", "Renamed Feed")
	if err != nil {
		t.Fatal(err)
	}
	if db.feedSources[id].DisplayName != "Renamed Feed" {
		t.Errorf("display_name not persisted: %+v", db.feedSources[id])
	}
}

func TestCoreAddEmailSourcePersistsDisplayName(t *testing.T) {
	ctx := context.Background()
	core, db, _ := newTestCore(t)

	id, err := core.AddEmailSource(ctx, "from:news@example.com", "", "", "Newsletter Rule")
	if err != nil {
		t.Fatal(err)
	}
	if db.emailSources[id].DisplayName != "Newsletter Rule" {
		t.Errorf("display_name not persisted: %+v", db.emailSources[id])
	}
}

// ---------------------------------------------------------------------------
// Core — AddFeedSource SSRF prevention (CodeRabbit: loopback/private IP rejection)
// ---------------------------------------------------------------------------

func TestCoreAddFeedSourceRejectsLoopbackIP(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "rss", "Bad", "http://127.0.0.1/feed", ""); !isBadInput(err) {
		t.Fatalf("loopback IP should be badInput, got %v", err)
	}
}

func TestCoreAddFeedSourceRejectsPrivateIP(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "rss", "Bad", "http://192.168.1.1/feed", ""); !isBadInput(err) {
		t.Fatalf("private IP should be badInput, got %v", err)
	}
}

func TestCoreAddFeedSourceRejectsNonHTTPScheme(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddFeedSource(ctx, "rss", "Bad", "ftp://example.com/feed", ""); !isBadInput(err) {
		t.Fatalf("non-http scheme should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Core — not-found on pause/resume for non-existent source
// ---------------------------------------------------------------------------

func TestCorePauseSourceNotFound(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.PauseSource(ctx, "youtube_channel:999"); err == nil {
		t.Fatal("PauseSource on non-existent source should return error")
	}
}

func TestCoreResumeSourceNotFound(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.ResumeSource(ctx, "rss:999"); err == nil {
		t.Fatal("ResumeSource on non-existent source should return error")
	}
}

// ---------------------------------------------------------------------------
// Core — BulkSources "untag" rejects empty tag
// ---------------------------------------------------------------------------

func TestCoreBulkUntagRejectsEmptyTag(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	result, err := core.BulkSources(ctx, "untag", []string{"rss:1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || result.Items[0].Error == "" {
		t.Errorf("untag with empty tag should fail per-item: %+v", result)
	}
}

// ---------------------------------------------------------------------------
// helpers local to this file
// ---------------------------------------------------------------------------

func sourceWriteContainsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
