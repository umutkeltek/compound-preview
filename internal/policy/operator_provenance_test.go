package policy

import "testing"

func TestHasOperatorProvenance(t *testing.T) {
	for _, actor := range []string{"operator", "operator-console", "operator:local", "human-review", "human:local", " OPERATOR:LOCAL "} {
		if !HasOperatorProvenance(actor) {
			t.Errorf("operator provenance rejected: %q", actor)
		}
	}
	for _, actor := range []string{"", "director", "codex", "example-reviewer", "test-operator", "rented-executor"} {
		if HasOperatorProvenance(actor) {
			t.Errorf("executor provenance accepted as operator: %q", actor)
		}
	}
}
