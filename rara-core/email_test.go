package main

import (
	"context"
	"testing"
)

// fakeEmailSource is a fixed list of collected emails — the read side of email ingest, mocked.
type fakeEmailSource struct {
	emails []EmailItem
	err    error
}

func (f fakeEmailSource) Emails(_ context.Context) ([]EmailItem, error) {
	return f.emails, f.err
}

// The email body cleaning (cleanEmailText) is no longer in rara-core — extrair is its own app,
// rara-glean, where the cleaner and its unit tests now live. The tests below cover what the core
// still owns of the email lane: the seed, ingest, and the reconciler routing extrair to its provider.

// ---------------------------------------------------------------------------
// Seed + ingest.
// ---------------------------------------------------------------------------

// TestSeedEmailLane: the extrair-email provider on `extrair` (accepts email) and the email
// flow that swaps transcrever for extrair.
func TestSeedEmailLane(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedEmailLane(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p, ok := db.providers[provExtrairEmail]
	if !ok {
		t.Fatalf("provider %q not seeded", provExtrairEmail)
	}
	if p.Capability != capExtrair || p.Runtime != runtimeCloudRun {
		t.Errorf("extrair-email = {%s,%s}, want {extrair,cloudrun}", p.Capability, p.Runtime)
	}
	if got := string(p.Constraints); got != `{"accepts":["email"]}` {
		t.Errorf("extrair-email constraints = %q, want accepts=[email]", got)
	}
	f, ok := db.flows[emailFlowName]
	if !ok || f.SourceType != laneEmail {
		t.Fatalf("email flow = %+v, want email source_type", f)
	}
	steps, _ := db.ListFlowSteps(ctx, f.ID)
	wantSeq := []string{capColetar, capGateBarato, capExtrair, capGateRico, capDestilar}
	if len(steps) != len(wantSeq) {
		t.Fatalf("got %d email flow steps, want %d", len(steps), len(wantSeq))
	}
	for i, s := range steps {
		if s.Capability != wantSeq[i] {
			t.Errorf("step %d = %s, want %s (extrair in place of transcrever)", i+1, s.Capability, wantSeq[i])
		}
	}
}

// TestIngestEmail: emails become items (lane=email, source_ref=message_id, PRIVATE), idempotent.
func TestIngestEmail(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedEmailLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	src := fakeEmailSource{emails: []EmailItem{
		{MessageID: "msg1", Subject: "One"},
		{MessageID: ""}, // malformed -> skipped
		{MessageID: "msg1"},
		{MessageID: "msg2", Subject: "Two"},
	}}
	n, err := IngestEmail(ctx, db, src)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 3 {
		t.Errorf("processed %d, want 3 (empty skipped)", n)
	}
	if len(db.items) != 2 {
		t.Errorf("dedup failed: %d items, want 2", len(db.items))
	}
	it := db.items[itemKey(laneEmail, "msg1")]
	if it.Sensitivity != sensitivityPrivate {
		t.Errorf("email item sensitivity = %q, want private", it.Sensitivity)
	}
}

// TestReconcileEmailUsesExtrairAndReachesToText: the email flow routes the to-text step to the
// extrair-email provider (not a transcrever provider), and once extrair completes the item
// reaches the to_text milestone — proving extrair is a first-class to-text capability.
func TestReconcileEmailUsesExtrairAndReachesToText(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	if err := SeedEmailLane(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestEmail(ctx, db, fakeEmailSource{emails: []EmailItem{{MessageID: "msg1"}}}); err != nil {
		t.Fatal(err)
	}
	itemID := db.items[itemKey(laneEmail, "msg1")].ID
	r := NewReconciler(db, &fakeActivator{})

	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_barato
		t.Fatal(err)
	}
	runGate(t, db, itemID, 2, gateBarato, decisionKeep)
	if err := r.ReconcileOnce(ctx); err != nil { // assign extrair (seq 3)
		t.Fatal(err)
	}
	if s, ok := stepBySeq(db, itemID, 3); !ok || s.Capability != capExtrair || s.AssignedProvider != provExtrairEmail {
		t.Fatalf("to-text step = %+v, want extrair+extrair-email", s)
	}
	// extrair worker finishes (produces the cleaned to-text artifact).
	completeStep(t, db, itemID, 3, "transcript-email-1")
	if err := r.ReconcileOnce(ctx); err != nil { // assign gate_rico
		t.Fatal(err)
	}
	if got := db.itemByID[itemID].Status; got != itemToText {
		t.Errorf("item status = %q, want to_text after extrair", got)
	}
	// gate_rico for a private email routes to the self-host variant (third-party excluded).
	if s, ok := stepBySeq(db, itemID, 4); !ok || s.AssignedProvider != provGateRicoLocal {
		t.Errorf("gate_rico step = %+v, want pending+gate-rico-local", s)
	}
}
