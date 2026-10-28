package harness

import (
	"testing"

	"compound/internal/policy"
)

// The policy package cannot import harness (harness already imports policy),
// so it restates the two posture words it gates desktop surfaces on. This pins
// the restatement: if the registry ever renames a posture, the desktop-surface
// hold would silently stop matching "unattended" and every lane would be read
// as interactive.
func TestPolicyPostureVocabularyMatchesRegistry(t *testing.T) {
	if policy.PostureInteractive != PostureInteractive || policy.PostureUnattended != PostureUnattended {
		t.Fatalf("policy posture vocabulary (%q,%q) drifted from harness (%q,%q)",
			policy.PostureInteractive, policy.PostureUnattended, PostureInteractive, PostureUnattended)
	}
	registry := DefaultRegistry()
	for name, want := range map[string]string{
		"claude_code":    policy.PostureInteractive,
		"claude_minimax": policy.PostureUnattended,
		"never_heard_of": policy.PostureUnattended,
	} {
		if got := registry.Posture(name); got != want {
			t.Fatalf("registry posture for %s = %q, want %q", name, got, want)
		}
	}
}
