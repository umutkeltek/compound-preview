package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

// letterFixtureReport mirrors the attention --json shape the daily digest
// wrapper pipes into scripts/attention-letter.py: authored cards, the retired
// generic fallback card, and a card too dense for eighth grade.
func letterFixtureReport() string {
	return `{
 "counts": {"needs_you": 3, "needs_you_groups": 3, "blocked_on_you": 2},
 "groups": [
  {"key": "panel|round", "area": "panel-verdicts", "band": "needs_you", "count": 2,
   "operator_summary": "The judge panel needs your call. Record it?",
   "operator_unlock": "the bakeoff winner is recorded",
   "action": {"policy_authority": {"allowed": true}}},
  {"key": "queue|legacy", "area": "product-content-queue", "band": "needs_you", "count": 1,
   "operator_summary": "product content queue: a new record needs your read.",
   "operator_unlock": ""},
  {"key": "dense|one", "area": "capability-evaluation-intake", "band": "needs_you", "count": 5,
   "operator_summary": "Capability evaluation instrumentation notwithstanding, deterministically categorizing antecedent observability paradigms necessitates adjudication.",
   "operator_unlock": "capability lifecycle changes only after review"},
  {"key": "watch|one", "area": "route-promotions", "band": "watching", "count": 1,
   "operator_summary": "Still watching.", "operator_unlock": ""}
 ]
}`
}

func runAttentionLetter(t *testing.T, report string) (string, error) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable; the letter renderer is python")
	}
	script := filepath.Join("..", "..", "scripts", "attention-letter.py")
	cmd := exec.Command(python, script)
	cmd.Stdin = strings.NewReader(report)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.String() + stderr.String(), err
	}
	return out.String(), nil
}

// TestAttentionLetterRendersPlainCardsWithinGradeCeiling is the digest half of
// the plain-language contract: every needs_you line carries the group's card
// (what/why plus the reply action), the retired generic fallback never ships,
// and the whole letter passes the shared eighth-grade ceiling — graded here by
// the Go seam over the python renderer's actual output.
func TestAttentionLetterRendersPlainCardsWithinGradeCeiling(t *testing.T) {
	letter, err := runAttentionLetter(t, letterFixtureReport())
	if err != nil {
		t.Fatalf("attention-letter failed: %v\n%s", err, letter)
	}
	if !strings.Contains(letter, "The judge panel needs your call.") {
		t.Fatalf("letter lost the authored card: %s", letter)
	}
	if !strings.Contains(letter, "Reply 01 DO or 01 HOLD.") {
		t.Fatalf("letter lost the one-word reply action: %s", letter)
	}
	if strings.Contains(letter, "needs your read") {
		t.Fatalf("retired generic fallback shipped in the digest letter: %s", letter)
	}
	if strings.Contains(letter, "instrumentation") {
		t.Fatalf("dense jargon card shipped instead of the plain count line: %s", letter)
	}
	if !strings.Contains(letter, "5 items wait on you") {
		t.Fatalf("dense card did not degrade to the plain count line: %s", letter)
	}
	if strings.Contains(letter, "Still watching.") {
		t.Fatalf("letter must render only needs_you groups: %s", letter)
	}
	for _, line := range strings.Split(strings.TrimSpace(letter), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if grade := reviewartifact.GradeLevel(line); grade > reviewartifact.ReadabilityGradeCeiling {
			t.Fatalf("letter line above grade %.1f (%.2f): %q", reviewartifact.ReadabilityGradeCeiling, grade, line)
		}
	}
	if size := len(letter); size > 3500 {
		t.Fatalf("letter exceeds the daily wrapper byte cap: %d", size)
	}
}

// TestAttentionLetterEmptyReport keeps the quiet path honest.
func TestAttentionLetterEmptyReport(t *testing.T) {
	letter, err := runAttentionLetter(t, `{"counts": {}, "groups": []}`)
	if err != nil {
		t.Fatalf("attention-letter failed on empty report: %v\n%s", err, letter)
	}
	if !strings.Contains(letter, "DECIDE 0 of 0 groups") {
		t.Fatalf("empty letter header wrong: %s", letter)
	}
}
