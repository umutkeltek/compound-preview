package lanes

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/harness"
)

// TestRedProofAliasVersusCanonicalDrifts pins the original R3B 2026-09-04
// bug in writing: install.sh's loop rendered packs under the alias
// `claude-minimax` (with dash) but the operator's `--drift` check uses
// the canonical id `claude_minimax` (with underscore) the registry
// uses. Those two strings land at DIFFERENT on-disk paths under
// $COMPOUND_HOME/context/instruction-packs/<...>/latest.json, so the
// canonical path stays drifted even after install.sh runs. The test
// reproduces that divergence byte-for-byte — without RefreshLane ever
// being involved — so the symptom that motivated R3B is documented in
// the test suite. The fix (RefreshLane + install.sh invoking it on
// canonical ids) is verified by the rest of the file; this test is the
// receipt that "without the fix, drift really is drifted".
func TestRedProofAliasVersusCanonicalDrifts(t *testing.T) {
	homePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "context", "instruction-packs"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: compound.installed_hooks.v0.1\n" +
		"hooks:\n" +
		" - harness: claude_minimax\n" +
		"   name: PreToolUse\n" +
		"   event: PreToolUse\n" +
		"   ref: compound\n" +
		"   revision: binary:/p/old\n" +
		"   status: current\n"
	if err := os.WriteFile(filepath.Join(homePath, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Step 1: install.sh's loop calls RenderInstructionPack under the
	// alias `claude-minimax`. This writes the alias path.
	if _, err := harness.RenderInstructionPack(homePath, "claude-minimax", "", time.Now()); err != nil {
		t.Fatalf("alias render: %v", err)
	}

	// Step 2: the operator checks the CANONICAL id `claude_minimax`.
	// Those are different on-disk paths. The canonical path was never
	// written, so drift must be `true`.
	report, err := harness.InstructionPackDrift(homePath, "claude_minimax", "", time.Now())
	if err != nil {
		t.Fatalf("canonical drift check: %v", err)
	}
	if !report.Drifted {
		t.Fatalf("R3B red proof: canonical-path drift = clean, want drifted (operator would not see the bug)")
	}
	t.Logf("R3B red proof: canonical-path drift = drifted (details=%v) — this is the symptom install.sh exhibited before R3B", report.Details)
}
