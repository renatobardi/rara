package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// MockDatabase — Skill methods (mirror the SQL contract, zero I/O)
// ---------------------------------------------------------------------------

func (m *MockDatabase) UpsertSkill(_ context.Context, name, description, content, config string, trusted bool) (int, error) {
	for i, s := range m.skills {
		if s.Name == name && s.DeletedAt == nil {
			m.skills[i].Description = description
			m.skills[i].Content = content
			m.skills[i].Config = config
			m.skills[i].Trusted = trusted
			return s.ID, nil
		}
	}
	id := m.nextSkillID
	m.nextSkillID++
	m.skills = append(m.skills, mockSkill{
		ID: id, Name: name, Description: description, Content: content, Config: config, Trusted: trusted,
	})
	return id, nil
}

func (m *MockDatabase) ListSkills(_ context.Context) ([]SkillRow, error) {
	var out []SkillRow
	for _, s := range m.skills {
		if s.DeletedAt != nil {
			continue
		}
		cfg := s.Config
		if cfg == "" {
			cfg = "{}"
		}
		out = append(out, SkillRow{
			ID: s.ID, Name: s.Name, Description: s.Description,
			Content: s.Content, Config: json.RawMessage(cfg), Trusted: s.Trusted,
		})
	}
	return out, nil
}

func (m *MockDatabase) DeleteSkill(_ context.Context, id int) error {
	tt := true
	for i, s := range m.skills {
		if s.ID == id {
			m.skills[i].DeletedAt = &tt
			return nil
		}
	}
	return nil
}

// skillActive mirrors the EXISTS (… deleted_at IS NULL) guard in the pgx file queries.
func (m *MockDatabase) skillActive(skillID int) bool {
	for _, s := range m.skills {
		if s.ID == skillID {
			return s.DeletedAt == nil
		}
	}
	return false
}

func (m *MockDatabase) ListSkillFiles(_ context.Context, skillID int) ([]SkillFile, error) {
	if !m.skillActive(skillID) {
		return nil, nil
	}
	var out []SkillFile
	for _, f := range m.skillFiles {
		if f.SkillID == skillID {
			out = append(out, SkillFile{ID: f.ID, SkillID: f.SkillID, Path: f.Path, Content: f.Content})
		}
	}
	return out, nil
}

func (m *MockDatabase) UpsertSkillFile(_ context.Context, skillID int, path, content string) (int, error) {
	if !m.skillActive(skillID) {
		return 0, errNotFound
	}
	for i, f := range m.skillFiles {
		if f.SkillID == skillID && f.Path == path {
			m.skillFiles[i].Content = content
			return f.ID, nil
		}
	}
	id := m.nextSkillFileID
	m.nextSkillFileID++
	m.skillFiles = append(m.skillFiles, mockSkillFile{ID: id, SkillID: skillID, Path: path, Content: content})
	return id, nil
}

func (m *MockDatabase) DeleteSkillFile(_ context.Context, skillID int, path string) error {
	kept := m.skillFiles[:0]
	for _, f := range m.skillFiles {
		if f.SkillID == skillID && f.Path == path {
			continue
		}
		kept = append(kept, f)
	}
	m.skillFiles = kept
	return nil
}

// ---------------------------------------------------------------------------
// Core.UpsertSkill / ListSkills / DeleteSkill
// ---------------------------------------------------------------------------

// mustSkill upserts a skill in test setup, failing the test on error (so a seam regression
// surfaces as the exact failure, never a masked false positive).
func mustSkill(t *testing.T, core *Core, ctx context.Context, in SkillInput) int {
	t.Helper()
	id, err := core.UpsertSkill(ctx, in)
	if err != nil {
		t.Fatalf("UpsertSkill(%q): %v", in.Name, err)
	}
	return id
}

// mustSkillFile upserts a skill file in test setup, failing on error.
func mustSkillFile(t *testing.T, core *Core, ctx context.Context, skillID int, in SkillFileInput) {
	t.Helper()
	if _, err := core.UpsertSkillFile(ctx, skillID, in); err != nil {
		t.Fatalf("UpsertSkillFile(%q): %v", in.Path, err)
	}
}

// mustList reads the skill list in test setup, failing on error.
func mustList(t *testing.T, core *Core, ctx context.Context) []SkillRow {
	t.Helper()
	got, err := core.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	return got
}

// mustFiles reads a skill's files in test setup, failing on error.
func mustFiles(t *testing.T, core *Core, ctx context.Context, skillID int) []SkillFile {
	t.Helper()
	got, err := core.ListSkillFiles(ctx, skillID)
	if err != nil {
		t.Fatalf("ListSkillFiles(%d): %v", skillID, err)
	}
	return got
}

