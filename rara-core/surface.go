// surface.go — Phase 5 deliverables #1, #2, #4: the control surface (HTTP core + auth).
//
// rara-core is always-on in the VPC (the reconciler loop). Phase 5 mounts an HTTP control
// surface ALONGSIDE that ticker in the same process, so a person or an agent can OBSERVE the
// running system (items by status, an item's steps, the quarantine, an item's gate decisions)
// and EDIT its config as data (flows/flow_steps, providers, routing_policies, gate_rules,
// interest_profile) — plus drive the two human-in-the-loop signals (thumbs on a distillation,
// quarantine review) by reusing the Phase 3 functions verbatim.
//
// The surface is two thin front-ends over ONE núcleo:
//
//	Core  — the operations layer (this file): every read/edit/action, validated once, over the
//	        Database seam (+ the LinkedIn store). It holds NO transport concern.
//	HTTP  — a REST adapter (this file): parse request -> Core -> JSON.
//	MCP   — a JSON-RPC adapter (mcp.go): tool call -> Core -> result. Same Core, same ops.
//
// Both adapters are pure over the seam, so the whole surface is unit-tested with the
// MockDatabase + httptest — zero real I/O. Auth is a single service token (it is personal, but
// not left open): a bearer-token middleware that fails CLOSED (an unset token refuses to serve).
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// lastReconcileNano is the Unix nanosecond timestamp of the most recent reconcile pass.
// Zero means never. Stamped by StampReconcile (called from the --loop runner in main.go)
// and read by the health handler — both in the same process, so a package-level atomic
// is the right seam (no DB write needed).
var lastReconcileNano int64

// StampReconcile records the current wall-clock time as the last reconcile pass.
func StampReconcile() { atomic.StoreInt64(&lastReconcileNano, time.Now().UnixNano()) }

// ---------------------------------------------------------------------------
// Health + usage report types
// ---------------------------------------------------------------------------

// HealthReport is the response shape for GET /v1/health.
type HealthReport struct {
	DBOk            bool           `json:"db_ok"`
	LastReconcileAt *time.Time     `json:"last_reconcile_at,omitempty"`
	Providers       ProviderHealth `json:"providers"`
}

// ProviderHealth aggregates provider heartbeat health for the health report.
type ProviderHealth struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
	// Stale counts RESIDENT providers whose heartbeat_at is older than defaultHealthTTL.
	// on_demand providers are exempt (scale-to-zero: they only heartbeat when active).
	Stale int `json:"stale"`
}

// UsageReport is the response shape for GET /v1/usage.
type UsageReport struct {
	Items         []ItemCount `json:"items"`
	ItemSteps     []StepCount `json:"item_steps"`
	Distillations int         `json:"distillations"`
	Quarantine    int         `json:"quarantine"`
}

