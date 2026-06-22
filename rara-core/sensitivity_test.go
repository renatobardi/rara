package main

import (
	"context"
	"testing"
)

// TestSeedSharedProvidersSensitivityTags: the cloud LLM providers carry the third_party tag and
// each has an untagged self-host (VPC) sibling — the matrix sensitivity routing relies on.
func TestSeedSharedProvidersSensitivityTags(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedYouTubeLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{provDistill, provGateBarato, provGateRico} {
		if got := string(db.providers[name].Constraints); got != `{"sensitivity":"third_party"}` {
			t.Errorf("%s constraints = %q, want third_party tag", name, got)
		}
	}
	for _, name := range []string{provDistillLocal, provGateBaratoLocal, provGateRicoLocal} {
		p, ok := db.providers[name]
		if !ok {
			t.Errorf("self-host provider %q not seeded", name)
			continue
		}
		if len(p.Constraints) != 0 {
			t.Errorf("%s should be untagged (eligible for private), got %q", name, string(p.Constraints))
		}
		if p.Runtime != runtimeVPC {
			t.Errorf("%s runtime = %q, want vpc (self-host)", name, p.Runtime)
		}
	}
}

// TestReconcileEmailRoutesSelfHost is the slice (c) payoff: a PRIVATE email item is kept off
// the third-party LLM providers — every LLM step (gate_barato, gate_rico, destilar) routes to
// the self-host variant. For public items, the self-host variant is also chosen first (VPC-first
// policy), but for different reasons: fallback ordering, not sensitivity exclusion. The extrair
// step (deterministic, no third-party model) routes to its provider regardless of sensitivity.
func TestReconcileEmailRoutesSelfHost(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedEmailLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Enable the lane so ingest runs (email ships disabled).
	ef := db.flows[emailFlowName]
	ef.Enabled = true
	if _, err := db.UpsertFlow(ctx, ef); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestEmail(ctx, db, fakeEmailSource{emails: []EmailItem{{MessageID: "msg1"}}}); err != nil {
		t.Fatal(err)
	}
	itemID := db.items[itemKey(laneEmail, "msg1")].ID
	r := NewReconciler(db)

	// gate_barato: third-party excluded for a private item -> self-host.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := db.itemSteps[itemStepKey{itemID, 2}]; s.AssignedProvider != provGateBaratoLocal {
		t.Errorf("gate_barato provider = %q, want %s (third-party excluded for private)", s.AssignedProvider, provGateBaratoLocal)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)

	// extrair: VPC-first routing picks winnow-vpc (no third-party sensitivity tag — eligible for
	// private content; VPC-first fallback ordering puts it before winnow-cloud).
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := db.itemSteps[itemStepKey{itemID, 3}]; s.AssignedProvider != provWinnowLocal {
		t.Errorf("extrair provider = %q, want %s (VPC-first routing)", s.AssignedProvider, provWinnowLocal)
	}
	completeStep(t, db, itemID, 3, "transcript-email-1")

	// gate_rico: self-host again.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := db.itemSteps[itemStepKey{itemID, 4}]; s.AssignedProvider != provGateRicoLocal {
		t.Errorf("gate_rico provider = %q, want %s", s.AssignedProvider, provGateRicoLocal)
	}
	runGate(t, db, itemID, 4, gateRico, decisionKeep)

	// destilar: self-host.
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := db.itemSteps[itemStepKey{itemID, 5}]; s.AssignedProvider != provDistillLocal {
		t.Errorf("destilar provider = %q, want %s (private content stays on self-host)", s.AssignedProvider, provDistillLocal)
	}
}

// TestReconcilePublicPrefersVPCFirst: a PUBLIC item (youtube) routes its LLM steps to the
// VPC-resident providers first (sift-vpc, assay-vpc, distill-vpc). The
// per-capability routing_policies pin the local-before-cloud fallback order, overriding the
// cost/quality score that would otherwise prefer the cloud variants.
func TestReconcilePublicPrefersVPCFirst(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	itemID := seedAndIngestOne(t, db, "vid1") // youtube, public
	r := NewReconciler(db)

	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := db.itemSteps[itemStepKey{itemID, 2}]; s.AssignedProvider != provGateBaratoLocal {
		t.Errorf("public gate_barato = %q, want the VPC-first %s", s.AssignedProvider, provGateBaratoLocal)
	}
}
