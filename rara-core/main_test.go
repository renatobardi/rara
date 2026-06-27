package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Pure validators (mirror the SQL CHECK enums)
// ---------------------------------------------------------------------------

func TestValidators(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) bool
		ok   []string
		bad  []string
	}{
		{"runtime", isValidRuntime, []string{runtimeLocal, runtimeCloudRun, runtimeVPC}, []string{"", "gpu", "edge"}},
		{"activation", isValidActivation, []string{activationResident, activationOnDemand}, []string{"", "lazy"}},
		{"itemStatus", isValidItemStatus, []string{itemDiscovered, itemToText, itemDistilled, itemDone, itemFiltered, itemQuarantine, itemFailed}, []string{"", "pending", "queued"}},
		{"stepStatus", isValidStepStatus, []string{stepPending, stepAssigned, stepRunning, stepDone, stepFailed, stepSkipped}, []string{"", "discovered", "to_text"}},
		{"gate", isValidGate, []string{gateBarato, gateRico}, []string{"", "gate_medio"}},
		{"decision", isValidDecision, []string{decisionKeep, decisionDrop, decisionDefer}, []string{"", "maybe"}},
		{"targetType", isValidTargetType, []string{targetItem, targetDistillation}, []string{"", "transcript"}},
	}
	for _, c := range cases {
		for _, v := range c.ok {
			if !c.fn(v) {
				t.Errorf("%s: %q should be valid", c.name, v)
			}
		}
		for _, v := range c.bad {
			if c.fn(v) {
				t.Errorf("%s: %q should be invalid", c.name, v)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// MockDatabase — an in-memory store that mirrors the SQL contract of
// migrations/001_initial_schema.sql: UNIQUE keys (upsert vs. duplicate-add),
// internal FK references (capability / flow / item / provider must exist), the CHECK
// enums (via the shared validators), and the append-only nature of the audit tables.
// Zero I/O — the whole persistence seam is exercised in memory.
// ---------------------------------------------------------------------------

var (
	errFKViolation     = errors.New("mock: foreign key violation")
	errUniqueViolation = errors.New("mock: unique violation")
	errCheckViolation  = errors.New("mock: check violation")
)

type itemStepKey struct {
	itemID int
	seq    int
}
type flowStepKey struct {
	flowID int
	seq    int
}

// capProviderError mirrors the scanProvider truncation guard so MockDatabase reads are
// consistent with the real pgxDatabase (both cap last_error via truncateErrorMsg).
func capProviderError(p Provider) Provider {
	if p.LastError != nil {
		if orig := *p.LastError; utf8.RuneCountInString(orig) > maxProviderErrorLen {
			s := truncateErrorMsg(orig)
			p.LastError = &s
		}
	}
	return p
}

// mock row types for source write backing stores (fatia #2)
type mockYTChannel struct {
	ID          int
	ChannelID   string
	ChannelName string
	DisplayName string
	Tags        []string
	Active      bool
}

type mockYTPlaylist struct {
	ID          int
	PlaylistID  string
	Title       string
	DisplayName string
	Tags        []string
	Active      bool
}

type mockFeedSource struct {
	ID          int
	Name        string
	SourceType  string
	Endpoint    string
	Cls         string
	DisplayName string
	Tags        []string
	Enabled     bool
}

type mockEmailSource struct {
	ID          int
	GmailQuery  string
	Label       string
	FromFilter  string
	DisplayName string
	Tags        []string
	Enabled     bool
}

type mockLinkedInProfile struct {
	ID          int
	ProfileURL  string
	DisplayName string
	Tags        []string
	Active      bool
}

type MockDatabase struct {
	capabilities map[string]Capability // UNIQUE(name)
	providers    map[string]Provider   // UNIQUE(name)
	flows        map[string]Flow       // UNIQUE(name)
	flowByID     map[int]bool          // FK target for flow_steps.flow_id / items.flow_id
	flowSteps    map[flowStepKey]FlowStep
	policies     map[string]RoutingPolicy // UNIQUE(scope)

	items     map[string]Item          // UNIQUE(lane, source_ref) -> key "lane\x00source_ref"
	itemByID  map[int]Item             // id -> item (for GetItem / ListActiveItems)
	itemSteps map[itemStepKey]ItemStep // UNIQUE(item_id, seq)
	// stepOrder records item_steps insertion order so the claim can mirror the SQL
	// ORDER BY id (FIFO over the pending frontier).
	stepOrder    map[itemStepKey]int
	nextStepSeqN int

	gateRules map[gateRuleKey]GateRule // UNIQUE(action, match_type, value)

	gateDecisions []GateDecision          // append-only
	feedback      []Feedback              // append-only
	profiles      map[int]InterestProfile // UNIQUE(version)
	distillations []Distillation          // cross-agent read-only seam (rara-distill owns the table)

	podcastFeeds map[int]PodcastFeed // rara-dial's table, written by the core surface (config)
	feedByURL    map[string]int      // UNIQUE(feed_url) -> id

	sources []SourceItem // backing store for sources_v mock (tests populate directly)

	// --- source write backing stores (fatia #2) ---
	ytChannels            map[int]mockYTChannel
	ytChannelByKey        map[string]int // UNIQUE(youtube_channel_id) -> id
	ytPlaylists           map[int]mockYTPlaylist
	ytPlaylistByKey       map[string]int // UNIQUE(youtube_playlist_id) -> id
	feedSources           map[int]mockFeedSource
	feedByNameEp          map[string]int // UNIQUE(name+"\x00"+endpoint) -> id
	emailSources          map[int]mockEmailSource
	linkedinProfiles      map[int]mockLinkedInProfile
	nextLinkedInProfileID int

	nextFlowID     int
	nextItemID     int
	nextFeedID     int
	nextYTChanID   int
	nextYTPlayID   int
	nextFeedSrcID  int
	nextEmailSrcID int

	// nowFn stamps CreatedAt on appended feedback / inserted profiles when the caller leaves it
	// zero (mirroring the SQL DEFAULT CURRENT_TIMESTAMP). Tests override it for determinism.
	nowFn func() time.Time
}

type gateRuleKey struct {
	action, matchType, value string
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		capabilities:          make(map[string]Capability),
		providers:             make(map[string]Provider),
		flows:                 make(map[string]Flow),
		flowByID:              make(map[int]bool),
		flowSteps:             make(map[flowStepKey]FlowStep),
		policies:              make(map[string]RoutingPolicy),
		items:                 make(map[string]Item),
		itemByID:              make(map[int]Item),
		itemSteps:             make(map[itemStepKey]ItemStep),
		stepOrder:             make(map[itemStepKey]int),
		gateRules:             make(map[gateRuleKey]GateRule),
		profiles:              make(map[int]InterestProfile),
		podcastFeeds:          make(map[int]PodcastFeed),
		feedByURL:             make(map[string]int),
		ytChannels:            make(map[int]mockYTChannel),
		ytChannelByKey:        make(map[string]int),
		ytPlaylists:           make(map[int]mockYTPlaylist),
		ytPlaylistByKey:       make(map[string]int),
		feedSources:           make(map[int]mockFeedSource),
		feedByNameEp:          make(map[string]int),
		emailSources:          make(map[int]mockEmailSource),
		linkedinProfiles:      make(map[int]mockLinkedInProfile),
		nextLinkedInProfileID: 1,
		nextFlowID:            1,
		nextItemID:            1,
		nextFeedID:            1,
		nextYTChanID:          1,
		nextYTPlayID:          1,
		nextFeedSrcID:         1,
		nextEmailSrcID:        1,
		nowFn:                 time.Now,
	}
}

func itemKey(lane, sourceRef string) string { return lane + "\x00" + sourceRef }

func (m *MockDatabase) UpsertCapability(_ context.Context, c Capability) error {
	m.capabilities[c.Name] = c // ON CONFLICT (name) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertProvider(_ context.Context, p Provider) error {
	if !isValidRuntime(p.Runtime) || !isValidActivation(p.Activation) {
		return errCheckViolation
	}
	if !isJSONObject(p.Env) {
		return errCheckViolation // CHECK (jsonb_typeof(env) = 'object')
	}
	if _, ok := m.capabilities[p.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	if p.Worker == "" {
		p.Worker = p.Name // mirror pgxDatabase guard
	}
	if p.App == "" {
		p.App = p.Name // mirror pgxDatabase guard
	}
	// Mirror the SQL ON CONFLICT: preserve runtime-owned columns the operator surface never sets.
	if existing, ok := m.providers[p.Name]; ok {
		p.HeartbeatAt = existing.HeartbeatAt     // owned by TouchProviderHeartbeat
		p.LastCollectAt = existing.LastCollectAt // owned by dispatcher (cadence clock)
		p.LastAttemptAt = existing.LastAttemptAt // owned by dispatcher (retry throttle)
		p.LastError = existing.LastError         // owned by runner on dispatch failure (P0d)
	}
	m.providers[p.Name] = p
	return nil
}

// SeedProvider mirrors SeedProvider in pgxDatabase: same as UpsertProvider but preserves the
// operator-owned enabled toggle so re-seeding never clobbers a pause/disable the operator set.
func (m *MockDatabase) SeedProvider(_ context.Context, p Provider) error {
	if !isValidRuntime(p.Runtime) || !isValidActivation(p.Activation) {
		return errCheckViolation
	}
	if !isJSONObject(p.Env) {
		return errCheckViolation
	}
	if _, ok := m.capabilities[p.Capability]; !ok {
		return errFKViolation
	}
	if p.Worker == "" {
		p.Worker = p.Name
	}
	if p.App == "" {
		p.App = p.Name
	}
	if existing, ok := m.providers[p.Name]; ok {
		p.Enabled = existing.Enabled             // operator-owned: seed must not overwrite
		p.HeartbeatAt = existing.HeartbeatAt     // owned by TouchProviderHeartbeat
		p.LastCollectAt = existing.LastCollectAt // owned by dispatcher (cadence clock)
		p.LastAttemptAt = existing.LastAttemptAt // owned by dispatcher (retry throttle)
		p.LastError = existing.LastError         // owned by runner on dispatch failure
	}
	m.providers[p.Name] = p
	return nil
}

func (m *MockDatabase) UpsertFlow(_ context.Context, f Flow) (int, error) {
	if existing, ok := m.flows[f.Name]; ok {
		f.ID = existing.ID // ON CONFLICT (name) DO UPDATE keeps the row id
		m.flows[f.Name] = f
		return f.ID, nil
	}
	f.ID = m.nextFlowID
	m.nextFlowID++
	m.flows[f.Name] = f
	m.flowByID[f.ID] = true
	return f.ID, nil
}

func (m *MockDatabase) UpsertFlowStep(_ context.Context, s FlowStep) error {
	if !m.flowByID[s.FlowID] {
		return errFKViolation // REFERENCES flows(id)
	}
	if _, ok := m.capabilities[s.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	m.flowSteps[flowStepKey{s.FlowID, s.Seq}] = s // ON CONFLICT (flow_id, seq) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertRoutingPolicy(_ context.Context, p RoutingPolicy) error {
	m.policies[p.Scope] = p // ON CONFLICT (scope) DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertPodcastFeed(_ context.Context, feedURL, title, displayName string) (int, error) {
	if id, ok := m.feedByURL[feedURL]; ok { // ON CONFLICT (feed_url)
		f := m.podcastFeeds[id]
		if title != "" { // empty title never wipes a title the dial already refreshed (COALESCE)
			f.Title = title
		}
		if displayName != "" { // same COALESCE semantics for display_name
			f.DisplayName = displayName
		}
		m.podcastFeeds[id] = f
		return id, nil
	}
	id := m.nextFeedID
	m.nextFeedID++
	m.podcastFeeds[id] = PodcastFeed{ID: id, FeedURL: feedURL, Title: title, DisplayName: displayName, Active: true} // DEFAULT TRUE
	m.feedByURL[feedURL] = id
	return id, nil
}

// --- source writes (fatia #2) ---

func (m *MockDatabase) UpsertYouTubeChannel(_ context.Context, channelID, channelName, displayName string) (int, error) {
	if id, ok := m.ytChannelByKey[channelID]; ok {
		ch := m.ytChannels[id]
		ch.ChannelName = channelName
		if displayName != "" {
			ch.DisplayName = displayName
		}
		m.ytChannels[id] = ch
		return id, nil
	}
	id := m.nextYTChanID
	m.nextYTChanID++
	apiID := fmt.Sprintf("youtube_channel:%d", id)
	m.ytChannels[id] = mockYTChannel{ID: id, ChannelID: channelID, ChannelName: channelName, DisplayName: displayName, Tags: []string{}, Active: true}
	m.ytChannelByKey[channelID] = id
	m.sources = append(m.sources, SourceItem{ApiID: apiID, Kind: "youtube_channel", DisplayName: displayName, Status: "active", Tags: []string{}})
	return id, nil
}

func (m *MockDatabase) UpsertYouTubePlaylist(_ context.Context, playlistID, title, displayName string) (int, error) {
	if id, ok := m.ytPlaylistByKey[playlistID]; ok {
		pl := m.ytPlaylists[id]
		pl.Title = title
		if displayName != "" {
			pl.DisplayName = displayName
		}
		m.ytPlaylists[id] = pl
		return id, nil
	}
	id := m.nextYTPlayID
	m.nextYTPlayID++
	apiID := fmt.Sprintf("youtube_playlist:%d", id)
	m.ytPlaylists[id] = mockYTPlaylist{ID: id, PlaylistID: playlistID, Title: title, DisplayName: displayName, Tags: []string{}, Active: true}
	m.ytPlaylistByKey[playlistID] = id
	m.sources = append(m.sources, SourceItem{ApiID: apiID, Kind: "youtube_playlist", DisplayName: displayName, Status: "active", Tags: []string{}})
	return id, nil
}

func (m *MockDatabase) UpsertFeedSource(_ context.Context, name, sourceType, endpoint, cls, displayName string) (int, error) {
	key := name + "\x00" + endpoint
	if id, ok := m.feedByNameEp[key]; ok {
		fs := m.feedSources[id]
		fs.SourceType = sourceType
		fs.Cls = cls
		if displayName != "" {
			fs.DisplayName = displayName
		}
		m.feedSources[id] = fs
		return id, nil
	}
	id := m.nextFeedSrcID
	m.nextFeedSrcID++
	apiID := fmt.Sprintf("%s:%d", sourceType, id)
	m.feedSources[id] = mockFeedSource{ID: id, Name: name, SourceType: sourceType, Endpoint: endpoint, Cls: cls, DisplayName: displayName, Tags: []string{}, Enabled: true}
	m.feedByNameEp[key] = id
	m.sources = append(m.sources, SourceItem{ApiID: apiID, Kind: sourceType, DisplayName: displayName, Status: "active", Tags: []string{}})
	return id, nil
}

func (m *MockDatabase) CreateEmailSource(_ context.Context, gmailQuery, label, fromFilter, displayName string) (int, error) {
	id := m.nextEmailSrcID
	m.nextEmailSrcID++
	apiID := fmt.Sprintf("email:%d", id)
	m.emailSources[id] = mockEmailSource{ID: id, GmailQuery: gmailQuery, Label: label, FromFilter: fromFilter, DisplayName: displayName, Tags: []string{}, Enabled: true}
	m.sources = append(m.sources, SourceItem{ApiID: apiID, Kind: "email", DisplayName: displayName, Status: "active", Tags: []string{}})
	return id, nil
}

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
	sourceDisplayName := profileURL
	if displayName != "" {
		sourceDisplayName = displayName
	}
	m.sources = append(m.sources, SourceItem{
		ApiID: apiID, Kind: "linkedin_profile", Lane: "linkedin",
		DisplayName: sourceDisplayName, Status: "active", Tags: []string{},
	})
	return id, nil
}

// setSourcesVStatus reflects an active/enabled change in the sources_v backing store.
func (m *MockDatabase) setSourcesVStatus(apiID string, active bool) {
	status := "active"
	if !active {
		status = "paused"
	}
	for i, s := range m.sources {
		if s.ApiID == apiID {
			m.sources[i].Status = status
			return
		}
	}
}

// setSourcesVTags reflects a tag change in the sources_v backing store.
func (m *MockDatabase) setSourcesVTags(apiID string, tags []string) {
	for i, s := range m.sources {
		if s.ApiID == apiID {
			m.sources[i].Tags = tags
			return
		}
	}
}

// setSourcesVDisplayName reflects a display_name change in the sources_v backing store.
func (m *MockDatabase) setSourcesVDisplayName(apiID, displayName string) {
	for i, s := range m.sources {
		if s.ApiID == apiID {
			m.sources[i].DisplayName = displayName
			return
		}
	}
}

func (m *MockDatabase) SetSourceActive(_ context.Context, apiID string, active bool) error {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return fmt.Errorf("mock: invalid api_id %q", apiID)
	}
	switch kind {
	case "youtube_channel":
		ch, exists := m.ytChannels[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		ch.Active = active
		m.ytChannels[id] = ch
	case "youtube_playlist":
		pl, exists := m.ytPlaylists[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		pl.Active = active
		m.ytPlaylists[id] = pl
	case "podcast":
		f, exists := m.podcastFeeds[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		f.Active = active
		m.podcastFeeds[id] = f
	case "rss", "html", "hn":
		fs, exists := m.feedSources[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		fs.Enabled = active
		m.feedSources[id] = fs
	case "email":
		es, exists := m.emailSources[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		es.Enabled = active
		m.emailSources[id] = es
	case "linkedin_profile":
		p, exists := m.linkedinProfiles[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		p.Active = active
		m.linkedinProfiles[id] = p
	default:
		return fmt.Errorf("mock: unknown kind %q in api_id %q", kind, apiID)
	}
	m.setSourcesVStatus(apiID, active)
	return nil
}

// SetSourceDeleted mirrors the soft-delete: the row vanishes from sources_v (m.sources),
// just as the real view filters deleted_at IS NULL. The backing write-store row is kept
// (content is preserved). Idempotent; an unknown id is a not-found error.
func (m *MockDatabase) SetSourceDeleted(_ context.Context, apiID string) error {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return fmt.Errorf("mock: invalid api_id %q", apiID)
	}
	var exists bool
	switch kind {
	case "youtube_channel":
		_, exists = m.ytChannels[id]
	case "youtube_playlist":
		_, exists = m.ytPlaylists[id]
	case "podcast":
		_, exists = m.podcastFeeds[id]
	case "rss", "html", "hn":
		_, exists = m.feedSources[id]
	case "email":
		_, exists = m.emailSources[id]
	case "linkedin_profile":
		_, exists = m.linkedinProfiles[id]
	default:
		return fmt.Errorf("mock: unknown kind %q in api_id %q", kind, apiID)
	}
	if !exists {
		return fmt.Errorf("source %q: %w", apiID, errNotFound)
	}
	// Drop it from the sources_v backing slice (idempotent — a no-op if already gone).
	for i, s := range m.sources {
		if s.ApiID == apiID {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MockDatabase) PatchSourceMeta(_ context.Context, apiID string, displayName *string, tags []string) error {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return fmt.Errorf("mock: invalid api_id %q", apiID)
	}
	switch kind {
	case "youtube_channel":
		ch, exists := m.ytChannels[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		if displayName != nil {
			ch.DisplayName = *displayName
		}
		if tags != nil {
			ch.Tags = tags
		}
		m.ytChannels[id] = ch
	case "youtube_playlist":
		pl, exists := m.ytPlaylists[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		if displayName != nil {
			pl.DisplayName = *displayName
		}
		if tags != nil {
			pl.Tags = tags
		}
		m.ytPlaylists[id] = pl
	case "podcast":
		f, exists := m.podcastFeeds[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		if displayName != nil {
			f.Title = *displayName // ponytail: podcast's display_name stored as Title in mock
		}
		m.podcastFeeds[id] = f
	case "rss", "html", "hn":
		fs, exists := m.feedSources[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		if displayName != nil {
			fs.DisplayName = *displayName
		}
		if tags != nil {
			fs.Tags = tags
		}
		m.feedSources[id] = fs
	case "email":
		es, exists := m.emailSources[id]
		if !exists {
			return fmt.Errorf("source %q: %w", apiID, errNotFound)
		}
		if displayName != nil {
			es.DisplayName = *displayName
		}
		if tags != nil {
			es.Tags = tags
		}
		m.emailSources[id] = es
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
	default:
		return fmt.Errorf("mock: unknown kind %q", kind)
	}
	if displayName != nil {
		m.setSourcesVDisplayName(apiID, *displayName)
	}
	if tags != nil {
		m.setSourcesVTags(apiID, tags)
	}
	return nil
}

// UpdateSourceConfig writes normalized config fields to the appropriate mock backing store,
// enforcing the same UNIQUE constraints the real tables have (duplicate URL/handle → badInput).
func (m *MockDatabase) UpdateSourceConfig(_ context.Context, apiID string, cfg map[string]string) error {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return fmt.Errorf("mock: invalid api_id %q", apiID)
	}
	switch kind {
	case "podcast":
		feedURL := cfg["feed_url"]
		// Enforce UNIQUE(feed_url): scan other rows for duplicates.
		for other, p := range m.podcastFeeds {
			if other != id && p.FeedURL == feedURL {
				return badInput("another source already uses this URL/handle")
			}
		}
		f, exists := m.podcastFeeds[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		// Remove old feedByURL entry if URL is changing.
		if f.FeedURL != feedURL {
			delete(m.feedByURL, f.FeedURL)
		}
		f.FeedURL = feedURL
		if t := cfg["title"]; t != "" {
			f.Title = t
		}
		m.podcastFeeds[id] = f
		m.feedByURL[feedURL] = id
	case "rss", "html":
		name := cfg["name"]
		endpoint := cfg["endpoint"]
		// Enforce UNIQUE(name, endpoint) via feedByNameEp.
		newKey := name + "\x00" + endpoint
		if existingID, clash := m.feedByNameEp[newKey]; clash && existingID != id {
			return badInput("another source already uses this URL/handle")
		}
		fs, exists := m.feedSources[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		oldKey := fs.Name + "\x00" + fs.Endpoint
		if oldKey != newKey {
			delete(m.feedByNameEp, oldKey)
		}
		fs.Name = name
		fs.Endpoint = endpoint
		m.feedSources[id] = fs
		m.feedByNameEp[newKey] = id
	case "hn":
		name := cfg["name"]
		fs, exists := m.feedSources[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		// hn has no endpoint; UNIQUE is just name for the mock.
		oldKey := fs.Name + "\x00" + fs.Endpoint
		newKey := name + "\x00" + fs.Endpoint
		if existingID, clash := m.feedByNameEp[newKey]; clash && existingID != id {
			return badInput("another source already uses this URL/handle")
		}
		delete(m.feedByNameEp, oldKey)
		fs.Name = name
		m.feedSources[id] = fs
		m.feedByNameEp[newKey] = id
	case "youtube_channel":
		chID := cfg["youtube_channel_id"]
		if existingID, clash := m.ytChannelByKey[chID]; clash && existingID != id {
			return badInput("another source already uses this URL/handle")
		}
		ch, exists := m.ytChannels[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		delete(m.ytChannelByKey, ch.ChannelID)
		ch.ChannelID = chID
		ch.ChannelName = cfg["channel_name"]
		m.ytChannels[id] = ch
		m.ytChannelByKey[chID] = id
	case "youtube_playlist":
		plID := cfg["youtube_playlist_id"]
		if existingID, clash := m.ytPlaylistByKey[plID]; clash && existingID != id {
			return badInput("another source already uses this URL/handle")
		}
		pl, exists := m.ytPlaylists[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		delete(m.ytPlaylistByKey, pl.PlaylistID)
		pl.PlaylistID = plID
		pl.Title = cfg["title"]
		m.ytPlaylists[id] = pl
		m.ytPlaylistByKey[plID] = id
	case "linkedin_profile":
		profileURL := cfg["profile_url"]
		// Enforce UNIQUE(profile_url).
		for other, p := range m.linkedinProfiles {
			if other != id && p.ProfileURL == profileURL {
				return badInput("another source already uses this URL/handle")
			}
		}
		p, exists := m.linkedinProfiles[id]
		if !exists {
			return badInput("source %q not found", apiID)
		}
		p.ProfileURL = profileURL
		m.linkedinProfiles[id] = p
	default:
		return fmt.Errorf("mock: UpdateSourceConfig: unknown kind %q", kind)
	}
	return nil
}

func (m *MockDatabase) ListSources(_ context.Context, f SourceFilter) (SourcesResult, error) {
	var filtered []SourceItem
	for _, s := range m.sources {
		if f.Kind != "" && s.Kind != f.Kind {
			continue
		}
		if f.Status != "" && s.Status != f.Status {
			continue
		}
		if f.Tag != "" {
			hasTag := false
			for _, t := range s.Tags {
				if t == f.Tag {
					hasTag = true
					break
				}
			}
			if !hasTag {
				continue
			}
		}
		if f.Q != "" {
			q := strings.ToLower(f.Q)
			if !strings.Contains(strings.ToLower(s.DisplayName), q) &&
				!strings.Contains(strings.ToLower(s.ConfigSummary), q) {
				continue
			}
		}
		item := s
		if item.Tags == nil {
			item.Tags = []string{}
		}
		filtered = append(filtered, item)
	}

	byStatus := make(map[string]int)
	byKind := make(map[string]int)
	for _, s := range filtered {
		byStatus[s.Status]++
		byKind[s.Kind]++
	}

	total := len(filtered)
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	if end > len(filtered) {
		end = len(filtered)
	}
	items := filtered[start:end]
	if items == nil {
		items = []SourceItem{}
	}

	return SourcesResult{
		Items: items, Page: page, PageSize: pageSize, Total: total,
		Counts: SourceCounts{ByStatus: byStatus, ByKind: byKind},
	}, nil
}

func (m *MockDatabase) GetSource(_ context.Context, apiID string) (SourceItem, bool, error) {
	for _, s := range m.sources {
		if s.ApiID == apiID {
			if s.Tags == nil {
				s.Tags = []string{}
			}
			return s, true, nil
		}
	}
	return SourceItem{}, false, nil
}

// ---------------------------------------------------------------------------
// Seed helpers for GetSourceConfig tests — insert at a caller-specified id so
// tests can assert a known api_id without relying on auto-increment order.
// ---------------------------------------------------------------------------

// SeedYouTubePlaylist inserts a playlist row at id with the given playlistID/title/displayName.
func (m *MockDatabase) SeedYouTubePlaylist(id int, playlistID, title, displayName string) {
	m.ytPlaylists[id] = mockYTPlaylist{ID: id, PlaylistID: playlistID, Title: title, DisplayName: displayName, Tags: []string{}, Active: true}
	m.ytPlaylistByKey[playlistID] = id
}

// SeedYouTubeChannel inserts a channel row at id with the given channelID/channelName/displayName.
func (m *MockDatabase) SeedYouTubeChannel(id int, channelID, channelName, displayName string) {
	m.ytChannels[id] = mockYTChannel{ID: id, ChannelID: channelID, ChannelName: channelName, DisplayName: displayName, Tags: []string{}, Active: true}
	m.ytChannelByKey[channelID] = id
}

// SeedPodcastFeed inserts a podcast_feed row at id with the given feedURL/title/displayName.
func (m *MockDatabase) SeedPodcastFeed(id int, feedURL, title, displayName string) {
	m.podcastFeeds[id] = PodcastFeed{ID: id, FeedURL: feedURL, Title: title, DisplayName: displayName, Active: true}
	m.feedByURL[feedURL] = id
}

// SeedFeedSource inserts a feed_source row at id with the given name/sourceType/endpoint/displayName.
func (m *MockDatabase) SeedFeedSource(id int, name, sourceType, endpoint, displayName string) {
	key := name + "\x00" + endpoint
	m.feedSources[id] = mockFeedSource{ID: id, Name: name, SourceType: sourceType, Endpoint: endpoint, DisplayName: displayName, Tags: []string{}, Enabled: true}
	m.feedByNameEp[key] = id
}

// SeedLinkedInProfile inserts a linkedin_profile row at id with the given profileURL/displayName.
func (m *MockDatabase) SeedLinkedInProfile(id int, profileURL, displayName string) {
	m.linkedinProfiles[id] = mockLinkedInProfile{ID: id, ProfileURL: profileURL, DisplayName: displayName, Tags: []string{}, Active: true}
}

// GetSourceConfig reads raw editable fields from the mock backing stores, keyed by registry field name.
// ponytail: mirrors pgxDatabase.GetSourceConfig; per-kind switch reads from the same mock maps the Upsert methods populate.
func (m *MockDatabase) GetSourceConfig(_ context.Context, apiID string) (map[string]string, bool, error) {
	kind, id, ok := parseSourceID(apiID)
	if !ok {
		return nil, false, fmt.Errorf("GetSourceConfig: invalid api_id %q", apiID)
	}
	switch kind {
	case "youtube_channel":
		ch, ok := m.ytChannels[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"channel_id": ch.ChannelID, "channel_name": ch.ChannelName}, true, nil
	case "youtube_playlist":
		pl, ok := m.ytPlaylists[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"playlist_url": "https://www.youtube.com/playlist?list=" + pl.PlaylistID}, true, nil
	case "podcast":
		f, ok := m.podcastFeeds[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"feed_url": f.FeedURL, "title": f.Title}, true, nil
	case "rss", "html":
		fs, ok := m.feedSources[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"name": fs.Name, "endpoint": fs.Endpoint}, true, nil
	case "hn":
		fs, ok := m.feedSources[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"name": fs.Name}, true, nil
	case "linkedin_profile":
		p, ok := m.linkedinProfiles[id]
		if !ok {
			return nil, false, nil
		}
		return map[string]string{"profile_url": p.ProfileURL}, true, nil
	case "email":
		return map[string]string{}, true, nil
	default:
		return nil, false, fmt.Errorf("GetSourceConfig: unknown kind %q", kind)
	}
}

func (m *MockDatabase) UpsertGateRule(_ context.Context, r GateRule) error {
	if !isValidRuleAction(r.Action) || !isValidMatchType(r.MatchType) {
		return errCheckViolation
	}
	m.gateRules[gateRuleKey{r.Action, r.MatchType, r.Value}] = r // ON CONFLICT DO UPDATE
	return nil
}

func (m *MockDatabase) UpsertItem(_ context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, errCheckViolation
	}
	it.Sensitivity = sensitivityOr(it.Sensitivity) // mirror the SQL DEFAULT 'public'
	if !isValidSensitivity(it.Sensitivity) {
		return 0, errCheckViolation
	}
	if !m.flowByID[it.FlowID] {
		return 0, errFKViolation // REFERENCES flows(id)
	}
	k := itemKey(it.Lane, it.SourceRef)
	if existing, ok := m.items[k]; ok {
		it.ID = existing.ID // ON CONFLICT (lane, source_ref) DO UPDATE keeps the row id
		m.items[k] = it
		m.itemByID[it.ID] = it
		return it.ID, nil
	}
	it.ID = m.nextItemID
	m.nextItemID++
	m.items[k] = it
	m.itemByID[it.ID] = it
	return it.ID, nil
}

// DiscoverItem mirrors the pgx idempotent discovery upsert: insert in the passed status,
// but on conflict PRESERVE the existing runtime status (re-stamping only flow fields).
func (m *MockDatabase) DiscoverItem(_ context.Context, it Item) (int, error) {
	if !isValidItemStatus(it.Status) {
		return 0, errCheckViolation
	}
	it.Sensitivity = sensitivityOr(it.Sensitivity) // mirror the SQL DEFAULT 'public'
	if !isValidSensitivity(it.Sensitivity) {
		return 0, errCheckViolation
	}
	if !m.flowByID[it.FlowID] {
		return 0, errFKViolation // REFERENCES flows(id)
	}
	k := itemKey(it.Lane, it.SourceRef)
	if existing, ok := m.items[k]; ok {
		// Re-discovery is a no-op (mirrors the pgx no-op ON CONFLICT): the flow stamp
		// (flow_id + flow_version) and runtime status are frozen at INSERT, so an in-flight
		// item finishes on the flow shape it was discovered with.
		return existing.ID, nil
	}
	it.ID = m.nextItemID
	m.nextItemID++
	m.items[k] = it
	m.itemByID[it.ID] = it
	return it.ID, nil
}

func (m *MockDatabase) UpsertItemStep(_ context.Context, s ItemStep) error {
	if !isValidStepStatus(s.Status) {
		return errCheckViolation
	}
	if _, ok := m.itemByID[s.ItemID]; !ok {
		return errFKViolation // REFERENCES items(id)
	}
	if _, ok := m.capabilities[s.Capability]; !ok {
		return errFKViolation // REFERENCES capabilities(name)
	}
	if s.AssignedProvider != "" {
		if _, ok := m.providers[s.AssignedProvider]; !ok {
			return errFKViolation // REFERENCES providers(name)
		}
	}
	k := itemStepKey{s.ItemID, s.Seq}
	if _, ok := m.stepOrder[k]; !ok {
		m.nextStepSeqN++
		m.stepOrder[k] = m.nextStepSeqN // record SERIAL-id insertion order for the claim
	}
	m.itemSteps[k] = s // ON CONFLICT (item_id, seq) DO UPDATE
	return nil
}

func (m *MockDatabase) InsertGateDecision(_ context.Context, d GateDecision) error {
	if !isValidGate(d.Gate) || !isValidDecision(d.Decision) {
		return errCheckViolation
	}
	if _, ok := m.itemByID[d.ItemID]; !ok {
		return errFKViolation
	}
	m.gateDecisions = append(m.gateDecisions, d) // append-only
	return nil
}

func (m *MockDatabase) InsertFeedback(_ context.Context, f Feedback) error {
	if !isValidTargetType(f.TargetType) || !isValidFeedbackSource(f.Source) {
		return errCheckViolation
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = m.nowFn() // mirror the SQL DEFAULT CURRENT_TIMESTAMP
	}
	m.feedback = append(m.feedback, f) // append-only
	return nil
}

func (m *MockDatabase) InsertInterestProfile(_ context.Context, p InterestProfile) error {
	if _, ok := m.profiles[p.Version]; ok {
		return errUniqueViolation // UNIQUE(version) — versions are immutable
	}
	p.Status = profileStatusOr(p.Status)
	if !isValidProfileStatus(p.Status) {
		return errCheckViolation
	}
	if p.Status == profileActive {
		// Mirror the partial unique index idx_interest_profile_active: at most one active row.
		for _, ex := range m.profiles {
			if ex.Status == profileActive {
				return errUniqueViolation
			}
		}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = m.nowFn()
	}
	m.profiles[p.Version] = p
	return nil
}

// --- Reads + claim (Phase 1) ----------------------------------------------
// These mirror the pgx implementations in store_reads.go: pure observation plus the one
// atomic claim. The claim simulates FOR UPDATE SKIP LOCKED by serving the lowest-insertion
// pending step and moving it to running, so a second claim never returns the same row.

// claimTime is the fixed heartbeat stamp the mock writes on claim (the real DB uses
// CURRENT_TIMESTAMP). Tests only assert it is non-nil.
var claimTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (m *MockDatabase) GetFlow(_ context.Context, name string) (Flow, bool, error) {
	f, ok := m.flows[name]
	return f, ok, nil
}

func (m *MockDatabase) GetItem(_ context.Context, id int) (Item, bool, error) {
	it, ok := m.itemByID[id]
	return it, ok, nil
}

func (m *MockDatabase) ListActiveItems(_ context.Context) ([]Item, error) {
	var out []Item
	for _, it := range m.items {
		switch it.Status {
		case itemDone, itemFiltered, itemFailed, itemQuarantine:
			// terminal — skip
		default:
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MockDatabase) ListFlowSteps(_ context.Context, flowID int) ([]FlowStep, error) {
	var out []FlowStep
	for k, s := range m.flowSteps {
		if k.flowID == flowID && s.Enabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *MockDatabase) ListItemSteps(_ context.Context, itemID int) ([]ItemStep, error) {
	var out []ItemStep
	for k, s := range m.itemSteps {
		if k.itemID == itemID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *MockDatabase) ListProvidersForCapability(_ context.Context, capability string) ([]Provider, error) {
	var out []Provider
	for _, p := range m.providers {
		if p.Capability == capability && p.Enabled {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MockDatabase) GetProvider(_ context.Context, name string) (Provider, bool, error) {
	p, ok := m.providers[name]
	return capProviderError(p), ok, nil
}

func (m *MockDatabase) GetRoutingPolicy(_ context.Context, scope string) (RoutingPolicy, bool, error) {
	p, ok := m.policies[scope]
	return p, ok, nil
}

func (m *MockDatabase) ListGateRules(_ context.Context) ([]GateRule, error) {
	var out []GateRule
	for _, r := range m.gateRules {
		if r.Enabled {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action < out[j].Action
		}
		if out[i].MatchType != out[j].MatchType {
			return out[i].MatchType < out[j].MatchType
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

func (m *MockDatabase) GetLatestInterestProfile(_ context.Context) (InterestProfile, bool, error) {
	best, found := InterestProfile{}, false
	for _, p := range m.profiles {
		if !found || p.Version > best.Version {
			best, found = p, true
		}
	}
	return best, found, nil
}

func (m *MockDatabase) GetActiveInterestProfile(_ context.Context) (InterestProfile, bool, error) {
	for _, p := range m.profiles {
		if p.Status == profileActive {
			return p, true, nil
		}
	}
	return InterestProfile{}, false, nil
}

func (m *MockDatabase) ListInterestProfiles(_ context.Context) ([]InterestProfile, error) {
	out := make([]InterestProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ActivateInterestProfile mirrors the pgx atomic swap: demote the current active, promote the
// target proposed version. A target that is absent or not proposed is rejected (and, since the
// mock applies the demote only after the check, nothing is mutated on rejection).
func (m *MockDatabase) ActivateInterestProfile(_ context.Context, version int) error {
	target, ok := m.profiles[version]
	if !ok || target.Status != profileProposed {
		return errProfileNotProposed
	}
	for v, p := range m.profiles {
		if p.Status == profileActive {
			p.Status = profileSuperseded
			m.profiles[v] = p
		}
	}
	target.Status = profileActive
	m.profiles[version] = target
	return nil
}

// LatestGateDecision returns the last-appended decision for (item, gate) — gate_decisions
// is append-only, so the highest-index match mirrors the SQL ORDER BY id DESC LIMIT 1.
func (m *MockDatabase) LatestGateDecision(_ context.Context, itemID int, gate string) (GateDecision, bool, error) {
	for i := len(m.gateDecisions) - 1; i >= 0; i-- {
		if d := m.gateDecisions[i]; d.ItemID == itemID && d.Gate == gate {
			return d, true, nil
		}
	}
	return GateDecision{}, false, nil
}

func (m *MockDatabase) ListQuarantinedItems(_ context.Context) ([]Item, error) {
	var out []Item
	for _, it := range m.items {
		if it.Status == itemQuarantine {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- Surface reads (Phase 5) ----------------------------------------------
// These mirror the pgx implementations in store_reads.go: pure observation + config-as-data.

func (m *MockDatabase) ListItemsByStatus(_ context.Context, status string) ([]Item, error) {
	var out []Item
	for _, it := range m.items {
		if it.Status == status {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MockDatabase) ListGateDecisions(_ context.Context, itemID int) ([]GateDecision, error) {
	var out []GateDecision
	for _, d := range m.gateDecisions { // append-only slice preserves insertion (id) order
		if d.ItemID == itemID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *MockDatabase) ListRecentDecisions(_ context.Context, limit int) ([]RecentDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	n := len(m.gateDecisions)
	out := make([]RecentDecision, 0, min(limit, n))
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		d := m.gateDecisions[i]
		out = append(out, RecentDecision{
			ID:       i + 1,
			ItemID:   d.ItemID,
			Gate:     d.Gate,
			Decision: d.Decision,
			Score:    d.Score,
			When:     "2026-01-01T00:00:00Z",
		})
	}
	return out, nil
}

func (m *MockDatabase) ListFlows(_ context.Context) ([]Flow, error) {
	var out []Flow
	for _, f := range m.flows {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MockDatabase) ListProviders(_ context.Context) ([]Provider, error) {
	var out []Provider
	for _, p := range m.providers {
		out = append(out, capProviderError(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MockDatabase) ListRoutingPolicies(_ context.Context) ([]RoutingPolicy, error) {
	var out []RoutingPolicy
	for _, p := range m.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, nil
}

func (m *MockDatabase) ListAllGateRules(_ context.Context) ([]GateRule, error) {
	var out []GateRule
	for _, r := range m.gateRules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action < out[j].Action
		}
		if out[i].MatchType != out[j].MatchType {
			return out[i].MatchType < out[j].MatchType
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

// TouchProviderHeartbeat stamps a real-clock heartbeat (consumed only by the router's
// real-clock health gate). An unknown name is a no-op, mirroring the pgx 0-row UPDATE.
func (m *MockDatabase) TouchProviderHeartbeat(_ context.Context, name string) error {
	if p, ok := m.providers[name]; ok {
		now := time.Now()
		p.HeartbeatAt = &now
		m.providers[name] = p
	}
	return nil
}

func (m *MockDatabase) ClaimPendingStep(_ context.Context, capability, provider string) (*ItemStep, error) {
	bestKey, bestOrder, found := itemStepKey{}, int(^uint(0)>>1), false
	for k, s := range m.itemSteps {
		if s.Capability == capability && s.AssignedProvider == provider && s.Status == stepPending {
			if o := m.stepOrder[k]; o < bestOrder { // lowest insertion order = FIFO by id
				bestKey, bestOrder, found = k, o, true
			}
		}
	}
	if !found {
		return nil, nil
	}
	s := m.itemSteps[bestKey]
	s.Status = stepRunning // pending -> running, atomically leaving the pending frontier
	s.Attempt++
	hb := claimTime
	s.HeartbeatAt = &hb
	m.itemSteps[bestKey] = s
	return &s, nil
}

func (m *MockDatabase) ListAssignedSteps(_ context.Context) ([]ItemStep, error) {
	var out []ItemStep
	for _, s := range m.itemSteps {
		if s.Status == stepPending && s.AssignedProvider != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ki := itemStepKey{out[i].ItemID, out[i].Seq}
		kj := itemStepKey{out[j].ItemID, out[j].Seq}
		return m.stepOrder[ki] < m.stepOrder[kj]
	})
	return out, nil
}

func (m *MockDatabase) ListRecentDistillations(_ context.Context, limit int) ([]DistillationSummary, error) {
	out := make([]DistillationSummary, 0)
	for i := len(m.distillations) - 1; i >= 0 && len(out) < limit; i-- {
		d := m.distillations[i]
		out = append(out, DistillationSummary{
			ID: d.ID, SourceType: d.SourceType, SourceRef: d.SourceRef,
			Title: d.Title, DocContext: d.DocContext,
			Engine: d.Engine, Status: d.Status, CreatedAt: d.CreatedAt,
		})
	}
	return out, nil
}

func (m *MockDatabase) GetDistillation(_ context.Context, id int) (Distillation, bool, error) {
	for _, d := range m.distillations {
		if d.ID == id {
			return d, true, nil
		}
	}
	return Distillation{}, false, nil
}

func (m *MockDatabase) RequeueSteps(_ context.Context, capability, fromStatus string, limit int, itemStatus string) (int, error) {
	if !isValidStepStatus(fromStatus) {
		return 0, errCheckViolation
	}
	if !isValidItemStatus(itemStatus) {
		return 0, errCheckViolation
	}
	// Collect matching steps in insertion order (mirrors ORDER BY id).
	type candidate struct {
		key   itemStepKey
		order int
	}
	var cands []candidate
	for k, s := range m.itemSteps {
		if s.Capability == capability && s.Status == fromStatus {
			cands = append(cands, candidate{k, m.stepOrder[k]})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].order < cands[j].order })
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	if len(cands) == 0 {
		return 0, nil
	}
	// Reset steps and collect affected item IDs (mirrors the SQL atomic unit).
	itemIDs := make(map[int]bool)
	for _, c := range cands {
		s := m.itemSteps[c.key]
		s.Status = stepPending
		s.Attempt = 0
		s.HeartbeatAt = nil
		s.AssignedProvider = ""
		s.Error = ""
		m.itemSteps[c.key] = s
		itemIDs[c.key.itemID] = true
	}
	for id := range itemIDs {
		it := m.itemByID[id]
		it.Status = itemStatus
		m.itemByID[id] = it
		m.items[itemKey(it.Lane, it.SourceRef)] = it
	}
	return len(cands), nil
}

func (m *MockDatabase) HealthPing(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (m *MockDatabase) UsageCounts(ctx context.Context) (UsageReport, error) {
	if err := ctx.Err(); err != nil {
		return UsageReport{}, err
	}
	var r UsageReport
	// items by (lane, status)
	counts := map[[2]string]int{}
	for _, it := range m.itemByID {
		counts[[2]string{it.Lane, it.Status}]++
	}
	r.Items = make([]ItemCount, 0)
	for k, c := range counts {
		r.Items = append(r.Items, ItemCount{Lane: k[0], Status: k[1], Count: c})
	}
	sort.Slice(r.Items, func(i, j int) bool {
		if r.Items[i].Lane != r.Items[j].Lane {
			return r.Items[i].Lane < r.Items[j].Lane
		}
		return r.Items[i].Status < r.Items[j].Status
	})
	for _, ic := range r.Items {
		if ic.Status == itemQuarantine {
			r.Quarantine += ic.Count
		}
	}
	// item_steps by (capability, status)
	stepCounts := map[[2]string]int{}
	for _, s := range m.itemSteps {
		stepCounts[[2]string{s.Capability, s.Status}]++
	}
	r.ItemSteps = make([]StepCount, 0)
	for k, c := range stepCounts {
		r.ItemSteps = append(r.ItemSteps, StepCount{Capability: k[0], Status: k[1], Count: c})
	}
	sort.Slice(r.ItemSteps, func(i, j int) bool {
		if r.ItemSteps[i].Capability != r.ItemSteps[j].Capability {
			return r.ItemSteps[i].Capability < r.ItemSteps[j].Capability
		}
		return r.ItemSteps[i].Status < r.ItemSteps[j].Status
	})
	r.Distillations = len(m.distillations)
	return r, nil
}

// WorkerMetrics mirrors the SQL rollup: groups item_steps by (assigned_provider, status),
// excludes unassigned rows, applies the since filter, and collapses to one WorkerMetric
// per provider sorted by name.
// Steps with nil UpdatedAt are excluded when since is set (treated as "before any window").
func (m *MockDatabase) WorkerMetrics(ctx context.Context, since *time.Time) ([]WorkerMetric, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byProvider := map[string]*WorkerMetric{}
	var order []string

	for _, s := range m.itemSteps {
		if s.AssignedProvider == "" {
			continue
		}
		if since != nil && (s.UpdatedAt == nil || s.UpdatedAt.Before(*since)) {
			continue
		}
		workerMetricAcc(byProvider, &order, s.AssignedProvider, s.Status, 1, s.UpdatedAt, float64(s.Attempt))
	}

	sort.Strings(order)
	out := make([]WorkerMetric, 0, len(order))
	for _, p := range order {
		wm := byProvider[p]
		workerMetricFinalize(wm)
		out = append(out, *wm)
	}
	return out, nil
}

// compile-time guarantee the mock satisfies the seam the pgx impl does.
var _ Database = (*MockDatabase)(nil)

// seedDistillation appends a distillation with the given id and content to the mock.
// CreatedAt is set to Unix(id, 0) so higher ids are always newer (deterministic ordering).
func seedDistillation(t *testing.T, db *MockDatabase, id int, content string) {
	t.Helper()
	src := "https://youtu.be/v" + strconv.Itoa(id)
	db.distillations = append(db.distillations, Distillation{
		DistillationSummary: DistillationSummary{
			ID: id, SourceType: "youtube", SourceRef: src,
			Engine: "gemini/gemini-2.5-flash", Status: "done",
			CreatedAt: time.Unix(int64(id), 0),
		},
		SourceKey: src, Pattern: "extract_wisdom",
		Content:          &content,
		Structured:       json.RawMessage(`{}`),
		StructuredStatus: "ok",
	})
}

// seedFlow inserts a flow and returns its id, for tests that need a valid FK target
// for flow_steps.flow_id / items.flow_id.
func seedFlow(t *testing.T, db *MockDatabase) int {
	t.Helper()
	fid, err := db.UpsertFlow(context.Background(), Flow{Name: "test_flow", SourceType: "news", Enabled: true, Version: 1})
	if err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	return fid
}

// ---------------------------------------------------------------------------
// Config-table contract
// ---------------------------------------------------------------------------

func TestCapabilityUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar, Description: "v1"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar, Description: "v2"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if len(db.capabilities) != 1 {
		t.Fatalf("UNIQUE(name) not honored: got %d rows", len(db.capabilities))
	}
	if got := db.capabilities[capDestilar].Description; got != "v2" {
		t.Errorf("upsert should replace: description = %q, want v2", got)
	}
}

func TestProviderRequiresCapability(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	p := Provider{Name: "caption-mac", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident}
	if err := db.UpsertProvider(ctx, p); !errors.Is(err, errFKViolation) {
		t.Fatalf("provider with missing capability should fail FK, got %v", err)
	}
	// Register the capability, then it succeeds.
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("provider upsert after capability exists: %v", err)
	}
}

func TestProviderRejectsBadEnum(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	bad := Provider{Name: "x", Capability: capTranscrever, Runtime: "gpu", Activation: activationResident}
	if err := db.UpsertProvider(ctx, bad); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid runtime should fail CHECK, got %v", err)
	}
}

func TestProviderUpsertDefaultsWorkerToName(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	p := Provider{Name: "caption-mac", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident}
	// Worker deliberately left empty — guard must default it to Name.
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := db.providers["caption-mac"].Worker; got != "caption-mac" {
		t.Errorf("Worker = %q, want %q", got, "caption-mac")
	}
}

func TestProviderUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capTranscrever}); err != nil {
		t.Fatalf("UpsertCapability: %v", err)
	}
	p := Provider{Name: "caption-mac", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident, Enabled: true}
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("first UpsertProvider: %v", err)
	}
	// Operator disables via UpsertProvider — the enabled toggle MUST be applied.
	p.Enabled = false
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("second UpsertProvider: %v", err)
	}
	if len(db.providers) != 1 {
		t.Fatalf("UNIQUE(name) not honored: %d rows", len(db.providers))
	}
	if db.providers["caption-mac"].Enabled {
		t.Errorf("operator UpsertProvider must apply the enabled toggle")
	}
}

// SeedProvider must not overwrite an operator's enabled toggle across re-seeds.
func TestSeedProviderPreservesEnabled(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capTranscrever}); err != nil {
		t.Fatalf("UpsertCapability: %v", err)
	}
	p := Provider{Name: "caption-mac", Capability: capTranscrever, Runtime: runtimeLocal, Activation: activationResident, Enabled: true, Description: "v1"}
	if err := db.SeedProvider(ctx, p); err != nil {
		t.Fatalf("first SeedProvider: %v", err)
	}
	// Operator disables via the console; then core-job seed runs again with Enabled: true and
	// an updated description — the toggle must be preserved, but the description must refresh.
	db.providers["caption-mac"] = func() Provider { pp := db.providers["caption-mac"]; pp.Enabled = false; return pp }()
	p.Enabled = true
	p.Description = "v2"
	if err := db.SeedProvider(ctx, p); err != nil {
		t.Fatalf("second SeedProvider: %v", err)
	}
	got := db.providers["caption-mac"]
	if got.Enabled {
		t.Errorf("re-seed must not overwrite the operator's enabled=false toggle")
	}
	if got.Description != "v2" {
		t.Errorf("re-seed must refresh non-enabled fields: description = %q, want v2", got.Description)
	}
}

func TestFlowUpsertReturnsStableID(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	id1, err := db.UpsertFlow(ctx, Flow{Name: "youtube_channels", SourceType: "youtube", Enabled: true, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.UpsertFlow(ctx, Flow{Name: "youtube_channels", SourceType: "youtube", Enabled: true, Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ON CONFLICT(name) must keep row id: %d != %d", id1, id2)
	}
	if len(db.flows) != 1 {
		t.Fatalf("UNIQUE(name) not honored: %d rows", len(db.flows))
	}
	if db.flows["youtube_channels"].Version != 2 {
		t.Errorf("version bump should persist")
	}
}

func TestFlowStepRequiresCapabilityAndUniqueSeq(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid, _ := db.UpsertFlow(ctx, Flow{Name: "news", SourceType: "news"})
	// Missing capability -> FK.
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capColetar}); !errors.Is(err, errFKViolation) {
		t.Fatalf("flow_step with missing capability should fail FK, got %v", err)
	}
	_ = db.UpsertCapability(ctx, Capability{Name: capColetar})
	_ = db.UpsertCapability(ctx, Capability{Name: capExtrair})
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capColetar}); err != nil {
		t.Fatal(err)
	}
	// Same (flow_id, seq) replaces, not duplicates.
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: fid, Seq: 1, Capability: capExtrair}); err != nil {
		t.Fatal(err)
	}
	if len(db.flowSteps) != 1 {
		t.Fatalf("UNIQUE(flow_id, seq) not honored: %d rows", len(db.flowSteps))
	}
	if db.flowSteps[flowStepKey{fid, 1}].Capability != capExtrair {
		t.Errorf("upsert should replace the step capability")
	}
}

func TestRoutingPolicyUniqueScope(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: "global", Fallback: []byte(`["a"]`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRoutingPolicy(ctx, RoutingPolicy{Scope: "global", Fallback: []byte(`["b"]`)}); err != nil {
		t.Fatal(err)
	}
	if len(db.policies) != 1 {
		t.Fatalf("UNIQUE(scope) not honored: %d rows", len(db.policies))
	}
	if string(db.policies["global"].Fallback) != `["b"]` {
		t.Errorf("upsert should replace policy fallback, got %s", db.policies["global"].Fallback)
	}
}

// ---------------------------------------------------------------------------
// Spine contract
// ---------------------------------------------------------------------------

func TestItemDedupByLaneSourceRef(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	id1, err := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	if err != nil {
		t.Fatal(err)
	}
	// Same natural key re-discovered: collapses to one row, id stable.
	id2, err := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemToText})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ON CONFLICT(lane, source_ref) must keep row id: %d != %d", id1, id2)
	}
	if len(db.items) != 1 {
		t.Fatalf("UNIQUE(lane, source_ref) not honored: %d rows", len(db.items))
	}
	// Same source_ref in a DIFFERENT lane is a distinct item.
	if _, err := db.UpsertItem(ctx, Item{Lane: "podcast", SourceRef: "vid123", FlowID: fid, FlowVersion: 1, Status: itemDiscovered}); err != nil {
		t.Fatal(err)
	}
	if len(db.items) != 2 {
		t.Fatalf("composite key should distinguish lanes: %d rows", len(db.items))
	}
}

func TestItemRejectsBadStatus(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	if _, err := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: "queued"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid item status should fail CHECK, got %v", err)
	}
}

func TestItemRequiresExistingFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, err := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: 999, FlowVersion: 1, Status: itemDiscovered}); !errors.Is(err, errFKViolation) {
		t.Fatalf("item on unknown flow should fail FK, got %v", err)
	}
}

func TestFlowStepRequiresExistingFlow(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capColetar})
	if err := db.UpsertFlowStep(ctx, FlowStep{FlowID: 999, Seq: 1, Capability: capColetar}); !errors.Is(err, errFKViolation) {
		t.Fatalf("flow_step on unknown flow should fail FK, got %v", err)
	}
}

func TestItemStepUniquePerItemSeqAndFKs(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})

	// FK: unknown item.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: 9999, Seq: 1, Capability: capTranscrever, Status: stepPending}); !errors.Is(err, errFKViolation) {
		t.Fatalf("item_step on unknown item should fail FK, got %v", err)
	}
	// FK: assigned_provider must exist when set.
	bad := ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepAssigned, AssignedProvider: "ghost"}
	if err := db.UpsertItemStep(ctx, bad); !errors.Is(err, errFKViolation) {
		t.Fatalf("item_step with unknown provider should fail FK, got %v", err)
	}
	// Happy path: pending step, no provider yet.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepPending}); err != nil {
		t.Fatal(err)
	}
	// Mutable in place: same (item_id, seq) advances status & bumps attempt (the
	// claim/retry pattern), one row.
	if err := db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepRunning, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if len(db.itemSteps) != 1 {
		t.Fatalf("UNIQUE(item_id, seq) not honored: %d rows", len(db.itemSteps))
	}
	got := db.itemSteps[itemStepKey{itemID, 1}]
	if got.Status != stepRunning || got.Attempt != 1 {
		t.Errorf("upsert should advance step in place: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Curation + learning contract (append-only / versioned)
// ---------------------------------------------------------------------------

func TestGateDecisionsAppendOnly(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	// Two runs of the same gate accumulate — history is the point (calibration sample).
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "rules"}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "llm-judge"}); err != nil {
		t.Fatal(err)
	}
	if len(db.gateDecisions) != 2 {
		t.Fatalf("gate_decisions must be append-only: %d rows", len(db.gateDecisions))
	}
	// Bad enum rejected.
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: "gate_medio", Decision: decisionKeep, DecidedBy: "x"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid gate should fail CHECK, got %v", err)
	}
}

// gate_rico carries both a confidence score (0..1) and an integer rank (ordering);
// the two are distinct columns so a rank position can exceed the score's [0,1] range.
func TestGateDecisionRichScoreAndRank(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "u", FlowID: fid, FlowVersion: 1, Status: itemToText})
	score := 0.82
	rank := 3
	if err := db.InsertGateDecision(ctx, GateDecision{
		ItemID: itemID, Gate: gateRico, Decision: decisionKeep,
		Score: &score, Rank: &rank, DecidedBy: "llm-judge",
	}); err != nil {
		t.Fatalf("rich gate decision with score+rank: %v", err)
	}
	got := db.gateDecisions[len(db.gateDecisions)-1]
	if got.Score == nil || *got.Score != score || got.Rank == nil || *got.Rank != rank {
		t.Errorf("score/rank not preserved: %+v", got)
	}
}

func TestFeedbackTargetTypeChecked(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetDistillation, TargetRef: "42", Signal: "up", Source: sourceUserExplicit}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFeedback(ctx, Feedback{TargetType: "transcript", TargetRef: "1", Signal: "up", Source: sourceUserExplicit}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid target_type should fail CHECK, got %v", err)
	}
	if len(db.feedback) != 1 {
		t.Fatalf("only the valid row should persist: %d rows", len(db.feedback))
	}
}

// feedback.source is pinned to a CHECK enum (migration 005): the three known sources are
// accepted (including the new kura_implicit), an unknown one is rejected.
func TestFeedbackSourceChecked(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	for _, src := range []string{sourceUserExplicit, sourceQuarantineReview, sourceKURAImplicit} {
		if err := db.InsertFeedback(ctx, Feedback{TargetType: targetDistillation, TargetRef: "1", Signal: signalUp, Source: src}); err != nil {
			t.Errorf("source %q should be accepted: %v", src, err)
		}
	}
	if err := db.InsertFeedback(ctx, Feedback{TargetType: targetDistillation, TargetRef: "1", Signal: signalUp, Source: "kura-usage"}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("unknown source should fail CHECK, got %v", err)
	}
	if !isValidFeedbackSource(sourceKURAImplicit) || isValidFeedbackSource("explicit") {
		t.Error("isValidFeedbackSource enum mismatch")
	}
}

func TestGateRuleUpsertIdempotentAndChecked(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	// Bad enums rejected.
	if err := db.UpsertGateRule(ctx, GateRule{Action: "block", MatchType: matchChannel, Value: "x", Enabled: true}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid action should fail CHECK, got %v", err)
	}
	if err := db.UpsertGateRule(ctx, GateRule{Action: ruleDeny, MatchType: "regex", Value: "x", Enabled: true}); !errors.Is(err, errCheckViolation) {
		t.Fatalf("invalid match_type should fail CHECK, got %v", err)
	}
	// Same (action, match_type, value) upserts in place (toggle enabled).
	r := GateRule{Action: ruleDeny, MatchType: matchTitleContains, Value: "drama", Enabled: true}
	if err := db.UpsertGateRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Enabled = false
	if err := db.UpsertGateRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(db.gateRules) != 1 {
		t.Fatalf("UNIQUE(action, match_type, value) not honored: %d rows", len(db.gateRules))
	}
	// A disabled rule is filtered out of the cascade read.
	rules, _ := db.ListGateRules(ctx)
	if len(rules) != 0 {
		t.Errorf("ListGateRules should return only enabled rules, got %d", len(rules))
	}
}

func TestGetLatestInterestProfile(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if _, ok, _ := db.GetLatestInterestProfile(ctx); ok {
		t.Error("no profile seeded yet -> found=false")
	}
	_ = db.InsertInterestProfile(ctx, InterestProfile{Version: 1})
	_ = db.InsertInterestProfile(ctx, InterestProfile{Version: 3})
	_ = db.InsertInterestProfile(ctx, InterestProfile{Version: 2})
	p, ok, _ := db.GetLatestInterestProfile(ctx)
	if !ok || p.Version != 3 {
		t.Errorf("latest profile = v%d (found=%v), want v3", p.Version, ok)
	}
}

func TestLatestGateDecision(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	if _, ok, _ := db.LatestGateDecision(ctx, itemID, gateBarato); ok {
		t.Error("no decision yet -> found=false")
	}
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionDefer, DecidedBy: "profile"})
	_ = db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "llm"})
	got, ok, _ := db.LatestGateDecision(ctx, itemID, gateBarato)
	if !ok || got.Decision != decisionKeep || got.DecidedBy != "llm" {
		t.Errorf("latest decision = %+v, want the keep (last appended)", got)
	}
	// A different gate is independent.
	if _, ok, _ := db.LatestGateDecision(ctx, itemID, gateRico); ok {
		t.Error("gate_rico has no decision -> found=false")
	}
}

func TestInterestProfileVersionImmutable(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	// Re-inserting the same version is rejected — revisions create a NEW version.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1}); !errors.Is(err, errUniqueViolation) {
		t.Fatalf("UNIQUE(version) should reject duplicate, got %v", err)
	}
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2}); err != nil {
		t.Fatal(err)
	}
	if len(db.profiles) != 2 {
		t.Fatalf("each version is a distinct immutable row: %d rows", len(db.profiles))
	}
}

