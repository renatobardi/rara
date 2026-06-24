package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fakeDoer canned-responds per request URL so the resolver is unit-testable with zero I/O.
type fakeDoer struct {
	lastURL string
	body    string
	status  int
	err     error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastURL = req.URL.String()
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func TestYTResolveRawChannelIDSkipsAPI(t *testing.T) {
	doer := &fakeDoer{body: `{"items":[]}`} // would yield not-found if hit
	r := &ytResolver{doer: doer, apiKey: "k", baseURL: "http://test"}

	got, err := r.resolve(context.Background(), "UC1234567890123456789012") // 24 chars, UC prefix
	if err != nil {
		t.Fatal(err)
	}
	if got != "UC1234567890123456789012" {
		t.Errorf("raw id should pass through, got %q", got)
	}
	if doer.lastURL != "" {
		t.Errorf("raw id must not call the API, called %q", doer.lastURL)
	}
}

// A malformed "UC…" (right prefix/length, illegal char) must NOT bypass — it falls
// through to API resolution instead of being persisted as a dead canonical id.
func TestYTResolveMalformedUCDoesNotBypass(t *testing.T) {
	doer := &fakeDoer{body: `{"items":[{"id":{"channelId":"UCsearchresult0000000000"}}]}`}
	r := &ytResolver{doer: doer, apiKey: "k", baseURL: "http://test"}

	got, err := r.resolve(context.Background(), "UC have a space here!!!!") // 24 chars, illegal
	if err != nil {
		t.Fatal(err)
	}
	if doer.lastURL == "" {
		t.Error("malformed UC id should fall through to the API, not bypass")
	}
	if got != "UCsearchresult0000000000" {
		t.Errorf("got %q", got)
	}
}

func TestYTResolveHandle(t *testing.T) {
	doer := &fakeDoer{body: `{"items":[{"id":"UChandleresolved000000000"}]}`}
	r := &ytResolver{doer: doer, apiKey: "k", baseURL: "http://test"}

	got, err := r.resolve(context.Background(), "@SomeHandle")
	if err != nil {
		t.Fatal(err)
	}
	if got != "UChandleresolved000000000" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(doer.lastURL, "/channels") || !strings.Contains(doer.lastURL, "forHandle=%40SomeHandle") {
		t.Errorf("expected channels?forHandle call, got %q", doer.lastURL)
	}
}

func TestYTResolveFreeTextSearch(t *testing.T) {
	doer := &fakeDoer{body: `{"items":[{"id":{"channelId":"UCsearchresult0000000000"}}]}`}
	r := &ytResolver{doer: doer, apiKey: "k", baseURL: "http://test"}

	got, err := r.resolve(context.Background(), "Some Channel Name")
	if err != nil {
		t.Fatal(err)
	}
	if got != "UCsearchresult0000000000" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(doer.lastURL, "/search") || !strings.Contains(doer.lastURL, "type=channel") {
		t.Errorf("expected search?type=channel call, got %q", doer.lastURL)
	}
}

func TestYTResolveNotFound(t *testing.T) {
	doer := &fakeDoer{body: `{"items":[]}`}
	r := &ytResolver{doer: doer, apiKey: "k", baseURL: "http://test"}

	if _, err := r.resolve(context.Background(), "Nonexistent Channel"); !isBadInput(err) {
		t.Fatalf("zero results should be badInput, got %v", err)
	}
}

// A transport error must not surface the request URL (it carries the API key).
func TestYTResolveErrorDoesNotLeakKey(t *testing.T) {
	leaky := &url.Error{
		Op:  "Get",
		URL: "http://test/channels?forHandle=%40h&key=SUPERSECRETKEY",
		Err: context.DeadlineExceeded,
	}
	r := &ytResolver{doer: &fakeDoer{err: leaky}, apiKey: "SUPERSECRETKEY", baseURL: "http://test"}

	_, err := r.resolve(context.Background(), "@h")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SUPERSECRETKEY") || strings.Contains(err.Error(), "key=") {
		t.Errorf("error leaked the API key: %v", err)
	}
}

func TestYTResolveHandleWithoutKeyFails(t *testing.T) {
	r := &ytResolver{doer: &fakeDoer{}, apiKey: "", baseURL: "http://test"}
	if _, err := r.resolve(context.Background(), "@handle"); err == nil {
		t.Fatal("handle resolution without an API key must fail")
	}
}
