package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPlaylistItemParsing tests the YouTube API item shape.
func TestPlaylistItemParsing(t *testing.T) {
	item := PlaylistItem{}
	item.ContentDetails.VideoID = "dQw4w9WgXcQ"
	item.Snippet.Title = "Test Video"
	item.Snippet.Position = 3

	if item.ContentDetails.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("VideoID = %q, want %q", item.ContentDetails.VideoID, "dQw4w9WgXcQ")
	}
	if item.Snippet.Title != "Test Video" {
		t.Errorf("Title = %q, want %q", item.Snippet.Title, "Test Video")
	}
	if item.Snippet.Position != 3 {
		t.Errorf("Position = %d, want 3", item.Snippet.Position)
	}
}

// TestPlaylistCreation tests the Playlist struct.
func TestPlaylistCreation(t *testing.T) {
	pl := Playlist{
		ID:                1,
		YoutubePlaylistID: "PLabc123",
		Title:             "My Playlist",
		PrivacyStatus:     "private",
		ItemCount:         10,
	}

	if pl.YoutubePlaylistID == "" {
		t.Error("Playlist YouTube ID is empty")
	}
	if pl.PrivacyStatus != "private" {
		t.Errorf("PrivacyStatus = %q, want %q", pl.PrivacyStatus, "private")
	}
	if pl.ItemCount != 10 {
		t.Errorf("ItemCount = %d, want 10", pl.ItemCount)
	}
}

// TestVideoURL tests the canonical watch URL builder.
func TestVideoURL(t *testing.T) {
	got := videoURL("abc123")
	want := "https://www.youtube.com/watch?v=abc123"
	if got != want {
		t.Errorf("videoURL = %q, want %q", got, want)
	}
}

// TestVideoKeyComposite tests that the dedup key is composite (playlist + video).
// This is the central difference from rara-harvest, where the key is the video
// id alone. The same video in two different playlists must yield two keys.
func TestVideoKeyComposite(t *testing.T) {
	sameVideoP1 := videoKey(1, "vid1")
	sameVideoP2 := videoKey(2, "vid1")
	if sameVideoP1 == sameVideoP2 {
		t.Errorf("same video in different playlists must have different keys, both = %q", sameVideoP1)
	}

	dup := videoKey(1, "vid1")
	if sameVideoP1 != dup {
		t.Errorf("same playlist+video must have identical keys: %q != %q", sameVideoP1, dup)
	}
}

// MockDatabase simulates the database for unit tests.
type MockDatabase struct {
	playlists map[string]Playlist // keyed by youtube_playlist_id
	videos    map[string]Video    // keyed by videoKey(playlistID, videoID)
	nextID    int
	err       error
}

// Video represents a stored playlist video.
type Video struct {
	PlaylistID  int
	VideoID     string
	Title       string
	URL         string
	PublishedAt time.Time
	Position    int
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		playlists: make(map[string]Playlist),
		videos:    make(map[string]Video),
		nextID:    1,
	}
}

// UpsertPlaylist mirrors the real upsert: idempotent on youtube_playlist_id,
// returning a stable internal id.
func (m *MockDatabase) UpsertPlaylist(ctx context.Context, pl Playlist) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if existing, ok := m.playlists[pl.YoutubePlaylistID]; ok {
		// Update metadata, keep the id.
		pl.ID = existing.ID
		m.playlists[pl.YoutubePlaylistID] = pl
		return existing.ID, nil
	}
	pl.ID = m.nextID
	m.nextID++
	m.playlists[pl.YoutubePlaylistID] = pl
	return pl.ID, nil
}

// UpsertVideo mirrors the real schema: UNIQUE(playlist_id, youtube_video_id).
// A video is keyed by playlist+video, so the same video can exist in many
// playlists, but is stored once per playlist.
func (m *MockDatabase) UpsertVideo(ctx context.Context, v Video) error {
	if m.err != nil {
		return m.err
	}
	key := videoKey(v.PlaylistID, v.VideoID)
	if _, exists := m.videos[key]; exists {
		return nil // idempotent within the same playlist
	}
	m.videos[key] = v
	return nil
}

