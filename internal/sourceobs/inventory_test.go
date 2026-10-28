package sourceobs

import (
	"testing"
	"time"

	"compound/internal/candidates"
)

var invNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func boolp(b bool) *bool { return &b }

func sampleRepos() []InventoryRepo {
	return []InventoryRepo{
		{Slug: "demo-health", Name: "demo-health", Domain: "products", RelativePath: "products/demo-health", Git: false, MarkerFiles: []string{"CLAUDE.md", "AGENTS.md"}, ProjectStrength: "weak"},
		{Slug: "sample-app", Name: "sample-app", Domain: "products", RelativePath: "products/sample-app", Git: true, Branch: "main", Dirty: boolp(false), RecentCommit: "d62f43f prepare release", MarkerFiles: []string{"AGENTS.md"}, ProjectStrength: "strong"},
	}
}

// FromInventory maps every per-repo row to a no-promote product_hypothesis candidate: the inventory
// is portfolio evidence awaiting adjudication, never promoted truth. RED if a row were promotable or
// mis-routed to another family.
func TestFromInventoryMapsRowsToNoPromoteProductHypothesis(t *testing.T) {
	intake := InventoryIntake{SourceCapture: "/tmp/inventory.json", Projects: sampleRepos()}
	obs, err := FromInventory(intake, "cafebabe1234", "sha256:body", invNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.MapsTo) != 2 {
		t.Fatalf("want 2 maps, got %d", len(obs.MapsTo))
	}
	for i, m := range obs.MapsTo {
		if m.CandidateKind != string(candidates.ProductHypothesis) {
			t.Fatalf("map %d kind=%q want product_hypothesis", i, m.CandidateKind)
		}
		if m.ObjectFamily != "product" {
			t.Fatalf("map %d family=%q want product", i, m.ObjectFamily)
		}
		if m.PromoteAllowed {
			t.Fatalf("map %d must be no-promote", i)
		}
	}
	if obs.SourceSystem != "project-miner" || obs.AdapterID != "compound.project-inventory" {
		t.Fatalf("wrong source identity: %s / %s", obs.SourceSystem, obs.AdapterID)
	}
	if obs.SourceRef.Revision != "cafebabe1234" {
		t.Fatalf("revision=%q", obs.SourceRef.Revision)
	}
	// body ref carried as evidence so candidates resolve back to the preserved original.
	foundBody := false
	for _, r := range obs.EvidenceRefs {
		if r == "sha256:body" {
			foundBody = true
		}
	}
	if !foundBody {
		t.Fatalf("evidence %v missing body ref", obs.EvidenceRefs)
	}
}

// The content hash IGNORES the volatile intake generated_at: two runs over an unchanged workspace
// (different wall-clock timestamps) must produce the identical content hash AND identical observation
// id, so an idempotent re-run overwrites the same records. This is the crux improvement over the
// change-stream door (which hashes a raw body that embeds a timestamp). RED if the hash keyed on
// generated_at, or if the observation id absorbed the timestamp.
func TestInventoryStableSHAIgnoresTimestamps(t *testing.T) {
	a := InventoryIntake{SourceCapture: "/tmp/inventory.json", GeneratedAt: "2026-07-19T09:00:00+03:00", Projects: sampleRepos()}
	b := InventoryIntake{SourceCapture: "/tmp/inventory.json", GeneratedAt: "2026-07-19T23:59:59+03:00", Projects: sampleRepos()}
	if InventoryContentSHA(a) != InventoryContentSHA(b) {
		t.Fatalf("content sha changed on generated_at alone: %s vs %s", InventoryContentSHA(a), InventoryContentSHA(b))
	}
	obsA, _ := FromInventory(a, InventoryContentSHA(a), "sha256:bodyA", invNow)
	obsB, _ := FromInventory(b, InventoryContentSHA(b), "sha256:bodyB", invNow)
	if obsA.ID != obsB.ID {
		t.Fatalf("observation id changed on generated_at alone: %s vs %s", obsA.ID, obsB.ID)
	}
	for i := range obsA.MapsTo {
		candidateA := CandidateFromMap(obsA, obsA.MapsTo[i], invNow)
		candidateB := CandidateFromMap(obsB, obsB.MapsTo[i], invNow)
		if candidateA.ID != candidateB.ID {
			t.Fatalf("candidate %d id changed on generated_at alone: %s vs %s", i, candidateA.ID, candidateB.ID)
		}
	}
	// Row order must not matter either: the canonicalizer sorts by relative_path.
	reordered := InventoryIntake{SourceCapture: "/tmp/inventory.json", Projects: []InventoryRepo{sampleRepos()[1], sampleRepos()[0]}}
	if InventoryContentSHA(reordered) != InventoryContentSHA(a) {
		t.Fatalf("content sha changed on row order alone")
	}
}

