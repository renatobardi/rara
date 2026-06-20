package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestConvertToUploadPlaylistID tests channel ID to upload playlist ID conversion
func TestConvertToUploadPlaylistID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard UC channel",
			input:    "UCkRfArvrzheW2E7b6SVV2vA",
			expected: "UUkRfArvrzheW2E7b6SVV2vA",
		},
		{
			name:     "short channel ID",
			input:    "UC",
			expected: "UU",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "single character",
			input:    "U",
			expected: "U",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertToUploadPlaylistID(tt.input)
			if result != tt.expected {
				t.Errorf("convertToUploadPlaylistID(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestTruncate verifies the log-bounding helper, including rune safety.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 500); got != "short" {
		t.Errorf("truncate short = %q, want unchanged", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q, want abc…", got)
	}
	if got := truncate("áéíóú", 2); got != "áé…" {
		t.Errorf("truncate multibyte = %q, want áé…", got)
	}
}

// TestPlaylistItemParsing tests YouTube API response parsing
func TestPlaylistItemParsing(t *testing.T) {
	item := PlaylistItem{
		ContentDetails: struct {
			VideoID string `json:"videoId"`
		}{
			VideoID: "dQw4w9WgXcQ",
		},
		Snippet: struct {
			Title       string    `json:"title"`
			PublishedAt time.Time `json:"publishedAt"`
		}{
			Title:       "Test Video",
			PublishedAt: time.Now(),
		},
	}

	if item.ContentDetails.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("VideoID = %q, want %q", item.ContentDetails.VideoID, "dQw4w9WgXcQ")
	}

	if item.Snippet.Title != "Test Video" {
		t.Errorf("Title = %q, want %q", item.Snippet.Title, "Test Video")
	}
}

// TestChannelScanning tests channel struct creation and validation
func TestChannelCreation(t *testing.T) {
	ch := Channel{
		ID:               1,
		YoutubeChannelID: "UCkRfArvrzheW2E7b6SVV2vA",
		ChannelName:      "Test Channel",
		Active:           true,
	}

	if ch.ID != 1 {
		t.Errorf("Channel ID = %d, want 1", ch.ID)
	}

	if !ch.Active {
		t.Errorf("Channel Active = %v, want true", ch.Active)
	}

	if ch.YoutubeChannelID == "" {
		t.Error("Channel YouTube ID is empty")
	}
}

// MockDatabase simulates database operations
type MockDatabase struct {
	channels []Channel
	videos   map[string]Video
	err      error
}

// Video represents a stored video
type Video struct {
	ChannelID   int
	VideoID     string
	Title       string
	URL         string
	PublishedAt time.Time
}

// MockFetchActiveChannels simulates database channel fetch
func (m *MockDatabase) FetchActiveChannels(ctx context.Context) ([]Channel, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.channels, nil
}

// MockUpsertVideo simulates the upsert, returning whether a new row was created
// (false = already existed and was refreshed), mirroring the real upsertVideo's
// (bool, error) contract.
//
// This mirrors the real schema exactly: youtube_video_id is globally UNIQUE and
// upsertVideo uses ON CONFLICT (youtube_video_id) DO UPDATE — so an existing
// video's metadata is refreshed in place. A video is keyed by its video ID alone,
// so the same video is stored once regardless of how many channels reference it.
func (m *MockDatabase) UpsertVideo(ctx context.Context, v Video) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	key := videoKey(v.VideoID)
	_, existed := m.videos[key]
	m.videos[key] = v // insert or refresh, mirroring ON CONFLICT DO UPDATE
	return !existed, nil
}

// videoKey creates the unique key for a video, matching the DB's
// UNIQUE(youtube_video_id) constraint.
func videoKey(videoID string) string {
	return videoID
}

// makePlaylistItem builds a PlaylistItem for tests with minimal boilerplate.
func makePlaylistItem(videoID, title string) PlaylistItem {
	item := PlaylistItem{}
	item.ContentDetails.VideoID = videoID
	item.Snippet.Title = title
	item.Snippet.PublishedAt = time.Now()
	return item
}