// ---------------------------------------------------------------------------
// proposed-vs-active invariants at the seam — the half of the interest_profile lifecycle the
// CONTROL plane owns: the at-most-one-active partial index and the human APPROVAL swap
// (ActivateInterestProfile). The reviser that PROPOSES versions now lives in rara-hone; approval
// stays here, behind the surface.
// ---------------------------------------------------------------------------

func TestInterestProfileOneActiveInvariant(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 1, Status: profileActive}); err != nil {
		t.Fatal(err)
	}
	// A second active row is rejected (mirrors the partial unique index).
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileActive}); err == nil {
		t.Error("a second active interest_profile should be rejected")
	}
	// A proposed row is fine.
	if err := db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileProposed}); err != nil {
		t.Fatal(err)
	}
}

func TestListAssignedSteps(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()

	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	_ = db.UpsertCapability(ctx, Capability{Name: capDestilar})
	if err := db.UpsertProvider(ctx, Provider{Name: "p1", Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertProvider(ctx, Provider{Name: "p2", Capability: capTranscrever, Runtime: runtimeVPC, Activation: activationResident, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	flowID, _ := db.UpsertFlow(ctx, Flow{Name: "test", SourceType: "youtube", Enabled: true})
	itemID, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v1", FlowID: flowID, Status: itemDiscovered})

	// Assigned step: pending + has assigned_provider.
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 1, Capability: capTranscrever, Status: stepPending, AssignedProvider: "p1"})
	// Unassigned step: pending but no provider.
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 2, Capability: capDestilar, Status: stepPending})
	// Running step: already claimed — not a dispatch target.
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: itemID, Seq: 3, Capability: capTranscrever, Status: stepRunning, AssignedProvider: "p2"})

	got, err := db.ListAssignedSteps(ctx)
	if err != nil {
		t.Fatalf("ListAssignedSteps: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 assigned step, got %d", len(got))
	}
	if got[0].AssignedProvider != "p1" {
		t.Errorf("want provider p1, got %q", got[0].AssignedProvider)
	}
}