// A census-level change must re-key the observation but only the changed row's candidate. RED if
// CandidateFromMap still includes the census observation id or revision in inventory candidate ids.
func TestInventoryCandidateIDsTrackOnlyChangedRow(t *testing.T) {
	run1 := sampleRepos()
	run2 := sampleRepos()
	run2[1].Dirty = boolp(true)

	intake1 := InventoryIntake{SourceCapture: "/tmp/inventory.json", Projects: run1}
	intake2 := InventoryIntake{SourceCapture: "/tmp/inventory.json", Projects: run2}
	obs1, err := FromInventory(intake1, InventoryContentSHA(intake1), "sha256:run1", invNow)
	if err != nil {
		t.Fatal(err)
	}
	obs2, err := FromInventory(intake2, InventoryContentSHA(intake2), "sha256:run2", invNow)
	if err != nil {
		t.Fatal(err)
	}

	if obs1.ID == obs2.ID {
		t.Fatalf("observation id did not change with the census: %s", obs1.ID)
	}
	unchanged1 := CandidateFromMap(obs1, obs1.MapsTo[0], invNow)
	unchanged2 := CandidateFromMap(obs2, obs2.MapsTo[0], invNow)
	if unchanged1.ID != unchanged2.ID {
		t.Fatalf("unchanged repo candidate id changed across censuses: %s vs %s", unchanged1.ID, unchanged2.ID)
	}
	changed1 := CandidateFromMap(obs1, obs1.MapsTo[1], invNow)
	changed2 := CandidateFromMap(obs2, obs2.MapsTo[1], invNow)
	if changed1.ID == changed2.ID {
		t.Fatalf("changed repo candidate id did not change: %s", changed1.ID)
	}
}

// The content hash DOES track meaningful inventory change: flip a repo's dirty flag or its recent
// commit and the census hash must move (otherwise a changed portfolio would silently reuse a stale
// observation). RED if the hash dropped these fields.
func TestInventoryStableSHATracksMeaningfulChange(t *testing.T) {
	base := InventoryContentSHA(InventoryIntake{Projects: sampleRepos()})
	changed := sampleRepos()
	changed[1].Dirty = boolp(true)
	if InventoryContentSHA(InventoryIntake{Projects: changed}) == base {
		t.Fatalf("content sha ignored a dirty-flag change")
	}
	commitChanged := sampleRepos()
	commitChanged[1].RecentCommit = "abc9999 new work"
	if InventoryContentSHA(InventoryIntake{Projects: commitChanged}) == base {
		t.Fatalf("content sha ignored a recent-commit change")
	}
}

// Two distinct repos get distinct, stable per-row hints so their candidate ids never collide onto
// one record (silent data loss); an identical row yields an identical hint (idempotent overwrite).
func TestInventoryEntityHintDistinctAndStable(t *testing.T) {
	rows := sampleRepos()
	ha, hb := inventoryEntityHint(rows[0]), inventoryEntityHint(rows[1])
	if ha == hb {
		t.Fatalf("distinct repos collided on hint %q", ha)
	}
	if inventoryEntityHint(rows[0]) != ha {
		t.Fatalf("hint not stable for identical row")
	}
}

func TestFromInventoryRejectsBadInput(t *testing.T) {
	good := sampleRepos()
	if _, err := FromInventory(InventoryIntake{SourceCapture: "/x", Projects: nil}, "sha", "", invNow); err == nil {
		t.Fatal("want error on zero projects")
	}
	if _, err := FromInventory(InventoryIntake{SourceCapture: "", Projects: good}, "sha", "", invNow); err == nil {
		t.Fatal("want error on empty source_capture")
	}
	if _, err := FromInventory(InventoryIntake{SourceCapture: "/x", Projects: good}, "", "", invNow); err == nil {
		t.Fatal("want error on empty content sha")
	}
	if _, err := FromInventory(InventoryIntake{SourceCapture: "/x", Projects: []InventoryRepo{{Domain: "products"}}}, "sha", "", invNow); err == nil {
		t.Fatal("want error on row lacking any identity (relative_path/slug/path)")
	}
}
