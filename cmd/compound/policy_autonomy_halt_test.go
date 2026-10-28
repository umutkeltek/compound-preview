package main

import (
	"strings"
	"testing"

	"compound/internal/policy"
)

// TestPolicyAutonomyHaltSurfacesState proves `compound policy autonomy-halt` reads the
// live kill-switch: an unset home reports not-halted, and an engaged global + per-class
// halt is surfaced in both the JSON and text forms.
func TestPolicyAutonomyHaltSurfacesState(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	unset := captureStdout(t, func() { cmdPolicy([]string{"autonomy-halt", "--json"}) })
	for _, want := range []string{
		`"authority_ref": "compound://registries/autonomy-halt"`,
		`"installed": false`,
		`"global_halt": false`,
		`"source": "unset"`,
	} {
		if !strings.Contains(unset, want) {
			t.Fatalf("unset autonomy-halt output missing %q:\n%s", want, unset)
		}
	}

	if err := policy.WriteAutonomyHalt(homePath, policy.AutonomyHalt{
		Global:          true,
		Reason:          "incident review",
		DisabledClasses: map[string]string{"job_health": "noisy"},
	}); err != nil {
		t.Fatalf("write halt: %v", err)
	}

	setJSON := captureStdout(t, func() { cmdPolicy([]string{"autonomy-halt", "--json"}) })
	for _, want := range []string{
		`"installed": true`,
		`"global_halt": true`,
		`"reason": "incident review"`,
		`"class": "job_health"`,
		`"disabled_class_count": 1`,
	} {
		if !strings.Contains(setJSON, want) {
			t.Fatalf("engaged autonomy-halt --json missing %q:\n%s", want, setJSON)
		}
	}

	setText := captureStdout(t, func() { cmdPolicy([]string{"autonomy-halt"}) })
	for _, want := range []string{"global_halt: true", "reason: incident review", "job_health"} {
		if !strings.Contains(setText, want) {
			t.Fatalf("engaged autonomy-halt text missing %q:\n%s", want, setText)
		}
	}
}

func TestPolicyAutonomyHaltCommandMutatesGlobalAndClassState(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	setGlobal := captureStdout(t, func() {
		cmdPolicy([]string{"autonomy-halt", "--global", "--reason", "incident response", "--json"})
	})
	for _, want := range []string{`"global_halt": true`, `"reason": "incident response"`} {
		if !strings.Contains(setGlobal, want) {
			t.Fatalf("global halt mutation missing %q:\n%s", want, setGlobal)
		}
	}

	disableClass := captureStdout(t, func() {
		cmdPolicy([]string{"autonomy-halt", "--disable-class", "job_health", "--reason", "noisy evidence", "--json"})
	})
	for _, want := range []string{`"class": "job_health"`, `"reason": "noisy evidence"`} {
		if !strings.Contains(disableClass, want) {
			t.Fatalf("class halt mutation missing %q:\n%s", want, disableClass)
		}
	}

	cleared := captureStdout(t, func() {
		cmdPolicy([]string{"autonomy-halt", "--clear-global", "--json"})
	})
	if !strings.Contains(cleared, `"global_halt": false`) || !strings.Contains(cleared, `"class": "job_health"`) {
		t.Fatalf("clearing global halt must preserve class halt:\n%s", cleared)
	}

	enabled := captureStdout(t, func() {
		cmdPolicy([]string{"autonomy-halt", "--enable-class", "job_health", "--json"})
	})
	if !strings.Contains(enabled, `"disabled_class_count": 0`) {
		t.Fatalf("enabling class did not clear class halt:\n%s", enabled)
	}
}
