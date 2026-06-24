// rara-courier is the 2.0 Email lane collector: a new, isolated agent that collects messages from
// Gmail (OAuth refresh-token auth, the rara-shelf pattern) and catalogs each into its own
// domain table, emails. Like every rara agent it shares nothing but the Neon database and never
// calls another agent — the control plane (rara-core) reads emails to build the items spine
// (lane=email, source_ref=message_id, sensitivity=private) and the extrair worker reads body to
// clean it. Idempotent: every run upserts on the Gmail message id.
//
// The Gmail JSON parsing (headers, base64url body) and the collector loop are pure over two
// seams — a GmailAPI (HTTP) and a Database (Neon) — so the logic is unit-tested with zero I/O
// (main_test.go). Email content is private; this agent only stores it, the router enforces
// where it may be processed.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// httpClient is shared across API calls to reuse TCP connections (keep-alive).
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Email is one collected message.
type Email struct {
	MessageID  string
	Sender     string
	Subject    string
	Body       string
	ReceivedAt *time.Time
}

// GmailAPI is the fetch seam (HTTP + OAuth). Tests inject a fake so the collector loop runs
// with zero network I/O.
type GmailAPI interface {
	ListMessageIDs(ctx context.Context, query string, max int) ([]string, error)
	GetMessage(ctx context.Context, id string) (Email, error)
}

// EmailSource is one enabled Gmail reading rule from the email_sources table.
// The courier iterates all active rules and composes a Gmail search query from
// the non-empty fields (gmail_query, label, from_filter ANDed together).
type EmailSource struct {
	ID          int
	DisplayName string
	GmailQuery  string
	Label       string
	FromFilter  string
}

