# Fontes — Edit All Config Fields (not just name + tags)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator edit every config field of a source (URL/handle/feed/name/title) from the Edit modal — today only `display_name` and `tags` are editable; changing a URL requires delete-and-recreate.

**Architecture:** Three layers, reusing existing patterns.
1. **Read** — new `GET /v1/sources/{id}/config` returns the source's *raw* editable fields (keyed by the same registry field names the create wizard uses), so the Edit modal can pre-fill them. `sources_v` only exposes the unified read-model (display_name, config_summary), which is insufficient to pre-fill per-kind fields.
2. **Write** — extend `SourcePatch` with an optional `config` map. `PatchSource` validates the config with the *same* helpers used at create time (channel resolve, playlist parse, endpoint URL check, LinkedIn URL normalize), then runs a per-kind `UPDATE … WHERE id=$1`. A UNIQUE-violation surfaces as a clear "another source already uses this" error.
3. **Frontend** — the Edit modal fetches `/config` on open, renders the kind's fields (reusing the wizard's field loop) pre-filled, and PATCHes the changed values.

**Tech Stack:** Go 1.26 (rara-core control plane + rara-console BFF), SvelteKit (Svelte 5 runes), PostgreSQL (Neon).

## Global Constraints

- **Identity-edit semantics (operator decision, 2026-06-27): "Recomeçar".** Editing a URL/identity field re-points the row; already-collected content stays under the old identity and the source restarts collection on the new URL. **Do NOT build history migration.** Duplicate-URL edits are **rejected** with a clear error (the existing UNIQUE constraints already enforce this: `feed_url`, `profile_url`, `youtube_channel_id`, `youtube_playlist_id`, `(name,endpoint)`).
- **Email out of scope** — handled in a separate session. The `email` kind keeps its current name+tags-only edit (no config fields wired in this plan).
- **LinkedIn `display_name` unchanged** — no handle/slug derivation; operator fixes the name manually.
- **TDD mandatory** — every logic change: failing test first (fluent harness over `MockDatabase`), then minimal impl, then green. Zero real I/O in unit tests.
- **Validation reuse, not duplication** — the edit path must call the same validation as the create path. Factor shared validators rather than re-implementing them.
- **No new dependency.** Everything uses stdlib + the existing pgx/SvelteKit stack.
- Per-agent build: `cd rara-core && make test`; `cd rara-console && make test` (Go) and `cd rara-console/web && npm run check`/`npm test` (front).

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `rara-core/main.go` | `SourcePatch` struct; `Database` interface; `pgxDatabase` SQL | Modify |
| `rara-core/surface.go` | `PatchSource`, validation helpers, `GET /config` route + handler, `addSource` (refactor validators out) | Modify |
| `rara-core/store_reads.go` | `GetSourceConfig` read | Modify |
| `rara-core/main_test.go` (+ `sources_test.go`) | `MockDatabase` config store; harness; tests | Modify |
| `rara-console/main.go` | BFF proxy `GET /api/sources/{id}/config` | Modify |
| `rara-console/main_test.go` | BFF proxy test | Modify |
| `rara-console/web/src/routes/fontes/+page.svelte` | Edit modal: fetch config, render fields, submit | Modify |

No new files — every change extends an existing seam.

---

## Task 1: Backend Read — `GET /v1/sources/{id}/config`

Returns the raw editable fields of one source, keyed by registry field names so the front can pre-fill the same inputs the wizard renders.

**Files:**
- Modify: `rara-core/main.go` (Database interface + `pgxDatabase.GetSourceConfig`)
- Modify: `rara-core/store_reads.go` (or keep SQL in main.go beside the other source reads — match where `GetSource` lives)
- Modify: `rara-core/surface.go` (Core method + route + handler)
- Test: `rara-core/sources_test.go`

