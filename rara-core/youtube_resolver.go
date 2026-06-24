package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ucChannelID matches a well-formed YouTube channel id: "UC" + 22 base64url chars.
// A malformed "UC…" string falls through to API resolution rather than being
// persisted verbatim as a (dead) canonical id.
var ucChannelID = regexp.MustCompile(`^UC[0-9A-Za-z_-]{22}$`)

// httpDoer is the minimal HTTP seam so the YouTube resolver is unit-testable with a fake
// (zero real I/O), mirroring how the rest of the core injects its dependencies.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ytResolver turns an operator-supplied channel reference (raw UC id, @handle, or free-text
// name) into a canonical youtube_channel_id via the YouTube Data API. It reuses the same
// env (YOUTUBE_API_KEY) and endpoint (youtube/v3) as rara-harvest.
type ytResolver struct {
	doer    httpDoer
	apiKey  string
	baseURL string // default https://www.googleapis.com/youtube/v3
}

// newYTResolver builds a resolver from the API key, with a shared 15s-timeout client.
func newYTResolver(apiKey string) *ytResolver {
	return &ytResolver{
		doer:    &http.Client{Timeout: 15 * time.Second},
		apiKey:  apiKey,
		baseURL: "https://www.googleapis.com/youtube/v3",
	}
}

// resolve returns the canonical UC… channel id for input.
//   - raw "UC…" (24 chars)  → used directly, no API call.
//   - "@handle"             → channels?part=id&forHandle=@handle
//   - free-text name        → search?part=snippet&type=channel&q=<name> (first result)
//
// Returns a badInput error when nothing matches.
func (r *ytResolver) resolve(ctx context.Context, input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", badInput("channel reference cannot be empty")
	}
	// A well-formed channel id ("UC" + 22 base64url chars) passes through untouched;
	// a malformed "UC…" string is resolved via the API rather than persisted as-is.
	if ucChannelID.MatchString(input) {
		return input, nil
	}
	if r.apiKey == "" {
		return "", fmt.Errorf("YOUTUBE_API_KEY is required to resolve channel reference %q", input)
	}
	if strings.HasPrefix(input, "@") {
		return r.resolveHandle(ctx, input)
	}
	return r.resolveSearch(ctx, input)
}

func (r *ytResolver) resolveHandle(ctx context.Context, handle string) (string, error) {
	params := url.Values{}
	params.Set("part", "id")
	params.Set("forHandle", handle)
	params.Set("key", r.apiKey)

	var resp struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := r.get(ctx, "/channels", params, &resp); err != nil {
		return "", err
	}
	if len(resp.Items) == 0 || resp.Items[0].ID == "" {
		return "", badInput("channel not found for handle %q", handle)
	}
	return resp.Items[0].ID, nil
}

func (r *ytResolver) resolveSearch(ctx context.Context, name string) (string, error) {
	params := url.Values{}
	params.Set("part", "snippet")
	params.Set("type", "channel")
	params.Set("q", name)
	params.Set("key", r.apiKey)

	var resp struct {
		Items []struct {
			ID struct {
				ChannelID string `json:"channelId"`
			} `json:"id"`
		} `json:"items"`
	}
	if err := r.get(ctx, "/search", params, &resp); err != nil {
		return "", err
	}
	if len(resp.Items) == 0 || resp.Items[0].ID.ChannelID == "" {
		return "", badInput("channel not found for %q", name)
	}
	if len(resp.Items) > 1 {
		log.Printf("yt-resolve: %q is ambiguous (%d results), picking first %q", name, len(resp.Items), resp.Items[0].ID.ChannelID)
	}
	return resp.Items[0].ID.ChannelID, nil
}

// get issues a GET to baseURL+path with params and decodes the JSON body into out.
func (r *ytResolver) get(ctx context.Context, path string, params url.Values, out any) error {
	// reqURL carries the API key as a query param — never log it.
	reqURL := r.baseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("youtube API %s build request: %w", path, err)
	}
	resp, err := r.doer.Do(req)
	if err != nil {
		// *url.Error stringifies the full request URL, which carries the API key as a
		// query param — strip it down to the underlying cause before surfacing/logging.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("youtube API %s request failed: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("YouTube API error (status %d) on %s", resp.StatusCode, path)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("youtube API %s decode response: %w", path, err)
	}
	return nil
}
