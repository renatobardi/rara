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
	"github.com/jackc/pgx/v5/pgconn"
)

// httpClient is shared across all API calls to reuse TCP connections (keep-alive).
// Creating a new client per call bypasses connection pooling and defeats keep-alive.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// pgxExecutor is the subset of pgx used by the upsert helpers, satisfied by both
// *pgx.Conn and pgx.Tx — so the same helper runs standalone or inside a
// transaction.
type pgxExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgxExecer is the minimal subset needed to stamp provider timestamps.
// *pgx.Conn satisfies this interface.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// stampProviderCollected sets providers.last_collect_at = now() for the named provider.
// Called on successful completion so the dispatcher can track cadence.
// Returns an error if no row was updated (provider not registered in providers table).
func stampProviderCollected(ctx context.Context, db pgxExecer, name string) error {
	tag, err := db.Exec(ctx, `UPDATE providers SET last_collect_at = now() WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("provider %q not found in providers table", name)
	}
	return nil
}

// Playlist is a YouTube playlist owned by the authenticated user.
type Playlist struct {
	ID                int
	YoutubePlaylistID string
	Title             string
	Description       string
	PrivacyStatus     string
	ItemCount         int
}

// PlaylistItem is a single video entry inside a playlist.
type PlaylistItem struct {
	Snippet struct {
		Title    string `json:"title"`
		Position int    `json:"position"`
	} `json:"snippet"`
	ContentDetails struct {
		VideoID          string    `json:"videoId"`
		VideoPublishedAt time.Time `json:"videoPublishedAt"`
	} `json:"contentDetails"`
}

// playlistsResponse models the playlists.list API response.
type playlistsResponse struct {
	NextPageToken string `json:"nextPageToken"`
	Items         []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"snippet"`
		Status struct {
			PrivacyStatus string `json:"privacyStatus"`
		} `json:"status"`
		ContentDetails struct {
			ItemCount int `json:"itemCount"`
		} `json:"contentDetails"`
	} `json:"items"`
}

// playlistItemsResponse models the playlistItems.list API response.
type playlistItemsResponse struct {
	NextPageToken string         `json:"nextPageToken"`
	Items         []PlaylistItem `json:"items"`
}

// tokenResponse models the OAuth token endpoint response.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	clientID := os.Getenv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")
	refreshToken := os.Getenv("GOOGLE_OAUTH_REFRESH_TOKEN")

	if databaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}
	if clientID == "" || clientSecret == "" || refreshToken == "" {
		log.Fatalf("GOOGLE_OAUTH_CLIENT_ID, GOOGLE_OAUTH_CLIENT_SECRET and GOOGLE_OAUTH_REFRESH_TOKEN are required")
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(connectCtx, databaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())

	log.Println("Connected to database successfully")

	// Exchange the long-lived refresh token for a short-lived access token.
	tokenCtx, tokenCancel := context.WithTimeout(context.Background(), 15*time.Second)
	accessToken, err := getAccessToken(tokenCtx, clientID, clientSecret, refreshToken)
	tokenCancel()
	if err != nil {
		log.Fatalf("Failed to obtain access token: %v", err)
	}
	log.Println("OAuth access token obtained")

	// Watch Later (WL) and History (HL) are NOT exposed by the YouTube Data API
	// since 2016 — there is no supported way to read them with an access token.
	log.Println("Note: Watch Later / History are not accessible via the Data API and are skipped")

	// Discover all of the user's playlists.
	discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	playlists, err := fetchMyPlaylists(discoverCtx, accessToken)
	discoverCancel()
	if err != nil {
		log.Fatalf("Failed to fetch playlists: %v", err)
	}

	if len(playlists) == 0 {
		log.Println("No playlists found")
		stampCtx, stampCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stampCancel()
		if err := stampProviderCollected(stampCtx, conn, "shelf"); err != nil {
			log.Printf("stamp provider collected: %v", err)
		}
		return
	}

	log.Printf("Discovered %d playlists\n", len(playlists))

	for _, pl := range playlists {
		// Give each playlist its own timeout proportional to its item count.
		// Base of 60s + 1s per item; minimum 60s, maximum 600s.
		// This prevents large playlists (500+ items, 10 paginated API calls)
		// from being cut off by a fixed budget suited only for small playlists.
		estimated := time.Duration(pl.ItemCount) * time.Second
		if estimated < 60*time.Second {
			estimated = 60 * time.Second
		}
		if estimated > 600*time.Second {
			estimated = 600 * time.Second
		}
		plCtx, plCancel := context.WithTimeout(context.Background(), estimated)
		err := processPlaylist(plCtx, conn, pl, accessToken)
		plCancel()
		if err != nil {
			log.Printf("Error processing playlist %s: %v\n", pl.YoutubePlaylistID, err)
			continue
		}
	}

	log.Println("Shelf job completed successfully")
	stampCtx, stampCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stampCancel()
	if err := stampProviderCollected(stampCtx, conn, "shelf"); err != nil {
		log.Printf("stamp provider collected: %v", err)
	}
}

// getAccessToken exchanges an OAuth refresh token for a short-lived access token.
func getAccessToken(ctx context.Context, clientID, clientSecret, refreshToken string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Do NOT log the full response body: Google may echo back client_secret
		// in certain error payloads. Surface only the status code.
		return "", fmt.Errorf("token endpoint returned unexpected status %d (check client_id, client_secret and refresh_token)", resp.StatusCode)
	}

	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned empty access_token")
	}
	return tok.AccessToken, nil
}

