package decisions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// TestStandingLawSupersededNeverLive is the falsifier-first depth test for the
// standing-law view: a record whose successor supersedes it must NEVER appear as
// a live top-level record — only nested (struck-through) under its successor. If
// AllActiveDecisions ever leaked the superseded record, or the builder listed it
// twice, this reds.
func TestStandingLawSupersededNeverLive(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()

	// Old ruling, then a superseding ruling naming it via --undo-of.
	if _, err := Record(store, home, RecordInput{
		Kind: KindRuling, ID: "old-exec-lane",
		Title:      "Execution routes through Sonnet subagents",
		ReviewedBy: "operator:test", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(store, home, RecordInput{
		Kind: KindRuling, ID: "new-exec-lane",
		Title:      "Execution routes through Codex only, never Opus subagents",
		ReviewedBy: "operator:test", UndoOf: "old-exec-lane",
		Now: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated live grant so search-narrowing has something to exclude.
	if _, err := Record(store, home, RecordInput{
		Kind: KindGrant, ID: "director-apply-grant",
		Title:      "Let the director apply job-health work on its own",
		ReviewedBy: "operator:test", Now: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// A parked harvested conflict candidate reversing the live exec-lane ruling.
	conflictClaimID := activeClaimIDForDecision(t, store, "new-exec-lane")
	writeConflictCandidate(t, home, conflictClaimID)

	view, err := StandingLaw(store, home, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// Both live records render; the superseded old ruling does NOT.
	live := map[string]StandingLawRecord{}
	for _, r := range view.Records {
		live[r.DecisionID] = r
	}
	if _, ok := live["old-exec-lane"]; ok {
		t.Fatal("superseded record 'old-exec-lane' leaked into live records — must only render nested under its successor")
	}
	successor, ok := live["new-exec-lane"]
	if !ok {
		t.Fatal("live successor 'new-exec-lane' missing from standing-law records")
	}
	if _, ok := live["director-apply-grant"]; !ok {
		t.Fatal("unrelated live grant missing from standing-law records")
	}

	// The supersession chain hangs the old ruling under the successor with a title.
	if len(successor.Supersedes) != 1 || successor.Supersedes[0].DecisionID != "old-exec-lane" {
		t.Fatalf("successor must carry old-exec-lane as a superseded ancestor, got %+v", successor.Supersedes)
	}
	if successor.Supersedes[0].Title == "" {
		t.Fatal("superseded ancestor must carry its title so the row can dim it, not show a bare id")
	}

	// Newest-first ordering: the grant (latest date) leads.
	if view.Records[0].DecisionID != "director-apply-grant" {
		t.Fatalf("records must be newest-first; got head %q", view.Records[0].DecisionID)
	}

	// The contradiction strip is present and links the conflicting live claim.
	if len(view.Conflicts) != 1 {
		t.Fatalf("expected exactly one conflict candidate in the strip, got %d", len(view.Conflicts))
	}
	if view.Conflicts[0].ConflictsWithClaim != conflictClaimID {
		t.Fatalf("conflict must link the live claim it reverses: got %q want %q",
			view.Conflicts[0].ConflictsWithClaim, conflictClaimID)
	}
	if view.Conflicts[0].ConflictingTitle != successor.Title {
		t.Fatalf("conflict must resolve the conflicting record's title for the card, got %q", view.Conflicts[0].ConflictingTitle)
	}

	// Counts reconcile.
	if view.Counts.Records != 2 || view.Counts.Rulings != 1 || view.Counts.Grants != 1 {
		t.Fatalf("counts drift: %+v", view.Counts)
	}
	if view.Counts.Superseded != 1 || view.Counts.Conflicts != 1 {
		t.Fatalf("superseded/conflict counts drift: %+v", view.Counts)
	}
}

func TestStandingLawConflictCandidatesRejectCustodyCorruption(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	writeConflictCandidate(t, home, "claim_live")
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	conflictBody := `{"id":"other","subject":"forged","qualifiers":{"harvest_outcome":"conflict","conflicts_with_claim":"claim_live"}}`
	if err := os.WriteFile(filepath.Join(dir, "mismatch.json"), []byte(conflictBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blank.json"), []byte(`{"id":"","subject":"forged","qualifiers":{"harvest_outcome":"conflict"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "linked.json")
	if err := os.WriteFile(outside, []byte(`{"id":"linked","subject":"forged","qualifiers":{"harvest_outcome":"conflict"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("not a canonical candidate"), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := StandingLaw(store, home, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Conflicts) != 1 || view.Conflicts[0].CandidateID != "harvest-ruling-deadbeef1234" {
		t.Fatalf("corrupt candidates became standing-law conflicts: %+v", view.Conflicts)
	}
	warnings := strings.Join(view.Warnings, "\n")
	for _, name := range []string{"blank.json", "linked.json", "mismatch.json", "scratch.tmp"} {
		if !strings.Contains(warnings, name) {
			t.Fatalf("warnings %q do not name corrupt entry %s", warnings, name)
		}
	}
}

func activeClaimIDForDecision(t *testing.T, store *mem.Store, decisionID string) string {
	t.Helper()
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range AllActiveDecisions(claims) {
		if LifecycleFromClaim(c).ID == decisionID {
			return c.ID
		}
	}
	t.Fatalf("no active claim for decision %q", decisionID)
	return ""
}

// writeConflictCandidate mirrors the shape internal/harvest writes for a
// conflict outcome (verdict=park, harvest_outcome=conflict, conflicts_with_claim).
func writeConflictCandidate(t *testing.T, home, conflictClaimID string) {
	t.Helper()
	rec := map[string]any{
		"id":        "harvest-ruling-deadbeef1234",
		"kind":      string(candidates.Decision),
		"entity":    "decision:session-ruling-deadbeef1234",
		"predicate": "decided",
		"object":    "Execution should route through Opus subagents",
		"subject":   "Ruling: Execution should route through Opus subagents",
		"statement": "Ruling: Execution should route through Opus subagents",
		"verdict":   candidates.Park,
		"qualifiers": map[string]string{
			"kind":                    KindRuling,
			"source":                  "sessions_harvest",
			"lifecycle_status":        "proposed",
			"harvest_outcome":         HarvestConflictOutcome,
			"session":                 "sess-conflict",
			"transcript":              "/tmp/sess-conflict.jsonl",
			"line":                    "42",
			"verbatim":                "actually execution should route through Opus subagents for everything i talk",
			"conflicts_with_claim":    conflictClaimID,
			"conflicts_with_decision": "new-exec-lane",
			"needs_operator":          "true",
		},
	}
	if err := candidates.WriteCandidate(home, candidates.Decision, "harvest-ruling-deadbeef1234", rec); err != nil {
		t.Fatal(err)
	}
}