// Database is the persistence seam: the idempotent email upsert and provider stamp. The pgx
// implementation talks to Neon; tests use an in-memory mock.
type Database interface {
	ListEmailSources(ctx context.Context) ([]EmailSource, error)
	UpsertEmail(ctx context.Context, e Email) error
	StampProviderCollected(ctx context.Context, name string) error
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
	max := 100
	if v := os.Getenv("MAIL_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := pgx.Connect(connectCtx, databaseURL)
	cancel()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer conn.Close(context.Background())
	log.Println("Connected to database successfully")

	tokenCtx, tokenCancel := context.WithTimeout(context.Background(), 15*time.Second)
	accessToken, err := getAccessToken(tokenCtx, clientID, clientSecret, refreshToken)
	tokenCancel()
	if err != nil {
		log.Fatalf("Failed to obtain access token: %v", err)
	}
	log.Println("OAuth access token obtained")

	api := &httpGmailAPI{accessToken: accessToken}
	n, err := run(context.Background(), &pgxDatabase{conn: conn}, api, max)
	if err != nil {
		log.Fatalf("mail collector: %v", err)
	}
	log.Printf("Mail job completed: %d emails catalogued", n)
}

// buildGmailQuery composes a Gmail search query from the non-empty fields of a source.
// label and from_filter are translated to Gmail search operators; gmail_query is appended as-is.
func buildGmailQuery(src EmailSource) string {
	parts := make([]string, 0, 3)
	if src.Label != "" {
		parts = append(parts, "label:"+src.Label)
	}
	if src.FromFilter != "" {
		parts = append(parts, "from:"+src.FromFilter)
	}
	if src.GmailQuery != "" {
		parts = append(parts, src.GmailQuery)
	}
	return strings.Join(parts, " ")
}

// collectIDs fetches and upserts each message id for one source rule.
// Returns the number of successfully upserted messages; per-message errors are logged and skipped.
func collectIDs(ctx context.Context, db Database, api GmailAPI, ids []string) int {
	n := 0
	for _, id := range ids {
		e, err := api.GetMessage(ctx, id)
		if err != nil {
			log.Printf("get message %s: %v", id, err)
			continue
		}
		if e.MessageID == "" {
			continue
		}
		if err := db.UpsertEmail(ctx, e); err != nil {
			log.Printf("upsert email %s: %v", id, err)
			continue
		}
		n++
	}
	return n
}

// run is the collector loop: for each enabled email_sources rule, list matching message IDs,
// fetch each, and upsert. Per-source list errors are logged and skipped — one bad source must
// not stall the rest. The DB deduplicates by message_id (ON CONFLICT), so the same message
// returned by two rules is upserted idempotently.
func run(ctx context.Context, db Database, api GmailAPI, max int) (int, error) {
	sources, err := db.ListEmailSources(ctx)
	if err != nil {
		return 0, fmt.Errorf("list email sources: %w", err)
	}
	catalogued := 0
	for _, src := range sources {
		q := buildGmailQuery(src)
		if q == "" {
			continue
		}
		ids, err := api.ListMessageIDs(ctx, q, max)
		if err != nil {
			log.Printf("list messages for source %d (%s): %v", src.ID, src.DisplayName, err)
			continue
		}
		catalogued += collectIDs(ctx, db, api, ids)
	}
	if err := db.StampProviderCollected(ctx, "courier"); err != nil {
		log.Printf("stamp provider courier: %v", err)
	}
	return catalogued, nil
}

// ---------------------------------------------------------------------------
// Gmail JSON parsing — pure (no I/O), so it is fully unit-tested.
// ---------------------------------------------------------------------------

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// gmailPart is one MIME part (recursive). Gmail nests multipart bodies under `parts`.
type gmailPart struct {
	MimeType string        `json:"mimeType"`
	Headers  []gmailHeader `json:"headers"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

type gmailMessage struct {
	ID           string    `json:"id"`
	InternalDate string    `json:"internalDate"`
	Payload      gmailPart `json:"payload"`
}

// parseMessageListJSON extracts the message ids and the next page token from a
// users.messages.list response.
func parseMessageListJSON(data []byte) (ids []string, nextPageToken string, err error) {
	var lr struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &lr); err != nil {
		return nil, "", err
	}
	for _, m := range lr.Messages {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, lr.NextPageToken, nil
}

// parseMessageJSON turns a users.messages.get (format=full) response into an Email: the From
// and Subject headers, the decoded body (preferring text/plain over text/html), and the
// internalDate as received_at.
func parseMessageJSON(data []byte) (Email, error) {
	var m gmailMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return Email{}, err
	}
	e := Email{
		MessageID: m.ID,
		Sender:    findHeader(m.Payload.Headers, "From"),
		Subject:   findHeader(m.Payload.Headers, "Subject"),
		Body:      extractBody(m.Payload),
	}
	if ms, err := strconv.ParseInt(strings.TrimSpace(m.InternalDate), 10, 64); err == nil && ms > 0 {
		t := time.UnixMilli(ms).UTC()
		e.ReceivedAt = &t
	}
	return e, nil
}

// findHeader returns a header value by name (case-insensitive), or "".
func findHeader(headers []gmailHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// extractBody returns the message body, preferring text/plain over text/html, walking the MIME
// tree. The raw HTML (when that is all there is) is handed downstream as-is — the extrair
// worker in rara-core strips it.
func extractBody(p gmailPart) string {
	plain, html := findBodies(p)
	if plain != "" {
		return plain
	}
	return html
}

// findBodies recursively collects the first text/plain and first text/html body it finds.
func findBodies(p gmailPart) (plain, html string) {
	switch {
	case strings.HasPrefix(p.MimeType, "text/plain"):
		plain = decodeB64URL(p.Body.Data)
	case strings.HasPrefix(p.MimeType, "text/html"):
		html = decodeB64URL(p.Body.Data)
	}
	for _, child := range p.Parts {
		cp, ch := findBodies(child)
		if plain == "" {
			plain = cp
		}
		if html == "" {
			html = ch
		}
	}
	return plain, html
}

// decodeB64URL decodes Gmail's URL-safe base64 body data, which is usually unpadded. Returns ""
// on any decode failure (a body we cannot decode is treated as empty rather than an error).
func decodeB64URL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return ""
}

// ---------------------------------------------------------------------------
// OAuth + HTTP Gmail API — the production GmailAPI.
// ---------------------------------------------------------------------------

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

// getAccessToken exchanges an OAuth refresh token for a short-lived access token (the
// rara-shelf pattern: a plain form POST, no oauth2 library).
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
		// Do NOT log the body: Google may echo back client_secret in some error payloads.
		return "", fmt.Errorf("token endpoint returned status %d (check client_id/secret/refresh_token)", resp.StatusCode)
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

type httpGmailAPI struct{ accessToken string }

const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me"

// ListMessageIDs lists message ids matching the query, following pagination up to max.
func (a *httpGmailAPI) ListMessageIDs(ctx context.Context, query string, max int) ([]string, error) {
	var ids []string
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("maxResults", strconv.Itoa(min(max, 500)))
		if query != "" {
			params.Set("q", query)
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		body, err := a.get(ctx, gmailBase+"/messages?"+params.Encode())
		if err != nil {
			return nil, err
		}
		pageIDs, next, err := parseMessageListJSON(body)
		if err != nil {
			return nil, err
		}
		ids = append(ids, pageIDs...)
		if next == "" || len(ids) >= max {
			break
		}
		pageToken = next
	}
	if len(ids) > max {
		ids = ids[:max]
	}
	return ids, nil
}

// GetMessage fetches and parses one message (format=full).
func (a *httpGmailAPI) GetMessage(ctx context.Context, id string) (Email, error) {
	body, err := a.get(ctx, gmailBase+"/messages/"+id+"?format=full")
	if err != nil {
		return Email{}, err
	}
	return parseMessageJSON(body)
}

// get performs an authenticated GET and returns the body bytes.
func (a *httpGmailAPI) get(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gmail API status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// pgx Database — Neon implementation of the persistence seam.
// ---------------------------------------------------------------------------

type pgxDatabase struct{ conn *pgx.Conn }

// StampProviderCollected updates the providers table to record the time of the last successful
// collection run for this agent.
func (d *pgxDatabase) StampProviderCollected(ctx context.Context, name string) error {
	const q = `UPDATE providers SET last_collect_at = now() WHERE name = $1`
	tag, err := d.conn.Exec(ctx, q, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("provider %q not found in providers table", name)
	}
	return nil
}

// ListEmailSources returns all enabled email reading rules from email_sources, ordered by id.
func (d *pgxDatabase) ListEmailSources(ctx context.Context) ([]EmailSource, error) {
	const q = `SELECT id, COALESCE(display_name,''), COALESCE(gmail_query,''), COALESCE(label,''), COALESCE(from_filter,'')
	           FROM email_sources WHERE enabled = true AND deleted_at IS NULL ORDER BY id`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailSource
	for rows.Next() {
		var s EmailSource
		if err := rows.Scan(&s.ID, &s.DisplayName, &s.GmailQuery, &s.Label, &s.FromFilter); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpsertEmail inserts an email, idempotent on message_id. On conflict it refreshes the
// metadata/body so an edited message propagates, but never collected_at/status.
func (d *pgxDatabase) UpsertEmail(ctx context.Context, e Email) error {
	const q = `
		INSERT INTO emails (message_id, sender, subject, body, received_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (message_id) DO UPDATE
		SET sender      = EXCLUDED.sender,
		    subject     = EXCLUDED.subject,
		    body        = EXCLUDED.body,
		    received_at = EXCLUDED.received_at`
	_, err := d.conn.Exec(ctx, q, e.MessageID, e.Sender, e.Subject, e.Body, e.ReceivedAt)
	return err
}
