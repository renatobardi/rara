# Fontes — Show URL in config_summary for YouTube Sources

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the fontes UI show the YouTube channel/playlist URL below the source name — the same way podcast, RSS, HTML, and HN already do.

**Architecture:** Single new migration (`027_sources_v_youtube_url.sql`) that rebuilds `sources_v` via `CREATE OR REPLACE VIEW`, changing the `config_summary` for `youtube_channel` and `youtube_playlist` rows from a name/title repeat to a constructed YouTube URL. No Go code changes and no frontend changes needed — the Svelte table already renders `config_summary` as a subtitle whenever it is non-empty.

**Tech Stack:** PostgreSQL (Neon), Go migrations applied by `database-core.yml` CI.

## Global Constraints

- Migration file numbering: next available is `027` (026 = `026_linkedin_profile_source.sql`).
- All migrations are idempotent: `DROP VIEW IF EXISTS sources_v; CREATE VIEW sources_v AS …` satisfies this.
- The view must be a **full re-declaration** of `sources_v` — PostgreSQL does not support partial view replacement. Copy the complete body from `026_linkedin_profile_source.sql` and change only the two `config_summary` lines. Use `DROP VIEW IF EXISTS` + `CREATE VIEW` (not `CREATE OR REPLACE VIEW`) because `config_summary` changes type from `varchar` to `text`; PostgreSQL forbids column type changes via `CREATE OR REPLACE VIEW`.
- No changes to `sources_v_deleted` — that view was retired in 025/026 (there is no live `sources_v_deleted`; 025 rebuilt `sources_v` itself; 026 did the same).
- TDD applies to rara-core Go code, not to SQL views. SQL view correctness is verified by running the migration against a Neon branch and querying the view.

---

## Root Cause

`sources_v` (`026_linkedin_profile_source.sql`, lines 35 and 51):

```sql
-- YouTube Channel
channel_name AS config_summary          -- ← same as display_name, no URL

-- YouTube Playlist
title        AS config_summary          -- ← same as display_name, no URL
```

The other source types already expose a URL:

| Kind | config_summary source | URL visible? |
|---|---|---|
| `youtube_channel` | `channel_name` | ❌ bug |
| `youtube_playlist` | `title` | ❌ bug |
| `podcast` | `feed_url` | ✅ |
| `rss` / `html` / `hn` | `endpoint` | ✅ |
| `linkedin_profile` | `profile_url` | ✅ |
| `email` | label / query | ✅ (different, out of scope) |

---

## Files

- **Create:** `rara-core/migrations/027_sources_v_youtube_url.sql`
- No other files change.

---

## Task 1: New Migration — Rebuild sources_v with YouTube URLs

**Files:**
- Create: `rara-core/migrations/027_sources_v_youtube_url.sql`

**Interfaces:**
- Consumes: `target_channels.youtube_channel_id` (already exists, format `UCxxxx…`)
- Consumes: `playlists.youtube_playlist_id` (already exists, format `PLxxxx…`)
- Produces: `config_summary` for `youtube_channel` = `https://www.youtube.com/channel/{youtube_channel_id}`
- Produces: `config_summary` for `youtube_playlist` = `https://www.youtube.com/playlist?list={youtube_playlist_id}`

- [ ] **Step 1: Create the migration file**

