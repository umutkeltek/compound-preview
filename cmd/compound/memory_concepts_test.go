package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestMemoryConceptCLIVerbs(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	fixture := filepath.Join("..", "..", "fixtures", "memory", "topics.json")
	importOut := captureStdout(t, func() {
		cmdMemory([]string{"import-topics", fixture, "--json"})
	})
	if !strings.Contains(importOut, `"topic_count": 3`) || !strings.Contains(importOut, `"edges_created": 4`) {
		t.Fatalf("import-topics output:\n%s", importOut)
	}

	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entity := "project:cli-concept-fixture"
	captured, err := store.Capture([]mem.Candidate{
		{
			Entity: entity, Predicate: "delivery_mode", Object: "automated", Confidence: 0.9,
			Derivation: "deterministic", ClaimType: "architecture", Qualifiers: map[string]string{"domain": "delivery"},
			PromotionBasis: candidates.BasisTrustedDeterministic, SourceTraceRef: "trace_cli_concept_mode",
			SnippetHash: "sha256:delivery-mode", TopicRefs: []string{"topic:application-platforms"},
		},
		{
			Entity: entity, Predicate: "delivery_owner", Object: "compound", Confidence: 0.9,
			Derivation: "deterministic", ClaimType: "architecture", Qualifiers: map[string]string{"domain": "delivery"},
			PromotionBasis: candidates.BasisTrustedDeterministic, SourceTraceRef: "trace_cli_concept_owner",
			SnippetHash: "sha256:delivery-owner", TopicRefs: []string{"topic:application-platforms"},
		},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := store.Adjudicate(candidate.ID, "promote", "", cliTestNow); err != nil {
			t.Fatal(err)
		}
	}

	foldOut := captureStdout(t, func() {
		cmdMemory([]string{"fold", "--entity", entity, "--json"})
	})
	var fold mem.PositionFoldReport
	if err := json.Unmarshal([]byte(foldOut), &fold); err != nil {
		t.Fatalf("parse fold json: %v\n%s", err, foldOut)
	}
	if fold.Count != 2 {
		t.Fatalf("fold report = %+v", fold)
	}

	positionOut := captureStdout(t, func() {
		cmdMemory([]string{"position", entity, "--topic", "topic:application-platforms", "--json"})
	})
	var position mem.PositionQueryResult
	if err := json.Unmarshal([]byte(positionOut), &position); err != nil {
		t.Fatalf("parse position json: %v\n%s", err, positionOut)
	}
	if len(position.Positions) != 2 {
		t.Fatalf("position result = %+v", position)
	}
	positionText := captureStdout(t, func() {
		cmdMemory([]string{"position", entity, "--topic", "topic:application-platforms"})
	})
	for _, detail := range position.Positions {
		p := detail.Position
		wantPositionLine := p.ID + " [predicate=" + p.Predicate + ", claim_type=" + p.ClaimType + "] = " + p.ActiveObject
		if !strings.Contains(positionText, wantPositionLine) {
			t.Fatalf("position text lost fact-slot identity %q:\n%s", wantPositionLine, positionText)
		}
	}

	recallOut := captureStdout(t, func() {
		cmdMemory([]string{"recall", entity, "--topic", "topic:application-platforms", "--json"})
	})
	var recall mem.RecallResult
	if err := json.Unmarshal([]byte(recallOut), &recall); err != nil {
		t.Fatalf("parse recall json: %v\n%s", err, recallOut)
	}
	if len(recall.Claims) != 2 {
		t.Fatalf("recall result = %+v", recall)
	}
}

func TestMemoryBakeoffExistingCommandPrintsClinicEvidence(t *testing.T) {
	out := captureStdout(t, func() {
		printMemoryBakeoffClinic(mem.BakeoffClinicReport{
			TopCauses: []mem.BakeoffClinicCause{{
				Cause: "held_by_review_gate", Count: 2,
				EvidenceRefs: []string{"casual:claim-a", "exact:claim-b"},
			}},
			RecommendedFixes: []string{"review the held claim; do not bypass the gate"},
		})
	})
	for _, want := range []string{
		"clinic_cause_1: held_by_review_gate (2)",
		"clinic_evidence_1: casual:claim-a,exact:claim-b",
		"recommended_fix: review the held claim; do not bypass the gate",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("existing bakeoff command output missing %q:\n%s", want, out)
		}
	}
}