// fetchMyPlaylists lists all playlists owned by the authenticated user,
// following pagination until exhausted.
func fetchMyPlaylists(ctx context.Context, accessToken string) ([]Playlist, error) {
	playlists := make([]Playlist, 0, 50)
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("mine", "true")
		params.Set("part", "snippet,contentDetails,status")
		params.Set("maxResults", "50")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		reqURL := "https://www.googleapis.com/youtube/v3/playlists?" + params.Encode()
		var pr playlistsResponse
		if err := getJSON(ctx, accessToken, reqURL, &pr); err != nil {
			return nil, err
		}

		for _, it := range pr.Items {
			playlists = append(playlists, Playlist{
				YoutubePlaylistID: it.ID,
				Title:             it.Snippet.Title,
				Description:       it.Snippet.Description,
				PrivacyStatus:     it.Status.PrivacyStatus,
				ItemCount:         it.ContentDetails.ItemCount,
			})
		}

		if pr.NextPageToken == "" {
			break
		}
		pageToken = pr.NextPageToken
	}

	return playlists, nil
}

// processPlaylist upserts the playlist row, then upserts every video in it.
func processPlaylist(ctx context.Context, conn *pgx.Conn, pl Playlist, accessToken string) error {
	log.Printf("Processing playlist: %s (%s, %d items)\n", pl.Title, pl.PrivacyStatus, pl.ItemCount)

	// Fetch over the network BEFORE opening the transaction, so the DB tx stays
	// short and never waits on HTTP.
	items, err := fetchPlaylistVideos(ctx, accessToken, pl.YoutubePlaylistID)
	if err != nil {
		return fmt.Errorf("failed to fetch playlist videos: %w", err)
	}

	// Wrap the playlist row and all its videos in one transaction so a crash
	// mid-playlist never leaves a playlist with only a partial set of videos —
	// either the whole playlist lands or none of it does.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	playlistID, err := upsertPlaylist(ctx, tx, pl)
	if err != nil {
		return fmt.Errorf("failed to upsert playlist: %w", err)
	}

	catalogued, skipped := 0, 0
	for _, item := range items {
		if item.ContentDetails.VideoID == "" {
			skipped++ // deleted/private items can lack a video id
			continue
		}
		if err := upsertPlaylistVideo(ctx, tx, playlistID, item); err != nil {
			// Abort the whole playlist for atomicity; the next run retries it.
			return fmt.Errorf("failed to upsert video %s: %w", item.ContentDetails.VideoID, err)
		}
		catalogued++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit playlist %s: %w", pl.YoutubePlaylistID, err)
	}

	log.Printf("Catalogued %d/%d videos for playlist %s (%d skipped — deleted/private)\n",
		catalogued, len(items), pl.Title, skipped)
	return nil
}

// fetchPlaylistVideos lists all items of a playlist, following pagination.
func fetchPlaylistVideos(ctx context.Context, accessToken, playlistID string) ([]PlaylistItem, error) {
	items := make([]PlaylistItem, 0, 50)
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("playlistId", playlistID)
		params.Set("part", "snippet,contentDetails")
		params.Set("maxResults", "50")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		reqURL := "https://www.googleapis.com/youtube/v3/playlistItems?" + params.Encode()
		var pir playlistItemsResponse
		if err := getJSON(ctx, accessToken, reqURL, &pir); err != nil {
			return nil, err
		}

		items = append(items, pir.Items...)

		if pir.NextPageToken == "" {
			break
		}
		pageToken = pir.NextPageToken
	}

	return items, nil
}

// getJSON performs an authenticated GET and decodes the JSON body into out.
func getJSON(ctx context.Context, accessToken, reqURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, rerr := io.ReadAll(resp.Body)
		errMsg := string(body)
		if rerr != nil {
			errMsg = fmt.Sprintf("(unable to read body: %v)", rerr)
		}
		return fmt.Errorf("YouTube API error (status %d): %s", resp.StatusCode, errMsg)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// videoURL builds the canonical watch URL for a video id.
func videoURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// upsertPlaylist inserts or updates a playlist and returns its internal id.
// Accepts any pgx executor (a *pgx.Conn or a pgx.Tx).
func upsertPlaylist(ctx context.Context, db pgxExecutor, pl Playlist) (int, error) {
	query := `
		INSERT INTO playlists (youtube_playlist_id, title, description, privacy_status, item_count)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (youtube_playlist_id) DO UPDATE
		SET title = EXCLUDED.title,
		    description = EXCLUDED.description,
		    privacy_status = EXCLUDED.privacy_status,
		    item_count = EXCLUDED.item_count,
		    updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`
	var id int
	err := db.QueryRow(ctx, query,
		pl.YoutubePlaylistID,
		pl.Title,
		pl.Description,
		pl.PrivacyStatus,
		pl.ItemCount,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// upsertPlaylistVideo inserts a video into a playlist, idempotent on the composite
// (playlist_id, youtube_video_id). On conflict it refreshes title/url/published_at
// and position, so a video's title edit or reordering within the playlist
// propagates on the next run. Accepts any pgx executor (a *pgx.Conn or a pgx.Tx).
func upsertPlaylistVideo(ctx context.Context, db pgxExecutor, playlistID int, item PlaylistItem) error {
	query := `
		INSERT INTO playlist_videos (playlist_id, youtube_video_id, title, url, published_at, position)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (playlist_id, youtube_video_id) DO UPDATE
		SET title = EXCLUDED.title,
		    url = EXCLUDED.url,
		    published_at = EXCLUDED.published_at,
		    position = EXCLUDED.position
	`

	var publishedAt *time.Time
	if !item.ContentDetails.VideoPublishedAt.IsZero() {
		publishedAt = &item.ContentDetails.VideoPublishedAt
	}

	_, err := db.Exec(ctx, query,
		playlistID,
		item.ContentDetails.VideoID,
		item.Snippet.Title,
		videoURL(item.ContentDetails.VideoID),
		publishedAt,
		item.Snippet.Position,
	)
	return err
}
