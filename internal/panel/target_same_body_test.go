package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestLoadCapabilityObservationTargetsUsesOneCustodiedBody(t *testing.T) {
	home := t.TempDir()
	id := "cobs_same_body_list"
	safeBody := writePanelSameBodyCandidate(t, home, candidates.CapabilityObs, id, map[string]any{
		"id":         id,
		"subject":    "safe-provider",
		"statement":  "safe-capability",
		"detail":     "safe-detail",
		"confidence": 0.9,
		"state":      "proposed",
	})
	attackerBody := marshalPanelSameBodyCandidate(t, map[string]any{
		"id":         id,
		"subject":    "attacker-provider",
		"statement":  "attacker-capability",
		"detail":     "attacker-detail",
		"confidence": 0.9,
		"state":      "proposed",
	})
	swaps := installPanelCandidatePageSwap(t, home, candidates.CapabilityObs, id, attackerBody)
	t.Cleanup(func() { assertNoPanelLoadArtifacts(t, home) })

	targets, err := LoadCapabilityObservationTargets(home, 0)
	if err != nil {
		t.Fatalf("load capability observation targets: %v", err)
	}
	if *swaps != 1 {
		t.Fatalf("page swaps = %d, want 1", *swaps)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	assertFrozenPanelCandidateTarget(t, targets[0], safeBody, "safe-capability", "attacker-capability")
}

func TestLoadDecisionCandidateTargetsUsesOneCustodiedBody(t *testing.T) {
	home := t.TempDir()
	id := "dcand_same_body_list"
	safeBody := writePanelSameBodyCandidate(t, home, candidates.Decision, id, map[string]any{
		"id":         id,
		"kind":       "release_decision",
		"subject":    "safe-subject",
		"statement":  "safe-statement",
		"qualifiers": map[string]string{"scope": "safe-scope"},
		"confidence": 0.9,
		"verdict":    "park",
	})
	attackerBody := marshalPanelSameBodyCandidate(t, map[string]any{
		"id":         id,
		"kind":       "release_decision",
		"subject":    "attacker-subject",
		"statement":  "attacker-statement",
		"qualifiers": map[string]string{"scope": "attacker-scope"},
		"confidence": 0.9,
		"verdict":    "park",
	})
	swaps := installPanelCandidatePageSwap(t, home, candidates.Decision, id, attackerBody)
	t.Cleanup(func() { assertNoPanelLoadArtifacts(t, home) })

	targets, err := LoadDecisionCandidateTargets(home, 0)
	if err != nil {
		t.Fatalf("load decision candidate targets: %v", err)
	}
	if *swaps != 1 {
		t.Fatalf("page swaps = %d, want 1", *swaps)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	assertFrozenPanelCandidateTarget(t, targets[0], safeBody, "safe-statement", "attacker-statement")
}

func TestLoadCapabilityObservationTargetInputUsesOneCustodiedBody(t *testing.T) {
	home := t.TempDir()
	id := "cobs_same_body_point"
	safeBody := writePanelSameBodyCandidate(t, home, candidates.CapabilityObs, id, map[string]any{
		"id":         id,
		"subject":    "safe-provider",
		"statement":  "safe-capability",
		"detail":     "safe-detail",
		"confidence": 0.9,
		"state":      "proposed",
	})
	attackerBody := marshalPanelSameBodyCandidate(t, map[string]any{
		"id":         id,
		"subject":    "attacker-provider",
		"statement":  "attacker-capability",
		"detail":     "attacker-detail",
		"confidence": 0.9,
		"state":      "proposed",
	})
	swaps := installPanelCandidateRecordSwap(t, home, candidates.CapabilityObs, id, attackerBody)
	t.Cleanup(func() { assertNoPanelLoadArtifacts(t, home) })

	target, err := LoadTargetInput(home, "capability_observation", id)
	if err != nil {
		t.Fatalf("load capability observation target input: %v", err)
	}
	if *swaps != 1 {
		t.Fatalf("record swaps = %d, want 1", *swaps)
	}
	assertFrozenPanelCandidateTarget(t, target, safeBody, "safe-capability", "attacker-capability")
}

func TestLoadDecisionCandidateTargetInputUsesOneCustodiedBody(t *testing.T) {
	home := t.TempDir()
	id := "dcand_same_body_point"
	safeBody := writePanelSameBodyCandidate(t, home, candidates.Decision, id, map[string]any{
		"id":         id,
		"kind":       "release_decision",
		"subject":    "safe-subject",
		"statement":  "safe-statement",
		"qualifiers": map[string]string{"scope": "safe-scope"},
		"confidence": 0.9,
		"verdict":    "park",
	})
	attackerBody := marshalPanelSameBodyCandidate(t, map[string]any{
		"id":         id,
		"kind":       "release_decision",
		"subject":    "attacker-subject",
		"statement":  "attacker-statement",
		"qualifiers": map[string]string{"scope": "attacker-scope"},
		"confidence": 0.9,
		"verdict":    "park",
	})
	swaps := installPanelCandidateRecordSwap(t, home, candidates.Decision, id, attackerBody)
	t.Cleanup(func() { assertNoPanelLoadArtifacts(t, home) })

	target, err := LoadTargetInput(home, "decision_candidate", id)
	if err != nil {
		t.Fatalf("load decision candidate target input: %v", err)
	}
	if *swaps != 1 {
		t.Fatalf("record swaps = %d, want 1", *swaps)
	}
	assertFrozenPanelCandidateTarget(t, target, safeBody, "safe-statement", "attacker-statement")
}

func writePanelSameBodyCandidate(t *testing.T, home string, kind candidates.Kind, id string, value any) []byte {
	t.Helper()
	body := marshalPanelSameBodyCandidate(t, value)
	dir := filepath.Join(home, candidates.InboxDir(kind))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir candidate inbox: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	return body
}

func marshalPanelSameBodyCandidate(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	return append(body, '\n')
}

func installPanelCandidatePageSwap(t *testing.T, home string, kind candidates.Kind, id string, attackerBody []byte) *int {
	t.Helper()
	previous := readPanelCandidatePage
	swaps := 0
	readPanelCandidatePage = func(gotHome string, gotKind candidates.Kind, after candidates.EntryCursor, scanLimit int) (candidates.Page, error) {
		page, err := previous(gotHome, gotKind, after, scanLimit)
		if err == nil && gotHome == home && gotKind == kind && swaps == 0 {
			replacePanelCandidateAfterCustody(t, home, kind, id, attackerBody)
			swaps++
		}
		return page, err
	}
	t.Cleanup(func() { readPanelCandidatePage = previous })
	return &swaps
}

func installPanelCandidateRecordSwap(t *testing.T, home string, kind candidates.Kind, id string, attackerBody []byte) *int {
	t.Helper()
	previous := readPanelCandidateRecord
	swaps := 0
	readPanelCandidateRecord = func(gotHome string, gotKind candidates.Kind, gotID string) (candidates.Record, error) {
		record, err := previous(gotHome, gotKind, gotID)
		if err == nil && gotHome == home && gotKind == kind && gotID == id && swaps == 0 {
			replacePanelCandidateAfterCustody(t, home, kind, id, attackerBody)
			swaps++
		}
		return record, err
	}
	t.Cleanup(func() { readPanelCandidateRecord = previous })
	return &swaps
}

func replacePanelCandidateAfterCustody(t *testing.T, home string, kind candidates.Kind, id string, attackerBody []byte) {
	t.Helper()
	dir := filepath.Join(home, candidates.InboxDir(kind))
	path := filepath.Join(dir, id+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original candidate: %v", err)
	}
	staged := filepath.Join(dir, "."+id+".attacker")
	if err := os.WriteFile(staged, attackerBody, 0o600); err != nil {
		t.Fatalf("write attacker candidate: %v", err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("replace candidate after custody: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replacement candidate: %v", err)
	}
	if os.SameFile(before, after) {
		t.Fatal("candidate replacement reused the custodied inode")
	}
}

func assertFrozenPanelCandidateTarget(t *testing.T, target TargetInput, safeBody []byte, safeMarker, attackerMarker string) {
	t.Helper()
	if !strings.Contains(target.Summary, safeMarker) {
		t.Errorf("summary = %q, want safe marker %q", target.Summary, safeMarker)
	}
	if strings.Contains(target.Summary, attackerMarker) {
		t.Errorf("summary escaped attacker marker %q: %q", attackerMarker, target.Summary)
	}
	if !bytes.Equal(target.Payload, safeBody) {
		t.Errorf("payload escaped frozen candidate body:\n got: %s\nwant: %s", target.Payload, safeBody)
	}
	if bytes.Contains(target.Payload, []byte(attackerMarker)) {
		t.Errorf("payload contains attacker marker %q", attackerMarker)
	}
}

func assertNoPanelLoadArtifacts(t *testing.T, home string) {
	t.Helper()
	for _, rel := range []string{"events", "receipts", "run_attempts", "spend", "transitions", "work"} {
		_, err := os.Lstat(filepath.Join(home, rel))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("read-only panel load created %s artifact: %v", rel, err)
		}
	}
}
