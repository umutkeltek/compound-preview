package harness

import (
	"sort"
	"strings"
	"testing"
)

// knownNamingViolations is a MIGRATION LEDGER, not an excuse list. Each entry is
// a name that violates docs/NAMING.md and predates the rule. Recording it here
// keeps it visible and keeps the guard live for everything else: a NEW violation
// fails immediately, which is the property that matters.
//
// Renaming is a migration (add the new id, alias the old one with a warning,
// move readers, then remove the alias) -- not an edit, because operator shell
// history spells the old name and no repository tracks that.
//
// claude_zai_full is the highest-priority entry: it is the trap most likely to be
// re-entered, because it reads as "more than claude_zai" while having strictly
// fewer capabilities (claude_zai is a full profile with four hooks genuinely
// installed; claude_zai_full is a provider lane with none).
var knownNamingViolations = map[string]string{
	"claude_zai_full": "comparative suffix; has FEWER capabilities than claude_zai. Rename during the lane-vocabulary migration.",
}

func TestRegisteredHarnessNamesFollowTheNamingContract(t *testing.T) {
	registry := DefaultRegistry()
	names := make([]string, 0, len(registry.Harnesses))
	for name := range registry.Harnesses {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		violations := ValidateHarnessName(name)
		_, exempt := knownNamingViolations[name]
		switch {
		case len(violations) == 0 && exempt:
			// The migration happened. Drop the ledger entry so the guard stops
			// carrying a debt that no longer exists -- a stale exemption is a
			// silent permission for the next violation.
			t.Errorf(
				"harness %q is now compliant; remove it from knownNamingViolations",
				name,
			)
		case len(violations) > 0 && !exempt:
			t.Errorf(
				"harness %q violates docs/NAMING.md:\n  - %s",
				name, strings.Join(violations, "\n  - "),
			)
		}
	}
}

// The validator has to actually reject things. A guard that passes everything is
// indistinguishable from no guard, and this codebase already shipped a test that
// asserted the registry matched a copy of itself.
func TestValidateHarnessNameRejectsEachContractViolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"claude_zai_full", "comparative suffix"},
		{"claude_max", "comparative suffix"},
		{"claude_ultra", "encodes effort"},
		{"lane_xhigh", "encodes effort"},
		{"claude_free", "encodes cost"},
		{"claude_metered", "encodes cost"},
		{"claude-code", "not snake_case"},
		{"ClaudeCode", "not snake_case"},
		{"", "empty"},
	} {
		got := ValidateHarnessName(tc.name)
		if len(got) == 0 {
			t.Errorf("ValidateHarnessName(%q) accepted a violating name", tc.name)
			continue
		}
		if !strings.Contains(strings.Join(got, " "), tc.want) {
			t.Errorf("ValidateHarnessName(%q) = %v, want a violation mentioning %q", tc.name, got, tc.want)
		}
	}
}

// And it must accept legitimate names, including a vendor whose name merely
// contains a reserved token. Matching on substrings rather than segments would
// flag those and train everyone to ignore the rule.
func TestValidateHarnessNameAcceptsLegitimateNames(t *testing.T) {
	for _, name := range []string{
		"claude_code", "codex", "hermes", "opencode", "claude_minimax", "claude_qwen",
		"freeform_runner", // contains "free" but not as a segment
		"fastlane_agent",  // contains "fast" but not as a segment
	} {
		if got := ValidateHarnessName(name); len(got) != 0 {
			t.Errorf("ValidateHarnessName(%q) rejected a legitimate name: %v", name, got)
		}
	}
}
