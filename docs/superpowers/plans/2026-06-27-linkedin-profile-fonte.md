# LinkedIn Profile Source — Fontes UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `linkedin_profile` as a first-class source kind in the Fontes admin UI so operators can create, pause, resume, tag, and delete LinkedIn profile URLs through the same wizard used for RSS, YouTube, podcast, and email sources.

**Architecture:** The `target_linkedin_profiles` table lives in `rara-clip`'s migrations but is in the shared Neon DB. `rara-core` is the right place to own the unified source surface — we extend `sources_v` (migration 026) to UNION ALL from that table, extend the `sourceKindsRegistry` and `validSourceKinds`, add the `CreateLinkedInProfile` DB method, and wire the `addSource` switch case. The console's wizard is already data-driven; only the ICONS glyph map needs a `linkedin` entry.

**Tech Stack:** Go (rara-core), SQL (Neon/PostgreSQL), Svelte 5 (rara-console UI).

## Global Constraints

- Go 1.26, `package main` flat layout per agent.
- TDD mandatory: write failing test first, then implementation, then `make test` must pass.
- Idempotency everywhere: all writes are upserts (`ON CONFLICT`); migration is `CREATE OR REPLACE` / `IF NOT EXISTS`.
- No real I/O in unit tests — all DB calls go through `MockDatabase`.
- CodeRabbit review before merge (project standard).
- URL validation must reject non-LinkedIn URLs and SSRF vectors (loopback/private IPs not applicable here — only LinkedIn.com allowed).
- Migration numbering: next is `026`.
- All code changes in `rara-core/` and one line in `rara-console/`.

---

## File Map

| File | Action | What changes |
|------|--------|-------------|
| `rara-core/migrations/026_linkedin_profile_source.sql` | CREATE | Adds `tags`, `display_name`, `deleted_at` (IF NOT EXISTS) to `target_linkedin_profiles`; partial live index; rewrites `sources_v` with new UNION ALL branch |
| `rara-core/surface.go` | MODIFY | `validSourceKinds`, `sourceKindsRegistry`, `Core.AddLinkedInProfile`, `addSource` switch case |
| `rara-core/main.go` | MODIFY | `Database` interface: `CreateLinkedInProfile`; `pgxDatabase` impl; `SetSourceActive` / `SetSourceDeleted` / `PatchSourceMeta` switch cases |
| `rara-core/main_test.go` | MODIFY | `MockDatabase` backing store + `CreateLinkedInProfile` + three switch cases |
| `rara-core/linkedin_sources_test.go` | CREATE | Core unit tests + HTTP surface tests |
| `rara-console/web/src/routes/fontes/+page.svelte` | MODIFY | Add `linkedin: 'in'` to `ICONS` map |

---

### Task 1: Migration — extend `target_linkedin_profiles` and `sources_v`

**Files:**
- Create: `rara-core/migrations/026_linkedin_profile_source.sql`

**Interfaces:**
- Produces: `sources_v` now contains `linkedin_profile` rows; `target_linkedin_profiles` has `tags`, `display_name`, `deleted_at` columns.

- [ ] **Step 1: Write the migration**