**Interfaces:**
- Produces: `GetSourceConfig(ctx, apiID string) (map[string]string, bool, error)` — `false` when the id doesn't exist. Keys per kind:
  - `youtube_channel`: `channel_id` (= stored `youtube_channel_id`), `channel_name`
  - `youtube_playlist`: `playlist_url` (= `https://www.youtube.com/playlist?list=<youtube_playlist_id>`)
  - `podcast`: `feed_url`, `title`
  - `rss` / `html`: `endpoint`, `name`
  - `hn`: `name`
  - `linkedin_profile`: `profile_url`
  - `email`: returns `{}` (out of scope; handler still 200s with empty map)

- [ ] **Step 1: Add the interface method to `Database` (main.go, near `GetSource` ~line 634)**

```go
	// GetSourceConfig returns one source's raw editable fields keyed by registry field name
	// (see sourceKindsRegistry). found=false if the id is absent. Used to pre-fill the Edit modal.
	GetSourceConfig(ctx context.Context, apiID string) (map[string]string, bool, error)
```

- [ ] **Step 2: Write the failing test (sources_test.go)**

```go
func TestGetSourceConfig_YouTubePlaylist(t *testing.T) {
	db := NewMockDatabase()
	// Seed a playlist row id=7 with a known youtube_playlist_id.
	db.SeedYouTubePlaylist(7, "PLabc123", "My List", "")
	core := NewCore(db)

	cfg, found, err := core.SourceConfig(context.Background(), "youtube_playlist:7")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := cfg["playlist_url"]; got != "https://www.youtube.com/playlist?list=PLabc123" {
		t.Fatalf("playlist_url = %q", got)
	}
}

func TestGetSourceConfig_NotFound(t *testing.T) {
	core := NewCore(NewMockDatabase())
	_, found, err := core.SourceConfig(context.Background(), "podcast:999")
	if err != nil || found {
		t.Fatalf("want found=false err=nil, got found=%v err=%v", found, err)
	}
}
```

(`SeedYouTubePlaylist` is added to MockDatabase in Step 4.)

- [ ] **Step 3: Run the test — expect FAIL (SourceConfig/GetSourceConfig undefined)**

Run: `cd rara-core && go test -run TestGetSourceConfig -v`
Expected: FAIL — `core.SourceConfig undefined` / `SeedYouTubePlaylist undefined`.

- [ ] **Step 4: Add the Core method + MockDatabase support**

In `surface.go` (next to `GetSource` wrappers ~line 600):

```go
// SourceConfig returns the raw editable fields of one source for the Edit modal.
func (c *Core) SourceConfig(ctx context.Context, apiID string) (map[string]string, bool, error) {
	if _, _, ok := parseSourceID(apiID); !ok {
		return nil, false, badInput("invalid source id %q (want kind:N)", apiID)
	}
	return c.db.GetSourceConfig(ctx, apiID)
}
```

In `main_test.go` (MockDatabase), back the playlist store and implement `GetSourceConfig` by reading the in-memory rows the mock already keeps for each kind. Minimal seed helper + read:

```go
func (m *MockDatabase) SeedYouTubePlaylist(id int, playlistID, title, displayName string) {
	m.playlists[id] = mockPlaylist{playlistID: playlistID, title: title, displayName: displayName}
}

func (m *MockDatabase) GetSourceConfig(ctx context.Context, apiID string) (map[string]string, bool, error) {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return nil, false, fmt.Errorf("bad api_id %q", apiID)
	}
	switch kind {
	case "youtube_playlist":
		p, ok := m.playlists[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{
			"playlist_url": "https://www.youtube.com/playlist?list=" + p.playlistID,
		}, true, nil
	// … other kinds mirror the same shape from their mock stores …
	}
	return map[string]string{}, false, nil
}
```

> If the MockDatabase doesn't yet keep per-kind rows, add the smallest store needed (a `map[int]mockPlaylist` etc.) mirroring the SQL the real impl reads. The mock must mirror the real contract.

- [ ] **Step 5: Run the test — expect PASS**

Run: `cd rara-core && go test -run TestGetSourceConfig -v`
Expected: PASS.

