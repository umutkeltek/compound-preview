package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/contract"
	"compound/internal/harness"
)

// captureStderr mirrors captureStdout for the stderr stream — needed by
// F3 because the usage-guard branch writes to stderr and exits non-zero.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestHarnessInstructionPackCLI_F1_JSONShape exercises the new
// `compound harness instruction-pack --harness <name> --json` verb: it must
// write $COMPOUND_HOME/context/instruction-packs/<h>/latest.json AND emit
// the rendered pack as JSON. The fixture mirrors the harness pkg's own
// buildInstructionPackFullFixture recipe so the CLI exercise faithfully
// reflects what RenderInstructionPack does end-to-end.
func TestHarnessInstructionPackCLI_F1_JSONShape(t *testing.T) {
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
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	authPath := harness.PermissionAuthorityPath(homePath, "claude_code")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
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
	out := captureStdout(t, func() {
		cmdHarness([]string{"instruction-pack", "--harness", "claude_code", "--context-pack", contextID, "--json"})
	})
	var got harness.InstructionPack
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse instruction-pack JSON: %v\n%s", err, out)
	}
	if got.Harness != "claude_code" {
		t.Fatalf("harness = %q, want claude_code", got.Harness)
	}
	if got.SchemaVersion != harness.InstructionPackSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", got.SchemaVersion, harness.InstructionPackSchemaVersion)
	}
	if got.PolicyCore.Absent {
		t.Fatalf("PolicyCore must be populated when authority exists: %+v", got.PolicyCore)
	}
	if got.Posture.Absent {
		t.Fatalf("Posture must be populated when an artifact exists: %+v", got.Posture)
	}

	// File on disk under temp home?
	written, err := os.Stat(harness.InstructionPackPath(homePath, "claude_code", ""))
	if err != nil {
		t.Fatalf("pack latest.json not written: %v", err)
	}
	if written.Size() == 0 {
		t.Fatalf("pack latest.json was empty")
	}
}

// TestHarnessInstructionPackCLI_F2_TextShape exercises the text branch —
// no --json → human-readable key=value lines and a content_sha256 banner.
func TestHarnessInstructionPackCLI_F2_TextShape(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	// Empty home — degraded render still must print all expected keys.
	out := captureStdout(t, func() {
		cmdHarness([]string{"instruction-pack", "--harness", "claude_code"})
	})
	for _, want := range []string{
		"# harness instruction-pack",
		"- harness: claude_code",
		"- schema_version:",
		"- policy_core:",
		"- posture:",
		"- installed_hooks:",
		"- effective_contract:",
		"- registry_revision:",
		"- content_sha256:",
		"- written_to:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q:\n%s", want, out)
		}
	}
}

// TestHarnessInstructionPackCLI_F3_UsageFailsOnEmptyHarness verifies the
// usage guard rejects missing --harness by writing a usage line to stderr
// and exiting non-zero. We use a subprocess because the in-process exit
// would terminate the test harness.
func TestHarnessInstructionPackCLI_F3_UsageFailsOnEmptyHarness(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_USAGE_CHILD") == "1" {
		homePath := t.TempDir()
		t.Setenv("COMPOUND_HOME", homePath)
		cmdHarness([]string{"instruction-pack"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessInstructionPackCLI_F3_UsageFailsOnEmptyHarness$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_INSTRUCTION_PACK_USAGE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("missing --harness must exit non-zero; output=%s", out)
	}
	if !strings.Contains(string(out), "usage:") {
		t.Fatalf("missing usage stderr: %s", out)
	}
}

// TestHarnessInstructionPackCLI_F4_DriftClean exercises the wired verb:
// `compound harness instruction-pack --harness <h> --drift --json` on a
// clean fixture must read back as Drifted=false, write nothing under
// $COMPOUND_HOME beyond what the initial render already produced, and exit
// 0. This is the no-op half of the F4 falsifier.
func TestHarnessInstructionPackCLI_F4_DriftClean(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    filepath.Join(t.TempDir(), "settings.json"),
		Permissions: harness.PermissionRules{
			Allow:       []string{"Read"},
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
	body, _ := json.Marshal(policy)
	if err := os.WriteFile(authPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	contextID := recordCLIInstructionPackContext(t, homePath, "claude_code")
	if _, err := contract.RenderInstructionPackForContext(homePath, "claude_code", "", contextID, cliTestNow); err != nil {
		t.Fatal(err)
	}
	filesBefore := countFiles(t, homePath)

	out := captureStdout(t, func() {
		cmdHarness([]string{"instruction-pack", "--harness", "claude_code", "--drift", "--json"})
	})
	var report harness.InstructionPackDriftReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse drift json: %v\n%s", err, out)
	}
	if report.Drifted {
		t.Fatalf("clean fixture must not drift: %+v", report)
	}
	if report.Harness != "claude_code" {
		t.Fatalf("harness = %q, want claude_code", report.Harness)
	}
	if got := countFiles(t, homePath); got != filesBefore {
		t.Fatalf("--drift without --emit-divergence must write nothing: files before=%d after=%d",
			filesBefore, got)
	}
}

// TestHarnessInstructionPackCLI_F5_DriftNoEmitNoWrite verifies the F5
// falsifier: --drift WITHOUT --emit-divergence on a drifted fixture must
// exit 1 (the verb's exit contract mirrors `compound harness drift`) and
// must NOT write any observation batch under
// state/instruction-pack-drift-observations/ — the emission code path must
// never run without the flag.
func TestHarnessInstructionPackCLI_F5_DriftNoEmitNoWrite(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_DRIFT_NO_EMIT_CHILD") == "1" {
		// Reuse the parent's pre-built drift via the COMPOUND_HOME it set
		// (writing a fresh fixture here would diverge from the drift the
		// parent asserted). cmdHarness exits 1 in-process via os.Exit and
		// terminates the subprocess.
		cmdHarness([]string{"instruction-pack", "--harness", "claude_code", "--drift", "--json"})
		return
	}
	homePath, targetPath, render := writeInstructionPackDriftObserveFixture(t)
	driftInstructionPackFixture(t, homePath, targetPath, render)
	filesBefore := countFiles(t, homePath)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessInstructionPackCLI_F5_DriftNoEmitNoWrite$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_INSTRUCTION_PACK_DRIFT_NO_EMIT_CHILD=1", "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("--drift on drifted fixture without --emit-divergence must exit 1, got %v; output:\n%s",
			err, out)
	}
	var report harness.InstructionPackDriftReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse drift json: %v\n%s", err, out)
	}
	if !report.Drifted {
		t.Fatalf("fixture should still drift: %+v", report)
	}
	if got := countFiles(t, homePath); got != filesBefore {
		t.Fatalf("--drift without --emit-divergence must write nothing: files before=%d after=%d",
			filesBefore, got)
	}
}

