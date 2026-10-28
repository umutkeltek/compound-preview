package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pin the current default blocks. Template changes must update these hashes
// and regenerate AGENTS.md/CLAUDE.md in the same change; task text stays out.
const (
	preChangeCodexSHA256      = "sha256:f190616a9f3434da175191054526c2214f04970c8b3a549113b3760ac12e36df"
	preChangeClaudeCodeSHA256 = "sha256:44f1b150a4e49622401e5a986834c4ede1560487beda58e2b3b100fd0c36aec2"
)

// Falsifier B1: default block ContentSHA256 is byte-identical pre/post change.
func TestDefaultBlockIsByteIdenticalPostChange(t *testing.T) {
	cases := []struct {
		harness   string
		preChange string
	}{
		{harness: "codex", preChange: preChangeCodexSHA256},
		{harness: "claude_code", preChange: preChangeClaudeCodeSHA256},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			// Target file intentionally missing — the render must still produce
			// the same Content (and same ContentSHA256) regardless of drift.
			in := Input{
				Harness:     tc.harness,
				Cwd:         root,
				Task:        "static parity proof",
				RepoKind:    "control-plane",
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			ec := Compile(in, DefaultPolicy(), fixedNow)
			// Render via the public default entry.
			render, err := RenderInstructionBlock(ec, root)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if render.ContentSHA256 != tc.preChange {
				t.Fatalf("%s default SHA256 drifted: pre=%s post=%s", tc.harness, tc.preChange, render.ContentSHA256)
			}
		})
	}
}

// Falsifier B3: the executor variant differs from default, contains the
// executor charter, contains `preset=<name>` in body, and the BEGIN/END
// marker lines are identical between the default and the variant.
func TestExecutorVariantDiffersAndCarriesCharterAndMarkers(t *testing.T) {
	cases := []struct {
		harness    string
		targetFile string
		preset     string
	}{
		{harness: "codex", targetFile: "AGENTS.md", preset: "executor"},
		{harness: "claude_code", targetFile: "CLAUDE.md", preset: "executor"},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.targetFile), []byte("# existing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			in := Input{
				Harness:     tc.harness,
				Cwd:         root,
				Task:        "first task",
				RepoKind:    "control-plane",
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			ec := Compile(in, DefaultPolicy(), fixedNow)

			def, err := RenderInstructionBlock(ec, root)
			if err != nil {
				t.Fatalf("default render: %v", err)
			}
			variant, err := RenderInstructionBlockVariant(ec, root, tc.preset)
			if err != nil {
				t.Fatalf("variant render: %v", err)
			}

			if variant.Content == def.Content {
				t.Fatalf("variant content equals default; variant must differ")
			}
			if variant.ContentSHA256 == def.ContentSHA256 {
				t.Fatalf("variant SHA256 equals default; variant must differ")
			}

			// Charter lines must be present in the variant, not in the default.
			charterLines := []string{
				"Work only inside the dispatched envelope/worktree",
				"Commit incrementally on the dispatched branch",
				"do not push, do not check out main",
				"Report back in the handoff format",
				"Escalate instead of improvising when blocked",
			}
			for _, want := range charterLines {
				if !strings.Contains(variant.Content, want) {
					t.Errorf("variant missing charter line %q:\n%s", want, variant.Content)
				}
				if strings.Contains(def.Content, want) {
					t.Errorf("default contains charter line %q (it must not):\n%s", want, def.Content)
				}
			}

			// preset=<name> must appear in the variant body, and the begin marker
			// must NOT carry the preset (markers are pre-change)
			bodyMarker := "preset=" + "`" + tc.preset + "`"
			if !strings.Contains(variant.Content, bodyMarker) {
				t.Errorf("variant body missing %q:\n%s", bodyMarker, variant.Content)
			}
			beginDefault := instructionBlockBeginMarker(tc.harness)
			beginVariant := extractBeginMarker(variant.Content)
			if beginDefault != beginVariant {
				t.Errorf("begin marker must be identical to default: default=%q variant=%q", beginDefault, beginVariant)
			}
			if !strings.Contains(variant.Content, instructionBlockEndMarker) {
				t.Errorf("variant is missing the end marker")
			}
			// Drift check is skipped for preset renders.
			if variant.Drifted {
				t.Errorf("variant reports drifted=true; preset renders must not drift-check")
			}
			if !containsDetail(variant.Details, "preset variant render; no tracked surface") {
				t.Errorf("variant must carry the no-tracked-surface detail; got %v", variant.Details)
			}
		})
	}
}

// Falsifier B4: empty preset == default exactly (same function path).
func TestEmptyPresetEqualsDefaultExactly(t *testing.T) {
	cases := []struct {
		harness    string
		targetFile string
	}{
		{harness: "codex", targetFile: "AGENTS.md"},
		{harness: "claude_code", targetFile: "CLAUDE.md"},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.targetFile), []byte("# existing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			in := Input{
				Harness:     tc.harness,
				Cwd:         root,
				Task:        "first task",
				RepoKind:    "control-plane",
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			ec := Compile(in, DefaultPolicy(), fixedNow)
			def, err := RenderInstructionBlock(ec, root)
			if err != nil {
				t.Fatalf("default: %v", err)
			}
			viaEmpty, err := RenderInstructionBlockVariant(ec, root, "   ")
			if err != nil {
				t.Fatalf("variant empty: %v", err)
			}
			if viaEmpty.Content != def.Content {
				t.Fatalf("empty preset must equal default exactly:\ndefault=%s\nempty=%s", def.Content, viaEmpty.Content)
			}
			if viaEmpty.ContentSHA256 != def.ContentSHA256 {
				t.Fatalf("empty preset SHA256 must equal default")
			}
			if viaEmpty.Drifted != def.Drifted {
				t.Fatalf("empty preset Drifted=%t, default Drifted=%t", viaEmpty.Drifted, def.Drifted)
			}
		})
	}
}

// Falsifier B7: variant does not leak task text. Mirrors the existing
// TestInstructionBlockIgnoresTaskTextAndDetectsOnlyManagedBlockDrift guard.
func TestExecutorVariantIgnoresTaskText(t *testing.T) {
	root := t.TempDir()
	first, err := RenderInstructionBlockVariant(instructionContractForTest("codex", "private task alpha"), root, "executor")
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderInstructionBlockVariant(instructionContractForTest("codex", "unrelated task beta"), root, "executor")
	if err != nil {
		t.Fatal(err)
	}
	if first.Content != second.Content || first.ContentSHA256 != second.ContentSHA256 {
		t.Fatalf("task text leaked into variant:\nfirst=%s\nsecond=%s", first.Content, second.Content)
	}
	if strings.Contains(first.Content, "private task alpha") || strings.Contains(first.Content, "unrelated task beta") {
		t.Fatalf("task-scoped text appeared in variant: %s", first.Content)
	}
}

// extractBeginMarker finds the BEGIN marker line in a rendered block. The
// marker schema is pinned by instructionBlockBeginMarker; this helper just
// resolves the rendered line so the falsifier can compare across presets.
func extractBeginMarker(content string) string {
	lines := strings.SplitN(content, "\n", 2)
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func containsDetail(details []string, want string) bool {
	for _, d := range details {
		if d == want {
			return true
		}
	}
	return false
}