```sql
-- migrations/026_linkedin_profile_source.sql
-- Adds target_linkedin_profiles (rara-clip) to the unified sources_v so the Fontes UI
-- can list, create, pause/resume, tag, and delete LinkedIn profile sources.
--
-- target_linkedin_profiles is owned by rara-clip (002_target_profiles.sql). This migration
-- adds the common source columns (tags, display_name, deleted_at) IF NOT EXISTS so it is safe
-- to apply regardless of whether rara-clip's migration ran before or after this one.
-- Idempotent: ALTER TABLE … IF NOT EXISTS + CREATE OR REPLACE VIEW.

-- ---------------------------------------------------------------------------
-- 1. Ensure common source columns exist on target_linkedin_profiles.
-- ---------------------------------------------------------------------------
ALTER TABLE target_linkedin_profiles
    ADD COLUMN IF NOT EXISTS tags         text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS display_name text,
    ADD COLUMN IF NOT EXISTS deleted_at   TIMESTAMPTZ;

-- Partial index over the live set (mirrors the other source tables in migration 025).
CREATE INDEX IF NOT EXISTS target_linkedin_profiles_live_idx
    ON target_linkedin_profiles (id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 2. Rebuild sources_v — same shape as 025, now including linkedin_profile.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW sources_v AS

-- YouTube channels — rara-harvest
SELECT
    format('youtube_channel:%s', id)           AS api_id,
    'youtube_channel'                           AS kind,
    'youtube'                                   AS lane,
    COALESCE(display_name, channel_name)        AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    channel_name                                AS config_summary,
    created_at,
    updated_at
FROM target_channels
WHERE deleted_at IS NULL

UNION ALL

-- YouTube playlists — rara-shelf
SELECT
    format('youtube_playlist:%s', id)           AS api_id,
    'youtube_playlist'                           AS kind,
    'youtube'                                    AS lane,
    COALESCE(display_name, title)                AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    title                                        AS config_summary,
    created_at,
    updated_at
FROM playlists
WHERE deleted_at IS NULL

UNION ALL

-- Podcast feeds — rara-dial
SELECT
    format('podcast:%s', id)                    AS api_id,
    'podcast'                                    AS kind,
    'podcast'                                    AS lane,
    COALESCE(display_name, title, feed_url)      AS display_name,
    tags,
    CASE WHEN active  THEN 'active' ELSE 'paused' END AS status,
    feed_url                                     AS config_summary,
    created_at,
    updated_at
FROM podcast_feeds
WHERE deleted_at IS NULL

UNION ALL

-- News/RSS/HTML/HN sources — rara-feed
SELECT
    format('%s:%s', source_type, id)            AS api_id,
    source_type                                  AS kind,
    'news'                                       AS lane,
    COALESCE(display_name, name)                 AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END AS status,
    endpoint                                     AS config_summary,
    created_at,
    updated_at
FROM feed_sources
WHERE deleted_at IS NULL

UNION ALL

-- Email reading rules — rara-courier
SELECT
    format('email:%s', id)                                     AS api_id,
    'email'                                                     AS kind,
    'email'                                                     AS lane,
    COALESCE(display_name, 'Email rule ' || id::text)          AS display_name,
    tags,
    CASE WHEN enabled THEN 'active' ELSE 'paused' END          AS status,
    COALESCE(label, gmail_query, from_filter)                   AS config_summary,
    created_at,
    updated_at
FROM email_sources
WHERE deleted_at IS NULL

UNION ALL

-- LinkedIn profiles — rara-clip (target_linkedin_profiles)
SELECT
    format('linkedin_profile:%s', id)                         AS api_id,
    'linkedin_profile'                                         AS kind,
    'linkedin'                                                 AS lane,
    COALESCE(display_name, profile_url)                       AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END           AS status,
    profile_url                                               AS config_summary,
    created_at,
    updated_at
FROM target_linkedin_profiles
WHERE deleted_at IS NULL;

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL (soft-deleted sources are hidden); status=active|paused derived from active/enabled flags';
```

- [ ] **Step 2: Verify migration is syntactically correct**

```bash
cd rara-core
# Run the migration test (migration_test.go applies all migrations in order against an ephemeral DB)
go test -run TestMigrations -v ./...
```

Expected: PASS — migration 026 applies cleanly after 025.

- [ ] **Step 3: Commit**

```bash
cd rara-core
git add migrations/026_linkedin_profile_source.sql
git commit -m "feat(core): migration 026 — add linkedin_profile to sources_v"
```

---

### Task 2: Core domain — `AddLinkedInProfile` + registry + switch

**Files:**
- Modify: `rara-core/surface.go` (lines ~502–659 and ~1148–1234)
- Modify: `rara-core/main.go` (Database interface ~615–631, pgxDatabase ~1055–1161)

**Interfaces:**
- Consumes: `c.db.CreateLinkedInProfile(ctx, profileURL, displayName string) (int, error)` (defined in Task 2, implemented in Task 2)
- Produces: `Core.AddLinkedInProfile(ctx, profileURL, displayName string) (int, error)` — called by the HTTP handler added in this same task.

**Step-by-step:**

- [ ] **Step 1: Write the failing tests** (`linkedin_sources_test.go`)

Create `rara-core/linkedin_sources_test.go`:

```go
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
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
cd rara-core
go test -run TestCoreAddLinkedInProfile -v ./...
go test -run TestHTTPAddSourceLinkedInProfile -v ./...
```

Expected: compile error — `core.AddLinkedInProfile` undefined.