// TestMockDatabaseIdempotency tests that upserting same video twice is safe
func TestMockDatabaseIdempotency(t *testing.T) {
	db := &MockDatabase{
		videos: make(map[string]Video),
	}

	video := Video{
		ChannelID:   1,
		VideoID:     "dQw4w9WgXcQ",
		Title:       "Test",
		URL:         "https://youtube.com/watch?v=dQw4w9WgXcQ",
		PublishedAt: time.Now(),
	}

	ctx := context.Background()

	isNew, err1 := db.UpsertVideo(ctx, video)
	if err1 != nil {
		t.Fatalf("First upsert failed: %v", err1)
	}
	if !isNew {
		t.Error("First upsert: want isNew=true")
	}

	if len(db.videos) != 1 {
		t.Errorf("After first upsert: videos count = %d, want 1", len(db.videos))
	}

	isNew, err2 := db.UpsertVideo(ctx, video)
	if err2 != nil {
		t.Fatalf("Second upsert failed: %v", err2)
	}
	if isNew {
		t.Error("Second upsert: want isNew=false (already exists)")
	}

	if len(db.videos) != 1 {
		t.Errorf("After second upsert: videos count = %d, want 1 (idempotent)", len(db.videos))
	}
}

// TestMockDatabaseMultipleVideos tests inserting multiple videos
func TestMockDatabaseMultipleVideos(t *testing.T) {
	db := &MockDatabase{
		videos: make(map[string]Video),
	}

	ctx := context.Background()

	videos := []Video{
		{ChannelID: 1, VideoID: "vid1", Title: "Video 1"},
		{ChannelID: 1, VideoID: "vid2", Title: "Video 2"},
		{ChannelID: 1, VideoID: "vid3", Title: "Video 3"},
	}

	for _, v := range videos {
		if _, err := db.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("Failed to upsert video %s: %v", v.VideoID, err)
		}
	}

	if len(db.videos) != 3 {
		t.Errorf("Videos count = %d, want 3", len(db.videos))
	}
}

// TestETLHarness is the main integration test harness
type ETLHarness struct {
	db            *MockDatabase
	channels      []Channel
	channelVideos map[int][]PlaylistItem
	t             *testing.T
	inserted      int // new rows created across the run
	updated       int // existing rows refreshed (ON CONFLICT DO UPDATE)
	skipped       int // items skipped (empty video id)
}

// NewETLHarness creates a new test harness
func NewETLHarness(t *testing.T) *ETLHarness {
	return &ETLHarness{
		db:            &MockDatabase{videos: make(map[string]Video)},
		t:             t,
		channelVideos: make(map[int][]PlaylistItem),
	}
}

// WithChannels sets up test channels
func (h *ETLHarness) WithChannels(channels []Channel) *ETLHarness {
	h.db.channels = channels
	h.channels = channels
	return h
}

// WithVideosForChannel attaches videos to a specific channel. This mirrors
// reality: a channel's uploads playlist contains only that channel's own
// (globally unique) videos — distinct channels do not share video IDs.
func (h *ETLHarness) WithVideosForChannel(channelID int, items ...PlaylistItem) *ETLHarness {
	h.channelVideos[channelID] = append(h.channelVideos[channelID], items...)
	return h
}

// Execute runs the ETL pipeline with mock data
func (h *ETLHarness) Execute(ctx context.Context) error {
	channels, err := h.db.FetchActiveChannels(ctx)
	if err != nil {
		return err
	}

	if len(channels) == 0 {
		h.t.Log("No active channels found")
		return nil
	}

	// Reset per-run counters so re-running Execute reports that run's outcome,
	// mirroring processChannel's per-run inserted/updated/skipped tallies.
	h.inserted, h.updated, h.skipped = 0, 0, 0
	for _, channel := range channels {
		for _, video := range h.channelVideos[channel.ID] {
			if video.ContentDetails.VideoID == "" {
				h.skipped++ // deleted/private items can lack a video id
				continue
			}
			v := Video{
				ChannelID:   channel.ID,
				VideoID:     video.ContentDetails.VideoID,
				Title:       video.Snippet.Title,
				URL:         "https://www.youtube.com/watch?v=" + video.ContentDetails.VideoID,
				PublishedAt: video.Snippet.PublishedAt,
			}
			isNew, err := h.db.UpsertVideo(ctx, v)
			if err != nil {
				return err
			}
			if isNew {
				h.inserted++
			} else {
				h.updated++
			}
		}
	}

	return nil
}

