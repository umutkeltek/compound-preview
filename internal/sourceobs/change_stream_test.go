package sourceobs

import (
	"testing"
	"time"

	"compound/internal/candidates"
)

var csNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// FromChangeStream maps Hermes-native candidate types to the right Compound candidate kinds and
// object families; repo/session drift is a product hypothesis, canon drift a capability observation.
func TestFromChangeStreamKindMapping(t *testing.T) {
	intake := ChangeStreamIntake{
		SourceCapture: "/tmp/capture.json",
		Candidates: []ChangeStreamCandidate{
			{Type: "repo-signal", EntityHint: "notekeep", Surface: "ASO"},
			{Type: "agent-history-row", EntityHint: "postready", Surface: "runtime"},
			{Type: "canon-file-signal", EntityHint: "~/.codex/config.toml", Surface: "harness-governance"},
		},
	}
	obs, err := FromChangeStream(intake, "cafebabe1234", "sha256:body", csNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.MapsTo) != 3 {
		t.Fatalf("want 3 maps, got %d", len(obs.MapsTo))
	}
	want := []struct {
		kind   candidates.Kind
		family string
	}{
		{candidates.ProductHypothesis, "product"},
		{candidates.ProductHypothesis, "product"},
		{candidates.CapabilityObs, "capability"},
	}
	for i, w := range want {
		if obs.MapsTo[i].CandidateKind != string(w.kind) {
			t.Fatalf("map %d kind=%q want %q", i, obs.MapsTo[i].CandidateKind, w.kind)
		}
		if obs.MapsTo[i].ObjectFamily != w.family {
			t.Fatalf("map %d family=%q want %q", i, obs.MapsTo[i].ObjectFamily, w.family)
		}
		if obs.MapsTo[i].PromoteAllowed {
			t.Fatalf("map %d must be no-promote", i)
		}
	}
	// revision + body ref carried as evidence so candidates resolve back to the preserved original.
	if obs.SourceRef.Revision != "cafebabe1234" {
		t.Fatalf("revision=%q", obs.SourceRef.Revision)
	}
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

// Two distinct candidates that share a slug and surface must still get distinct, stable entity
// hints so their candidate ids do not collide onto one record (silent data loss). RED if the hint
// ignored the summary discriminator.
func TestChangeStreamEntityHintDistinctPerCandidate(t *testing.T) {
	a := ChangeStreamCandidate{Type: "repo-signal", EntityHint: "notekeep", Surface: "ASO", Summary: "dirty tree"}
	b := ChangeStreamCandidate{Type: "repo-signal", EntityHint: "notekeep", Surface: "ASO", Summary: "recent commit"}
	ha, hb := changeStreamEntityHint(a), changeStreamEntityHint(b)
	if ha == hb {
		t.Fatalf("distinct candidates collided on hint %q", ha)
	}
	// stable: same candidate => same hint (idempotent re-run overwrites, not duplicates).
	if changeStreamEntityHint(a) != ha {
		t.Fatalf("hint not stable for identical candidate")
	}
}

func TestFromChangeStreamRejectsBadInput(t *testing.T) {
	if _, err := FromChangeStream(ChangeStreamIntake{SourceCapture: "/x", Candidates: nil}, "sha", "", csNow); err == nil {
		t.Fatal("want error on zero candidates")
	}
	if _, err := FromChangeStream(ChangeStreamIntake{SourceCapture: "", Candidates: []ChangeStreamCandidate{{Type: "repo-signal", EntityHint: "x"}}}, "sha", "", csNow); err == nil {
		t.Fatal("want error on empty source_capture")
	}
	if _, err := FromChangeStream(ChangeStreamIntake{SourceCapture: "/x", Candidates: []ChangeStreamCandidate{{Type: "nope", EntityHint: "x"}}}, "sha", "", csNow); err == nil {
		t.Fatal("want error on unknown type")
	}
}