- [ ] **Step 6: Implement the real `pgxDatabase.GetSourceConfig` (main.go, beside the other source SQL)**

```go
// GetSourceConfig reads the raw editable fields for one source, keyed by registry field name.
func (d *pgxDatabase) GetSourceConfig(ctx context.Context, apiID string) (map[string]string, bool, error) {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return nil, false, fmt.Errorf("GetSourceConfig: invalid api_id %q", apiID)
	}
	cfg := map[string]string{}
	switch kind {
	case "youtube_channel":
		var chID, name string
		err := d.conn.QueryRow(ctx,
			`SELECT youtube_channel_id, COALESCE(channel_name,'') FROM target_channels WHERE id=$1 AND deleted_at IS NULL`, id,
		).Scan(&chID, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cfg["channel_id"], cfg["channel_name"] = chID, name
	case "youtube_playlist":
		var plID string
		err := d.conn.QueryRow(ctx,
			`SELECT youtube_playlist_id FROM playlists WHERE id=$1 AND deleted_at IS NULL`, id,
		).Scan(&plID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cfg["playlist_url"] = "https://www.youtube.com/playlist?list=" + plID
	case "podcast":
		var feedURL, title string
		err := d.conn.QueryRow(ctx,
			`SELECT feed_url, COALESCE(title,'') FROM podcast_feeds WHERE id=$1 AND deleted_at IS NULL`, id,
		).Scan(&feedURL, &title)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cfg["feed_url"], cfg["title"] = feedURL, title
	case "rss", "html", "hn":
		var name, endpoint string
		err := d.conn.QueryRow(ctx,
			`SELECT name, COALESCE(endpoint,'') FROM feed_sources WHERE id=$1 AND deleted_at IS NULL`, id,
		).Scan(&name, &endpoint)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cfg["name"] = name
		if kind != "hn" {
			cfg["endpoint"] = endpoint
		}
	case "linkedin_profile":
		var profileURL string
		err := d.conn.QueryRow(ctx,
			`SELECT profile_url FROM target_linkedin_profiles WHERE id=$1 AND deleted_at IS NULL`, id,
		).Scan(&profileURL)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cfg["profile_url"] = profileURL
	case "email":
		return map[string]string{}, true, nil // out of scope; modal shows name+tags only
	default:
		return nil, false, fmt.Errorf("GetSourceConfig: unknown kind %q", kind)
	}
	return cfg, true, nil
}
```

- [ ] **Step 7: Wire the route + handler (surface.go)**

Route (next to the other source routes ~line 962):

```go
	mux.HandleFunc("GET /v1/sources/{source_id}/config", h.getSourceConfig)
```

Handler:

```go
func (h *httpSurface) getSourceConfig(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	cfg, found, err := h.core.SourceConfig(r.Context(), apiID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "source not found"})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
```

- [ ] **Step 8: Run full core tests + lint**

