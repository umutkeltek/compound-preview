package work

import (
	"strings"
	"testing"
)

// TestValidateWorkListFlagsCoversSilentIgnoreBug locks the bug:
// `compound work list --json` silently printed a fixed-width table (exit 0),
// and `compound work list --bogus` was accepted the same way, so typos and
// downstream jq failures both looked like "no entries". The validator must
// accept the canonical set and reject every other value with an error that
// names the canonical flags so the operator can recover.
func TestValidateWorkListFlagsCoversSilentIgnoreBug(t *testing.T) {
	t.Parallel()
	for _, ok := range workListFlags {
		if err := ValidateWorkListFlags([]string{ok}); err != nil {
			t.Errorf("ValidateWorkListFlags(%q) = %v, want nil", ok, err)
		}
	}
	// Default (no flags) must be accepted: callers that want JSON must opt
	// in with --json, and human-only invocations stay legal.
	if err := ValidateWorkListFlags(nil); err != nil {
		t.Errorf("ValidateWorkListFlags(nil) = %v, want nil", err)
	}
	bad := []string{
		"--bogus",
		"--JSON",
		"--json=pretty",
		"-j",
		"--source",
		"--state",
		"work_id_positional",
	}
	for _, b := range bad {
		err := ValidateWorkListFlags([]string{b})
		if err == nil {
			t.Errorf("ValidateWorkListFlags(%q) = nil, want error", b)
			continue
		}
		msg := err.Error()
		// Error message must name every valid flag so the operator can see
		// what they should have typed.
		for _, ok := range workListFlags {
			if !strings.Contains(msg, ok) {
				t.Errorf("error for %q does not name valid flag %q: %s", b, ok, msg)
			}
		}
	}
}
