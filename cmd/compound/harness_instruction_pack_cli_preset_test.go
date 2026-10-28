package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"compound/internal/harness"
)

// stripPASSBanner trims the trailing go-test PASS/FAIL line that re-exec'd
// subprocesses emit on stderr. CombinedOutput mixes both streams and the
// JSON we care about is the prefix. Used by P6 because the F5/F6 path
// either parses a single-line JSON or inspects output verbatim, while P6
// wants the rendered pack shape regardless of banner.
func stripPASSBanner(out []byte) []byte {
	if i := bytes.Index(out, []byte("\nPASS\n")); i >= 0 {
		return out[:i]
	}
	if i := bytes.Index(out, []byte("\nFAIL\t")); i >= 0 {
		return out[:i]
	}
	if i := bytes.Index(out, []byte("\nFAIL\n")); i >= 0 {
		return out[:i]
	}
	return out
}

// TestHarnessInstructionPackCLI_P6_PresetRenderAndDrift is the falsifier for
// the CLI surface of packet S2 (work_ff2ca97d915f):
//
//	--preset on the render verb must land on the preset path AND carry the
//	  preset in the body;
//	--drift --preset must compare against the preset path (not the default
//	  latest.json) and exit 1 + drift on a missing preset pack, exactly as
//	  the cold-start drift case does.
//
// The CLI exercises the full seam (cmd flag → main → harness) so this
// assertion catches regressions that internal-package tests would miss: a
// harness package that compiles but is no longer wired to the CLI.
func TestHarnessInstructionPackCLI_P6_PresetRenderAndDrift(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_CHILD") == "1" {
		// Mirror the F5/F6 re-entry: the parent built the fixture and
		// passes the home + flag combo; we re-exec cmdHarness inside the
		// subprocess so the os.Exit drift branch is observable. We use
		// -test.v=false so the PASS/FAIL banner from go test doesn't
		// contaminate CombinedOutput (the JSON output of cmdHarness
		// shares stdout with go test's test harness banner).
		preset := os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_NAME")
		action := os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_ACTION")
		args := []string{"instruction-pack", "--harness", "claude_code"}
		switch action {
		case "render":
			args = append(args, "--preset", preset, "--context-pack", os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_CONTEXT_ID"), "--json")
		case "drift":
			args = append(args, "--preset", preset, "--drift", "--json")
		case "driftmiss":
			args = append(args, "--preset", "never-rendered-preset.invalid", "--drift", "--json")
		case "badpreset":
			args = append(args, "--preset", "../escape", "--json")
		default:
			os.Exit(3)
		}
		cmdHarness(args)
		return
	}

	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(targetPath, []byte(`{"theme":"dark"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow:       []string{"Read", "mcp__gbrain__*"},
			Deny:        []string{"Bash(rm:*)"},
			Ask:         []string{"WebFetch"},
			DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	authPath := harness.PermissionAuthorityPath(homePath, "claude_code")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(homePath, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}

	contextID := recordCLIInstructionPackContext(t, homePath, "claude_code")
	run := func(name, preset, action string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessInstructionPackCLI_P6_PresetRenderAndDrift$", "-test.v=false")
		cmd.Env = append(os.Environ(),
			"COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_CHILD=1",
			"COMPOUND_TEST_INSTRUCTION_PACK_CONTEXT_ID="+contextID,
			"COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_NAME="+preset,
			"COMPOUND_TEST_INSTRUCTION_PACK_CLI_PRESET_ACTION="+action,
			"COMPOUND_HOME="+homePath,
		)
		return cmd.CombinedOutput()
	}

	// --- Render branch: --preset lands on the preset path. ---
	const preset = "minimax-exec"
	out, err := run(t.Name(), preset, "render")
	out = stripPASSBanner(out)
	if err != nil {
		t.Fatalf("--preset render must exit 0, got %v; output:\n%s", err, out)
	}
	var rendered harness.InstructionPack
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("parse render JSON: %v\n%s", err, out)
	}
	if rendered.Preset != preset {
		t.Fatalf("rendered pack must carry preset %q, got %q", preset, rendered.Preset)
	}
	presetPath := filepath.Join(homePath, "context", "instruction-packs", "claude_code", preset, "latest.json")
	defaultPath := filepath.Join(homePath, "context", "instruction-packs", "claude_code", "latest.json")
	if _, err := os.Stat(presetPath); err != nil {
		t.Fatalf("preset pack not written to %s: %v", presetPath, err)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("--preset render must NOT write the default pack; default path exists: %v", err)
	}

	// --- Drift branch: --preset against the rendered file is clean. ---
	out, err = run(t.Name(), preset, "drift")
	out = stripPASSBanner(out)
	var exitErr *exec.ExitError
	if err != nil || errors.As(err, &exitErr) && exitErr.ExitCode() != 0 {
		t.Fatalf("--drift on freshly-rendered preset pack must exit 0, got %v; output:\n%s", err, out)
	}
	var cleanReport harness.InstructionPackDriftReport
	if err := json.Unmarshal(out, &cleanReport); err != nil {
		t.Fatalf("parse drift JSON: %v\n%s", err, out)
	}
	if cleanReport.Drifted {
		t.Fatalf("fresh preset pack must be drift-clean: %+v", cleanReport)
	}
	if cleanReport.Preset != preset {
		t.Fatalf("drift report must carry preset %q, got %q", preset, cleanReport.Preset)
	}
	if cleanReport.Path != presetPath {
		t.Fatalf("drift report path = %s, want %s", cleanReport.Path, presetPath)
	}

	// --- Drift branch: missing preset pack → exit 1, Drift=true, details
	// carry the missing-on-disk sentinel. ---
	out, err = run(t.Name(), preset, "driftmiss")
	out = stripPASSBanner(out)
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("--drift on missing preset pack must exit 1, got %v; output:\n%s", err, out)
	}
	var missingReport harness.InstructionPackDriftReport
	if err := json.Unmarshal(out, &missingReport); err != nil {
		t.Fatalf("parse missing-drift JSON: %v\n%s", err, out)
	}
	if !missingReport.Drifted {
		t.Fatalf("missing preset must drift: %+v", missingReport)
	}
	if missingReport.ObservedDigest != "" {
		t.Fatalf("missing preset observed digest must be empty, got %s", missingReport.ObservedDigest)
	}
	if missingReport.Path != filepath.Join(homePath, "context", "instruction-packs", "claude_code", "never-rendered-preset.invalid", "latest.json") {
		t.Fatalf("missing preset path mismatch: %s", missingReport.Path)
	}

	// --- Invalid preset via CLI must surface a non-zero exit. ---
	if _, err = run(t.Name(), preset, "badpreset"); err == nil {
		t.Fatalf("invalid --preset must error out")
	}
}