// ItemCount is one (lane, status) cell from the items GROUP BY.
type ItemCount struct {
	Lane   string `json:"lane"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// StepCount is one (capability, status) cell from the item_steps GROUP BY.
type StepCount struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	Count      int    `json:"count"`
}

// WorkerMetric is the per-provider rollup for GET /v1/workers/metrics.
// Feeds the 4 metric cards on the Workers screen (CONSOLE-WORKERS.pt-BR.md §8).
type WorkerMetric struct {
	Provider       string         `json:"provider"`
	Total          int            `json:"total"`
	ByStatus       map[string]int `json:"by_status"`
	Done           int            `json:"done"`
	Failed         int            `json:"failed"`
	SuccessRate    float64        `json:"success_rate"` // done/(done+failed); 0 when both are 0
	Queue          int            `json:"queue"`        // pending+assigned+running
	AvgAttempt     float64        `json:"avg_attempt"`
	LastActivityAt *time.Time     `json:"last_activity_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Core — the operations layer (the "núcleo" both adapters drive).
// ---------------------------------------------------------------------------

// Core is the surface's operation layer: it validates inputs once and delegates to the
// persistence seam (reads + idempotent config upserts) and the reused Phase 3 / LinkedIn
// orchestration. Transport-agnostic — the HTTP and MCP adapters both call these methods.
type Core struct {
	db    Database
	inbox LinkedInPostStore
	// resolveChannel turns a channel reference (raw UC id, @handle, or name) into a
	// canonical youtube_channel_id. Set from YOUTUBE_API_KEY at startup; when nil the
	// input is used verbatim (e.g. unit tests that pass raw ids).
	resolveChannel func(ctx context.Context, input string) (string, error)
}

// NewCore wires the operations layer over the seam and the LinkedIn store.
func NewCore(db Database, inbox LinkedInPostStore) *Core { return &Core{db: db, inbox: inbox} }

// errNotFound is returned by Core methods when a resource cannot be found; HTTP maps it to 404.
var errNotFound = errors.New("not found")

// badInputError marks a caller error (bad id, unknown status, invalid enum) so the adapters
// answer 400, not 500. Genuine seam failures stay unwrapped and answer 500.
type badInputError struct{ msg string }

func (e badInputError) Error() string { return e.msg }
func badInput(format string, a ...any) error {
	return badInputError{msg: fmt.Sprintf(format, a...)}
}

// --- State reads ----------------------------------------------------------

// ListItems returns the items in a given lifecycle status (validated).
func (c *Core) ListItems(ctx context.Context, status string) ([]Item, error) {
	if !isValidItemStatus(status) {
		return nil, badInput("unknown item status %q", status)
	}
	return c.db.ListItemsByStatus(ctx, status)
}

// ItemSteps returns an item's runtime steps (the item_steps view).
func (c *Core) ItemSteps(ctx context.Context, itemID int) ([]ItemStep, error) {
	if itemID <= 0 {
		return nil, badInput("item id must be positive, got %d", itemID)
	}
	return c.db.ListItemSteps(ctx, itemID)
}

// ItemDecisions returns an item's full gate_decisions audit trail.
func (c *Core) ItemDecisions(ctx context.Context, itemID int) ([]GateDecision, error) {
	if itemID <= 0 {
		return nil, badInput("item id must be positive, got %d", itemID)
	}
	return c.db.ListGateDecisions(ctx, itemID)
}

// RecentDecisions returns the global gate_decisions feed, newest first.
func (c *Core) RecentDecisions(ctx context.Context, limit int) ([]RecentDecision, error) {
	return c.db.ListRecentDecisions(ctx, limit)
}

// Quarantine lists the deferred (quarantine) items — the cold-start review sample.
func (c *Core) Quarantine(ctx context.Context) ([]Item, error) {
	return c.db.ListQuarantinedItems(ctx)
}

// --- Config reads ---------------------------------------------------------

func (c *Core) Flows(ctx context.Context) ([]Flow, error) { return c.db.ListFlows(ctx) }
func (c *Core) FlowSteps(ctx context.Context, flowID int) ([]FlowStep, error) {
	if flowID <= 0 {
		return nil, badInput("flow id must be positive, got %d", flowID)
	}
	return c.db.ListFlowSteps(ctx, flowID)
}
func (c *Core) Providers(ctx context.Context) ([]Provider, error) { return c.db.ListProviders(ctx) }

// Worker is a logical binary grouping one or more provider placements.
type Worker struct {
	Name       string     `json:"name"`
	Capability string     `json:"capability"`
	Placements []Provider `json:"placements"`
}

// Workers returns all providers grouped by their Worker field (fallback: Name), with both the
// worker list and each placements slice sorted by name.
func (c *Core) Workers(ctx context.Context) ([]Worker, error) {
	providers, err := c.db.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	index := map[string]*Worker{}
	order := []string{}
	for _, p := range providers {
		key := p.Worker
		if key == "" {
			key = p.Name
		}
		if _, exists := index[key]; !exists {
			index[key] = &Worker{Name: key, Capability: p.Capability}
			order = append(order, key)
		}
		index[key].Placements = append(index[key].Placements, p)
	}
	sort.Strings(order)
	workers := make([]Worker, len(order))
	for i, name := range order {
		w := index[name]
		sort.Slice(w.Placements, func(a, b int) bool {
			return w.Placements[a].Name < w.Placements[b].Name
		})
		workers[i] = *w
	}
	return workers, nil
}

// AvailableProvider is the restricted view of a Provider sent to the hosts editor.
// Internal fields (runner_url, heartbeat_at, constraints, env) are intentionally omitted.
type AvailableProvider struct {
	Name       string `json:"name"`
	Capability string `json:"capability"`
	Runtime    string `json:"runtime"`
	Activation string `json:"activation"`
	Enabled    bool   `json:"enabled"`
}

// StepHostsResponse is the shape returned by GET /v1/flows/{id}/steps/{seq}/hosts.
type StepHostsResponse struct {
	Providers []string            `json:"providers"` // current per-step priority list (may be empty)
	Available []AvailableProvider `json:"available"` // all enabled providers for the step's capability
}

// findFlowStep returns the FlowStep with the given seq inside a flow, or errNotFound.
func (c *Core) findFlowStep(ctx context.Context, flowID, seq int) (FlowStep, error) {
	steps, err := c.db.ListFlowSteps(ctx, flowID)
	if err != nil {
		return FlowStep{}, err
	}
	for _, s := range steps {
		if s.Seq == seq {
			return s, nil
		}
	}
	return FlowStep{}, errNotFound
}

// StepHosts returns the per-step host priority list and the full set of available providers
// for a step's capability. Returns badInput when the flow or seq cannot be resolved.
func (c *Core) StepHosts(ctx context.Context, flowID, seq int) (StepHostsResponse, error) {
	fs, err := c.findFlowStep(ctx, flowID, seq)
	if err != nil {
		return StepHostsResponse{}, err
	}
	raw, err := c.db.ListProvidersForCapability(ctx, fs.Capability)
	if err != nil {
		return StepHostsResponse{}, err
	}
	avail := make([]AvailableProvider, len(raw))
	for i, p := range raw {
		avail[i] = AvailableProvider{Name: p.Name, Capability: p.Capability, Runtime: p.Runtime, Activation: p.Activation, Enabled: p.Enabled}
	}
	var names []string
	if fb := stepFallbackFromOptions(fs.Options); len(fb) > 0 {
		if err := json.Unmarshal(fb, &names); err != nil {
			return StepHostsResponse{}, fmt.Errorf("flow %d step %d: malformed providers in options: %w", flowID, seq, err)
		}
	}
	if names == nil {
		names = []string{}
	}
	return StepHostsResponse{Providers: names, Available: avail}, nil
}

// validateStepProviders checks that each provider exists, is enabled, matches the required
// capability, and appears at most once in the list.
func (c *Core) validateStepProviders(ctx context.Context, capability string, providers []string) error {
	seen := make(map[string]bool, len(providers))
	for _, name := range providers {
		if seen[name] {
			return badInput("duplicate provider %q in hosts list", name)
		}
		seen[name] = true
		p, ok, err := c.db.GetProvider(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return badInput("provider %q does not exist", name)
		}
		if !p.Enabled {
			return badInput("provider %q is disabled", name)
		}
		if p.Capability != capability {
			return badInput("provider %q has capability %q, want %q", name, p.Capability, capability)
		}
	}
	return nil
}

// SetStepHosts updates the per-step host priority list for a flow step.
// Validates: flow+seq must exist; each provider must exist, be enabled, and match the
// step's capability; no duplicates. An empty list clears the override.
func (c *Core) SetStepHosts(ctx context.Context, flowID, seq int, providers []string) error {
	fs, err := c.findFlowStep(ctx, flowID, seq)
	if err != nil {
		return err
	}

	if err := c.validateStepProviders(ctx, fs.Capability, providers); err != nil {
		return err
	}

	// Merge into options: preserve existing keys, update/clear providers.
	var opts map[string]json.RawMessage
	if len(fs.Options) > 0 {
		if err := json.Unmarshal(fs.Options, &opts); err != nil {
			return err
		}
	}
	if opts == nil {
		opts = make(map[string]json.RawMessage)
	}
	if len(providers) == 0 {
		delete(opts, "providers")
	} else {
		b, err := json.Marshal(providers)
		if err != nil {
			return err
		}
		opts["providers"] = b
	}
	var newOpts json.RawMessage
	if len(opts) > 0 {
		if newOpts, err = json.Marshal(opts); err != nil {
			return err
		}
	}
	fs.Options = newOpts
	return c.db.UpsertFlowStep(ctx, fs)
}
func (c *Core) RoutingPolicies(ctx context.Context) ([]RoutingPolicy, error) {
	return c.db.ListRoutingPolicies(ctx)
}
func (c *Core) GateRules(ctx context.Context) ([]GateRule, error) { return c.db.ListAllGateRules(ctx) }

// --- Podcast sources (control-plane config) -------------------------------
// Managing what to collect is an operator decision, so the core surface WRITES podcast_feeds —
// just as it already owns flows/providers. The table's DDL is rara-dial's; the collector keeps
// only READING active=true. This is the first core write into a collector's table: operator
// config, not dial domain.

// AddPodcastFeed idempotently adds a feed (the dial then collects it). Title is optional — the dial
// fills it on collection; displayName is an optional UI-only override. A blank feed_url is a caller
// error (400). Toggling/listing/deleting a feed go through the generic source endpoints (podcast:N).
func (c *Core) AddPodcastFeed(ctx context.Context, feedURL, title, displayName string) (int, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return 0, badInput("feed_url cannot be empty")
	}
	// feed_url is fetched later by the dial collector, so it gets the same public-URL guard as
	// the rss/html feed sources — reject non-http(s) schemes and loopback/private IP literals.
	if err := validateEndpointURL(feedURL); err != nil {
		return 0, err
	}
	return c.db.UpsertPodcastFeed(ctx, feedURL, strings.TrimSpace(title), strings.TrimSpace(displayName))
}

