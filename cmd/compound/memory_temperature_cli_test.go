package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

// writeCLIUsageProjection fabricates a minimal capability-usage projection referencing one claim
// (consumed => warm) so a temperature --rebuild has a reinforcement signal to join.
func writeCLIUsageProjection(t *testing.T, homePath, claimID string) {
	t.Helper()
	doc := map[string]any{
		"schema_version": "compound.capability_usage.v0.1",
		"injected_refs": []map[string]any{{
			"ref":            "claim://" + claimID,
			"injected_count": 3,
			"consumed_count": 3,
			"ignored_count":  0,
			"last_injected":  cliTestNow.Format(time.RFC3339),
		}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "projections", "capability-usage.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCmdMemoryTemperatureRebuildAndRead exercises the `compound memory temperature` verb
// end-to-end: the not-built hint, --rebuild (which writes the projection — the lander's live
// artifact step), and --json read reflecting the warmed claim.
func TestCmdMemoryTemperatureRebuildAndRead(t *testing.T) {
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
		Entity:          "compound",
		Predicate:       "temp_pred",
		Object:          "temperature fact",
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		PromotionBasis:  candidates.BasisCuratedPage,
		SourceTraceRef:  "ev_temp",
		SnippetHash:     "sha256:temp",
		Visibility:      "contextual",
		ObservationDate: "2026-06-30T00:00:00Z",
	}}, cliTestNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: got %d err %v", len(got), err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", cliTestNow)
	if err != nil || res.Claim == nil {
		t.Fatalf("promote: %v", err)
	}
	claimID := res.Claim.ID
	writeCLIUsageProjection(t, homePath, claimID)

	// Before any rebuild the projection is absent — the verb hints, it does not error.
	pre := captureStdout(t, func() { cmdMemory([]string{"temperature"}) })
	if !strings.Contains(pre, "not built yet") {
		t.Fatalf("pre-rebuild read should hint to rebuild, got:\n%s", pre)
	}

	// --rebuild joins usage against current claims and writes the projection.
	rebuilt := captureStdout(t, func() { cmdMemory([]string{"temperature", "--rebuild"}) })
	if !strings.Contains(rebuilt, "rebuilt") || !strings.Contains(rebuilt, "entries: 1") {
		t.Fatalf("rebuild output:\n%s", rebuilt)
	}
	if _, err := os.Stat(filepath.Join(homePath, "projections", "memory-temperature.json")); err != nil {
		t.Fatalf("projection file not written: %v", err)
	}

	// --json read reflects the warmed claim with a positive temperature.
	jsonOut := captureStdout(t, func() { cmdMemory([]string{"temperature", "--json"}) })
	var proj mem.TemperatureProjection
	if err := json.Unmarshal([]byte(jsonOut), &proj); err != nil {
		t.Fatalf("parse --json output: %v\n%s", err, jsonOut)
	}
	if len(proj.Entries) != 1 || proj.Entries[0].ClaimID != claimID {
		t.Fatalf("entries = %+v, want single entry for %s", proj.Entries, claimID)
	}
	if proj.Entries[0].Temperature <= 0 {
		t.Fatalf("consumed claim temperature = %v, want > 0", proj.Entries[0].Temperature)
	}
}