Run: `cd rara-core && make lint && make test`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
cd rara-core
git add main.go surface.go store_reads.go sources_test.go main_test.go
git commit -m "feat(core): GET /v1/sources/{id}/config returns raw editable fields"
```

---

## Task 2: Backend Write — Edit config fields via PATCH

Extend `SourcePatch` with an optional `config` map, validate it with the create-time helpers, and apply a per-kind UPDATE. UNIQUE violation → clear conflict error.

**Files:**
- Modify: `rara-core/main.go` (`SourcePatch`, Database interface, `pgxDatabase.UpdateSourceConfig`)
- Modify: `rara-core/surface.go` (`PatchSource`, factor validators out of `AddFeedSource`/`AddLinkedInProfile`/`AddYouTubePlaylist`/`AddYouTubeChannel`)
- Test: `rara-core/sources_test.go`

**Interfaces:**
- Consumes: `parseSourceID`, `extractPlaylistID` (surface.go:413), `validateEndpointURL` (surface.go:434), `c.resolveChannel` (surface.go:385).
- Produces:
  - `SourcePatch.Config map[string]string` (JSON `config`)
  - `normalizeSourceConfig(ctx, kind string, cfg map[string]string) (map[string]string, error)` — returns normalized fields (resolved channel id, parsed playlist id, normalized LinkedIn url) or a `badInput` error.
  - `UpdateSourceConfig(ctx, apiID string, cfg map[string]string) error` — per-kind UPDATE; UNIQUE violation → `badInput`.

- [ ] **Step 1: Extend `SourcePatch` (main.go:389)**

```go
type SourcePatch struct {
	DisplayName *string           `json:"display_name"`
	Tags        []string          `json:"tags"`
	Config      map[string]string `json:"config"` // raw editable fields, keyed by registry field name; nil = unchanged
}
```

- [ ] **Step 2: Write the failing test — edit a podcast feed_url (sources_test.go)**

```go
func TestPatchSource_EditPodcastFeedURL(t *testing.T) {
	db := NewMockDatabase()
	db.SeedPodcast(3, "https://old.example/feed.rss", "Old", "")
	core := NewCore(db)

	err := core.PatchSource(context.Background(), "podcast:3", SourcePatch{
		Config: map[string]string{"feed_url": "https://new.example/feed.rss", "title": "New"},
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := db.podcasts[3]
	if got.feedURL != "https://new.example/feed.rss" || got.title != "New" {
		t.Fatalf("podcast not updated: %+v", got)
	}
}

func TestPatchSource_EditEndpointRejectsBadURL(t *testing.T) {
	db := NewMockDatabase()
	db.SeedFeedSource(5, "rss", "Blog", "https://ok.example/feed")
	core := NewCore(db)

	err := core.PatchSource(context.Background(), "rss:5", SourcePatch{
		Config: map[string]string{"name": "Blog", "endpoint": "ftp://nope"},
	})
	if err == nil {
		t.Fatal("want validation error for non-http endpoint")
	}
}

func TestPatchSource_DuplicateURLConflict(t *testing.T) {
	db := NewMockDatabase()
	db.SeedPodcast(1, "https://a.example/feed", "A", "")
	db.SeedPodcast(2, "https://b.example/feed", "B", "")
	core := NewCore(db)

	// Editing #2's feed_url to #1's existing URL must conflict.
	err := core.PatchSource(context.Background(), "podcast:2", SourcePatch{
		Config: map[string]string{"feed_url": "https://a.example/feed"},
	})
	if err == nil {
		t.Fatal("want conflict error on duplicate feed_url")
	}
}
```

- [ ] **Step 3: Run — expect FAIL**

Run: `cd rara-core && go test -run TestPatchSource_Edit -v && go test -run TestPatchSource_Duplicate -v`
Expected: FAIL (Config field ignored / Seed helpers undefined).

- [ ] **Step 4: Factor the validators out of the Add* methods (surface.go)**

Add `normalizeSourceConfig` and make `AddFeedSource`/`AddLinkedInProfile` call the shared validators (no behavior change to create):

```go
// normalizeSourceConfig validates a kind's editable fields the same way create does,
// returning the normalized fields (resolved channel id, parsed playlist id, normalized
// LinkedIn url). The returned map is what UpdateSourceConfig writes.
func (c *Core) normalizeSourceConfig(ctx context.Context, kind string, cfg map[string]string) (map[string]string, error) {
	out := map[string]string{}
	switch kind {
	case "youtube_channel":
		ref := strings.TrimSpace(cfg["channel_id"])
		if ref == "" {
			return nil, badInput("channel_id cannot be empty")
		}
		id := ref
		if c.resolveChannel != nil {
			resolved, err := c.resolveChannel(ctx, ref)
			if err != nil {
				return nil, err
			}
			id = resolved
		}
		out["youtube_channel_id"] = id
		name := strings.TrimSpace(cfg["channel_name"])
		if name == "" {
			name = ref
		}
		out["channel_name"] = name
	case "youtube_playlist":
		raw := strings.TrimSpace(cfg["playlist_url"])
		if raw == "" {
			return nil, badInput("playlist_url cannot be empty")
		}
		plID, err := extractPlaylistID(raw)
		if err != nil {
			return nil, badInput("%v", err)
		}
		out["youtube_playlist_id"] = plID
	case "podcast":
		feedURL := strings.TrimSpace(cfg["feed_url"])
		if feedURL == "" {
			return nil, badInput("feed_url cannot be empty")
		}
		if err := validateEndpointURL(feedURL); err != nil {
			return nil, err
		}
		out["feed_url"], out["title"] = feedURL, strings.TrimSpace(cfg["title"])
	case "rss", "html", "hn":
		name := strings.TrimSpace(cfg["name"])
		if name == "" {
			return nil, badInput("name cannot be empty")
		}
		out["name"] = name
		if kind != "hn" {
			endpoint := strings.TrimSpace(cfg["endpoint"])
			if endpoint == "" {
				return nil, badInput("endpoint cannot be empty for kind %q", kind)
			}
			if err := validateEndpointURL(endpoint); err != nil {
				return nil, err
			}
			out["endpoint"] = endpoint
		}
	case "linkedin_profile":
		u, err := normalizeLinkedInURL(cfg["profile_url"])
		if err != nil {
			return nil, err
		}
		out["profile_url"] = u
	case "email":
		return nil, badInput("editing email config is out of scope")
	default:
		return nil, badInput("unknown kind %q", kind)
	}
	return out, nil
}
```

Extract the LinkedIn normalization currently inline in `AddLinkedInProfile` into `normalizeLinkedInURL(raw string) (string, error)` and have `AddLinkedInProfile` call it (keeps create behavior identical, DRY).

- [ ] **Step 5: Update `PatchSource` to apply config (surface.go:506)**

```go
func (c *Core) PatchSource(ctx context.Context, apiID string, patch SourcePatch) error {
	kind, _, ok := parseSourceID(apiID)
	if !ok {
		return badInput("invalid source id %q (want kind:N)", apiID)
	}
	if len(patch.Config) > 0 {
		norm, err := c.normalizeSourceConfig(ctx, kind, patch.Config)
		if err != nil {
			return err
		}
		// Atomically apply config + meta in one call to avoid partial updates.
		return c.db.PatchSourceFull(ctx, apiID, norm, patch.DisplayName, patch.Tags)
	}
	// display_name / tags only — no config change (existing behavior; no-op when both nil).
	if patch.DisplayName != nil || patch.Tags != nil {
		return c.db.PatchSourceMeta(ctx, apiID, patch.DisplayName, patch.Tags)
	}
	return nil
}
```

- [ ] **Step 6: Add `PatchSourceFull` to the interface + pgx impl (main.go)**

`PatchSourceFull` runs config + meta writes in a single transaction so the source is never
partially updated. `DisplayName`/`Tags` are nil-safe (same semantics as `PatchSourceMeta`).

Interface (near `PatchSourceMeta` ~line 627):

```go
	// PatchSourceFull atomically writes a source's normalized config fields and optional
	// display_name/tags in one transaction. A UNIQUE violation is returned as a badInput.
	PatchSourceFull(ctx context.Context, apiID string, cfg map[string]string, displayName *string, tags []string) error
```

Impl (beside `PatchSourceMeta` ~line 1136):

```go
func (d *pgxDatabase) PatchSourceFull(ctx context.Context, apiID string, cfg map[string]string, displayName *string, tags []string) error {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return fmt.Errorf("PatchSourceFull: invalid api_id %q", apiID)
	}
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var (
		tag pgconn.CommandTag
	)
	switch kind {
	case "youtube_channel":
		tag, err = tx.Exec(ctx,
			`UPDATE target_channels SET youtube_channel_id=$2, channel_name=$3, updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["youtube_channel_id"], cfg["channel_name"])
	case "youtube_playlist":
		tag, err = tx.Exec(ctx,
			`UPDATE playlists SET youtube_playlist_id=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["youtube_playlist_id"])
	case "podcast":
		tag, err = tx.Exec(ctx,
			`UPDATE podcast_feeds SET feed_url=$2, title=NULLIF($3,''), updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["feed_url"], cfg["title"])
	case "rss", "html":
		tag, err = tx.Exec(ctx,
			`UPDATE feed_sources SET name=$2, endpoint=$3, updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["name"], cfg["endpoint"])
	case "hn":
		tag, err = tx.Exec(ctx,
			`UPDATE feed_sources SET name=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["name"])
	case "linkedin_profile":
		tag, err = tx.Exec(ctx,
			`UPDATE target_linkedin_profiles SET profile_url=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$1`,
			id, cfg["profile_url"])
	default:
		return fmt.Errorf("PatchSourceFull: unknown kind %q", kind)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return badInput("another source already uses this URL/handle")
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return badInput("source %q not found", apiID)
	}
	if displayName != nil || tags != nil {
		if err := patchSourceMetaTx(ctx, tx, apiID, displayName, tags); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
```

> `patchSourceMetaTx` is the transaction-aware variant of `PatchSourceMeta` — extract the SQL body from `PatchSourceMeta` into it and have both call it. `pgconn`/`errors` are already imported.

- [ ] **Step 7: Implement Seed helpers + `PatchSourceFull` in MockDatabase (main_test.go)**

Mirror the SQL: store per-kind rows, enforce the same UNIQUE keys the real tables have (so `TestPatchSource_DuplicateURLConflict` exercises the conflict path against the mock). Example for podcast:

```go
func (m *MockDatabase) SeedPodcast(id int, feedURL, title, displayName string) {
	m.podcasts[id] = mockPodcast{feedURL: feedURL, title: title, displayName: displayName}
}

func (m *MockDatabase) PatchSourceFull(ctx context.Context, apiID string, cfg map[string]string, displayName *string, tags []string) error {
	kind, id, _ := parseSourceID(apiID)
	switch kind {
	case "podcast":
		for other, p := range m.podcasts {
			if other != id && p.feedURL == cfg["feed_url"] {
				return badInput("another source already uses this URL/handle")
			}
		}
		p := m.podcasts[id]
		p.feedURL, p.title = cfg["feed_url"], cfg["title"]
		m.podcasts[id] = p
	// … rss/html/hn, youtube_channel, youtube_playlist, linkedin_profile mirror their UNIQUE keys …
	}
	// apply display_name / tags (same logic as MockDatabase.PatchSourceMeta)
	if displayName != nil || tags != nil {
		// … mirror PatchSourceMeta mock logic here …
	}
	return nil
}
```

- [ ] **Step 8: Run the Task-2 tests — expect PASS**

Run: `cd rara-core && go test -run TestPatchSource -v`
Expected: PASS (edit applies, bad URL rejected, duplicate conflicts).

- [ ] **Step 9: Run full core suite + lint**

Run: `cd rara-core && make lint && make test`
Expected: PASS (create-path tests still green — validators were factored, behavior unchanged).

- [ ] **Step 10: Commit**

```bash
cd rara-core
git add main.go surface.go sources_test.go main_test.go
git commit -m "feat(core): edit source config fields via PATCH (validated, conflict-safe)"
```

---

## Task 3: BFF Proxy — `GET /api/sources/{id}/config`

The console BFF must forward the new read to the core (the PATCH path already forwards `config` as-is — `handlePatchSource` streams the body unchanged, so no BFF change is needed for writes).

**Files:**
- Modify: `rara-console/main.go` (route + handler)
- Test: `rara-console/main_test.go`

**Interfaces:**
- Consumes: `isSourceID` (main.go:333), `fetchCore` (main.go:304), the route mux (~line 837).

- [ ] **Step 1: Write the failing test (main_test.go)**

```go
func TestHandleSourceConfig_Proxies(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sources/podcast:3/config" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"feed_url":"https://x/feed","title":"X"}`))
	}))
	defer core.Close()
	srv := newTestServer(t, core.URL) // existing helper used by sibling tests

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/sources/podcast:3/config", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "feed_url") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
```

> Match the construction helper the existing `rara-console` source-proxy tests use (e.g. `newTestServer`/`handler()`); copy their setup verbatim rather than inventing one.

- [ ] **Step 2: Run — expect FAIL (404, no route)**

Run: `cd rara-console && go test -run TestHandleSourceConfig -v`
Expected: FAIL with 404.

- [ ] **Step 3: Add route + handler (main.go, next to line 837)**

Route:

```go
	mux.HandleFunc("GET /api/sources/{source_id}/config", s.handleSourceConfig)
