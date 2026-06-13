// linkedin.go — Phase 5 deliverable #3: the LinkedIn lane (manual-inbox collector).
//
// LinkedIn is a "nice-to-have" lane the architecture folds into the same template as every
// other lane (ARCHITECTURE-2.0, "Source lanes"): coletar -> gate_barato -> extrair ->
// gate_rico -> destilar. Only two things are LinkedIn-specific: (1) the collector is a MANUAL
// inbox — a person pastes a post's URL + text through the surface (MCP tool / HTTP endpoint),
// instead of an automated crawl; and (2) the to-text step is `extrair` (the post is already
// text), pinned to the lane with constraints={"accepts":["linkedin"]}.
//
// The collector is deliberately swappable: SubmitLinkedInPost writes the post to the
// linkedin_posts domain table behind the LinkedInPostStore seam and discovers the spine item.
// A Bright Data collector (Phase 6) writes the SAME table behind the SAME contract, so the
// flow, the extractor and the gates never change — only who fills linkedin_posts.
//
// Everything here is PURE orchestration over the Database seam + the store seam (zero I/O of
// its own) plus the deterministic cleanPostText, so the whole lane is unit-tested with the
// MockDatabase and a fake store. The pgx implementations (the store write + the extract
// runner) live at the I/O edge in runners.go, like every other lane's.
package main

import (
	"context"
	"fmt"
	"strings"
)

// Lane + flow names and provider names for the LinkedIn lane (mirroring the other lanes).
const (
	laneLinkedIn      = "linkedin"
	linkedinFlowName  = "linkedin"
	provManualInbox   = "manual-inbox"     // coletar — manual post submission (swappable for Bright Data)
	provExtrairLinked = "extrair-linkedin" // extrair — LinkedIn post normalizer (accepts linkedin)
)

// LinkedInPost is one manually-submitted post: its canonical URL (the spine's natural key)
// and the post text, with the author optional (carried as the gate's "channel" signal).
type LinkedInPost struct {
	URL    string
	Author string
	Text   string
}

// LinkedInPostStore is the domain-write seam for the manual-inbox collector — and, later, for
// the Bright Data collector behind the same contract. UpsertLinkedInPost is idempotent on the
// URL (a resubmission refreshes in place). Mocked in tests; the pgx implementation
// (pgxLinkedInInbox, runners.go) writes the linkedin_posts table.
type LinkedInPostStore interface {
	UpsertLinkedInPost(ctx context.Context, p LinkedInPost) error
}

// SubmitLinkedInPost is the manual-inbox collector: it cleans the pasted post, upserts it into
// the linkedin_posts domain table (so the gate/extractor can read it), and discovers the spine
// item (lane=linkedin, source_ref=url, sensitivity=public). Idempotent on the URL — a
// resubmission refreshes the post and collapses onto the existing item. Returns the item id.
//
// Order matters: the domain row is written BEFORE the item is discovered, so by the time the
// reconciler assigns gate_barato (a later pass) the metadata it reads already exists.
func SubmitLinkedInPost(ctx context.Context, db Database, store LinkedInPostStore, p LinkedInPost) (int, error) {
	url := strings.TrimSpace(p.URL)
	if url == "" {
		return 0, fmt.Errorf("linkedin: url is required")
	}
	// Validate there is real content (reject a pure-whitespace/empty paste at the door), but
	// store the RAW post: the to-text cleaning is the `extrair` step's job, exactly as the email
	// lane stores raw bodies in `emails` and the Bright Data swap will store raw HTML here.
	if cleanPostText(p.Text) == "" {
		return 0, fmt.Errorf("linkedin: post text is empty")
	}
	flow, found, err := db.GetFlow(ctx, linkedinFlowName)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("linkedin: flow %q not seeded (run SeedLinkedInLane first)", linkedinFlowName)
	}
	if err := store.UpsertLinkedInPost(ctx, LinkedInPost{
		URL: url, Author: strings.TrimSpace(p.Author), Text: strings.TrimSpace(p.Text),
	}); err != nil {
		return 0, err
	}
	// Public content: LinkedIn posts are world-readable, so (unlike email) third-party models
	// may process them — the default sensitivity, stated explicitly for the reader.
	return db.DiscoverItem(ctx, Item{
		Lane:        laneLinkedIn,
		SourceRef:   url,
		FlowID:      flow.ID,
		FlowVersion: flow.Version,
		Status:      itemDiscovered,
		Sensitivity: sensitivityPublic,
	})
}

// cleanPostText normalizes a pasted LinkedIn post into the text the gates/distill judge. Posts
// are already text, so this is lighter than email's cleaner: strip HTML if the body carries any
// (so a future Bright Data collector that yields HTML needs no change), then collapse blank
// runs and trim. It does NOT strip signatures/quotes (a post has neither). Deterministic and
// cheap — the real "to-text" work the lane does.
func cleanPostText(raw string) string {
	s := raw
	if reHTMLish.MatchString(s) {
		s = stripHTML(s)
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		out = append(out, strings.TrimRight(ln, " \t\r"))
	}
	cleaned := reBlankRun.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
	return strings.TrimSpace(cleaned)
}

// SeedLinkedInLane writes the LinkedIn lane: shared capabilities/providers/config plus the
// manual-inbox collector and the LinkedIn extractor (extrair-linkedin), and the linkedin flow.
// The lane template matches email's — `extrair` is the to-text step (the post is already text)
// — and only the collector and the extractor's `accepts` differ. Idempotent: safe on every boot.
func SeedLinkedInLane(ctx context.Context, db Database) error {
	if err := seedCapabilities(ctx, db); err != nil {
		return err
	}
	if err := seedSharedProviders(ctx, db); err != nil {
		return err
	}
	// coletar: the manual inbox. Like the other collectors it is never actually routed
	// (coletar is auto-satisfied by the reconciler — the item already exists once submitted);
	// the row is seeded for completeness and the future Bright Data swap behind the same name.
	if err := db.UpsertProvider(ctx, Provider{
		Name: provManualInbox, Capability: capColetar, Runtime: runtimeVPC, Activation: activationResident,
		Cost: 0.10, Quality: 0.95, LatencyMs: 100,
		Constraints: []byte(`{"accepts":["linkedin"]}`), Enabled: true,
	}); err != nil {
		return err
	}
	// extrair: deterministic post normalization — any runtime, accepts only linkedin (so it
	// never competes with the email extractor, and the email extractor never grabs a post).
	if err := db.UpsertProvider(ctx, Provider{
		Name: provExtrairLinked, Capability: capExtrair, Runtime: runtimeCloudRun, Activation: activationOnDemand,
		Cost: 0.20, Quality: 0.85, LatencyMs: 1000,
		Constraints: []byte(`{"accepts":["linkedin"]}`), Enabled: true,
	}); err != nil {
		return err
	}
	if err := seedLaneFlow(ctx, db, linkedinFlowName, laneLinkedIn,
		[]string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}); err != nil {
		return err
	}
	return seedSharedConfig(ctx, db)
}
