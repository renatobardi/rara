// linkedin_collect.go — Phase 6 PIECE 1: the Bright Data LinkedIn collector.
//
// Phase 5 shipped the LinkedIn lane with a MANUAL inbox collector (a person pastes a post's
// URL + text through the surface). The architecture always called that a stand-in: "manual-inbox
// provider now; Bright Data later" — "swap collector behind the same contract, flow unchanged".
//
// This is that swap. The Bright Data collector writes the SAME linkedin_posts domain table
// behind the SAME contract the manual inbox uses — SubmitLinkedInPost (UpsertLinkedInPost +
// DiscoverItem lane=linkedin, sensitivity=public). Nothing downstream changes: the flow, the
// extrair-linkedin worker, gate_barato/gate_rico and distill never know which collector filled
// the table. The manual inbox STAYS as a fallback (a person can still paste a post the crawl
// missed); both providers write the one table.
//
// CollectLinkedIn is PURE orchestration over the Database seam + the LinkedInPostStore seam +
// the LinkedInCollector seam (zero I/O of its own), so the whole collect lane is unit-tested
// with the MockDatabase and a fake collector. The Bright Data fetch — the only real I/O — lives
// behind LinkedInCollector at the I/O edge (brightDataLinkedInSource, runners.go), exactly like
// every other lane's collector/source.
package main

import (
	"context"
	"log"
	"strings"
)

// LinkedInCollector is the fetch seam for an AUTOMATED LinkedIn collector: it yields the posts
// to ingest. The Bright Data implementation (brightDataLinkedInSource) is the production source;
// tests inject a fake so the collect orchestration is verified with zero network I/O. It is the
// read side of the lane, mirroring the other lanes' ingest sources (pgxSpineSource,
// pgxPodcastSource, pgxEmailSource) — the difference is only that LinkedIn's source is an
// external API (Bright Data) rather than a sibling agent's domain table.
type LinkedInCollector interface {
	FetchPosts(ctx context.Context) ([]LinkedInPost, error)
}

// CollectLinkedIn runs the automated collector: fetch the current batch of posts and feed EACH
// through the exact same contract the manual inbox uses (SubmitLinkedInPost) — so the domain row
// and the spine item are written identically, and re-collecting converges (idempotent on the
// post URL). Returns the number of posts ingested.
//
// Batch resilience: a post the source yields with no URL or no real text is SKIPPED (Bright Data
// occasionally returns partial rows), not fatal — one junk row must not abort the whole crawl.
// A genuine SubmitLinkedInPost failure (e.g. the flow is not seeded, or a database error) IS
// propagated: it is a real fault, not a per-post data quirk.
func CollectLinkedIn(ctx context.Context, db Database, store LinkedInPostStore, collector LinkedInCollector) (int, error) {
	posts, err := collector.FetchPosts(ctx)
	if err != nil {
		return 0, err
	}
	collected := 0
	for _, p := range posts {
		// Pre-filter the partial rows the source can yield, so a SubmitLinkedInPost error below
		// is always a real fault (not an empty paste) and is worth aborting on.
		if strings.TrimSpace(p.URL) == "" || !postHasContent(p.Text) {
			log.Printf("collect linkedin: skipping partial post (url=%q, empty text=%v)", p.URL, !postHasContent(p.Text))
			continue
		}
		if _, err := SubmitLinkedInPost(ctx, db, store, p); err != nil {
			return collected, err
		}
		collected++
	}
	return collected, nil
}