// --- Source writes (fatia #2) — per-kind create + cross-kind edit/toggle ---

// AddYouTubeChannel registers a YouTube channel for collection (rara-harvest's table).
// channelRef may be a raw channel id (UC…), an @handle, or a free-text name — it is
// resolved to the canonical youtube_channel_id (the UNIQUE key) via the YouTube API.
// The operator-facing channel_name keeps the original reference (handle/name); the
// resolved UC id is not surfaced in the UI. displayName is optional (UI-only).
func (c *Core) AddYouTubeChannel(ctx context.Context, channelRef, channelName, displayName string) (int, error) {
	channelRef = strings.TrimSpace(channelRef)
	if channelRef == "" {
		return 0, badInput("channel_id cannot be empty")
	}
	channelID := channelRef
	if c.resolveChannel != nil {
		resolved, err := c.resolveChannel(ctx, channelRef)
		if err != nil {
			return 0, err
		}
		channelID = resolved
	}
	if channelName == "" {
		channelName = channelRef
	}
	return c.db.UpsertYouTubeChannel(ctx, channelID, channelName, displayName)
}

// AddYouTubePlaylist registers a YouTube playlist for collection (rara-shelf's table).
// playlistURL may be a full YouTube URL (list= param is parsed) or a raw playlist ID.
func (c *Core) AddYouTubePlaylist(ctx context.Context, playlistURL, displayName string) (int, error) {
	raw := strings.TrimSpace(playlistURL)
	if raw == "" {
		return 0, badInput("playlist_url cannot be empty")
	}
	playlistID, err := extractPlaylistID(raw)
	if err != nil {
		return 0, badInput("%v", err)
	}
	return c.db.UpsertYouTubePlaylist(ctx, playlistID, raw, displayName)
}

// extractPlaylistID returns the playlist ID from a YouTube URL or a bare ID.
func extractPlaylistID(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		// Not a URL: treat as a raw playlist ID.
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid playlist URL: %w", err)
	}
	id := u.Query().Get("list")
	if id == "" {
		return "", fmt.Errorf("playlist URL has no list= parameter: %q", raw)
	}
	return id, nil
}

// validFeedKinds are the source_type values feed_sources accepts.
var validFeedKinds = map[string]bool{"rss": true, "html": true, "hn": true}

// validateEndpointURL rejects non-HTTP(S) schemes and loopback/private IP literals.
// Domain names that resolve to private IPs are checked at fetch time by rara-feed.
func validateEndpointURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return badInput("invalid endpoint URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return badInput("endpoint must use http or https, got %q", u.Scheme)
	}
	if host := u.Hostname(); host != "" {
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return badInput("endpoint %q is not a public address", host)
			}
		}
	}
	return nil
}

// AddFeedSource adds an RSS/HTML/HN source to the feed collector's table.
// For rss/html, endpoint is required and must be a public http(s) URL; for hn it may be empty.
func (c *Core) AddFeedSource(ctx context.Context, kind, name, endpoint, displayName string) (int, error) {
	if !validFeedKinds[kind] {
		return 0, badInput("unknown feed kind %q (want rss|html|hn)", kind)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, badInput("feed source name cannot be empty")
	}
	if kind != "hn" {
		if strings.TrimSpace(endpoint) == "" {
			return 0, badInput("endpoint cannot be empty for kind %q", kind)
		}
		if err := validateEndpointURL(endpoint); err != nil {
			return 0, err
		}
	}
	cls := "b-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	return c.db.UpsertFeedSource(ctx, name, kind, endpoint, cls, displayName)
}

// normalizeLinkedInProfileURL validates and normalizes a LinkedIn profile/company URL.
// It accepts both linkedin.com and www.linkedin.com prefixes, normalizing to www.
// Returns the normalized URL or a badInput error.
func normalizeLinkedInProfileURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", badInput("profile_url cannot be empty")
	}
	// Normalize linkedin.com → www.linkedin.com so the same profile isn't stored twice.
	u = strings.Replace(u, "https://linkedin.com/", "https://www.linkedin.com/", 1)
	if !strings.HasPrefix(u, "https://www.linkedin.com/") {
		return "", badInput("profile_url must be a LinkedIn URL (https://www.linkedin.com/… or https://linkedin.com/…)")
	}
	// Restrict to collectable path types; /feed/, /jobs/, etc. are not profile URLs.
	path := strings.TrimPrefix(u, "https://www.linkedin.com")
	if !strings.HasPrefix(path, "/in/") && !strings.HasPrefix(path, "/company/") && !strings.HasPrefix(path, "/showcase/") {
		return "", badInput("profile_url must point to a person (/in/), company (/company/), or showcase (/showcase/) page")
	}
	return u, nil
}

// AddLinkedInProfile upserts a LinkedIn profile URL into target_linkedin_profiles.
// profileURL must be a canonical LinkedIn profile or company URL (https://www.linkedin.com/…
// or https://linkedin.com/…). Idempotent on profile_url.
func (c *Core) AddLinkedInProfile(ctx context.Context, profileURL, displayName string) (int, error) {
	normalized, err := normalizeLinkedInProfileURL(profileURL)
	if err != nil {
		return 0, err
	}
	return c.db.CreateLinkedInProfile(ctx, normalized, displayName)
}

// AddEmailSource adds an email reading rule to the courier's table.
// At least one of gmailQuery, label, or fromFilter must be non-empty.
func (c *Core) AddEmailSource(ctx context.Context, gmailQuery, label, fromFilter, displayName string) (int, error) {
	if strings.TrimSpace(gmailQuery) == "" && strings.TrimSpace(label) == "" && strings.TrimSpace(fromFilter) == "" {
		return 0, badInput("at least one of gmail_query, label, from_filter must be set")
	}
	return c.db.CreateEmailSource(ctx, gmailQuery, label, fromFilter, displayName)
}

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
		out["youtube_playlist_id"], out["title"] = plID, raw
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
		u, err := normalizeLinkedInProfileURL(cfg["profile_url"])
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

// PatchSource updates display_name, tags, and/or config fields on any source identified by api_id.
// All fields are optional; omitting all is a valid no-op.
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
		if err := c.db.UpdateSourceConfig(ctx, apiID, norm); err != nil {
			return err
		}
	}
	// display_name / tags update (existing behavior; no-op when both nil).
	if patch.DisplayName != nil || patch.Tags != nil {
		return c.db.PatchSourceMeta(ctx, apiID, patch.DisplayName, patch.Tags)
	}
	return nil
}

// PauseSource sets a source to paused (active/enabled=false). Idempotent.
func (c *Core) PauseSource(ctx context.Context, apiID string) error {
	return c.toggleSourceActive(ctx, apiID, false)
}