func TestUpsertSkillCreatesAndLists(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)

	if _, err := core.UpsertSkill(ctx, SkillInput{Name: "docslide", Description: "carousels", Content: "# hi"}); err != nil {
		t.Fatalf("UpsertSkill: %v", err)
	}
	got, err := core.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(got) != 1 || got[0].Name != "docslide" || got[0].Content != "# hi" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Trusted {
		t.Error("new skill must default trusted=false")
	}
	if string(got[0].Config) != "{}" {
		t.Errorf("config = %s, want {}", got[0].Config)
	}
}

func TestUpsertSkillRejectsEmptyName(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.UpsertSkill(ctx, SkillInput{Name: "  "})
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Fatalf("want badInput, got %v", err)
	}
}

func TestUpsertSkillRejectsNonObjectConfig(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	_, err := core.UpsertSkill(ctx, SkillInput{Name: "x", Config: json.RawMessage(`[1,2]`)})
	var bad badInputError
	if !errors.As(err, &bad) {
		t.Fatalf("want badInput for array config, got %v", err)
	}
}

func TestUpsertSkillTogglesTrusted(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	mustSkill(t, core, ctx, SkillInput{Name: "x", Trusted: true})
	got := mustList(t, core, ctx)
	if !got[0].Trusted {
		t.Fatal("trusted=true not persisted")
	}
}

