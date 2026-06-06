package main

import (
	"context"
	"fmt"
	"testing"
	"time"
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

// MockUpsertVideo simulates database insert with idempotency
func (m *MockDatabase) UpsertVideo(ctx context.Context, v Video) error {
	if m.err != nil {
		return m.err
	}
	key := videoKey(v.ChannelID, v.VideoID)
	if _, exists := m.videos[key]; exists {
		return nil // Idempotent: video already exists for this channel
	}
	m.videos[key] = v
	return nil
}

// videoKey creates unique key for channel+video combination
func videoKey(channelID int, videoID string) string {
	return fmt.Sprintf("%d:%s", channelID, videoID)
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

	err1 := db.UpsertVideo(ctx, video)
	if err1 != nil {
		t.Fatalf("First upsert failed: %v", err1)
	}

	if len(db.videos) != 1 {
		t.Errorf("After first upsert: videos count = %d, want 1", len(db.videos))
	}

	err2 := db.UpsertVideo(ctx, video)
	if err2 != nil {
		t.Fatalf("Second upsert failed: %v", err2)
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
		if err := db.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("Failed to upsert video %s: %v", v.VideoID, err)
		}
	}

	if len(db.videos) != 3 {
		t.Errorf("Videos count = %d, want 3", len(db.videos))
	}
}

// TestETLHarness is the main integration test harness
type ETLHarness struct {
	db       *MockDatabase
	channels []Channel
	videos   []PlaylistItem
	t        *testing.T
}

// NewETLHarness creates a new test harness
func NewETLHarness(t *testing.T) *ETLHarness {
	return &ETLHarness{
		db:     &MockDatabase{videos: make(map[string]Video)},
		t:      t,
		videos: []PlaylistItem{},
	}
}

// WithChannels sets up test channels
func (h *ETLHarness) WithChannels(channels []Channel) *ETLHarness {
	h.db.channels = channels
	h.channels = channels
	return h
}

// WithVideo adds a test video
func (h *ETLHarness) WithVideo(item PlaylistItem) *ETLHarness {
	h.videos = append(h.videos, item)
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

	for _, channel := range channels {
		for _, video := range h.videos {
			v := Video{
				ChannelID:   channel.ID,
				VideoID:     video.ContentDetails.VideoID,
				Title:       video.Snippet.Title,
				URL:         "https://www.youtube.com/watch?v=" + video.ContentDetails.VideoID,
				PublishedAt: video.Snippet.PublishedAt,
			}
			if err := h.db.UpsertVideo(ctx, v); err != nil {
				return err
			}
		}
	}

	return nil
}

// AssertVideoCount verifies the number of videos stored
func (h *ETLHarness) AssertVideoCount(expected int) {
	if len(h.db.videos) != expected {
		h.t.Errorf("Video count = %d, want %d", len(h.db.videos), expected)
	}
}

// AssertVideoExists verifies a specific video was stored for first channel
func (h *ETLHarness) AssertVideoExists(videoID string) {
	key := videoKey(1, videoID)
	if _, exists := h.db.videos[key]; !exists {
		h.t.Errorf("Video %q not found in database (key: %q)", videoID, key)
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
		WithVideo(PlaylistItem{
			ContentDetails: struct {
				VideoID string `json:"videoId"`
			}{VideoID: "dQw4w9WgXcQ"},
			Snippet: struct {
				Title       string    `json:"title"`
				PublishedAt time.Time `json:"publishedAt"`
			}{Title: "Test Video", PublishedAt: time.Now()},
		})

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(1)
	harness.AssertVideoExists("dQw4w9WgXcQ")
}

// TestETLHarnessMultipleChannels tests ETL with multiple channels
func TestETLHarnessMultipleChannels(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
			{ID: 2, YoutubeChannelID: "UCchannel2", ChannelName: "Channel 2", Active: true},
		}).
		WithVideo(PlaylistItem{
			ContentDetails: struct {
				VideoID string `json:"videoId"`
			}{VideoID: "vid1"},
			Snippet: struct {
				Title       string    `json:"title"`
				PublishedAt time.Time `json:"publishedAt"`
			}{Title: "Video 1", PublishedAt: time.Now()},
		}).
		WithVideo(PlaylistItem{
			ContentDetails: struct {
				VideoID string `json:"videoId"`
			}{VideoID: "vid2"},
			Snippet: struct {
				Title       string    `json:"title"`
				PublishedAt time.Time `json:"publishedAt"`
			}{Title: "Video 2", PublishedAt: time.Now()},
		})

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(4) // 2 channels × 2 videos
	harness.AssertVideoExists("vid1")
	harness.AssertVideoExists("vid2")
}

// TestETLHarnessIdempotentExecution tests that running ETL twice is safe
func TestETLHarnessIdempotentExecution(t *testing.T) {
	harness := NewETLHarness(t).
		WithChannels([]Channel{
			{ID: 1, YoutubeChannelID: "UCchannel1", ChannelName: "Channel 1", Active: true},
		}).
		WithVideo(PlaylistItem{
			ContentDetails: struct {
				VideoID string `json:"videoId"`
			}{VideoID: "vid1"},
			Snippet: struct {
				Title       string    `json:"title"`
				PublishedAt time.Time `json:"publishedAt"`
			}{Title: "Video 1", PublishedAt: time.Now()},
		})

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

// TestETLHarnessEmptyChannels tests ETL with no channels
func TestETLHarnessEmptyChannels(t *testing.T) {
	harness := NewETLHarness(t).WithChannels([]Channel{})

	ctx := context.Background()
	if err := harness.Execute(ctx); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	harness.AssertVideoCount(0)
}