// ResumeSource restores a source to active. Idempotent.
func (c *Core) ResumeSource(ctx context.Context, apiID string) error {
	return c.toggleSourceActive(ctx, apiID, true)
}

// validSourceKinds are the api_id prefixes accepted by the cross-kind source operations.
var validSourceKinds = map[string]bool{
	"youtube_channel": true, "youtube_playlist": true, "podcast": true,
	"rss": true, "html": true, "hn": true, "email": true,
	"linkedin_profile": true,
}

// DeleteSource soft-deletes a source (sets deleted_at) so it DISAPPEARS from sources_v and
// the Console list, while its already-collected content (videos, episodes, distillations)
// is preserved — no hard DELETE, no cascade. Distinct from PauseSource, which only flips
// active/enabled=false and keeps the source listed as paused. Idempotent.
func (c *Core) DeleteSource(ctx context.Context, apiID string) error {
	kind, _, ok := parseSourceID(apiID)
	if !ok {
		return badInput("invalid source id %q (want kind:N)", apiID)
	}
	if !validSourceKinds[kind] {
		return badInput("unknown source kind %q", kind)
	}
	return c.db.SetSourceDeleted(ctx, apiID)
}

func (c *Core) toggleSourceActive(ctx context.Context, apiID string, active bool) error {
	kind, _, ok := parseSourceID(apiID)
	if !ok {
		return badInput("invalid source id %q (want kind:N)", apiID)
	}
	if !validSourceKinds[kind] {
		return badInput("unknown source kind %q", kind)
	}
	return c.db.SetSourceActive(ctx, apiID, active)
}

// validBulkActions are the allowed actions for POST /v1/sources/bulk.
// "delete" is the destructive action (soft-delete via deleted_at); there is no "archive".
var validBulkActions = map[string]bool{"pause": true, "resume": true, "tag": true, "untag": true, "delete": true}

// BulkSources applies an action to multiple sources. Results are per-item: a failure on one
// does not stop the others (relayed in BulkSourcesResult).
func (c *Core) BulkSources(ctx context.Context, action string, ids []string, tag string) (BulkSourcesResult, error) {
	if !validBulkActions[action] {
		return BulkSourcesResult{}, badInput("unknown bulk action %q (want pause|resume|tag|untag|delete)", action)
	}
	result := BulkSourcesResult{Items: make([]BulkSourceEntry, 0, len(ids))}
	for _, id := range ids {
		entry := BulkSourceEntry{ID: id}
		var err error
		switch action {
		case "pause":
			err = c.PauseSource(ctx, id)
		case "delete":
			err = c.DeleteSource(ctx, id)
		case "resume":
			err = c.ResumeSource(ctx, id)
		case "tag":
			if tag == "" {
				err = badInput("tag action requires a non-empty tag")
			} else {
				err = c.addTag(ctx, id, tag)
			}
		case "untag":
			if tag == "" {
				err = badInput("untag action requires a non-empty tag")
			} else {
				err = c.removeTag(ctx, id, tag)
			}
		}
		if err != nil {
			entry.Error = err.Error()
			result.Failed++
		} else {
			entry.OK = true
			result.Applied++
		}
		result.Items = append(result.Items, entry)
	}
	return result, nil
}

// addTag appends a tag to the source's tags array (no-op if already present).
func (c *Core) addTag(ctx context.Context, apiID, tag string) error {
	src, found, err := c.db.GetSource(ctx, apiID)
	if err != nil {
		return err
	}
	tags := src.Tags
	if !found {
		// Source not in sources_v yet (newly created); patch with just the tag.
		tags = []string{}
	}
	for _, t := range tags {
		if t == tag {
			return nil // already present
		}
	}
	tags = append(tags, tag)
	return c.db.PatchSourceMeta(ctx, apiID, nil, tags)
}

// removeTag removes a tag from the source's tags array (no-op if absent).
func (c *Core) removeTag(ctx context.Context, apiID, tag string) error {
	src, _, err := c.db.GetSource(ctx, apiID)
	if err != nil {
		return err
	}
	filtered := src.Tags[:0:0]
	for _, t := range src.Tags {
		if t != tag {
			filtered = append(filtered, t)
		}
	}
	return c.db.PatchSourceMeta(ctx, apiID, nil, filtered)
}

// --- Unified source listing (sources_v, fatia #1) -------------------------

// sourceKind builds a SourceKind entry. All kinds support pause and tags, and every
// kind ends with a display_name field (human-readable override); callers supply the rest.
func sourceKind(kind, label, lane, icon, targetApp string, fields ...SourceField) SourceKind {
	return SourceKind{
		Kind: kind, Label: label, Lane: lane, Icon: icon, TargetApp: targetApp,
		SupportsPause: true, SupportsTags: true,
		Fields: append(fields, SourceField{Name: "display_name", Label: "Display name", Type: "text"}),
	}
}

// sourceKindsRegistry is the config-driven registry of source kinds (drives the wizard UI).
// A new kind = one entry here + one write endpoint in fatia #2. No table needed.
var sourceKindsRegistry = []SourceKind{
	sourceKind("youtube_channel", "YouTube Channel", "youtube", "youtube", "rara-harvest",
		// channel_id accepts a raw channel id (UC…), an @handle, or a free-text name; it is
		// resolved to the canonical youtube_channel_id via the YouTube API at creation time.
		SourceField{Name: "channel_id", Label: "Channel ID, handle, or name", Type: "text", Required: true, Placeholder: "UCxxxx… , @handle, or channel name"},
		SourceField{Name: "channel_name", Label: "Channel name", Type: "text"},
	),
	sourceKind("youtube_playlist", "YouTube Playlist", "youtube", "youtube", "rara-shelf",
		SourceField{Name: "playlist_url", Label: "Playlist URL", Type: "url", Required: true, Placeholder: "https://youtube.com/playlist?list=..."},
	),
	sourceKind("podcast", "Podcast Feed", "podcast", "podcast", "rara-dial",
		SourceField{Name: "feed_url", Label: "Feed URL", Type: "url", Required: true, Placeholder: "https://example.com/feed.rss"},
		SourceField{Name: "title", Label: "Title", Type: "text"},
	),
	sourceKind("rss", "RSS Feed", "news", "rss", "rara-feed",
		SourceField{Name: "endpoint", Label: "Feed URL", Type: "url", Required: true, Placeholder: "https://example.com/feed.rss"},
		SourceField{Name: "name", Label: "Name", Type: "text", Required: true},
	),
	sourceKind("html", "HTML Page", "news", "globe", "rara-feed",
		SourceField{Name: "endpoint", Label: "Page URL", Type: "url", Required: true, Placeholder: "https://example.com"},
		SourceField{Name: "name", Label: "Name", Type: "text", Required: true},
	),
	sourceKind("hn", "Hacker News", "news", "hackernews", "rara-feed",
		SourceField{Name: "name", Label: "Name", Type: "text", Required: true},
	),
	sourceKind("email", "Email Reading Rule", "email", "mail", "rara-courier",
		SourceField{Name: "gmail_query", Label: "Gmail query", Type: "text", Placeholder: "from:newsletter@example.com"},
		SourceField{Name: "label", Label: "Gmail label", Type: "text"},
		SourceField{Name: "from_filter", Label: "Sender filter", Type: "text"},
	),
	sourceKind("linkedin_profile", "LinkedIn Profile / Company", "linkedin", "linkedin", "rara-clip",
		SourceField{Name: "profile_url", Label: "Profile URL", Type: "url", Required: true, Placeholder: "https://www.linkedin.com/in/handle  or  /company/name"},
	),
}

