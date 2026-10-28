package schedule

import "testing"

func TestRepairPlanAcceptsExplicitTerminalHandoff(t *testing.T) {
	_, _, err := parseRepairFlags([]string{"plan", "--work", "work_successor", "--actor", "operator:fixture", "--plist", "/example/observer.plist", "--candidate", "/example/candidate.plist", "--source-proofs", "/example/proofs.json", "--handoff-from", "ldrp_predecessor"})
	if err != nil {
		t.Fatalf("explicit predecessor must reach the native owner for proof validation: %v", err)
	}
}