func TestListAssignedStepsInsertionOrder(t *testing.T) {
	// The real DB uses ORDER BY id (FIFO). The mock must mirror that, not sort by (ItemID, Seq).
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	if err := db.UpsertProvider(ctx, Provider{Name: "p1", Capability: capTranscrever, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	flowID, _ := db.UpsertFlow(ctx, Flow{Name: "test", SourceType: "youtube", Enabled: true})
	id1, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v1", FlowID: flowID, Status: itemDiscovered})
	id2, _ := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "v2", FlowID: flowID, Status: itemDiscovered})

	// Insert step for id2 (higher ItemID) FIRST → insertion order 1.
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: id2, Seq: 1, Capability: capTranscrever, Status: stepPending, AssignedProvider: "p1"})
	// Insert step for id1 (lower ItemID) SECOND → insertion order 2.
	_ = db.UpsertItemStep(ctx, ItemStep{ItemID: id1, Seq: 1, Capability: capTranscrever, Status: stepPending, AssignedProvider: "p1"})

	got, err := db.ListAssignedSteps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 steps, got %d", len(got))
	}
	// id2's step was inserted first → must be got[0] (FIFO).
	if got[0].ItemID != id2 {
		t.Errorf("ListAssignedSteps[0].ItemID = %d, want %d (insertion order, not ItemID sort)", got[0].ItemID, id2)
	}
}

