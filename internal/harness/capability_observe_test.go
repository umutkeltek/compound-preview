package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLaneFixture(t *testing.T, settings, wrapper string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "settings.json")
	wp := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(sp, []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if err := os.WriteFile(wp, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return sp, wp
}

// This is the real shape of the lane that was mis-declared: a settings file with
// no hooks key whatsoever, launched with skills and MCP switched off, registered
// as having every hook plus skills plus MCP. Eight over-claims, all of which
// routed work to an executor that could not report back.
func TestObserveCatchesEveryOverclaimOnALaneWithNoHooksAndNoMCP(t *testing.T) {
	settings := `{"model":"example","effortLevel":"high","permissions":{},"theme":"dark"}`
	wrapper := "#!/bin/zsh\nexec sb run claude --with example --no-mcp --no-skills \"$@\"\n"
	sp, wp := writeLaneFixture(t, settings, wrapper)

	observed, err := ObserveLaneCapabilities(sp, wp)
	if err != nil {
		t.Fatalf("ObserveLaneCapabilities: %v", err)
	}
	declared := Capabilities{
		SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: true,
		PostToolUseEvent: true, StopFeedback: true, SessionEndReceipt: true,
		NativeSkills: true, MCP: true,
	}
	report := CapabilityDrift("example_lane", declared, observed)
	if !report.Drifted {
		t.Fatal("a registry claiming hooks+skills+mcp over a lane with none must drift")
	}
	if report.Overclaims != 8 {
		t.Fatalf("want 8 overclaims, got %d: %+v", report.Overclaims, report.Mismatches)
	}
	for _, m := range report.Mismatches {
		if !m.Overclaimed {
			t.Fatalf("every mismatch here is an overclaim; %s was not marked", m.Capability)
		}
	}
}

// A lane that really does register hooks must NOT be reported as drifted, or the
// check becomes noise and gets ignored — which is how the original lie survived.
func TestObserveAcceptsALaneWhoseFilesBackItsDeclaration(t *testing.T) {
	settings := `{"hooks":{"SessionStart":[],"UserPromptSubmit":[],"PreToolUse":[]}}`
	wrapper := "#!/bin/zsh\nexec sb run claude --with example \"$@\"\n"
	sp, wp := writeLaneFixture(t, settings, wrapper)

	observed, err := ObserveLaneCapabilities(sp, wp)
	if err != nil {
		t.Fatalf("ObserveLaneCapabilities: %v", err)
	}
	declared := Capabilities{
		SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: true,
		NativeSkills: true, MCP: true,
	}
	report := CapabilityDrift("example_lane", declared, observed)
	if report.Drifted {
		t.Fatalf("a backed declaration must not drift: %+v", report.Mismatches)
	}
}

// Under-claiming is a mismatch but not an overclaim. Conflating them would rank a
// wasted capability alongside work routed into silence.
func TestUnderclaimIsDriftButNotAnOverclaim(t *testing.T) {
	settings := `{"hooks":{"SessionStart":[]}}`
	wrapper := "#!/bin/zsh\nexec sb run claude --with example \"$@\"\n"
	sp, wp := writeLaneFixture(t, settings, wrapper)

	observed, err := ObserveLaneCapabilities(sp, wp)
	if err != nil {
		t.Fatalf("ObserveLaneCapabilities: %v", err)
	}
	report := CapabilityDrift("example_lane", Capabilities{}, observed)
	if !report.Drifted {
		t.Fatal("declared-false vs observed-true is still drift")
	}
	if report.Overclaims != 0 {
		t.Fatalf("under-claiming must not count as an overclaim, got %d", report.Overclaims)
	}
}

// An unreadable settings path must be an error, never a confident "no hooks".
// The lane doctor already reports a settings path nothing writes to; silently
// reading that as "this harness has no hooks" would launder the bug into a fact.
func TestUnreadableSettingsIsAnErrorNotAConfidentNegative(t *testing.T) {
	_, wp := writeLaneFixture(t, `{}`, "#!/bin/zsh\n")
	if _, err := ObserveLaneCapabilities(filepath.Join(t.TempDir(), "absent.json"), wp); err == nil {
		t.Fatal("a missing settings file must error, not report zero capabilities")
	}
	sp, _ := writeLaneFixture(t, `{}`, "#!/bin/zsh\n")
	if _, err := ObserveLaneCapabilities(sp, filepath.Join(t.TempDir(), "absent-wrapper")); err == nil {
		t.Fatal("a missing wrapper must error, not imply skills and MCP are enabled")
	}
}
