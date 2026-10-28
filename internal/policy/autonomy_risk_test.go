package policy

import (
	"testing"

	"compound/internal/envelope"
)

func TestAutonomyOperatorOnlyFrontierCannotBeNarrowedByConfig(t *testing.T) {
	tests := []envelope.ActionIntent{
		{MutationClass: "external_side_effect", Reversibility: "easy", BlastRadius: "local"},
		{MutationClass: "public", Reversibility: "easy", BlastRadius: "local"},
		{MutationClass: "destructive", Reversibility: "easy", BlastRadius: "local"},
		{MutationClass: "financial", Reversibility: "easy", BlastRadius: "local"},
		{MutationClass: "security", Reversibility: "easy", BlastRadius: "local"},
		{MutationClass: "local_reversible", Reversibility: "irreversible", BlastRadius: "local"},
		{MutationClass: "local_reversible", Reversibility: "hard", BlastRadius: "local"},
		{MutationClass: "local_reversible", Reversibility: "none", BlastRadius: "local"},
		{MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "public"},
	}
	for _, action := range tests {
		if reason := AutonomyOperatorOnlyReason(action, Config{}); reason == "" {
			t.Errorf("operator-only action passed empty config: %+v", action)
		}
	}
	if reason := AutonomyOperatorOnlyReason(envelope.ActionIntent{MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local"}, Config{}); reason != "" {
		t.Fatalf("local reversible action was denied: %s", reason)
	}
}