```sql
-- rara-core/migrations/027_sources_v_youtube_url.sql
-- Fix config_summary for youtube_channel and youtube_playlist rows in sources_v.
-- Previously both used the human-readable name/title (identical to display_name, no URL).
-- Now they expose a constructed YouTube URL so the Fontes UI subtitle is actionable.
-- Full view re-declaration required by PostgreSQL.
-- DROP + CREATE (not CREATE OR REPLACE): config_summary changes from varchar to text; PostgreSQL
-- forbids column type changes via CREATE OR REPLACE VIEW.
-- Idempotent — safe to re-apply.

DROP VIEW IF EXISTS sources_v;
CREATE VIEW sources_v AS

-- YouTube channels — rara-harvest
SELECT
    format('youtube_channel:%s', id)                                      AS api_id,
    'youtube_channel'                                                      AS kind,
    'youtube'                                                              AS lane,
    COALESCE(display_name, channel_name)                                   AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END                       AS status,
    format('https://www.youtube.com/channel/%s', youtube_channel_id)       AS config_summary,
    created_at,
    updated_at
FROM target_channels
WHERE deleted_at IS NULL

UNION ALL

-- YouTube playlists — rara-shelf
SELECT
    format('youtube_playlist:%s', id)                                      AS api_id,
    'youtube_playlist'                                                     AS kind,
    'youtube'                                                              AS lane,
    COALESCE(display_name, title)                                          AS display_name,
    tags,
    CASE WHEN active THEN 'active' ELSE 'paused' END                       AS status,
    format('https://www.youtube.com/playlist?list=%s', youtube_playlist_id) AS config_summary,
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

COMMENT ON VIEW sources_v IS 'Unified read-only view of all collectable sources; deleted_at IS NULL; config_summary shows the actionable URL for every kind';
```

- [ ] **Step 2: Verify the diff is exactly two `config_summary` lines changed**

```bash
diff \
  <(grep -A3 "config_summary" rara-core/migrations/026_linkedin_profile_source.sql) \
  <(grep -A3 "config_summary" rara-core/migrations/027_sources_v_youtube_url.sql)
```

Expected diff: only the YouTube Channel and YouTube Playlist `config_summary` expressions differ.

- [ ] **Step 3: Lint/build rara-core to ensure no Go code references break**

```bash
cd rara-core && make lint && make test
```

Expected: all tests pass (no Go code touches `config_summary` content directly — it is just read via `scanSourceItem`).

- [ ] **Step 4: Apply migration to a Neon branch and verify**

```bash
# Create an ephemeral Neon branch, apply the migration, run a spot-check query.
# (Use the Neon MCP or the neon CLI — whichever is available in the session.)
```

Spot-check query to run on the branch:

```sql
-- Should return youtube.com/channel/UC... URLs, not plain channel names
SELECT api_id, display_name, config_summary
FROM sources_v
WHERE kind = 'youtube_channel'
LIMIT 5;

-- Should return youtube.com/playlist?list=PL... URLs, not plain titles
SELECT api_id, display_name, config_summary
FROM sources_v
WHERE kind = 'youtube_playlist'
LIMIT 5;

-- Sanity-check others are unchanged
SELECT kind, display_name, config_summary
FROM sources_v
WHERE kind IN ('podcast', 'rss', 'html', 'hn', 'linkedin_profile')
LIMIT 10;
```

- [ ] **Step 5: Commit**

```bash
git add rara-core/migrations/027_sources_v_youtube_url.sql
git commit -m "fix(core): show YouTube channel/playlist URL in sources_v config_summary"
```

---

## Notes

- **Podcast/RSS/HTML/HN/LinkedIn already correct.** Those types map `config_summary` to `feed_url`, `endpoint`, or `profile_url` — all actionable URLs. No change needed.
- **LinkedIn left as-is (operator decision).** `linkedin_profile` is a single kind labeled "LinkedIn Profile / Company" (`surface.go:681`) over one table (`target_linkedin_profiles`); it stores both `/in/` and `/company/` URLs. Today `display_name = COALESCE(display_name, profile_url)` and `config_summary = profile_url`, so when no `display_name` is set the URL appears in both columns. The operator will set the human display name manually per row — **do NOT derive a handle/slug from the URL** and do NOT change the LinkedIn branch of the view.
- **No frontend change.** `+page.svelte` lines 800-802 already render `config_summary` as a subtitle `<span>` with a `title` tooltip on hover. The URL is visible on hover but is **not** a clickable `<a>` link. Turning it into a real link is a separate UI task.
- **Email out of scope** per the user's explicit instruction.