- [ ] **Step 3: Add `Database.CreateLinkedInProfile` to the interface in `main.go`**

Find the Database interface block around line 617 (after `CreateEmailSource`). Add:

```go
// CreateLinkedInProfile upserts a LinkedIn profile URL into target_linkedin_profiles.
// Idempotent on profile_url; display_name is preserved on empty re-add (COALESCE).
CreateLinkedInProfile(ctx context.Context, profileURL, displayName string) (int, error)
```

- [ ] **Step 4: Add `linkedin_profile` cases to the three dispatch switches in `main.go`**

In `SetSourceActive` (around line 1055), add after the `"email":` case:

```go
case "linkedin_profile":
    q = `UPDATE target_linkedin_profiles SET active = $2, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
```

In `SetSourceDeleted` (around line 1088), add after the `"email":` case:

```go
case "linkedin_profile":
    q = `UPDATE target_linkedin_profiles SET deleted_at = COALESCE(deleted_at, now()), updated_at = CURRENT_TIMESTAMP WHERE id = $1`
```

In `PatchSourceMeta` (around line 1119), add after the `"email":` case:

```go
case "linkedin_profile":
    tag, err = d.conn.Exec(ctx,
        `UPDATE target_linkedin_profiles SET display_name=COALESCE($2,display_name), tags=COALESCE($3,tags), updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
        id, displayName, tags)
```

- [ ] **Step 5: Add `pgxDatabase.CreateLinkedInProfile` implementation in `main.go`**

Add after `CreateEmailSource` (around line 1051):

```go
func (d *pgxDatabase) CreateLinkedInProfile(ctx context.Context, profileURL, displayName string) (int, error) {
	const q = `
		INSERT INTO target_linkedin_profiles (profile_url, display_name)
		VALUES ($1, $2)
		ON CONFLICT (profile_url) DO UPDATE
		SET display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), target_linkedin_profiles.display_name)
		RETURNING id`
	var id int
	return id, d.conn.QueryRow(ctx, q, profileURL, nullStr(displayName)).Scan(&id)
}
```

- [ ] **Step 6: Add `Core.AddLinkedInProfile` to `surface.go`**

Add after `AddEmailSource` (around line 481):

```go
// AddLinkedInProfile upserts a LinkedIn profile URL into target_linkedin_profiles.
// profileURL must be a canonical LinkedIn profile or company URL (https://www.linkedin.com/…
// or https://linkedin.com/…). Idempotent on profile_url.
func (c *Core) AddLinkedInProfile(ctx context.Context, profileURL, displayName string) (int, error) {
	profileURL = strings.TrimSpace(profileURL)
	if profileURL == "" {
		return 0, badInput("profile_url cannot be empty")
	}
	if !strings.HasPrefix(profileURL, "https://www.linkedin.com/") &&
		!strings.HasPrefix(profileURL, "https://linkedin.com/") {
		return 0, badInput("profile_url must be a LinkedIn URL (https://www.linkedin.com/… or https://linkedin.com/…)")
	}
	return c.db.CreateLinkedInProfile(ctx, profileURL, displayName)
}
```

- [ ] **Step 7: Add `"linkedin_profile"` to `validSourceKinds` in `surface.go`**

Find `validSourceKinds` map (around line 503). Add:

```go
var validSourceKinds = map[string]bool{
    "youtube_channel": true, "youtube_playlist": true, "podcast": true,
    "rss": true, "html": true, "hn": true, "email": true,
    "linkedin_profile": true,
}
```

- [ ] **Step 8: Add `linkedin_profile` to `sourceKindsRegistry` in `surface.go`**

Find `sourceKindsRegistry` slice (around line 629). Append after the `email` entry:

```go
sourceKind("linkedin_profile", "LinkedIn Profile", "linkedin", "linkedin", "rara-clip",
    SourceField{Name: "profile_url", Label: "Profile URL", Type: "url", Required: true,
        Placeholder: "https://www.linkedin.com/in/handle"},
),
```

- [ ] **Step 9: Add `linkedin_profile` case to `addSource` in `surface.go`**

Find the `addSource` switch (around line 1151). Add before `default:`:

