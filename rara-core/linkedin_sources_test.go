package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// Core unit tests — linkedin_profile source CRUD
// ---------------------------------------------------------------------------

func TestCoreAddLinkedInProfileStoresURL(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)

	id, err := core.AddLinkedInProfile(ctx, "https://www.linkedin.com/in/handle", "Handle Display")
	if err != nil {
		t.Fatal(err)
	}
	got := db.linkedinProfiles[id]
	if got.ProfileURL != "https://www.linkedin.com/in/handle" {
		t.Errorf("profile_url not stored: %+v", got)
	}
	if got.DisplayName != "Handle Display" {
		t.Errorf("display_name not stored: %+v", got)
	}
	if !got.Active {
		t.Errorf("profile should be active on creation: %+v", got)
	}
}

func TestCoreAddLinkedInProfileIdempotent(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)

	id1, err := core.AddLinkedInProfile(ctx, "https://www.linkedin.com/in/handle", "First Name")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := core.AddLinkedInProfile(ctx, "https://www.linkedin.com/in/handle", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("re-add of same profile_url should be idempotent: id1=%d id2=%d", id1, id2)
	}
	// display_name preserved on empty re-add (COALESCE)
	if db.linkedinProfiles[id1].DisplayName != "First Name" {
		t.Errorf("display_name should be preserved on empty re-add: %+v", db.linkedinProfiles[id1])
	}
}

func TestCoreAddLinkedInProfileRejectsEmptyURL(t *testing.T) {
	ctx := t.Context()
	core, _, _ := newTestCore(t)
	if _, err := core.AddLinkedInProfile(ctx, "   ", ""); !isBadInput(err) {
		t.Fatalf("empty profile_url should be badInput, got %v", err)
	}
}

func TestCoreAddLinkedInProfileRejectsNonLinkedInURL(t *testing.T) {
	ctx := t.Context()
	core, _, _ := newTestCore(t)
	if _, err := core.AddLinkedInProfile(ctx, "https://example.com/in/user", ""); !isBadInput(err) {
		t.Fatalf("non-LinkedIn URL should be badInput, got %v", err)
	}
}

func TestCoreAddLinkedInProfileAcceptsCompanyURL(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)
	id, err := core.AddLinkedInProfile(ctx, "https://www.linkedin.com/company/acme", "Acme")
	if err != nil {
		t.Fatalf("company URL should be accepted: %v", err)
	}
	if db.linkedinProfiles[id].ProfileURL != "https://www.linkedin.com/company/acme" {
		t.Errorf("company URL not stored: %+v", db.linkedinProfiles[id])
	}
}

func TestCoreAddLinkedInProfileNormalizesBareDomain(t *testing.T) {
	ctx := t.Context()
	core, db, _ := newTestCore(t)
	id, err := core.AddLinkedInProfile(ctx, "https://linkedin.com/in/handle", "Handle")
	if err != nil {
		t.Fatalf("bare linkedin.com URL should be accepted: %v", err)
	}
	// Normalized to www.linkedin.com before storage.
	if db.linkedinProfiles[id].ProfileURL != "https://www.linkedin.com/in/handle" {
		t.Errorf("URL not normalized to www form: %+v", db.linkedinProfiles[id])
	}
}

func TestCoreAddLinkedInProfileRejectsInvalidPath(t *testing.T) {
	ctx := t.Context()
	core, _, _ := newTestCore(t)
	for _, u := range []string{
		"https://www.linkedin.com/feed/",
		"https://www.linkedin.com/jobs/",
		"https://www.linkedin.com/",
	} {
		if _, err := core.AddLinkedInProfile(ctx, u, ""); !isBadInput(err) {
			t.Errorf("URL %q should be badInput, got %v", u, err)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP surface tests
// ---------------------------------------------------------------------------

func TestHTTPAddSourceLinkedInProfileStoresURL(t *testing.T) {
	core, db, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)

	rec := do(t, h, http.MethodPost, "/v1/sources/linkedin_profile",
		`{"profile_url":"https://www.linkedin.com/in/handle","display_name":"Handle"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body.String())
	}
	var added struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	got := db.linkedinProfiles[added.ID]
	if got.ProfileURL != "https://www.linkedin.com/in/handle" || got.DisplayName != "Handle" {
		t.Errorf("profile not created as expected: %+v", got)
	}
}

func TestHTTPAddSourceLinkedInProfileEmptyURLIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/sources/linkedin_profile", `{"profile_url":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty profile_url should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAddSourceLinkedInProfileNonLinkedInURLIs400(t *testing.T) {
	core, _, _ := newTestCore(t)
	h := NewSurfaceMux(core, testToken)
	rec := do(t, h, http.MethodPost, "/v1/sources/linkedin_profile",
		`{"profile_url":"https://example.com/in/user"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-LinkedIn URL should be 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