// TestHarnessInstructionPackCLI_F6_DriftEmitWritesObservation verifies the
// wired --drift --emit-divergence path end-to-end: a drifted fixture must
// (a) exit 1, (b) write an observation batch under
// state/instruction-pack-drift-observations/, and (c) carry the
// observation_applied side-channel in the JSON output so downstream tools
// can confirm the emission.
func TestHarnessInstructionPackCLI_F6_DriftEmitWritesObservation(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_INSTRUCTION_PACK_DRIFT_EMIT_CHILD") == "1" {
		// Reuse the parent's pre-built drift via the COMPOUND_HOME it set.
		cmdHarness([]string{"instruction-pack", "--harness", "claude_code", "--drift", "--emit-divergence", "--json"})
		return
	}
	homePath, targetPath, render := writeInstructionPackDriftObserveFixture(t)
	driftInstructionPackFixture(t, homePath, targetPath, render)
	filesBefore := countFiles(t, homePath)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessInstructionPackCLI_F6_DriftEmitWritesObservation$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_INSTRUCTION_PACK_DRIFT_EMIT_CHILD=1", "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("--drift --emit-divergence on drifted fixture must exit 1, got %v; output:\n%s",
			err, out)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("parse emitted JSON: %v\n%s", err, out)
	}
	if applied, ok := raw["observation_applied"].(bool); !ok || !applied {
		t.Fatalf("JSON output must carry observation_applied=true, got %s", out)
	}
	if id, _ := raw["observation_id"].(string); id == "" {
		t.Fatalf("JSON output must carry observation_id, got %s", out)
	}
	if got := countFiles(t, homePath); got <= filesBefore {
		t.Fatalf("--drift --emit-divergence must write at least one new file: files before=%d after=%d",
			filesBefore, got)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "state", "instruction-pack-drift-observations"))
	if err != nil {
		t.Fatalf("state/instruction-pack-drift-observations/ not created: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("emission directory must contain at least one batch file")
	}
}

func recordCLIInstructionPackContext(t *testing.T, homePath, harnessName string) string {
	t.Helper()
	in := contract.Input{Harness: harnessName, Cwd: t.TempDir(), Task: "review synthetic fixtures", RepoKind: "product", RepoKindVia: "flag"}
	sealed, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: contract.Compile(in, contract.DefaultPolicy(), cliTestNow),
		ContextPack:       contract.Pack(in, contract.DefaultPolicy(), cliTestNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RecordRuntimeMaterialization(homePath, sealed); err != nil {
		t.Fatal(err)
	}
	return sealed.ContextPack.ID
}

func seedCLIInstructionPackContext(t *testing.T, homePath, harnessName, preset string) {
	t.Helper()
	contextID := recordCLIInstructionPackContext(t, homePath, harnessName)
	if _, err := contract.RenderInstructionPackForContext(homePath, harnessName, preset, contextID, cliTestNow); err != nil {
		t.Fatal(err)
	}
}