```go
case "linkedin_profile":
    var req struct {
        ProfileURL  string `json:"profile_url"`
        DisplayName string `json:"display_name"`
    }
    if !decodeJSON(w, r, &req) {
        return
    }
    id, err := h.core.AddLinkedInProfile(r.Context(), req.ProfileURL, req.DisplayName)
    if err != nil {
        writeErr(w, err)
        return
    }
    writeJSON(w, http.StatusOK, map[string]int{"id": id})
```

- [ ] **Step 10: Run tests — confirm they compile and the new ones pass**

```bash
cd rara-core
go test -run TestCoreAddLinkedInProfile -v ./...
go test -run TestHTTPAddSourceLinkedInProfile -v ./...
```

Expected: still fails — `MockDatabase` doesn't implement `CreateLinkedInProfile` yet.

---

### Task 3: MockDatabase — wire `linkedin_profile` in the test harness

**Files:**
- Modify: `rara-core/main_test.go`

**Interfaces:**
- Consumes: `mockLinkedInProfile` struct (defined here)
- Produces: `MockDatabase` satisfies `Database` interface for `linkedin_profile`

- [ ] **Step 1: Add `mockLinkedInProfile` struct to `main_test.go`**

Find where `mockEmailSource` is declared (around line 159 area). Add after it:

```go
type mockLinkedInProfile struct {
	ID          int
	ProfileURL  string
	DisplayName string
	Tags        []string
	Active      bool
}
```

- [ ] **Step 2: Add `linkedinProfiles` map + counter to `MockDatabase` struct**

Find the struct fields block (around line 140). Add after `emailSources`:

```go
linkedinProfiles      map[int]mockLinkedInProfile
nextLinkedInProfileID int
```

- [ ] **Step 3: Initialize in `newMockDatabase()`**

Find `newMockDatabase()` (around line 178). Add after `emailSources: make(map[int]mockEmailSource)`:

```go
linkedinProfiles:      make(map[int]mockLinkedInProfile),
nextLinkedInProfileID: 1,
```

- [ ] **Step 4: Implement `MockDatabase.CreateLinkedInProfile`**

Add after `MockDatabase.CreateEmailSource`:

```go
func (m *MockDatabase) CreateLinkedInProfile(_ context.Context, profileURL, displayName string) (int, error) {
	// Idempotent on profile_url.
	for id, p := range m.linkedinProfiles {
		if p.ProfileURL == profileURL {
			if displayName != "" {
				p.DisplayName = displayName
				m.linkedinProfiles[id] = p
				m.setSourcesVDisplayName(fmt.Sprintf("linkedin_profile:%d", id), displayName)
			}
			return id, nil
		}
	}
	id := m.nextLinkedInProfileID
	m.nextLinkedInProfileID++
	apiID := fmt.Sprintf("linkedin_profile:%d", id)
	m.linkedinProfiles[id] = mockLinkedInProfile{
		ID: id, ProfileURL: profileURL, DisplayName: displayName, Tags: []string{}, Active: true,
	}
	m.sources = append(m.sources, SourceItem{
		ApiID: apiID, Kind: "linkedin_profile", Lane: "linkedin",
		DisplayName: profileURL, Status: "active", Tags: []string{},
	})
	return id, nil
}
```

- [ ] **Step 5: Add `linkedin_profile` to `SetSourceActive` mock**

Find `MockDatabase.SetSourceActive` switch (around line 432). Add after `"email":` case:

```go
case "linkedin_profile":
    p, exists := m.linkedinProfiles[id]
    if !exists {
        return fmt.Errorf("source %q: %w", apiID, errNotFound)
    }
    p.Active = active
    m.linkedinProfiles[id] = p
```

(The `m.setSourcesVStatus(apiID, active)` call at the bottom of the function already handles the view update.)

- [ ] **Step 6: Add `linkedin_profile` to `SetSourceDeleted` mock**

Find `MockDatabase.SetSourceDeleted` switch (around line 484). Add after `"email":` case:

```go
case "linkedin_profile":
    _, exists = m.linkedinProfiles[id]
```

- [ ] **Step 7: Add `linkedin_profile` to `PatchSourceMeta` mock**

Find `MockDatabase.PatchSourceMeta` switch (around line 516). Add after `"email":` case:

```go
case "linkedin_profile":
    p, exists := m.linkedinProfiles[id]
    if !exists {
        return fmt.Errorf("source %q: %w", apiID, errNotFound)
    }
    if displayName != nil {
        p.DisplayName = *displayName
    }
    if tags != nil {
        p.Tags = tags
    }
    m.linkedinProfiles[id] = p
```

