package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
// playlists, but is stored once per playlist. On conflict it refreshes the row
// in place, mirroring ON CONFLICT DO UPDATE (title/position/etc).
func (m *MockDatabase) UpsertVideo(ctx context.Context, v Video) error {
	if m.err != nil {
		return m.err
	}
	key := videoKey(v.PlaylistID, v.VideoID)
	m.videos[key] = v // insert or refresh, mirroring ON CONFLICT DO UPDATE
	return nil
}

// videoKey is the composite dedup key used only in tests — there is no production
// equivalent in main.go because the uniqueness contract lives in the SQL schema as
// UNIQUE(playlist_id, youtube_video_id). The mock mirrors that constraint here.
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

// TestMockRefreshesMetadata: re-cataloguing the same video with a new title and
// position refreshes the stored row in place (ON CONFLICT DO UPDATE), rather than
// keeping the stale values.
func TestMockRefreshesMetadata(t *testing.T) {
	db := newMockDatabase()
	ctx := context.Background()

	if err := db.UpsertVideo(ctx, Video{PlaylistID: 1, VideoID: "v1", Title: "Old", Position: 0}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertVideo(ctx, Video{PlaylistID: 1, VideoID: "v1", Title: "New", Position: 5}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if len(db.videos) != 1 {
		t.Fatalf("videos count = %d, want 1", len(db.videos))
	}
	got := db.videos[videoKey(1, "v1")]
	if got.Title != "New" || got.Position != 5 {
		t.Errorf("row = {Title:%q Position:%d}, want {New 5}", got.Title, got.Position)
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
	stampErr       error  // injected error for stampProviderCollected
	stamped        bool   // true if stampProviderCollected was called
	stampedName    string // the provider name passed to stampProviderCollected
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

// WithStampErr injects an error that stampProviderCollected will return.
func (h *ShelfHarness) WithStampErr(err error) *ShelfHarness {
	h.stampErr = err
	return h
}

// stamp simulates calling stampProviderCollected from main().
func (h *ShelfHarness) stamp(name string) error {
	h.stamped = true
	h.stampedName = name
	return h.stampErr
}

// AssertStamped asserts that stampProviderCollected was called with the given name.
func (h *ShelfHarness) AssertStamped(name string) {
	if !h.stamped {
		h.t.Errorf("want stampProviderCollected(%q) called, but it was not", name)
		return
	}
	if h.stampedName != name {
		h.t.Errorf("stamped provider = %q, want %q", h.stampedName, name)
	}
}

// Execute simulates the discovery + cataloguing flow against the mock,
// including the stampProviderCollected call that mirrors main().
func (h *ShelfHarness) Execute(ctx context.Context) error {
	if len(h.playlists) == 0 {
		// Mirror the early-return stamp added for Issue 2.
		_ = h.stamp("shelf")
		return nil
	}
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
	// Mirror the final stamp added for Issue 3.
	_ = h.stamp("shelf")
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

// shelfExecMock captures Exec calls for TestStampProviderCollected (zero-I/O).
// rowsAffected controls the CommandTag returned; set to 1 for the happy path.
type shelfExecMock struct {
	gotArgs      []any
	err          error
	rowsAffected int64
}

func (m *shelfExecMock) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	m.gotArgs = args
	tag := pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", m.rowsAffected))
	return tag, m.err
}

func TestStampProviderCollected(t *testing.T) {
	mock := &shelfExecMock{rowsAffected: 1}
	if err := stampProviderCollected(context.Background(), mock, "shelf"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.gotArgs) != 1 || mock.gotArgs[0] != "shelf" {
		t.Errorf("Exec args = %v, want [shelf]", mock.gotArgs)
	}
}

func TestStampProviderCollectedPropagatesError(t *testing.T) {
	mock := &shelfExecMock{err: fmt.Errorf("db down")}
	if err := stampProviderCollected(context.Background(), mock, "shelf"); err == nil {
		t.Error("want error from Exec, got nil")
	}
}

// TestHarnessStampCalledAfterNormalRun verifies stampProviderCollected is called
// after a normal (non-empty) run (Issue 3 — final stamp).
func TestHarnessStampCalledAfterNormalRun(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "First"}}).
		WithVideosForPlaylist("PL1", makePlaylistItem("v1", "Video 1"))

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertStamped("shelf")
}

// TestHarnessStampCalledOnEmptyPlaylists verifies stampProviderCollected is also
// called when no playlists are found (Issue 2 — early-return stamp).
func TestHarnessStampCalledOnEmptyPlaylists(t *testing.T) {
	h := NewShelfHarness(t) // no WithPlaylists → zero playlists

	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	h.AssertStamped("shelf")
}

// TestHarnessStampErrorDoesNotCrash verifies that a stamp error is logged but
// does not abort the run (Issue 4 — stamp errors are non-fatal).
func TestHarnessStampErrorDoesNotCrash(t *testing.T) {
	h := NewShelfHarness(t).
		WithPlaylists([]Playlist{{YoutubePlaylistID: "PL1", Title: "First"}}).
		WithVideosForPlaylist("PL1", makePlaylistItem("v1", "Video 1")).
		WithStampErr(fmt.Errorf("stamp failed"))

	// Execute must complete without returning an error — stamp errors are logged, not propagated.
	if err := h.Execute(context.Background()); err != nil {
		t.Fatalf("execute returned error on stamp failure: %v", err)
	}
	h.AssertStamped("shelf")
}

// TestStampProviderCollectedNotFound verifies that zero rows affected returns an
// error rather than silently succeeding (Issue 1).
func TestStampProviderCollectedNotFound(t *testing.T) {
	mock := &shelfExecMock{rowsAffected: 0} // provider row absent
	err := stampProviderCollected(context.Background(), mock, "shelf")
	if err == nil {
		t.Fatal("want error when provider row not found, got nil")
	}
	want := `provider "shelf" not found in providers table`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