// videoKey is the composite dedup key matching UNIQUE(playlist_id, youtube_video_id).
func videoKey(playlistID int, videoID string) string {
	return fmt.Sprintf("%d:%s", playlistID, videoID)
}

// makePlaylistItem builds a PlaylistItem for tests with minimal boilerplate.
func makePlaylistItem(videoID, title string) PlaylistItem {
	item := PlaylistItem{}
	item.ContentDetails.VideoID = videoID
	item.ContentDetails.VideoPublishedAt = time.Now()
	item.Snippet.Title = title
	return item
}

// TestMockIdempotencySamePlaylist: same video + same playlist twice → 1 row.
func TestMockIdempotencySamePlaylist(t *testing.T) {
	db := newMockDatabase()
	ctx := context.Background()
	v := Video{PlaylistID: 1, VideoID: "vid1", Title: "V1"}

	if err := db.UpsertVideo(ctx, v); err != nil {
		t.Fatalf("first upsert failed: %v", err)
	}
	if err := db.UpsertVideo(ctx, v); err != nil {
		t.Fatalf("second upsert failed: %v", err)
	}
	if len(db.videos) != 1 {
		t.Errorf("videos count = %d, want 1 (idempotent)", len(db.videos))
	}
}

// TestMockSameVideoTwoPlaylists: the same video in two playlists → 2 rows.
// This is the explicit contrast with rara-harvest's global uniqueness.
func TestMockSameVideoTwoPlaylists(t *testing.T) {
	db := newMockDatabase()
	ctx := context.Background()

	if err := db.UpsertVideo(ctx, Video{PlaylistID: 1, VideoID: "shared", Title: "Shared"}); err != nil {
		t.Fatalf("upsert p1 failed: %v", err)
	}
	if err := db.UpsertVideo(ctx, Video{PlaylistID: 2, VideoID: "shared", Title: "Shared"}); err != nil {
		t.Fatalf("upsert p2 failed: %v", err)
	}

	if len(db.videos) != 2 {
		t.Errorf("videos count = %d, want 2 (same video in two playlists)", len(db.videos))
	}
}

// TestMockMultipleVideos: several distinct videos in one playlist.
func TestMockMultipleVideos(t *testing.T) {
	db := newMockDatabase()
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := db.UpsertVideo(ctx, Video{PlaylistID: 1, VideoID: id}); err != nil {
			t.Fatalf("upsert %s failed: %v", id, err)
		}
	}
	if len(db.videos) != 3 {
		t.Errorf("videos count = %d, want 3", len(db.videos))
	}
}

// ShelfHarness is the fluent integration harness.
type ShelfHarness struct {
	db             *MockDatabase
	playlists      []Playlist
	playlistVideos map[string][]PlaylistItem // keyed by youtube_playlist_id
	t              *testing.T
}

func NewShelfHarness(t *testing.T) *ShelfHarness {
	return &ShelfHarness{
		db:             newMockDatabase(),
		playlistVideos: make(map[string][]PlaylistItem),
		t:              t,
	}
}

// WithPlaylists registers the playlists "discovered" from the API.
func (h *ShelfHarness) WithPlaylists(playlists []Playlist) *ShelfHarness {
	h.playlists = playlists
	return h
}

// WithVideosForPlaylist attaches videos to a playlist by its youtube id.
func (h *ShelfHarness) WithVideosForPlaylist(youtubePlaylistID string, items ...PlaylistItem) *ShelfHarness {
	h.playlistVideos[youtubePlaylistID] = append(h.playlistVideos[youtubePlaylistID], items...)
	return h
}