func TestProviderRunnerURL(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capTranscrever})
	p := Provider{
		Name: "vpc-scribe", Capability: capTranscrever,
		Runtime: runtimeVPC, Activation: activationResident,
		Enabled:   true,
		RunnerURL: "http://100.64.1.1:7800",
	}
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got, ok, err := db.GetProvider(ctx, "vpc-scribe")
	if err != nil || !ok {
		t.Fatalf("GetProvider: ok=%v err=%v", ok, err)
	}
	if got.RunnerURL != "http://100.64.1.1:7800" {
		t.Errorf("RunnerURL not round-tripped, got %q", got.RunnerURL)
	}
}

func TestProviderEnv(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.UpsertCapability(ctx, Capability{Name: capDestilar})
	// env carries per-run NON-secret config the worker reads (DISTILL_PROVIDER) plus an unknown
	// key the editor must not drop — the round-trip preserves bytes verbatim.
	p := Provider{
		Name: "distill", Capability: capDestilar,
		Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Enabled: true,
		Env:     []byte(`{"DISTILL_PROVIDER":"distill","FUTURE_KNOB":"x"}`),
	}
	if err := db.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got, ok, err := db.GetProvider(ctx, "distill")
	if err != nil || !ok {
		t.Fatalf("GetProvider: ok=%v err=%v", ok, err)
	}
	if string(got.Env) != `{"DISTILL_PROVIDER":"distill","FUTURE_KNOB":"x"}` {
		t.Errorf("Env not round-tripped (unknown keys must survive), got %q", got.Env)
	}
}

