package main

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestCmdMemoryCanonJSON(t *testing.T) {
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
		Entity:         "compound",
		Predicate:      "canon_pred",
		Object:         "canon fact",
		Confidence:     0.9,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		PromotionBasis: candidates.BasisCuratedPage,
		SourceTraceRef: "ev_canon",
		SnippetHash:    "sha256:canon",
		Visibility:     "contextual",
	}}, cliTestNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: %v", err)
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "test", cliTestNow); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdMemory([]string{"canon", "--json"}) })
	var rep mem.CanonClassReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if rep.SchemaVersion != mem.CanonClassReportSchemaVersion {
		t.Fatalf("schema = %s", rep.SchemaVersion)
	}
	if rep.Total < 1 || rep.Distribution["decision"] < 1 {
		t.Fatalf("distribution = %+v total=%d", rep.Distribution, rep.Total)
	}

	// Second run is read-only and byte-identical in structure.
	out2 := captureStdout(t, func() { cmdMemory([]string{"canon", "--json"}) })
	if out2 != out {
		t.Fatalf("canon --json not stable:\n---\n%s\n---\n%s", out, out2)
	}

	human := captureStdout(t, func() { cmdMemory([]string{"canon"}) })
	if !strings.Contains(human, "memory canon") || !strings.Contains(human, "decision") {
		t.Fatalf("human output:\n%s", human)
	}
}