// Execute simulates the discovery + cataloguing flow against the mock.
func (h *ShelfHarness) Execute(ctx context.Context) error {
	for _, pl := range h.playlists {
		id, err := h.db.UpsertPlaylist(ctx, pl)
		if err != nil {
			return err
		}
		for _, item := range h.playlistVideos[pl.YoutubePlaylistID] {
			if item.ContentDetails.VideoID == "" {
				continue
			}
			v := Video{
				PlaylistID:  id,
				VideoID:     item.ContentDetails.VideoID,
				Title:       item.Snippet.Title,
				URL:         videoURL(item.ContentDetails.VideoID),
				PublishedAt: item.ContentDetails.VideoPublishedAt,
				Position:    item.Snippet.Position,
			}
			if err := h.db.UpsertVideo(ctx, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *ShelfHarness) AssertPlaylistCount(expected int) {
	if len(h.db.playlists) != expected {
		h.t.Errorf("playlist count = %d, want %d", len(h.db.playlists), expected)
	}
}

func (h *ShelfHarness) AssertVideoCount(expected int) {
	if len(h.db.videos) != expected {
		h.t.Errorf("video count = %d, want %d", len(h.db.videos), expected)
	}
}

func (h *ShelfHarness) AssertVideoExists(playlistYoutubeID, videoID string) {
	pl, ok := h.db.playlists[playlistYoutubeID]
	if !ok {
		h.t.Errorf("playlist %q not found", playlistYoutubeID)
		return
	}
	if _, exists := h.db.videos[videoKey(pl.ID, videoID)]; !exists {
		h.t.Errorf("video %q not found in playlist %q", videoID, playlistYoutubeID)
	}
}

// TestHarnessSinglePlaylist: one playlist with videos.
func TestHarnessSinglePlaylist(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "First", PrivacyStatus: "private"}}).
		WithVideosForPlaylist("PL1", makePlaylistItem("v1", "Video 1"), makePlaylistItem("v2", "Video 2"))

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertPlaylistCount(1)
	h.AssertVideoCount(2)
	h.AssertVideoExists("PL1", "v1")
	h.AssertVideoExists("PL1", "v2")
}

// TestHarnessMultiplePlaylistsSharedVideo: the same video in two playlists is
// catalogued under each (2 rows), reflecting the composite uniqueness.
func TestHarnessMultiplePlaylistsSharedVideo(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{
			{YoutubePlaylistID: "PL1", Title: "First"},
			{YoutubePlaylistID: "PL2", Title: "Second"},
		}).
		WithVideosForPlaylist("PL1", makePlaylistItem("shared", "Shared")).
		WithVideosForPlaylist("PL2", makePlaylistItem("shared", "Shared"))

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertPlaylistCount(2)
	h.AssertVideoCount(2) // same video, two playlists → two rows
	h.AssertVideoExists("PL1", "shared")
	h.AssertVideoExists("PL2", "shared")
}

// TestHarnessIdempotentExecution: running twice does not duplicate.
func TestHarnessIdempotentExecution(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "First"}}).
		WithVideosForPlaylist("PL1", makePlaylistItem("v1", "Video 1"))

	ctx := context.Background()
	if err := h.Execute(ctx); err != nil {
		t.Fatalf("first execution failed: %v", err)
	}
	h.AssertVideoCount(1)
	h.AssertPlaylistCount(1)

	if err := h.Execute(ctx); err != nil {
		t.Fatalf("second execution failed: %v", err)
	}
	h.AssertVideoCount(1) // still 1 (idempotent)
	h.AssertPlaylistCount(1)
}

// TestHarnessEmptyPlaylists: playlists with no videos catalogue zero videos.
func TestHarnessEmptyPlaylists(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "Empty"}})

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertPlaylistCount(1)
	h.AssertVideoCount(0)
}

// TestHarnessSkipsEmptyVideoID: items without a video id are skipped.
func TestHarnessSkipsEmptyVideoID(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "First"}}).
		WithVideosForPlaylist("PL1", makePlaylistItem("", "Deleted video"), makePlaylistItem("v1", "Good"))

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertVideoCount(1)
	h.AssertVideoExists("PL1", "v1")
}
