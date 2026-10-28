package main

import (
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/mem"
)

// The one-line undo the volatile sweep's cooled receipts point at:
// `compound memory volatile-restore <cooled_claim_id>` restores a
// volatile-cooled claim with a fresh clock.
func TestCmdMemoryVolatileRestore(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{{
		Entity:         "proj:cli-vol",
		Predicate:      "build_status",
		Object:         "green",
		Confidence:     0.9,
		Derivation:     "deterministic",
		ClaimType:      "status",
		PromotionBasis: "runtime_proof",
		SourceTraceRef: "ev_vol_cli",
		SnippetHash:    "sha256:volcli",
		Visibility:     "volatile",
		ExpiresAt:      cliTestNow.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
	}}, cliTestNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: %v", err)
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	report, err := store.SynthesisSweep([]string{"volatile"}, cliTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.VolatileCooledCount != 1 {
		t.Fatalf("fixture not cooled: %+v", report)
	}
	cooledID := report.VolatileActions[0].NewClaimID

	out := captureStdout(t, func() { cmdMemory([]string{"volatile-restore", cooledID}) })
	if !strings.Contains(out, "restored") || !strings.Contains(out, "volatile") {
		t.Fatalf("restore output should report the restored volatile claim, got:\n%s", out)
	}

	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claims {
		if c.Entity == "proj:cli-vol" && c.Visibility == "volatile" && c.CooledReason == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored claim not current volatile: %+v", claims)
	}
}