// maxSourcePageSize caps page_size for GET /v1/sources to prevent resource exhaustion.
const maxSourcePageSize = 200

// SourceKinds returns the static source-kind registry (feeds the wizard UI).
func (c *Core) SourceKinds() []SourceKind { return sourceKindsRegistry }

// Sources lists sources from sources_v with optional filters, pagination, and counts.
func (c *Core) Sources(ctx context.Context, f SourceFilter) (SourcesResult, error) {
	if f.PageSize > maxSourcePageSize {
		f.PageSize = maxSourcePageSize
	}
	return c.db.ListSources(ctx, f)
}

// Source returns a single source by api_id (found=false if absent).
func (c *Core) Source(ctx context.Context, apiID string) (SourceItem, bool, error) {
	return c.db.GetSource(ctx, apiID)
}

// SourceConfig returns the raw editable fields of one source for the Edit modal.
func (c *Core) SourceConfig(ctx context.Context, apiID string) (map[string]string, bool, error) {
	if _, _, ok := parseSourceID(apiID); !ok {
		return nil, false, badInput("invalid source id %q (want kind:N)", apiID)
	}
	return c.db.GetSourceConfig(ctx, apiID)
}

// InterestProfile returns the ACTIVE preferences document (the version in force the gate reads),
// not merely the latest — a `proposed` revision is invisible here until approved.
func (c *Core) InterestProfile(ctx context.Context) (InterestProfile, bool, error) {
	return c.db.GetActiveInterestProfile(ctx)
}

// InterestProfiles returns every version (active + proposed + superseded), so an operator can see
// a pending proposal and decide whether to approve it.
func (c *Core) InterestProfiles(ctx context.Context) ([]InterestProfile, error) {
	return c.db.ListInterestProfiles(ctx)
}

// --- Config edits (idempotent upserts; a new profile version is append-only) ----

func (c *Core) UpsertFlow(ctx context.Context, f Flow) (int, error) { return c.db.UpsertFlow(ctx, f) }
func (c *Core) UpsertFlowStep(ctx context.Context, s FlowStep) error {
	return c.db.UpsertFlowStep(ctx, s)
}
func (c *Core) UpsertProvider(ctx context.Context, p Provider) error {
	// Validate the enums here so a bad value is a 400 (caller input), not a 500 (the db CHECK
	// would reject it deeper, but the surface should name it as a client error).
	if !isValidRuntime(p.Runtime) {
		return badInput("invalid runtime %q (want local|cloudrun|vpc)", p.Runtime)
	}
	if !isValidActivation(p.Activation) {
		return badInput("invalid activation %q (want resident|on_demand)", p.Activation)
	}
	// Reject placements that would give a worker inconsistent capabilities. Two providers sharing
	// the same worker field must always agree on capability — enforced here since the DB has no
	// (worker, capability) unique constraint.
	// ponytail: TOCTOU — two concurrent upserts can both pass this check before either writes.
	// Acceptable: UpsertProvider is an operator action (never concurrent). Upgrade path: add a
	// UNIQUE constraint on (worker, capability) in providers and surface the violation as badInput.
	if p.Worker != "" {
		all, err := c.db.ListProviders(ctx)
		if err != nil {
			return fmt.Errorf("list providers: %w", err)
		}
		for _, sib := range all {
			if sib.Worker == p.Worker && sib.Name != p.Name && sib.Capability != p.Capability {
				return badInput("worker %q already has capability %q; placement %q with capability %q is inconsistent", p.Worker, sib.Capability, p.Name, p.Capability)
			}
		}
	}
	// heartbeat_at is RUNTIME liveness (owned by TouchProviderHeartbeat), not config. A
	// full-record config upsert would clobber it — so PRESERVE the live value across an edit
	// (and leave it nil for a brand-new provider, which then gets the router's bootstrap grace).
	// This is why a `heartbeat_at` in the request body is ignored.
	if existing, found, err := c.db.GetProvider(ctx, p.Name); err != nil {
		return err
	} else if found {
		p.HeartbeatAt = existing.HeartbeatAt
	} else {
		p.HeartbeatAt = nil
	}
	return c.db.UpsertProvider(ctx, p)
}
func (c *Core) UpsertRoutingPolicy(ctx context.Context, p RoutingPolicy) error {
	return c.db.UpsertRoutingPolicy(ctx, p)
}
func (c *Core) UpsertGateRule(ctx context.Context, r GateRule) error {
	if !isValidRuleAction(r.Action) {
		return badInput("invalid action %q (want allow|deny)", r.Action)
	}
	if !isValidMatchType(r.MatchType) {
		return badInput("invalid match_type %q (want channel|title_contains)", r.MatchType)
	}
	return c.db.UpsertGateRule(ctx, r)
}
func (c *Core) AddInterestProfile(ctx context.Context, p InterestProfile) error {
	if p.Version <= 0 {
		return badInput("interest_profile version must be positive, got %d", p.Version)
	}
	// A manually added version is a PROPOSAL — it never takes effect until approved, exactly like
	// a reviser-generated one. (The bootstrap v1 active row is seeded directly, not via here.)
	p.Status = profileProposed
	return c.db.InsertInterestProfile(ctx, p)
}

// ApproveProfile activates a proposed interest_profile version (human approval), demoting the
// prior active. A non-positive or non-proposed version is a caller error (400).
func (c *Core) ApproveProfile(ctx context.Context, version int) error {
	if version <= 0 {
		return badInput("interest_profile version must be positive, got %d", version)
	}
	if err := c.db.ActivateInterestProfile(ctx, version); err != nil {
		if errors.Is(err, errProfileNotProposed) {
			return badInput("interest_profile v%d is not a proposed version (already active, superseded, or absent)", version)
		}
		return err
	}
	return nil
}

// --- Human-in-the-loop (reuse the Phase 3 functions verbatim) --------------

// --- Distillation reads (cross-agent) ---------------------------------------

const (
	distillationListDefault = 50
	distillationListMax     = 200
)

// RecentDistillations returns the most recently created distillations (light projection,
// no content). limit=0 → 50; above 200 → capped at 200.
func (c *Core) RecentDistillations(ctx context.Context, limit int) ([]DistillationSummary, error) {
	if limit <= 0 {
		limit = distillationListDefault
	}
	if limit > distillationListMax {
		limit = distillationListMax
	}
	return c.db.ListRecentDistillations(ctx, limit)
}

