package harness

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Falsifiers P1-P5 for packet S2 (work_ff2ca97d915f): instruction packs keyed
// by (harness, preset).
//
// The preset axis exists so two switchback presets over ONE harness produce
// distinct, content-addressed packs instead of racing for the same
// latest.json. The load-bearing constraint is that the axis is ADDITIVE: an
// empty preset must reproduce today's path AND today's pack identity byte for
// byte, or every already-rendered pack in every existing home silently
// re-identifies on upgrade.
//
// These tests deliberately pin literals (a hardcoded path string in P1, a
// hardcoded pre-change hash in P4) rather than comparing against the
// functions under test. A test that asks the new code what the old code
// would have said cannot detect the regression it exists to catch.

// instructionPackPresetCountFiles counts regular files under root. P5 uses it
// to prove a rejected preset is inert — the harness package cannot reach
// cmd/compound's countFiles, so this mirrors it locally.
func instructionPackPresetCountFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return count
}

// TestInstructionPackPath_P1_EmptyPresetMatchesPreChangeLiteral pins the
// empty-preset path against a HARDCODED literal captured from the pre-change
// implementation. Comparing against a re-derived filepath.Join would pass
// even if the whole scheme moved, which is exactly the regression that would
// orphan every existing on-disk pack.
func TestInstructionPackPath_P1_EmptyPresetMatchesPreChangeLiteral(t *testing.T) {
	const home = "/tmp/compound-home.invalid"
	// Literal captured from the pre-change InstructionPackPath at base
	// 50e8496a — NOT re-derived from the function under test.
	const wantPreChange = "/tmp/compound-home.invalid/context/instruction-packs/claude_code/latest.json"

	if got := InstructionPackPath(home, "claude_code", ""); got != wantPreChange {
		t.Fatalf("empty preset must reproduce the pre-change path\n got: %s\nwant: %s", got, wantPreChange)
	}

	// A non-empty preset inserts exactly one segment before latest.json.
	const wantPreset = "/tmp/compound-home.invalid/context/instruction-packs/claude_code/minimax-exec/latest.json"
	if got := InstructionPackPath(home, "claude_code", "minimax-exec"); got != wantPreset {
		t.Fatalf("preset path mismatch\n got: %s\nwant: %s", got, wantPreset)
	}

	// Whitespace around a preset must not fork identity.
	if got := InstructionPackPath(home, "claude_code", "  minimax-exec  "); got != wantPreset {
		t.Fatalf("preset must be trimmed\n got: %s\nwant: %s", got, wantPreset)
	}
}

// TestRenderInstructionPack_P2_PresetForksPathAndIdentity is the core claim of
// the packet: two renders over the SAME home, differing ONLY in preset, must
// land on different paths and carry different identities. Same home means
// every layer input (policy_core, posture, roster) is identical, so any
// identity difference can only come from the preset axis itself.
func TestRenderInstructionPack_P2_PresetForksPathAndIdentity(t *testing.T) {
	homePath, _, _ := buildInstructionPackFullFixture(t)

	base, err := RenderInstructionPack(homePath, "claude_code", "", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	presetPack, err := RenderInstructionPack(homePath, "claude_code", "minimax-exec", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}

	basePath := InstructionPackPath(homePath, "claude_code", "")
	presetPath := InstructionPackPath(homePath, "claude_code", "minimax-exec")
	if basePath == presetPath {
		t.Fatalf("preset must fork the write path, both were %s", basePath)
	}

	// Both files exist independently — the preset render must not have
	// clobbered or moved the default pack.
	for _, p := range []string{basePath, presetPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected pack at %s: %v", p, err)
		}
	}

	if base.ContentSHA256 == presetPack.ContentSHA256 {
		t.Fatalf("two presets over identical layer inputs must not share identity: %s", base.ContentSHA256)
	}
	if base.Preset != "" {
		t.Fatalf("default render must carry no preset, got %q", base.Preset)
	}
	if presetPack.Preset != "minimax-exec" {
		t.Fatalf("preset render must record its preset, got %q", presetPack.Preset)
	}
}

