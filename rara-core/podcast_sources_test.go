package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// Podcast sources are unified (#4b): creation flows through the same
// POST /v1/sources/{kind} wildcard as every other kind (case "podcast" →
// AddPodcastFeed). The dedicated /v1/sources/podcast GET/POST/PUT routes are
// gone; listing is the generic sources_v, pause/delete the generic id paths.
// ---------------------------------------------------------------------------

func TestCoreAddPodcastFeedStoresDisplayName(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)

	id, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "Example Pod", "AI Weekly")
	if err != nil {
		t.Fatal(err)
	}
	got := db.podcastFeeds[id]
	if got.DisplayName != "AI Weekly" {
		t.Errorf("display_name not stored: %+v", got)
	}
	if got.Title != "Example Pod" || !got.Active {
		t.Errorf("feed not created as expected: %+v", got)
	}
}

func TestCoreAddPodcastFeedIdempotent(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)

	id1, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Re-adding the same URL (now with a title) returns the SAME row and refreshes the title.
	id2, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "Example Pod", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("re-add of same feed_url should be idempotent: id1=%d id2=%d", id1, id2)
	}
	if got := db.podcastFeeds[id1]; got.Title != "Example Pod" {
		t.Errorf("title not refreshed on re-add: %+v", got)
	}
}

func TestCoreAddPodcastFeedRejectsEmptyURL(t *testing.T) {
	ctx := t.Context()
	core, _, _ := newTestCore(t)
	if _, err := core.AddPodcastFeed(ctx, "   ", "title", ""); !isBadInput(err) {
		t.Fatalf("empty feed_url should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP surface — podcast now rides the POST /v1/sources/{kind} wildcard.
// ---------------------------------------------------------------------------

func TestHTTPAddSourcePodcastStoresDisplayName(t *testing.T) {
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/podcast",
		`{"feed_url":"https://a.example/rss","title":"A","display_name":"Show A"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body.String())
	}
	var added struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	got := db.podcastFeeds[added.ID]
	if got.FeedURL != "https://a.example/rss" || got.Title != "A" || got.DisplayName != "Show A" {
		t.Errorf("podcast not created with display_name: %+v", got)
	}
}

func TestHTTPAddSourcePodcastEmptyURLIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/sources/podcast", `{"feed_url":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty feed_url should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// isBadInput reports whether err is a surface badInputError (a 400-class caller error).
func isBadInput(err error) bool {
	var bad badInputError
	return err != nil && errors.As(err, &bad)
}