```

Handler (beside `handlePatchSource` ~line 349):

```go
// handleSourceConfig proxies GET /v1/sources/{source_id}/config — the source's raw editable
// fields, used to pre-fill the Edit modal.
func (s *server) handleSourceConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("source_id")
	if !isSourceID(id) {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/sources/"+id+"/config")
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
```

> If `fetchCore` doesn't propagate upstream non-200 status (e.g. 404), match how `handleSourceKinds` handles it; reuse the same error mapping the sibling handlers use.

- [ ] **Step 4: Run — expect PASS**

Run: `cd rara-console && go test -run TestHandleSourceConfig -v`
Expected: PASS.

- [ ] **Step 5: Full BFF tests + lint, commit**

```bash
cd rara-console && make lint && make test
git add main.go main_test.go
git commit -m "feat(console): proxy GET /api/sources/{id}/config"
```

---

## Task 4: Frontend — Edit modal edits all config fields

Fetch the source's config on open, render the kind's fields (reuse the wizard's field loop) pre-filled, and PATCH `{display_name, tags, config}`.

**Files:**
- Modify: `rara-console/web/src/routes/fontes/+page.svelte`

**Interfaces:**
- Consumes: `kindMap`/`wizardKind` field model (`SourceKind.fields`), `apiPath`, `errMsg`, `reloadAll`, `supportsTags`, the i18n `t.fontes.*` strings.
- Produces: editable `editConfig` state submitted as `config` in the PATCH body.

- [ ] **Step 1: Add edit-config state + a field accessor (script section, near line 117)**

```ts
	let editConfig = $state<Record<string, string>>({});
	let editConfigLoading = $state(false);
	// Fields to render in the Edit modal for the current source's kind (same registry the wizard uses).
	const editKind = $derived(editSource ? kindMap.get(editSource.kind) : undefined);
```

- [ ] **Step 2: Fetch config when opening the modal (replace `openEdit`, line 487)**

```ts
	async function openEdit(s: SourceItem) {
		editSource = s;
		editDisplayName = s.display_name;
		editTags = [...s.tags];
		editTagInput = '';
		editError = '';
		editConfig = {};
		// Pre-fill the per-kind config fields (URL/handle/name/title) from the backend.
		editConfigLoading = true;
		try {
			const res = await fetch(apiPath(s.api_id, '/config'));
			if (res.ok) editConfig = await res.json();
		} catch {
			/* leave fields blank; operator can still edit name/tags */
		} finally {
			editConfigLoading = false;
		}
	}
```

> Verify `apiPath(id, suffix)` produces `/api/sources/<id>/config` — `togglePause` already calls `apiPath(s.api_id, '/pause')` (line 562), so the two-arg form exists.

- [ ] **Step 3: Render the config fields in the Edit modal (insert before the display_name label, ~line 982)**

```svelte
				{#if editConfigLoading}
					<p class="text-[12px] text-muted">{t.fontes.editLoading}</p>
				{:else if editKind}
					{#each editKind.fields ?? [] as f (f.name)}
						{#if f.name !== 'display_name'}
							<label class="flex flex-col gap-1 text-[13px]">
								<span class="text-muted">{f.label}{f.required ? ' *' : ''}</span>
								<input
									type={f.type === 'url' ? 'url' : 'text'}
									bind:value={editConfig[f.name]}
									placeholder={f.placeholder ?? ''}
									class="rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
								/>
							</label>
						{/if}
					{/each}
				{/if}
```

(The registry appends `display_name` as a field; we skip it here because the modal already has a dedicated display-name input below.)

- [ ] **Step 4: Send config in `submitEdit` (replace the payload build, lines 507-516)**

```ts
		const payload: { display_name: string; tags?: string[]; config?: Record<string, string> } = {
			display_name: editDisplayName.trim()
		};
		if (supportsTags(editSource.kind)) {
			addEditTag();
			payload.tags = editTags;
		}
		// Collect required-field validation + non-empty config (skip email, which has no editable fields here).
		const cfgFields = (editKind?.fields ?? []).filter((f) => f.name !== 'display_name');
		if (cfgFields.length > 0) {
			const cfg: Record<string, string> = {};
			for (const f of cfgFields) {
				const v = (editConfig[f.name] ?? '').trim();
				if (f.required && !v) {
					editError = `${f.label}: ${t.fontes.wizardRequired}`;
					return;
				}
				cfg[f.name] = v; // always include key so backend can clear optional fields
			}
			payload.config = cfg;
		}
```

(The `fetch(apiPath(editSource.api_id), { method: 'PATCH', … body: JSON.stringify(payload) })` call below stays unchanged.)

- [ ] **Step 5: Remove the "fields not editable" note + add i18n strings**

Delete line 1015 (`<p …>{t.fontes.editFieldsNote}</p>`). Add `editLoading` to the i18n table that holds `t.fontes.*` (search the repo for `editFieldsNote` to find the strings file); add e.g. `editLoading: 'Loading fields…'` (and the pt-BR variant if the file is bilingual). Remove the now-unused `editFieldsNote` key.

- [ ] **Step 6: Typecheck + build the web app**

Run: `cd rara-console/web && npm run check && npm run build`
Expected: no type errors; build succeeds.

- [ ] **Step 7: Manual verification (the only end-to-end gate)**

Run the console against a Neon branch, open Fontes, click **Edit** on:
- a YouTube Channel → channel id + name pre-filled, editable; save re-resolves.
- a Podcast → feed_url + title pre-filled; change feed_url, save, confirm the list subtitle updates.
- an RSS → endpoint + name editable.
- a LinkedIn → profile_url editable.
- Attempt to set a podcast feed_url to one that already exists → expect the conflict error toast/message.

- [ ] **Step 8: Commit**

```bash
cd rara-console
git add web/src/routes/fontes/+page.svelte web/src/<i18n-strings-file>
git commit -m "feat(console): edit all source config fields in the Fontes edit modal"
```

---

## Self-Review Notes

- **Spec coverage:** read (Task 1) + write (Task 2) + BFF (Task 3) + UI (Task 4) cover every kind except `email` (explicitly out of scope) and `linkedin_profile` display_name derivation (operator does manually). ✓
- **Identity-edit = "Recomeçar":** enforced by *not* migrating history and by surfacing UNIQUE conflicts (Task 2, Step 6). ✓
- **Validation reuse:** `normalizeSourceConfig` calls the same `extractPlaylistID`/`validateEndpointURL`/`resolveChannel`/`normalizeLinkedInURL` helpers create uses; `AddLinkedInProfile` refactored to call the extracted `normalizeLinkedInURL` (no create-behavior change). ✓
- **Type consistency:** `SourceConfig`/`GetSourceConfig` keys (`channel_id`, `playlist_url`, `feed_url`, `title`, `endpoint`, `name`, `profile_url`) match the registry field names the wizard renders, so the same map round-trips read→edit→write. The DB UPDATE consumes the *normalized* keys (`youtube_channel_id`, `youtube_playlist_id`) that `normalizeSourceConfig` produces — not the raw input keys. ✓
- **Open verification:** the exact i18n strings file path and the MockDatabase per-kind store shapes are repo details the implementer confirms by grep at Step time; every other step has concrete code.
```
