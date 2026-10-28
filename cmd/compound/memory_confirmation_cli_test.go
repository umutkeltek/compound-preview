package main

import (
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestMemoryAdjudicateExplicitConfirmationCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	spanRef := "transcript-span:sha256:session:line:1:sha256:preference"
	captured, err := store.Capture([]mem.Candidate{{
		Entity:       "operator",
		Predicate:    "preference.communication",
		Object:       "prefer concise progress updates",
		Confidence:   0.95,
		Derivation:   "extracted",
		ClaimType:    "preference",
		Visibility:   "private",
		EvidenceRefs: []string{spanRef},
	}}, cliTestNow)
	if err != nil || len(captured) != 1 {
		t.Fatalf("capture = %+v, err=%v", captured, err)
	}
	if _, err := store.Adjudicate(captured[0].ID, candidates.Park, "work_user_shadow", cliTestNow); err != nil {
		t.Fatal(err)
	}
	confirmationOut := captureStdout(t, func() {
		cmdMemory([]string{
			"confirmation", captured[0].ID,
			"--quote", "Keep this as my communication preference.",
			"--reviewed-by", "operator:test",
		})
	})
	if !strings.Contains(confirmationOut, "recorded confirmation") {
		t.Fatalf("confirmation output = %q", confirmationOut)
	}
	evidence, err := store.Evidence()
	if err != nil || len(evidence) != 1 {
		t.Fatalf("confirmation evidence = %+v, err=%v", evidence, err)
	}
	confirmation := evidence[0]

	out := captureStdout(t, func() {
		cmdMemory([]string{
			"adjudicate", captured[0].ID,
			"--verdict", candidates.Promote,
			"--confirmation-ref", confirmation.ID,
			"--work", "work_user_shadow",
		})
	})
	if !strings.Contains(out, "promoted claim") {
		t.Fatalf("adjudicate output = %q", out)
	}
	claims, err := store.AllCurrent()
	if err != nil || len(claims) != 1 {
		t.Fatalf("current claims = %+v, err=%v", claims, err)
	}
	if claims[0].PromotionBasis != candidates.BasisExplicitConfirmation ||
		!stringSliceContains(claims[0].EvidenceRefs, spanRef) ||
		!stringSliceContains(claims[0].EvidenceRefs, confirmation.ID) {
		t.Fatalf("promoted claim lost confirmation provenance: %+v", claims[0])
	}
}