// TestProviderEnvMustBeObject mirrors the DB CHECK (jsonb_typeof(env)='object'): env is injected
// key=value when waking a worker, so a non-object (array/scalar/null) would break the dispatch.
// Empty env is allowed — it defaults to '{}'.
func TestProviderEnvMustBeObject(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := db.UpsertCapability(ctx, Capability{Name: capDestilar}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	base := Provider{Name: "p", Capability: capDestilar, Runtime: runtimeCloudRun, Activation: activationOnDemand, Enabled: true}
	for _, bad := range []string{`["a"]`, `"x"`, `42`, `null`, `{"K":1}`, `{"K":true}`, `{"K":null}`, `{"K":{"nested":"value"}}`, `{"K":["a","b"]}`, `{"A":"v","B":1}`} {
		p := base
		p.Env = []byte(bad)
		if err := db.UpsertProvider(ctx, p); !errors.Is(err, errCheckViolation) {
			t.Errorf("env %s: want errCheckViolation, got %v", bad, err)
		}
	}
	for _, ok := range []string{``, `{}`, `{"K":"v"}`} {
		p := base
		p.Env = []byte(ok)
		if err := db.UpsertProvider(ctx, p); err != nil {
			t.Errorf("env %q: want accepted, got %v", ok, err)
		}
	}
}

func TestActivateInterestProfileSwap(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	_ = db.InsertInterestProfile(ctx, InterestProfile{Version: 1, Status: profileActive})
	_ = db.InsertInterestProfile(ctx, InterestProfile{Version: 2, Status: profileProposed})

	if err := db.ActivateInterestProfile(ctx, 2); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	if db.profiles[1].Status != profileSuperseded {
		t.Errorf("v1 should be superseded, got %q", db.profiles[1].Status)
	}
	if db.profiles[2].Status != profileActive {
		t.Errorf("v2 should be active, got %q", db.profiles[2].Status)
	}
	// Activating a non-proposed version is rejected, and nothing changes.
	if err := db.ActivateInterestProfile(ctx, 1); err == nil {
		t.Error("activating a superseded version should error")
	}
	if db.profiles[2].Status != profileActive {
		t.Error("a rejected activation must not mutate the current active")
	}
}

func TestSeedYouTubeLaneCaptionAppIsTranscribe(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatalf("SeedYouTubeLane: %v", err)
	}
	p, ok, err := db.GetProvider(ctx, provASRYouTube)
	if err != nil || !ok {
		t.Fatalf("GetProvider(%q): ok=%v err=%v", provASRYouTube, ok, err)
	}
	if p.App != "transcribe" {
		t.Errorf("caption-mac App = %q, want %q", p.App, "transcribe")
	}
}

func TestSeedPodcastLaneEchoAppIsTranscribe(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedPodcastLane(ctx, db); err != nil {
		t.Fatalf("SeedPodcastLane: %v", err)
	}
	p, ok, err := db.GetProvider(ctx, provASRDirectAudio)
	if err != nil || !ok {
		t.Fatalf("GetProvider(%q): ok=%v err=%v", provASRDirectAudio, ok, err)
	}
	if p.App != "transcribe" {
		t.Errorf("echo-cloud App = %q, want %q", p.App, "transcribe")
	}
}