// AssertInsertedCount verifies how many new rows the last Execute created.
func (h *ETLHarness) AssertInsertedCount(expected int) {
	if h.inserted != expected {
		h.t.Errorf("inserted count = %d, want %d", h.inserted, expected)
	}
}

// AssertUpdatedCount verifies how many existing rows the last Execute refreshed.
func (h *ETLHarness) AssertUpdatedCount(expected int) {
	if h.updated != expected {
		h.t.Errorf("updated count = %d, want %d", h.updated, expected)
	}
}

// AssertSkippedCount verifies how many items the last Execute skipped (empty id).
func (h *ETLHarness) AssertSkippedCount(expected int) {
	if h.skipped != expected {
		h.t.Errorf("skipped count = %d, want %d", h.skipped, expected)
	}
}

// AssertVideoCount verifies the number of videos stored
func (h *ETLHarness) AssertVideoCount(expected int) {
	if len(h.db.videos) != expected {
		h.t.Errorf("Video count = %d, want %d", len(h.db.videos), expected)
	}
}

// AssertVideoExists verifies a specific video was stored
func (h *ETLHarness) AssertVideoExists(videoID string) {
	if _, exists := h.db.videos[videoKey(videoID)]; !exists {
		h.t.Errorf("Video %q not found in database", videoID)
	}
}

// TestETLHarnessSingleChannel tests ETL with single channel
func TestETLHarnessSingleChannel(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{
				ID:               1,
				YoutubeChannelID: "UCkRfArvrzheW2E7b6SVV2vA",
				ChannelName:      "Test Channel",
				Active:           true,
			},
		}).
		WithVideosForChannel(1, makePlaylistItem("dQw4w9WgXcQ", "Test Video"))

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(1)
	harness.AssertVideoExists("dQw4w9WgXcQ")
}

// TestETLHarnessMultipleChannels tests ETL with multiple channels, each
// harvesting its own distinct videos (as happens in production).
func TestETLHarnessMultipleChannels(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
			{ID: 2, YoutubeChannelID: "UCchannel2", ChannelName: "Channel 2", Active: true},
		}).
		WithVideosForChannel(1, makePlaylistItem("vid1", "Video 1"), makePlaylistItem("vid2", "Video 2")).
		WithVideosForChannel(2, makePlaylistItem("vid3", "Video 3"), makePlaylistItem("vid4", "Video 4"))

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(4) // 4 distinct, globally-unique videos across 2 channels
	harness.AssertVideoExists("vid1")
	harness.AssertVideoExists("vid4")
}

// TestETLHarnessGlobalVideoUniqueness documents the schema contract: because
// youtube_video_id is globally UNIQUE, the same video referenced by two
// channels is stored exactly once.
func TestETLHarnessGlobalVideoUniqueness(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
			{ID: 2, YoutubeChannelID: "UCchannel2", ChannelName: "Channel 2", Active: true},
		}).
		WithVideosForChannel(1, makePlaylistItem("shared", "Shared Video")).
		WithVideosForChannel(2, makePlaylistItem("shared", "Shared Video"))

	if err := harness.Execute(context.Background()); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(1) // global UNIQUE(youtube_video_id) → stored once
}

// TestETLHarnessIdempotentExecution tests that running ETL twice is safe
func TestETLHarnessIdempotentExecution(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
		}).
		WithVideosForChannel(1, makePlaylistItem("vid1", "Video 1"))

	ctx := context.Background()

	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("First execution failed: %v", err)
	}
	harness.AssertVideoCount(1)

	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Second execution failed: %v", err)
	}

	harness.AssertVideoCount(1) // Still 1, not 2 (idempotent)
}