// TestRenderInstructionPack_P3_TwoPresetsDistinctAndEachDriftClean proves the
// falsifier the envelope names directly: two presets over ONE harness yield
// distinct pack identities, and the drift comparator resolves each against
// its OWN path rather than collapsing them. Without per-preset path scoping
// the second render would overwrite the first and both drift probes would
// still read clean — so drift-clean alone is not sufficient evidence; the
// identity inequality below is what makes it binding.
func TestRenderInstructionPack_P3_TwoPresetsDistinctAndEachDriftClean(t *testing.T) {
	homePath, _, _ := buildInstructionPackFullFixture(t)

	presetA, err := RenderInstructionPackWithContractRef(homePath, "claude_code", "preset-a", instructionPackFixtureContractRef(), instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	presetB, err := RenderInstructionPackWithContractRef(homePath, "claude_code", "preset-b", instructionPackFixtureContractRef(), instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}

	if presetA.ContentSHA256 == presetB.ContentSHA256 {
		t.Fatalf("distinct presets must yield distinct identities, both were %s", presetA.ContentSHA256)
	}

	// Each preset pack must be drift-clean against its own path, and the
	// two paths must be genuinely different files.
	pathA := InstructionPackPath(homePath, "claude_code", "preset-a")
	pathB := InstructionPackPath(homePath, "claude_code", "preset-b")
	if pathA == pathB {
		t.Fatalf("preset paths collided: %s", pathA)
	}

	for _, tc := range []struct {
		preset string
		want   string
	}{
		{"preset-a", presetA.ContentSHA256},
		{"preset-b", presetB.ContentSHA256},
	} {
		report, err := InstructionPackDrift(homePath, "claude_code", tc.preset, instructionPackTestNow)
		if err != nil {
			t.Fatalf("drift(%s): %v", tc.preset, err)
		}
		if report.Drifted {
			t.Fatalf("preset %s should be drift-clean against its own path: %+v", tc.preset, report)
		}
		if report.Preset != tc.preset {
			t.Fatalf("drift report must carry its preset, got %q want %q", report.Preset, tc.preset)
		}
		if report.Path != InstructionPackPath(homePath, "claude_code", tc.preset) {
			t.Fatalf("drift report path must be preset-scoped, got %s", report.Path)
		}

		// Read the on-disk file back and confirm it is the pack we think
		// it is — this is what proves the two presets did not overwrite
		// each other on the way to disk.
		body, err := os.ReadFile(report.Path)
		if err != nil {
			t.Fatal(err)
		}
		var observed InstructionPack
		if err := json.Unmarshal(body, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.ContentSHA256 != tc.want {
			t.Fatalf("on-disk pack for %s has identity %s, want %s (presets overwrote each other)",
				tc.preset, observed.ContentSHA256, tc.want)
		}
		if observed.Preset != tc.preset {
			t.Fatalf("on-disk pack preset mismatch: got %q want %q", observed.Preset, tc.preset)
		}
	}

	// A missing preset pack is drift (mirrors F11) and must not be masked
	// by the sibling preset's file existing.
	missing, err := InstructionPackDrift(homePath, "claude_code", "preset-never-rendered", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !missing.Drifted {
		t.Fatalf("unrendered preset must report drift: %+v", missing)
	}
	if missing.ObservedDigest != "" {
		t.Fatalf("missing preset pack must have empty observed digest, got %s", missing.ObservedDigest)
	}
}

// TestRenderInstructionPack_P4_EmptyPresetIdentityUnchanged is the
// backward-compatibility floor: an empty preset must NOT perturb the hash
// beyond whatever the home's own inputs already impose, AND must add no
// `preset` key to the on-disk body.
//
// The obvious way to prove this — pinning the empty-preset hash against a
// main-branch commit SHA — rots every time main moves the registry (or any
// other body term) and becomes a falsifier that fails for reasons
// unrelated to the behavior it guards. Instead, P4 derives what an
// empty-preset identity *should* be at runtime, by reusing the same code
// path with two complementary observations:
//
//	(a) id stability — two empty-preset renders over the same home at the
//	    same clock must be byte-identical. If they ever differ, the preset
//	    axis is leaking clock or randomness into the body.
//
//	(b) preset sensitivity — a non-empty preset over the same home must
//	    produce a DIFFERENT identity. If it does not, the preset axis is
//	    inert and the wire feature has no effect.
//
// The conjunction: empty is stable AND empty differs from non-empty. That
// pins "empty preset = today, exactly" without naming any external commit.
//
// Byte-level absence (no `"preset":` key in the on-disk body) is asserted
// separately because it is a file-format invariant, not a function-of-state
// observation — empty-preset stays absent forever, regardless of main.
func TestRenderInstructionPack_P4_EmptyPresetIdentityUnchanged(t *testing.T) {
	homePath := t.TempDir()

	// (a) Two empty-preset renders over the same home must be identical.
	first, err := RenderInstructionPack(homePath, "claude_code", "", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderInstructionPack(homePath, "claude_code", "", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentSHA256 != second.ContentSHA256 {
		t.Fatalf("empty-preset identity is not stable across renders\n first: %s\nsecond: %s",
			first.ContentSHA256, second.ContentSHA256)
	}
	if first.Preset != "" {
		t.Fatalf("empty preset must carry no Preset field, got %q", first.Preset)
	}

	// Byte-level absence check — runs every time, no external pin.
	body, err := os.ReadFile(InstructionPackPath(homePath, "claude_code", ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "preset") {
		t.Fatalf("empty-preset pack body must not mention preset:\n%s", string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["preset"]; ok {
		t.Fatal("empty-preset pack body must have no preset key")
	}

	// (b) A non-empty preset over the same home must diverge.
	presetPack, err := RenderInstructionPack(homePath, "claude_code", "minimax-exec", instructionPackTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if presetPack.ContentSHA256 == first.ContentSHA256 {
		t.Fatal("non-empty preset rendered over same inputs must not equal empty-preset identity — preset axis is inert")
	}
	if presetPack.Preset != "minimax-exec" {
		t.Fatalf("non-empty preset must record Preset field, got %q", presetPack.Preset)
	}
}

// TestRenderInstructionPack_P5_InvalidPresetRejected pins the path-traversal
// guard. The preset lands in a filesystem path, so a preset carrying a
// separator or a parent-dir hop would let a caller write outside the
// instruction-packs tree. Rejection must be loud AND inert — an error that
// still wrote a file would be worse than no guard, because it would look
// handled.
func TestRenderInstructionPack_P5_InvalidPresetRejected(t *testing.T) {
	for _, bad := range []string{
		"../escape",
		"..",
		"nested/preset",
		"back\\slash",
		"a/../../b",
		"/absolute",
	} {
		t.Run(bad, func(t *testing.T) {
			homePath := t.TempDir()
			before := instructionPackPresetCountFiles(t, homePath)

			_, err := RenderInstructionPack(homePath, "claude_code", bad, instructionPackTestNow)
			if err == nil {
				t.Fatalf("preset %q must be rejected", bad)
			}
			// %q in the validator escapes backslashes inside the message,
			// so the assertion goes through fmt.Sprintf("%q", bad) which
			// applies the same escaping — keeps the check robust to how
			// the validator formats the offending value.
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
				t.Fatalf("error must name the invalid preset %q, got: %v", bad, err)
			}

			if after := instructionPackPresetCountFiles(t, homePath); after != before {
				t.Fatalf("rejected preset wrote %d file(s); rejection must be inert", after-before)
			}

			// The drift comparator must reject identically — otherwise a
			// traversal preset could still be used to probe arbitrary paths.
			if _, err := InstructionPackDrift(homePath, "claude_code", bad, instructionPackTestNow); err == nil {
				t.Fatalf("drift must reject preset %q", bad)
			}
		})
	}
}