// GetDistillation returns the full distillation (with content) for the given id.
// id <= 0 or not found → badInput (surfaces as 400).
func (c *Core) GetDistillation(ctx context.Context, id int) (Distillation, error) {
	if id <= 0 {
		return Distillation{}, badInput("distillation id must be positive, got %d", id)
	}
	d, found, err := c.db.GetDistillation(ctx, id)
	if err != nil {
		return Distillation{}, err
	}
	if !found {
		return Distillation{}, badInput("distillation %d not found", id)
	}
	return d, nil
}

// CaptureFeedback records explicit thumbs on a distillation (deliverable #4 of Phase 3).
func (c *Core) CaptureFeedback(ctx context.Context, distillationID, signal string) error {
	if err := CaptureDistillationFeedback(ctx, c.db, distillationID, signal); err != nil {
		return badInput("%v", err) // its errors are all caller-input (bad signal / empty id)
	}
	return nil
}

// ReviewQuarantineItem resolves a quarantined item (up rescues, down confirms the drop).
func (c *Core) ReviewQuarantineItem(ctx context.Context, itemID int, signal string) error {
	if signal != signalUp && signal != signalDown {
		return badInput("signal must be %q or %q, got %q", signalUp, signalDown, signal)
	}
	if itemID <= 0 {
		return badInput("item id must be positive, got %d", itemID)
	}
	// A missing item or one not actually in quarantine is a caller error (400), not a 500.
	// Pre-check it here so the surface names it clearly; ReviewQuarantine re-checks (harmless).
	if it, found, err := c.db.GetItem(ctx, itemID); err != nil {
		return err
	} else if !found || it.Status != itemQuarantine {
		return badInput("item %d is not in quarantine", itemID)
	}
	return ReviewQuarantine(ctx, c.db, itemID, signal)
}

// Health returns a degraded-safe aggregate of system health: DB connectivity, the most recent
// reconcile timestamp, and a provider heartbeat summary. Sub-checks that fail are reported as
// false/zero — the endpoint never 500s on a partial failure.
func (c *Core) Health(ctx context.Context) HealthReport {
	var r HealthReport
	r.DBOk = c.db.HealthPing(ctx) == nil

	if n := atomic.LoadInt64(&lastReconcileNano); n > 0 {
		t := time.Unix(0, n)
		r.LastReconcileAt = &t
	}

	if providers, err := c.db.ListProviders(ctx); err == nil {
		now := time.Now()
		for _, p := range providers {
			r.Providers.Total++
			if p.Enabled {
				r.Providers.Enabled++
			}
			// Resident providers are stale if their heartbeat is old; on_demand providers are
			// exempt (scale-to-zero: they only heartbeat when active, not while idle).
			if p.Activation == activationResident && p.HeartbeatAt != nil &&
				now.Sub(*p.HeartbeatAt) > defaultHealthTTL {
				r.Providers.Stale++
			}
		}
	}
	return r
}

// Usage returns exact COUNT(*) GROUP BY aggregates for items, item_steps, and distillations.
// Cross-agent tables (distillations) degrade gracefully when absent (table not deployed).
func (c *Core) Usage(ctx context.Context) (UsageReport, error) {
	return c.db.UsageCounts(ctx)
}

// WorkerMetrics returns the per-provider step rollup for the Workers screen metric cards.
// since restricts the window (nil = all-time).
func (c *Core) WorkerMetrics(ctx context.Context, since *time.Time) ([]WorkerMetric, error) {
	metrics, err := c.db.WorkerMetrics(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("core worker metrics: %w", err)
	}
	return metrics, nil
}

// SubmitLinkedIn is the stash collector (deliverable #3): upsert the post + discover the
// spine item. Returns the item id.
func (c *Core) SubmitLinkedIn(ctx context.Context, p LinkedInPost) (int, error) {
	id, err := SubmitLinkedInPost(ctx, c.db, c.inbox, p)
	if err != nil {
		return 0, badInput("%v", err) // url/text validation are caller-input
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// HTTP adapter
// ---------------------------------------------------------------------------

// NewSurfaceMux builds the authenticated REST router over Core, with the MCP adapter mounted at
// /mcp. /healthz is exempt from auth (a liveness probe). The token must be non-empty (checked by
// the caller, ServeSurface); the middleware fails closed regardless.
func NewSurfaceMux(core *Core, token string) http.Handler {
	mux := http.NewServeMux()
	h := &httpSurface{core: core}

	// Health + usage.
	mux.HandleFunc("GET /v1/health", h.health)
	mux.HandleFunc("GET /v1/usage", h.usage)

	// State reads.
	mux.HandleFunc("GET /v1/items", h.listItems)
	mux.HandleFunc("GET /v1/items/{id}/steps", h.itemSteps)
	mux.HandleFunc("GET /v1/items/{id}/decisions", h.itemDecisions)
	mux.HandleFunc("GET /v1/decisions", h.listDecisions)
	mux.HandleFunc("GET /v1/quarantine", h.quarantine)
	mux.HandleFunc("GET /v1/distillations", h.listDistillations)
	mux.HandleFunc("GET /v1/distillations/{id}", h.getDistillation)

	// Config reads.
	mux.HandleFunc("GET /v1/flows", h.listFlows)
	mux.HandleFunc("GET /v1/flows/{id}/steps", h.flowSteps)
	mux.HandleFunc("GET /v1/flows/{flow_id}/steps/{seq}/hosts", h.listStepHosts)
	mux.HandleFunc("PUT /v1/flows/{flow_id}/steps/{seq}/hosts", h.setStepHosts)
	mux.HandleFunc("GET /v1/providers", h.listProviders)
	mux.HandleFunc("GET /v1/workers", h.listWorkers)
	mux.HandleFunc("GET /v1/routing-policies", h.listRoutingPolicies)
	mux.HandleFunc("GET /v1/gate-rules", h.listGateRules)
	mux.HandleFunc("GET /v1/interest-profile", h.getInterestProfile)
	mux.HandleFunc("GET /v1/interest-profile/versions", h.listInterestProfiles)

	// Sources — unified registry + listing (fatia #1).
	mux.HandleFunc("GET /v1/source-kinds", h.listSourceKinds)
	mux.HandleFunc("GET /v1/sources", h.listSources)
	mux.HandleFunc("GET /v1/sources/{source_id}", h.getSource)
	mux.HandleFunc("GET /v1/sources/{source_id}/config", h.getSourceConfig)

	// Sources — fatia #2 writes. Bulk registered first (exact path wins over {kind} wildcard).
	mux.HandleFunc("POST /v1/sources/bulk", h.bulkSources)
	// Per-kind create: dispatches by {kind} value (youtube_channel|youtube_playlist|rss|html|hn|email).
	mux.HandleFunc("POST /v1/sources/{kind}", h.addSource)
	// Cross-kind edit + delete (soft-delete via deleted_at — the source disappears from the list).
	// Note: {source_id}/pause and /resume have deeper paths than {source_id} so they are matched
	// first by the Go 1.22 mux (longer patterns win).
	mux.HandleFunc("PATCH /v1/sources/{source_id}", h.patchSource)
	mux.HandleFunc("DELETE /v1/sources/{source_id}", h.deleteSource)
	mux.HandleFunc("POST /v1/sources/{source_id}/pause", h.pauseSource)
	mux.HandleFunc("POST /v1/sources/{source_id}/resume", h.resumeSource)

	// Config edits (idempotent upserts; a new profile version is append-only).
	mux.HandleFunc("PUT /v1/flows", h.upsertFlow)
	mux.HandleFunc("PUT /v1/flow-steps", h.upsertFlowStep)
	mux.HandleFunc("PUT /v1/providers", h.upsertProvider)
	mux.HandleFunc("PUT /v1/routing-policies", h.upsertRoutingPolicy)
	mux.HandleFunc("PUT /v1/gate-rules", h.upsertGateRule)
	mux.HandleFunc("POST /v1/interest-profile", h.addInterestProfile)
	mux.HandleFunc("POST /v1/interest-profile/approve", h.approveInterestProfile)

	// Human-in-the-loop.
	mux.HandleFunc("POST /v1/feedback/distillation", h.feedbackDistillation)
	mux.HandleFunc("POST /v1/quarantine/review", h.reviewQuarantine)

	// Worker metrics rollup (CONSOLE-WORKERS.pt-BR.md §8, slice 2/9).
	mux.HandleFunc("GET /v1/workers/metrics", h.workerMetrics)

	// LinkedIn manual inbox.
	mux.HandleFunc("POST /v1/linkedin/inbox", h.linkedinInbox)

	// MCP adapter (thin JSON-RPC front-end over the SAME Core).
	mux.Handle("POST /mcp", newMCPServer(core))

	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return authMiddleware(token, mux)
}

// authMiddleware enforces a single service token via `Authorization: Bearer <token>`, in
// constant time. It fails CLOSED — an empty configured token rejects everything — so the
// surface is never accidentally open. /live is exempt (an unauthenticated liveness probe).
func authMiddleware(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live" {
			next.ServeHTTP(w, r)
			return
		}
		got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type httpSurface struct{ core *Core }

// --- read handlers --------------------------------------------------------

func (h *httpSurface) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.core.Health(r.Context()))
}