// TestETLHarnessReportsRealInsertedUpdated verifies the run reports accurate
// inserted vs updated counts. The first run inserts both videos; the second run
// inserts nothing and updates both (the previous bug reported len(videos) as
// "inserted/updated" regardless of what actually changed).
func TestETLHarnessReportsRealInsertedUpdated(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
		}).
		WithVideosForChannel(1, makePlaylistItem("vid1", "Video 1"), makePlaylistItem("vid2", "Video 2"))

	ctx := context.Background()

	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("First execution failed: %v", err)
	}
	harness.AssertInsertedCount(2)
	harness.AssertUpdatedCount(0)

	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Second execution failed: %v", err)
	}
	harness.AssertInsertedCount(0) // nothing new
	harness.AssertUpdatedCount(2)  // both already present, refreshed
}

// TestETLHarnessSkipsEmptyVideoID: items without a video id (deleted/private)
// are skipped, matching rara-shelf — no empty row is inserted.
func TestETLHarnessSkipsEmptyVideoID(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
		}).
		WithVideosForChannel(1, makePlaylistItem("", "Deleted"), makePlaylistItem("vid1", "Good"))

	if err := harness.Execute(context.Background()); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	harness.AssertVideoCount(1) // only the valid one stored
	harness.AssertInsertedCount(1)
	harness.AssertSkippedCount(1)
	harness.AssertVideoExists("vid1")
}

// TestETLHarnessRefreshesMetadata: a re-run with a changed title updates the
// stored row in place (ON CONFLICT DO UPDATE), rather than keeping the old title.
func TestETLHarnessRefreshesMetadata(t *testing.T) {
	ctx := context.Background()
	h := NewETLHarness(t).
		WithChannels([]Channel{{ID: 1, YoutubeChannelID: "UCc", ChannelName: "C", Active: true}}).
		WithVideosForChannel(1, makePlaylistItem("vid1", "Old Title"))
	if err := h.Execute(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Same video id, new title on a second discovery.
	h2 := NewETLHarness(t)
	h2.db = h.db // reuse the same store
	h2.WithChannels([]Channel{{ID: 1, YoutubeChannelID: "UCc", ChannelName: "C", Active: true}}).
		WithVideosForChannel(1, makePlaylistItem("vid1", "New Title"))
	if err := h2.Execute(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := h.db.videos[videoKey("vid1")].Title; got != "New Title" {
		t.Errorf("title = %q, want refreshed to %q", got, "New Title")
	}
}

// TestETLHarnessEmptyChannels tests ETL with no channels
func TestETLHarnessEmptyChannels(t *testing.T) {
	harness := NewETLHarness(t).WithChannels([]Channel{})

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(0)
}

// execMock captures Exec calls so TestStampProviderCollected runs zero-I/O.
// rows controls how many RowsAffected the mock reports (default 0).
type execMock struct {
	gotArgs []any
	err     error
	rows    int64 // rows to report via CommandTag
}

func (m *execMock) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	m.gotArgs = args
	if m.err != nil {
		return pgconn.CommandTag{}, m.err
	}
	tag := pgconn.NewCommandTag("UPDATE " + fmt.Sprintf("%d", m.rows))
	return tag, nil
}

func TestStampProviderCollected(t *testing.T) {
	mock := &execMock{rows: 1}
	if err := stampProviderCollected(context.Background(), mock, "harvest"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.gotArgs) != 1 || mock.gotArgs[0] != "harvest" {
		t.Errorf("Exec args = %v, want [harvest]", mock.gotArgs)
	}
}

func TestStampProviderCollectedPropagatesError(t *testing.T) {
	mock := &execMock{err: errBoom{}}
	if err := stampProviderCollected(context.Background(), mock, "harvest"); err == nil {
		t.Error("want error from Exec, got nil")
	}
}

// TestStampProviderCollectedNotFound verifies that zero rows affected (provider
// not present in the providers table) is treated as an error, not a silent no-op.
func TestStampProviderCollectedNotFound(t *testing.T) {
	mock := &execMock{rows: 0} // UPDATE matched nothing
	err := stampProviderCollected(context.Background(), mock, "harvest")
	if err == nil {
		t.Fatal("want error when provider not found, got nil")
	}
	want := `provider "harvest" not found in providers table`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