(The `setSourcesVDisplayName` / `setSourcesVTags` helpers are called at the bottom of the function already — verify that the existing code calls them generically. If it only calls for specific kinds, also add the calls here.)

- [ ] **Step 8: Run the full test suite**

```bash
cd rara-core
make test
```

Expected: PASS — all existing tests plus the new linkedin_sources_test.go pass.

- [ ] **Step 9: Run race detector**

```bash
make test-race
```

Expected: PASS — no data races.

- [ ] **Step 10: Commit**

```bash
cd rara-core
git add surface.go main.go main_test.go linkedin_sources_test.go migrations/026_linkedin_profile_source.sql
git commit -m "feat(core): linkedin_profile source kind — registry, Core method, DB dispatch, tests"
```

---

### Task 4: Console — add LinkedIn glyph to ICONS map

**Files:**
- Modify: `rara-console/web/src/routes/fontes/+page.svelte` (line ~53)

**Interfaces:**
- Consumes: `ICONS` map key `"linkedin"` (the `icon` field value from `sourceKindsRegistry`)
- Produces: LinkedIn sources display `'in'` glyph in the Fontes table

- [ ] **Step 1: Add `linkedin` to ICONS**

Find the `ICONS` map (around line 53 of `+page.svelte`):

```ts
const ICONS: Record<string, string> = {
    youtube: '▶',
    podcast: '🎙',
    rss: '📡',
    globe: '🌐',
    hackernews: 'Y',
    mail: '✉'
};
```

Add `linkedin: 'in'`:

```ts
const ICONS: Record<string, string> = {
    youtube: '▶',
    podcast: '🎙',
    rss: '📡',
    globe: '🌐',
    hackernews: 'Y',
    mail: '✉',
    linkedin: 'in'
};
```

- [ ] **Step 2: Build the console to verify no TypeScript errors**

```bash
cd rara-console/web
npm run build
```

Expected: build succeeds with no errors.

- [ ] **Step 3: Commit**

```bash
cd rara-console
git add web/src/routes/fontes/+page.svelte
git commit -m "feat(console): add linkedin icon glyph to Fontes source kind map"
```

---

## Self-Review

**Spec coverage:**
- [x] LinkedIn profile URL visible in Fontes list (sources_v UNION ALL branch — Task 1)
- [x] New wizard entry in "Escolha o tipo da fonte" picker (sourceKindsRegistry entry — Task 2)
- [x] Wizard step 2 shows `profile_url` field (SourceField in registry — Task 2)
- [x] Create via POST /v1/sources/linkedin_profile (addSource switch + Core method — Task 2)
- [x] Pause/resume (SetSourceActive — Task 2 + validSourceKinds — Task 2)
- [x] Delete (SetSourceDeleted — Task 2 + validSourceKinds — Task 2)
- [x] Tag/edit display_name (PatchSourceMeta — Task 2)
- [x] Icon glyph in table (ICONS map — Task 4)
- [x] URL validation rejects non-LinkedIn and empty (AddLinkedInProfile — Task 2)
- [x] Idempotency on profile_url (CreateLinkedInProfile ON CONFLICT — Tasks 2+3)
- [x] TDD: failing test before implementation (Tasks 2+3)

**Placeholder scan:** None — all steps have concrete code.

**Type consistency:**
- `AddLinkedInProfile` is called in `addSource` with `req.ProfileURL, req.DisplayName` — matches the Core method signature `(ctx, profileURL, displayName string)`.
- `CreateLinkedInProfile` on the interface and the pgxDatabase implementation both take `(ctx, profileURL, displayName string) (int, error)` — consistent.
- Mock returns `id` starting at 1; `nextLinkedInProfileID` initialized to 1 — matches other counters.

**Edge case note:** The `RETURNING id` in `CreateLinkedInProfile`'s SQL won't fire on a `DO UPDATE` for a conflict on profile_url unless we also return on conflict. The correct idiom is:

```sql
ON CONFLICT (profile_url) DO UPDATE
SET display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), target_linkedin_profiles.display_name)
RETURNING id
```

PostgreSQL's `INSERT … ON CONFLICT DO UPDATE … RETURNING` always returns the final row's id (whether inserted or updated), so this is correct.
