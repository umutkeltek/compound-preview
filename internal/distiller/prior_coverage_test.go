package distiller

import "testing"

// Depth probe: checkPriorCoverage must derive its axis vocabulary from the
// PriorCoverage struct's json tags (one source), so EVERY field is checkable —
// including the six the old hand-typed map omitted (decisions, preferences,
// architectures, statuses, constraints, capability_nodes) — and a typo'd eval
// axis is reported as unknown rather than silently passing. This test fails if
// the single-source coupling regresses to a hand-maintained subset.
func TestCheckPriorCoverageSingleSource(t *testing.T) {
	got := PriorCoverage{
		CurrentClaims: 1, Decisions: 2, Preferences: 3, Architectures: 4,
		Statuses: 5, Constraints: 6, SkillCandidates: 7, DecisionCandidates: 8,
		FailurePatterns: 9, TasteObservations: 10, ProductHypotheses: 11,
		RouteObservations: 12, CapabilityObs: 13, EvalCases: 14, MethodUpgrades: 15,
		PolicyPatches: 16, CapabilityNodes: 17, NoPromoteDecisions: 18,
	}

	// The six axes the previous hand-typed map omitted must now pass when correct.
	want := map[string]int{
		"decisions": 2, "preferences": 3, "architectures": 4,
		"statuses": 5, "constraints": 6, "capability_nodes": 17,
	}
	for _, c := range checkPriorCoverage(want, got) {
		if c.Status != "pass" {
			t.Fatalf("axis %s: status=%s detail=%q, want pass", c.Name, c.Status, c.Detail)
		}
	}

	// Every json tag on the struct is a valid, checkable axis.
	for axis := range priorCoverageFields(got) {
		res := checkPriorCoverage(map[string]int{axis: 0}, got)
		if len(res) != 1 || res[0].Detail == "unknown field" {
			t.Fatalf("axis %q from struct tag reported unknown", axis)
		}
	}

	// A typo'd / non-struct axis is reported unknown (not silently passed).
	un := checkPriorCoverage(map[string]int{"bogus_axis": 1}, got)
	if len(un) != 1 || un[0].Status != "fail" || un[0].Detail != "unknown field" {
		t.Fatalf("bogus axis: got %+v, want one unknown-field fail", un)
	}

	// A wrong expected value on a real axis still fails.
	bad := checkPriorCoverage(map[string]int{"decisions": 999}, got)
	if len(bad) != 1 || bad[0].Status != "fail" {
		t.Fatalf("wrong value: got %+v, want fail", bad)
	}
}
