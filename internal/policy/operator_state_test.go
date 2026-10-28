package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportOperatorStateWritesAuthorityAndAppliesCeiling(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "operator-state.json")
	raw := []byte(`{
  "mode": "closure_over_expansion",
  "needs_me": [],
  "blocked": [],
  "autonomy_lever": {
    "level": "L1",
    "set_at": "2026-06-30T12:00:00Z",
    "set_by": "fixture",
    "meaning": "synthetic ceiling"
  }
}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ImportOperatorState(homePath, sourcePath, fixedNow)
	if err != nil {
		t.Fatalf("import operator state: %v", err)
	}
	if report.AuthorityRef != OperatorStateAuthorityRef || report.AutonomyLevel != "L1" {
		t.Fatalf("bad import report: %+v", report)
	}
	if _, err := os.Stat(OperatorStatePath(homePath)); err != nil {
		t.Fatalf("operator authority missing: %v", err)
	}
	records, err := LoadOperatorStateSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 || records[0].AuthorityStatus != "compound_primary" || records[0].SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("bad source authority records: %+v", records)
	}
	cfg, _, err := LoadConfig(homePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	pd := Evaluate(mkAction("local_reversible", "easy", "docs/x.md", "repo"), cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("L1 ceiling should hold local reversible mutation, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !testContains(pd.EvidenceRefs, OperatorStateAuthorityRef) {
		t.Fatalf("policy evidence refs missing operator authority: %v", pd.EvidenceRefs)
	}
	if !reasonContains(pd.Reasons, "operator autonomy ceiling L1") {
		t.Fatalf("policy reasons missing autonomy ceiling: %v", pd.Reasons)
	}
}

func TestOperatorStateDefaultAndL3DoNotBypassIrreversibleClasses(t *testing.T) {
	state := OperatorState{}
	if level := OperatorStateLevel(state); level != defaultAutonomyLevel {
		t.Fatalf("empty operator state level = %q, want %q", level, defaultAutonomyLevel)
	}
	cfg := DefaultConfig()
	ApplyOperatorState(&cfg, OperatorState{AutonomyLever: OperatorAutonomyLever{Level: "L3"}})
	local := Evaluate(mkAction("local_reversible", "easy", "docs/x.md", "repo"), cfg, fixedNow)
	if local.Outcome != "allow_with_obligations" {
		t.Fatalf("L3 should leave local reversible baseline usable, got %q reasons=%v", local.Outcome, local.Reasons)
	}
	public := Evaluate(mkAction("public", "manual", "site/index.html", "public"), cfg, fixedNow)
	if public.Outcome != "hold" {
		t.Fatalf("L3 must not bypass public hold, got %q reasons=%v", public.Outcome, public.Reasons)
	}
}

func TestOperatorStateRejectsInvalidAutonomyLevel(t *testing.T) {
	_, err := parseOperatorState([]byte(`{"autonomy_lever":{"level":"L9"}}`))
	if err == nil || !strings.Contains(err.Error(), "invalid autonomy_lever.level") {
		t.Fatalf("expected invalid autonomy level error, got %v", err)
	}
}
