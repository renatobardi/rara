package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Core operations — podcast sources (control-plane config; the podcast_feeds
// table is rara-dial's, the core is the operator's write path into it).
// ---------------------------------------------------------------------------

func TestCoreAddPodcastFeedIdempotent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	id1, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "")
	if err != nil {
		t.Fatal(err)
	}
	// Re-adding the same URL (now with a title) returns the SAME row and refreshes the title.
	id2, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "Example Pod")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("re-add of same feed_url should be idempotent: id1=%d id2=%d", id1, id2)
	}
	feeds, err := core.PodcastFeeds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(feeds))
	}
	if feeds[0].Title != "Example Pod" {
		t.Errorf("title not refreshed on re-add: %+v", feeds[0])
	}
	if !feeds[0].Active {
		t.Errorf("new feed should default active=true: %+v", feeds[0])
	}
}

func TestCoreAddPodcastFeedRejectsEmptyURL(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if _, err := core.AddPodcastFeed(ctx, "   ", "title"); !isBadInput(err) {
		t.Fatalf("empty feed_url should be badInput, got %v", err)
	}
}

func TestCoreSetPodcastFeedActiveToggles(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id, err := core.AddPodcastFeed(ctx, "https://feed.example/rss", "X")
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SetPodcastFeedActive(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	feeds, _ := core.PodcastFeeds(ctx)
	if feeds[0].Active {
		t.Errorf("feed should be inactive after toggle: %+v", feeds[0])
	}
}

func TestCoreSetPodcastFeedActiveRejectsBadID(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	if err := core.SetPodcastFeedActive(ctx, 0, true); !isBadInput(err) {
		t.Fatalf("id<=0 should be badInput, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP surface — /v1/sources/podcast (behind the bearer)
// ---------------------------------------------------------------------------

func TestHTTPAddPodcastFeedReflectsInList(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/podcast", `{"feed_url":"https://a.example/rss","title":"A"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/sources/podcast", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d: %s", rec.Code, rec.Body.String())
	}
	var feeds []PodcastFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feeds); err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].FeedURL != "https://a.example/rss" || feeds[0].Title != "A" {
		t.Errorf("feeds = %+v, want [A]", feeds)
	}
}

func TestHTTPTogglePodcastFeedActive(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/podcast", `{"feed_url":"https://a.example/rss"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body.String())
	}
	var added struct {
		ID int `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &added)

	rec = do(t, h, http.MethodPut, "/v1/sources/podcast", `{"id":`+strconv.Itoa(added.ID)+`,"active":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: got %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/sources/podcast", "")
	var feeds []PodcastFeed
	_ = json.Unmarshal(rec.Body.Bytes(), &feeds)
	if len(feeds) != 1 || feeds[0].Active {
		t.Errorf("feed should be inactive: %+v", feeds)
	}
}

func TestHTTPAddPodcastFeedEmptyURLIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/sources/podcast", `{"feed_url":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty feed_url should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPPodcastSourcesRequireAuth(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	for _, tc := range []struct {
		method, target, body string
	}{
		{http.MethodGet, "/v1/sources/podcast", ""},
		{http.MethodPost, "/v1/sources/podcast", `{"feed_url":"https://a.example/rss"}`},
		{http.MethodPut, "/v1/sources/podcast", `{"id":1,"active":false}`},
	} {
		rec := httptest.NewRecorder()
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.target, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.target, http.NoBody)
		}
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with wrong token: got %d, want 401", tc.method, tc.target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), testToken) {
			t.Errorf("%s %s leaked the service token in the response body", tc.method, tc.target)
		}
	}
}

// isBadInput reports whether err is a surface badInputError (a 400-class caller error).
func isBadInput(err error) bool {
	var bad badInputError
	return err != nil && errors.As(err, &bad)
}
