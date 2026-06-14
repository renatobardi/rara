package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
)

// b64 encodes a body the way Gmail does (URL-safe, unpadded).
func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// ---------------------------------------------------------------------------
// parseMessageJSON / parseMessageListJSON — pure parsing, zero I/O.
// ---------------------------------------------------------------------------

func TestParseMessageListJSON(t *testing.T) {
	data := []byte(`{"messages":[{"id":"a"},{"id":"b"},{"id":""}],"nextPageToken":"TOK"}`)
	ids, next, err := parseMessageListJSON(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("ids = %v, want [a b] (empty id skipped)", ids)
	}
	if next != "TOK" {
		t.Errorf("nextPageToken = %q, want TOK", next)
	}
}

func TestParseMessageJSONMultipart(t *testing.T) {
	// A multipart/alternative message: text/plain preferred over text/html.
	msg := fmt.Sprintf(`{
		"id": "msg-1",
		"internalDate": "1717977600000",
		"payload": {
			"mimeType": "multipart/alternative",
			"headers": [
				{"name":"From","value":"Alice <alice@example.com>"},
				{"name":"Subject","value":"Hello there"}
			],
			"parts": [
				{"mimeType":"text/plain","body":{"data":"%s"}},
				{"mimeType":"text/html","body":{"data":"%s"}}
			]
		}
	}`, b64("the plain body"), b64("<p>the html body</p>"))

	e, err := parseMessageJSON([]byte(msg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.MessageID != "msg-1" {
		t.Errorf("id = %q", e.MessageID)
	}
	if e.Sender != "Alice <alice@example.com>" || e.Subject != "Hello there" {
		t.Errorf("headers = %q / %q", e.Sender, e.Subject)
	}
	if e.Body != "the plain body" {
		t.Errorf("body = %q, want the plain body (text/plain preferred)", e.Body)
	}
	if e.ReceivedAt == nil || e.ReceivedAt.UTC().Format("2006-01-02") != "2024-06-10" {
		t.Errorf("received_at not parsed from internalDate: %v", e.ReceivedAt)
	}
}

func TestParseMessageJSONHTMLOnly(t *testing.T) {
	// No text/plain part -> fall back to the html body (raw; the extractor strips it later).
	msg := fmt.Sprintf(`{
		"id": "msg-2",
		"payload": {
			"mimeType": "text/html",
			"headers": [{"name":"Subject","value":"HTML only"}],
			"body": {"data":"%s"}
		}
	}`, b64("<div>only html</div>"))
	e, err := parseMessageJSON([]byte(msg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Body != "<div>only html</div>" {
		t.Errorf("body = %q, want the raw html", e.Body)
	}
}

func TestParseMessageJSONMalformed(t *testing.T) {
	if _, err := parseMessageJSON([]byte("not json")); err == nil {
		t.Error("malformed message JSON should error")
	}
}

func TestDecodeB64URL(t *testing.T) {
	if got := decodeB64URL(b64("hi")); got != "hi" {
		t.Errorf("decode = %q, want hi", got)
	}
	if got := decodeB64URL(""); got != "" {
		t.Errorf("empty -> empty, got %q", got)
	}
	if got := decodeB64URL("!!!not base64!!!"); got != "" {
		t.Errorf("undecodable -> empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// MockDatabase + fake GmailAPI — the collector loop, zero I/O.
// ---------------------------------------------------------------------------

type MockDatabase struct {
	emails map[string]Email // keyed by message_id (UNIQUE)
	err    error
}

func newMockDatabase() *MockDatabase { return &MockDatabase{emails: map[string]Email{}} }

func (m *MockDatabase) UpsertEmail(_ context.Context, e Email) error {
	if m.err != nil {
		return m.err
	}
	m.emails[e.MessageID] = e // ON CONFLICT (message_id) DO UPDATE
	return nil
}

var _ Database = (*MockDatabase)(nil)

// fakeGmail serves canned ids and messages; getErr/missing simulate per-message failures.
type fakeGmail struct {
	ids      []string
	messages map[string]Email
	listErr  error
	getErr   map[string]error
}

func (f *fakeGmail) ListMessageIDs(_ context.Context, _ string, max int) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.ids) > max {
		return f.ids[:max], nil
	}
	return f.ids, nil
}

func (f *fakeGmail) GetMessage(_ context.Context, id string) (Email, error) {
	if err := f.getErr[id]; err != nil {
		return Email{}, err
	}
	e, ok := f.messages[id]
	if !ok {
		return Email{}, fmt.Errorf("no such message %q", id)
	}
	return e, nil
}

var _ GmailAPI = (*fakeGmail)(nil)

func TestRunCollectsEmails(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	api := &fakeGmail{
		ids: []string{"a", "b"},
		messages: map[string]Email{
			"a": {MessageID: "a", Subject: "First", Body: "body a"},
			"b": {MessageID: "b", Subject: "Second", Body: "body b"},
		},
	}
	n, err := run(ctx, db, api, "newer_than:30d", 100)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 || len(db.emails) != 2 {
		t.Errorf("catalogued %d / stored %d, want 2/2", n, len(db.emails))
	}
}

func TestRunIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	api := &fakeGmail{ids: []string{"a"}, messages: map[string]Email{"a": {MessageID: "a", Body: "x"}}}
	if _, err := run(ctx, db, api, "", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, db, api, "", 100); err != nil {
		t.Fatal(err)
	}
	if len(db.emails) != 1 {
		t.Errorf("re-run duplicated: %d emails, want 1", len(db.emails))
	}
}

func TestRunSkipsBadMessage(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	api := &fakeGmail{
		ids:      []string{"a", "b"},
		messages: map[string]Email{"b": {MessageID: "b", Body: "ok"}},
		getErr:   map[string]error{"a": errors.New("boom")},
	}
	n, err := run(ctx, db, api, "", 100)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 || len(db.emails) != 1 {
		t.Errorf("catalogued %d, want 1 (bad message skipped)", n)
	}
}

func TestRunSurfacesListError(t *testing.T) {
	db := newMockDatabase()
	api := &fakeGmail{listErr: errors.New("api down")}
	if _, err := run(context.Background(), db, api, "", 100); err == nil {
		t.Error("a list error should abort the run")
	}
}