func (h *httpSurface) usage(w http.ResponseWriter, r *http.Request) {
	report, err := h.core.Usage(r.Context())
	writeResult(w, report, err)
}

func (h *httpSurface) workerMetrics(w http.ResponseWriter, r *http.Request) {
	var since *time.Time
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 365 {
			writeResult(w, nil, badInput("days must be a positive integer between 1 and 365"))
			return
		}
		t := time.Now().Add(-time.Duration(n) * 24 * time.Hour)
		since = &t
	}
	metrics, err := h.core.WorkerMetrics(r.Context(), since)
	writeResult(w, metrics, err)
}

func (h *httpSurface) listItems(w http.ResponseWriter, r *http.Request) {
	items, err := h.core.ListItems(r.Context(), r.URL.Query().Get("status"))
	writeResult(w, items, err)
}

func (h *httpSurface) itemSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	steps, err := h.core.ItemSteps(r.Context(), id)
	writeResult(w, steps, err)
}

func (h *httpSurface) itemDecisions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	decs, err := h.core.ItemDecisions(r.Context(), id)
	writeResult(w, decs, err)
}

func (h *httpSurface) listDecisions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	decs, err := h.core.RecentDecisions(r.Context(), limit)
	writeResult(w, decs, err)
}

func (h *httpSurface) quarantine(w http.ResponseWriter, r *http.Request) {
	items, err := h.core.Quarantine(r.Context())
	writeResult(w, items, err)
}

func (h *httpSurface) listDistillations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.core.RecentDistillations(r.Context(), limit)
	writeResult(w, items, err)
}

func (h *httpSurface) getDistillation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := h.core.GetDistillation(r.Context(), id)
	writeResult(w, d, err)
}

func (h *httpSurface) listFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := h.core.Flows(r.Context())
	writeResult(w, flows, err)
}

func (h *httpSurface) flowSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	steps, err := h.core.FlowSteps(r.Context(), id)
	writeResult(w, steps, err)
}

func (h *httpSurface) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.core.Providers(r.Context())
	writeResult(w, providers, err)
}

func (h *httpSurface) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := h.core.Workers(r.Context())
	writeResult(w, workers, err)
}

func (h *httpSurface) listRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.core.RoutingPolicies(r.Context())
	writeResult(w, policies, err)
}

func (h *httpSurface) listGateRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.core.GateRules(r.Context())
	writeResult(w, rules, err)
}

func (h *httpSurface) listSourceKinds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.core.SourceKinds())
}

func (h *httpSurface) listSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := SourceFilter{
		Kind:   q.Get("kind"),
		Status: q.Get("status"),
		Tag:    q.Get("tag"),
		Q:      q.Get("q"),
	}
	if ps := q.Get("page_size"); ps != "" {
		if n, err := strconv.Atoi(ps); err == nil && n > 0 {
			f.PageSize = n
		}
	}
	if p := q.Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			f.Page = n
		}
	}
	result, err := h.core.Sources(r.Context(), f)
	writeResult(w, result, err)
}

func (h *httpSurface) getSource(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	src, found, err := h.core.Source(r.Context(), apiID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, `{"error":"source not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

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

// --- source write handlers (fatia #2) ---

// addSource dispatches POST /v1/sources/{kind} to the right Core method.
func (h *httpSurface) addSource(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	switch kind {
	case "youtube_channel":
		var req struct {
			ChannelID   string `json:"channel_id"`
			ChannelName string `json:"channel_name"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := h.core.AddYouTubeChannel(r.Context(), req.ChannelID, req.ChannelName, req.DisplayName)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"id": id})

	case "youtube_playlist":
		var req struct {
			PlaylistURL string `json:"playlist_url"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := h.core.AddYouTubePlaylist(r.Context(), req.PlaylistURL, req.DisplayName)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"id": id})

	case "rss", "html", "hn":
		var req struct {
			Name        string `json:"name"`
			Endpoint    string `json:"endpoint"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := h.core.AddFeedSource(r.Context(), kind, req.Name, req.Endpoint, req.DisplayName)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"id": id})

	case "email":
		var req struct {
			GmailQuery  string `json:"gmail_query"`
			Label       string `json:"label"`
			FromFilter  string `json:"from_filter"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := h.core.AddEmailSource(r.Context(), req.GmailQuery, req.Label, req.FromFilter, req.DisplayName)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"id": id})

	case "podcast":
		var req struct {
			FeedURL     string `json:"feed_url"`
			Title       string `json:"title"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := h.core.AddPodcastFeed(r.Context(), req.FeedURL, req.Title, req.DisplayName)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"id": id})

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

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown source kind: " + kind})
	}
}