func TestDeleteSkillSoftDeletes(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	id := mustSkill(t, core, ctx, SkillInput{Name: "x"})
	if err := core.DeleteSkill(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := mustList(t, core, ctx); len(got) != 0 {
		t.Fatalf("soft-deleted skill still listed: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Core skill files
// ---------------------------------------------------------------------------

func TestSkillFileCRUD(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	sid := mustSkill(t, core, ctx, SkillInput{Name: "x"})

	mustSkillFile(t, core, ctx, sid, SkillFileInput{Path: "utils.py", Content: "print(1)"})
	if files := mustFiles(t, core, ctx, sid); len(files) != 1 || files[0].Path != "utils.py" {
		t.Fatalf("got %+v", files)
	}
	if err := core.DeleteSkillFile(ctx, sid, "utils.py"); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if files := mustFiles(t, core, ctx, sid); len(files) != 0 {
		t.Fatalf("file not deleted: %+v", files)
	}
}

func TestUpsertSkillFileRejectsTraversalPath(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	sid := mustSkill(t, core, ctx, SkillInput{Name: "x"})
	for _, p := range []string{"", "../etc/passwd", "/abs/path", "a/../../b", `C:\temp\x`, "a.txt:stream", "SKILL.md", "skill.MD"} {
		if _, err := core.UpsertSkillFile(ctx, sid, SkillFileInput{Path: p, Content: "x"}); err == nil {
			t.Errorf("path %q accepted, want rejection", p)
		}
	}
}

func TestSkillFilesGuardedBySoftDeletedParent(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	sid := mustSkill(t, core, ctx, SkillInput{Name: "x"})
	mustSkillFile(t, core, ctx, sid, SkillFileInput{Path: "utils.py", Content: "x"})
	if err := core.DeleteSkill(ctx, sid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Listing a soft-deleted skill's files must return nothing.
	if files := mustFiles(t, core, ctx, sid); len(files) != 0 {
		t.Errorf("soft-deleted skill still exposes files: %+v", files)
	}
	// Writing to a soft-deleted skill must fail (errNotFound → 404).
	if _, err := core.UpsertSkillFile(ctx, sid, SkillFileInput{Path: "new.py", Content: "y"}); err == nil {
		t.Error("upsert file on soft-deleted skill accepted, want rejection")
	}
}

// ---------------------------------------------------------------------------
// parseSkillMD
// ---------------------------------------------------------------------------

func TestParseSkillMD(t *testing.T) {
	md := "---\nname: linkedin-docslide\ndescription: Criar Docslides\n---\n# Body\ntext"
	name, desc, content := parseSkillMD([]byte(md))
	if name != "linkedin-docslide" {
		t.Errorf("name = %q", name)
	}
	if desc != "Criar Docslides" {
		t.Errorf("desc = %q", desc)
	}
	if content != md {
		t.Errorf("content must be the full SKILL.md, got %q", content)
	}
}

// ---------------------------------------------------------------------------
// Core.ImportSkill (fetch seam injected — zero real I/O)
// ---------------------------------------------------------------------------

func TestImportSkillFetchesAndStoresUntrusted(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	core.fetchURL = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("---\nname: linkedin-docslide\ndescription: d\n---\n# SKILL"), nil
	}
	row, err := core.ImportSkill(ctx, "https://clawhub.example/skills/linkedin-docslide")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if row.Name != "linkedin-docslide" || row.Trusted {
		t.Fatalf("imported skill must be named + untrusted, got %+v", row)
	}
	got := mustList(t, core, ctx)
	if len(got) != 1 || got[0].Trusted {
		t.Fatalf("imported skill not persisted untrusted: %+v", got)
	}
	// Provenance recorded in config.
	if !strings.Contains(string(got[0].Config), "source_url") {
		t.Errorf("config missing source_url provenance: %s", got[0].Config)
	}
}

func TestImportSkillRejectsSSRFURL(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	called := false
	core.fetchURL = func(_ context.Context, _ string) ([]byte, error) {
		called = true
		return nil, errors.New("should not fetch")
	}
	// Cover the SSRF vectors validateEndpointURL blocks — loopback, private ranges, cloud metadata,
	// link-local and bad schemes — so a regression in any one is caught.
	for _, u := range []string{
		"http://169.254.169.254/latest",
		"http://localhost/x",
		"http://127.0.0.1/x",
		"http://10.0.0.1/x",
		"http://172.16.0.1/x",
		"http://192.168.1.1/x",
		"http://metadata.google.internal/x",
		"file:///etc/passwd",
	} {
		if _, err := core.ImportSkill(ctx, u); err == nil {
			t.Errorf("SSRF URL %q accepted, want rejection", u)
		}
	}
	if called {
		t.Fatal("fetchURL was called for an SSRF-blocked URL — the guard must reject before fetching")
	}
}

func TestImportSkillRejectsMissingName(t *testing.T) {
	ctx := context.Background()
	core, _, _ := newTestCore(t)
	core.fetchURL = func(_ context.Context, _ string) ([]byte, error) { return []byte("# no frontmatter"), nil }
	if _, err := core.ImportSkill(ctx, "https://clawhub.example/x"); err == nil {
		t.Fatal("SKILL.md without name accepted, want rejection")
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestSkillsHTTPRoundTrip(t *testing.T) {
	core, _, _ := newTestCore(t)
	mux := NewSurfaceMux(core, "tok")
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("PUT", "/v1/skills", `{"name":"docslide","content":"# hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT /v1/skills: %d %s", rec.Code, rec.Body)
	}
	rec := do("GET", "/v1/skills", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docslide") {
		t.Fatalf("GET /v1/skills: %d %s", rec.Code, rec.Body)
	}
}

// authedDo runs an authenticated request against a surface mux and returns the recorder.
func authedDo(t *testing.T, mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSkillImportHTTP(t *testing.T) {
	core, _, _ := newTestCore(t)
	core.fetchURL = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("---\nname: imported\ndescription: d\n---\n# SKILL"), nil
	}
	mux := NewSurfaceMux(core, "tok")

	rec := authedDo(t, mux, "POST", "/v1/skills/import", `{"url":"https://clawhub.example/imported"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "imported") {
		t.Fatalf("POST /v1/skills/import: %d %s", rec.Code, rec.Body)
	}
	// Imported skill must be queryable and untrusted.
	list := authedDo(t, mux, "GET", "/v1/skills", "")
	if !strings.Contains(list.Body.String(), `"trusted":false`) {
		t.Errorf("imported skill should be untrusted: %s", list.Body)
	}
}

func TestSkillImportHTTPRejectsSSRF(t *testing.T) {
	core, _, _ := newTestCore(t)
	core.fetchURL = func(_ context.Context, _ string) ([]byte, error) { return []byte("---\nname: x\n---\n"), nil }
	mux := NewSurfaceMux(core, "tok")
	rec := authedDo(t, mux, "POST", "/v1/skills/import", `{"url":"http://169.254.169.254/x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import of SSRF URL: status=%d, want 400; body=%s", rec.Code, rec.Body)
	}
}

func TestSkillFilesHTTP(t *testing.T) {
	core, _, _ := newTestCore(t)
	ctx := context.Background()
	sid := mustSkill(t, core, ctx, SkillInput{Name: "docslide"})
	mux := NewSurfaceMux(core, "tok")
	base := fmt.Sprintf("/v1/skills/%d/files", sid)

	// PUT a file.
	if rec := authedDo(t, mux, "PUT", base, `{"path":"utils.py","content":"print(1)"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT %s: %d %s", base, rec.Code, rec.Body)
	}
	// GET lists it.
	if rec := authedDo(t, mux, "GET", base, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "utils.py") {
		t.Fatalf("GET %s: %d %s", base, rec.Code, rec.Body)
	}
	// A traversal path is rejected (400) before persistence.
	if rec := authedDo(t, mux, "PUT", base, `{"path":"../escape","content":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT traversal path: status=%d, want 400", rec.Code)
	}
	// DELETE removes it.
	if rec := authedDo(t, mux, "DELETE", base, `{"path":"utils.py"}`); rec.Code != http.StatusOK {
		t.Fatalf("DELETE %s: %d %s", base, rec.Code, rec.Body)
	}
	if files := mustFiles(t, core, ctx, sid); len(files) != 0 {
		t.Errorf("file not deleted over HTTP: %+v", files)
	}
}

// A non-numeric {id} in the files path must 400 (not reach the handler logic).
func TestSkillFilesHTTPRejectsBadID(t *testing.T) {
	core, _, _ := newTestCore(t)
	mux := NewSurfaceMux(core, "tok")
	if rec := authedDo(t, mux, "GET", "/v1/skills/abc/files", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("GET /v1/skills/abc/files: status=%d, want 400", rec.Code)
	}
}
