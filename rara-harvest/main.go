package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Channel struct {
	ID               int
	YoutubeChannelID string
	ChannelName      string
	Active           bool
}

type PlaylistItem struct {
	ContentDetails struct {
		VideoID string `json:"videoId"`
	} `json:"contentDetails"`
	Snippet struct {
		Title       string    `json:"title"`
		PublishedAt time.Time `json:"publishedAt"`
	} `json:"snippet"`
}

type PlaylistResponse struct {
	Items []PlaylistItem `json:"items"`
}

// httpClient is shared across all channel fetches so HTTP connections are reused
// across the ~100 channels processed per run (a fresh client per call would defeat
// keep-alive). 15s timeout bounds any single YouTube Data API call.
var httpClient = &http.Client{Timeout: 15 * time.Second}

func main() {
	apiKey := os.Getenv("YOUTUBE_API_KEY")
	databaseURL := os.Getenv("DATABASE_URL")

	if apiKey == "" {
		log.Fatalf("YOUTUBE_API_KEY environment variable is required")
	}
	if databaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}

	// Connection has its own short budget, independent of per-channel work.
	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(connectCtx, databaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())

	log.Println("Connected to database successfully")

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 15*time.Second)
	channels, err := fetchActiveChannels(fetchCtx, conn)
	fetchCancel()
	if err != nil {
		log.Fatalf("Failed to fetch channels: %v", err)
	}

	if len(channels) == 0 {
		log.Println("No active channels found")
		return
	}

	log.Printf("Processing %d channels\n", len(channels))

	for _, channel := range channels {
		// Each channel gets its own timeout budget so one slow channel
		// cannot starve the rest of the job.
		channelCtx, channelCancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := processChannel(channelCtx, conn, channel, apiKey)
		channelCancel()
		if err != nil {
			log.Printf("Error processing channel %s: %v\n", channel.YoutubeChannelID, err)
			continue
		}
	}

	log.Println("ETL job completed successfully")
}

func fetchActiveChannels(ctx context.Context, conn *pgx.Conn) ([]Channel, error) {
	rows, err := conn.Query(ctx, "SELECT id, youtube_channel_id, channel_name, active FROM target_channels WHERE active = true")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	channels := make([]Channel, 0, 10)
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch.ID, &ch.YoutubeChannelID, &ch.ChannelName, &ch.Active); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}

	if rows.Err() != nil {
		return nil, rows.Err()
	}

	return channels, nil
}

func processChannel(ctx context.Context, conn *pgx.Conn, channel Channel, apiKey string) error {
	uploadPlaylistID := convertToUploadPlaylistID(channel.YoutubeChannelID)
	log.Printf("Processing channel: %s (playlist: %s)\n", channel.ChannelName, uploadPlaylistID)

	videos, err := fetchLatestVideos(ctx, apiKey, uploadPlaylistID)
	if err != nil {
		return fmt.Errorf("failed to fetch videos: %w", err)
	}

	if len(videos) == 0 {
		log.Printf("No videos found for channel %s\n", channel.ChannelName)
		return nil
	}

	inserted, skipped, failed := 0, 0, 0
	for _, video := range videos {
		isNew, err := upsertVideo(ctx, conn, channel.ID, video)
		switch {
		case err != nil:
			failed++
			log.Printf("Warning: Failed to upsert video %s: %v\n", video.ContentDetails.VideoID, err)
		case isNew:
			inserted++
		default:
			skipped++ // already present (ON CONFLICT DO NOTHING)
		}
	}

	log.Printf("Channel %s: %d inserted, %d skipped, %d failed (of %d fetched)\n",
		channel.ChannelName, inserted, skipped, failed, len(videos))
	return nil
}

func convertToUploadPlaylistID(channelID string) string {
	// Only "UC..." channel IDs map to an uploads playlist ("UU...").
	// Anything else is returned unchanged rather than silently corrupted.
	if !strings.HasPrefix(channelID, "UC") {
		return channelID
	}
	return "UU" + channelID[2:]
}

func fetchLatestVideos(ctx context.Context, apiKey, uploadPlaylistID string) ([]PlaylistItem, error) {
	params := url.Values{}
	params.Set("playlistId", uploadPlaylistID)
	params.Set("part", "snippet,contentDetails")
	params.Set("maxResults", "5")
	params.Set("key", apiKey)

	// reqURL carries the API key as a query param — never log it.
	reqURL := "https://www.googleapis.com/youtube/v3/playlistItems?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		errMsg := string(body)
		if err != nil {
			errMsg = fmt.Sprintf("(unable to read body: %v)", err)
		}
		return nil, fmt.Errorf("YouTube API error (status %d): %s", resp.StatusCode, errMsg)
	}

	var playlistResp PlaylistResponse
	if err := json.NewDecoder(resp.Body).Decode(&playlistResp); err != nil {
		return nil, err
	}

	return playlistResp.Items, nil
}

// upsertVideo inserts a video, returning whether a new row was actually created
// (false means it already existed via ON CONFLICT DO NOTHING). The caller uses
// this to report accurate inserted/skipped counts.
func upsertVideo(ctx context.Context, conn *pgx.Conn, channelID int, video PlaylistItem) (bool, error) {
	videoURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", video.ContentDetails.VideoID)

	query := `
		INSERT INTO channel_videos (channel_id, youtube_video_id, title, url, published_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (youtube_video_id) DO NOTHING
	`

	commandTag, err := conn.Exec(ctx, query,
		channelID,
		video.ContentDetails.VideoID,
		video.Snippet.Title,
		videoURL,
		video.Snippet.PublishedAt,
	)

	if err != nil {
		return false, err
	}

	inserted := commandTag.RowsAffected() > 0
	if inserted {
		log.Printf("Inserted video: %s - %s\n", video.ContentDetails.VideoID, video.Snippet.Title)
	}

	return inserted, nil
}