func (h *httpSurface) patchSource(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	var patch SourcePatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.PatchSource(r.Context(), apiID, patch))
}

func (h *httpSurface) deleteSource(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	writeResult(w, okResult{OK: true}, h.core.DeleteSource(r.Context(), apiID))
}

func (h *httpSurface) pauseSource(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	writeResult(w, okResult{OK: true}, h.core.PauseSource(r.Context(), apiID))
}

func (h *httpSurface) resumeSource(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("source_id")
	writeResult(w, okResult{OK: true}, h.core.ResumeSource(r.Context(), apiID))
}

func (h *httpSurface) bulkSources(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
		Tag    string   `json:"tag"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.core.BulkSources(r.Context(), req.Action, req.IDs, req.Tag)
	writeResult(w, result, err)
}

func (h *httpSurface) getInterestProfile(w http.ResponseWriter, r *http.Request) {
	prof, found, err := h.core.InterestProfile(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, `{"error":"no active interest_profile"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, prof)
}

func (h *httpSurface) listInterestProfiles(w http.ResponseWriter, r *http.Request) {
	profs, err := h.core.InterestProfiles(r.Context())
	writeResult(w, profs, err)
}

func (h *httpSurface) approveInterestProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.ApproveProfile(r.Context(), req.Version))
}

// --- edit handlers --------------------------------------------------------

func (h *httpSurface) upsertFlow(w http.ResponseWriter, r *http.Request) {
	var f Flow
	if !decodeJSON(w, r, &f) {
		return
	}
	id, err := h.core.UpsertFlow(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": id})
}

func (h *httpSurface) upsertFlowStep(w http.ResponseWriter, r *http.Request) {
	var s FlowStep
	if !decodeJSON(w, r, &s) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertFlowStep(r.Context(), s))
}

func (h *httpSurface) listStepHosts(w http.ResponseWriter, r *http.Request) {
	flowID, ok := pathIntParam(w, r, "flow_id")
	if !ok {
		return
	}
	seq, ok := pathIntParam(w, r, "seq")
	if !ok {
		return
	}
	resp, err := h.core.StepHosts(r.Context(), flowID, seq)
	writeResult(w, resp, err)
}

func (h *httpSurface) setStepHosts(w http.ResponseWriter, r *http.Request) {
	flowID, ok := pathIntParam(w, r, "flow_id")
	if !ok {
		return
	}
	seq, ok := pathIntParam(w, r, "seq")
	if !ok {
		return
	}
	var req struct {
		Providers *[]string `json:"providers"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Providers == nil {
		writeErr(w, badInput("providers field is required"))
		return
	}
	writeResult(w, okResult{OK: true}, h.core.SetStepHosts(r.Context(), flowID, seq, *req.Providers))
}

func (h *httpSurface) upsertProvider(w http.ResponseWriter, r *http.Request) {
	var p Provider
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertProvider(r.Context(), p))
}

func (h *httpSurface) upsertRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	var p RoutingPolicy
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertRoutingPolicy(r.Context(), p))
}

func (h *httpSurface) upsertGateRule(w http.ResponseWriter, r *http.Request) {
	var rule GateRule
	if !decodeJSON(w, r, &rule) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.UpsertGateRule(r.Context(), rule))
}

func (h *httpSurface) addInterestProfile(w http.ResponseWriter, r *http.Request) {
	var p InterestProfile
	if !decodeJSON(w, r, &p) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.AddInterestProfile(r.Context(), p))
}

// --- action handlers ------------------------------------------------------

func (h *httpSurface) feedbackDistillation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DistillationID string `json:"distillation_id"`
		Signal         string `json:"signal"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.CaptureFeedback(r.Context(), req.DistillationID, req.Signal))
}

func (h *httpSurface) reviewQuarantine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ItemID int    `json:"item_id"`
		Signal string `json:"signal"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeResult(w, okResult{OK: true}, h.core.ReviewQuarantineItem(r.Context(), req.ItemID, req.Signal))
}

func (h *httpSurface) linkedinInbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string `json:"url"`
		Author string `json:"author"`
		Text   string `json:"text"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := h.core.SubmitLinkedIn(r.Context(), LinkedInPost{URL: req.URL, Author: req.Author, Text: req.Text})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"item_id": id})
}

// --- http helpers ---------------------------------------------------------

type okResult struct {
	OK bool `json:"ok"`
}

// pathID parses the {id} path wildcard as a positive int, answering 400 on a bad value.
func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	return pathIntParam(w, r, "id")
}

// pathIntParam parses a named path wildcard as a positive int, answering 400 on bad input.
func pathIntParam(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	v, err := strconv.Atoi(r.PathValue(name))
	if err != nil || v <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid " + name + " in path"})
		return 0, false
	}
	return v, true
}

// maxBodyBytes caps a request body (1 MiB) — far above any config row or pasted post, but a
// backstop against an unbounded body exhausting memory. Exceeding it fails the decode -> 400.
const maxBodyBytes = 1 << 20

// decodeJSON decodes a (size-capped) JSON request body, answering 400 on a malformed/oversized
// body. DisallowUnknownFields is deliberate: this is a config-edit API, so a mistyped field
// should be a visible 400, not silently dropped.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// writeResult writes data as 200 JSON, or maps err to a status (400 badInput, else 500).
func writeResult(w http.ResponseWriter, data any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// writeErr maps an error to its HTTP status: badInputError → 400, errNotFound → 404, else 500.
// 500 responses return a generic message; the actual error is logged server-side to avoid
// leaking internal details (stack frames, table names, config values) to callers.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var bad badInputError
	if errors.As(err, &bad) {
		status = http.StatusBadRequest
	} else if errors.Is(err, errNotFound) {
		status = http.StatusNotFound
	}
	if status == http.StatusInternalServerError {
		log.Printf("surface: internal error: %v", err)
		writeJSON(w, status, map[string]string{"error": "internal server error"})
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("surface: encode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Server lifecycle
// ---------------------------------------------------------------------------

// ServeSurface runs the control surface until ctx is cancelled, then shuts it down gracefully.
// It fails closed: an empty token is refused (the surface is personal, but never left open).
// Called both standalone (`core-job surface`) and from the reconciler loop (same process,
// alongside the ticker — the always-on VPC deployment).
func ServeSurface(ctx context.Context, core *Core, addr, token string) error {
	if token == "" {
		return fmt.Errorf("surface: SURFACE_TOKEN is required (refusing to serve without auth)")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewSurfaceMux(core, token),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("rara-core surface: listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
