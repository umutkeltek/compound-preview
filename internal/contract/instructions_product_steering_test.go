package contract

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Nested product-steering takeover (outcome 2). The legacy umut-os marker
// stays a whole-file bootstrap; the nested product-steering pair is replaced
// in place with prefix/suffix preserved byte-for-byte. Any mixed-marker file
// refuses in BOTH normal and bootstrap modes without touching bytes.

const (
	umutOSLegacyFirstLine = "<!-- GENERATED from operating-system/umut-os/spine/steering.config.ts; do NOT hand-edit. -->"
	productSteeringBegin  = "<!-- BEGIN GENERATED spine/product-steering · do not edit · regen: bun umut-os/spine/gen-product-steering.ts --apply -->"
	productSteeringEnd    = "<!-- END GENERATED spine/product-steering -->"
	unknownVariantBegin   = "<!-- BEGIN GENERATED spine/product-steering · hand-edited variant -->"
)

func wrapWithProductSteering(prefix, suffix, inner string) string {
	return prefix + "\n" + productSteeringBegin + "\n" + inner + "\n" + productSteeringEnd + "\n" + suffix
}

func writeTarget(t *testing.T, root, name, body string) string {
	t.Helper()
	target := filepath.Join(root, name)
	if err := os.WriteFile(target, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return target
}

func mustRender(t *testing.T, harness, task, root string) InstructionBlockRender {
	t.Helper()
	render, err := RenderInstructionBlock(instructionContractForTest(harness, task), root)
	if err != nil {
		t.Fatal(err)
	}
	return render
}

// refuseAndPreserve asserts the materialization refuses and leaves the file
// byte-for-byte untouched.
func refuseAndPreserve(t *testing.T, render InstructionBlockRender, target, original string, bootstrap bool) {
	t.Helper()
	options := InstructionMaterializationOptions{BootstrapTakeover: bootstrap}
	if _, err := MaterializeInstructionBlock(render, options); err == nil {
		t.Fatalf("mixed-marker file accepted (bootstrap=%v):\n%s", bootstrap, original)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("refusal modified the file (bootstrap=%v):\nwant: %q\ngot:  %q", bootstrap, original, got)
	}
}

// Falsifier PS1: a file with ONLY the product-steering pair is replaced
// block-in-place; prefix/suffix survive byte-for-byte; second apply is a
// no-op normal materialization.
func TestBootstrapReplaceNestedProductSteeringReplacesBlockOnlyAndPreservesSurroundingText(t *testing.T) {
	for _, tc := range []struct {
		harness string
		target  string
		prefix  string
		suffix  string
	}{
		{harness: "codex", target: "AGENTS.md", prefix: "# project-wide prefix\n", suffix: "\n# project-wide suffix\n"},
		{harness: "claude_code", target: "CLAUDE.md", prefix: "# claude-wide prefix\n", suffix: "\n# claude-wide suffix\n"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			target := writeTarget(t, root, tc.target, wrapWithProductSteering(tc.prefix, tc.suffix, "stale product-steering prose\n"))
			render := mustRender(t, tc.harness, "nested-block fixture", root)

			report, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{BootstrapTakeover: true})
			if err != nil {
				t.Fatalf("nested-block bootstrap refused: %v", err)
			}
			if !report.Changed || !report.BootstrapTakeover || report.BootstrapKind != BootstrapKindNestedBlock {
				t.Fatalf("nested report = %+v", report)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			body := string(got)
			if !strings.HasPrefix(body, tc.prefix) || !strings.HasSuffix(body, tc.suffix) {
				t.Fatalf("prefix/suffix not preserved:\n%s", body)
			}
			if strings.Contains(body, "stale product-steering prose") || strings.Contains(body, productSteeringFamilySignature) {
				t.Fatalf("nested takeover retained the legacy block:\n%s", body)
			}
			if !strings.Contains(body, "BEGIN COMPOUND GENERATED INSTRUCTIONS") {
				t.Fatalf("nested takeover lost the managed content:\n%s", body)
			}

			clean, err := MaterializeInstructionBlock(mustRender(t, tc.harness, "nested-block fixture", root), InstructionMaterializationOptions{})
			if err != nil {
				t.Fatalf("second materialization failed: %v", err)
			}
			if clean.Changed {
				t.Fatalf("second materialization not idempotent: %+v", clean)
			}
			repeat, err := MaterializeInstructionBlock(mustRender(t, tc.harness, "nested-block fixture", root), InstructionMaterializationOptions{BootstrapTakeover: true})
			if err != nil || repeat.Changed || repeat.BootstrapTakeover {
				t.Fatalf("repeat bootstrap must be a normal no-op: report=%+v err=%v", repeat, err)
			}
		})
	}
}

func TestNestedInstructionTakeoverPreservesSuffixAndPassesGitWhitespace(t *testing.T) {
	for _, harness := range []string{"codex", "claude_code"} {
		for _, suffix := range []string{"\n", "", "\n# Unmanaged suffix — keep exactly.\n"} {
			t.Run(harness+"/"+fmt.Sprintf("suffix-bytes-%d", len(suffix)), func(t *testing.T) {
				root := t.TempDir()
				name := "AGENTS.md"
				if harness == "claude_code" {
					name = "CLAUDE.md"
				}
				prefix := "# Unmanaged project — keep exactly.\n\n"
				target := writeTarget(t, root, name, prefix+productSteeringBegin+"\nstale\n"+productSteeringEnd+suffix)
				git := func(args ...string) {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = root
					cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v\n%s", args, err, out)
					}
				}
				git("init", "--quiet")
				git("add", name)
				git("-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "synthetic preimage")
				render := mustRender(t, harness, "nested EOF fixture", root)
				if _, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{BootstrapTakeover: true}); err != nil {
					t.Fatal(err)
				}
				git("diff", "--check")
				got, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				inner := render.Content
				if strings.HasPrefix(suffix, "\n") {
					inner = strings.TrimSuffix(inner, "\n")
				}
				if string(got) != prefix+inner+suffix {
					t.Fatalf("unmanaged bytes changed: %q", got)
				}
				stat, err := os.Stat(target)
				if err != nil || stat.Mode().Perm() != 0o640 {
					t.Fatalf("mode changed: %v %v", stat, err)
				}
				current := mustRender(t, harness, "nested EOF fixture", root)
				if current.Drifted {
					t.Fatalf("compiler output drifted: %+v", current)
				}
				second, err := MaterializeInstructionBlock(current, InstructionMaterializationOptions{})
				if err != nil || second.Changed {
					t.Fatalf("not idempotent: %+v %v", second, err)
				}
			})
		}
	}
}

// Falsifier PS7 (review correction 6): every mixed-marker shape refuses in
// BOTH modes, before the valid-managed-block branch can leave obsolete
// instructions active, with bytes preserved.
func TestMaterializationRefusesMixedMarkerFamiliesInBothModes(t *testing.T) {
	root := t.TempDir()
	render := mustRender(t, "codex", "mixed-marker fixture", root)
	foreign := strings.Replace(render.Content, "harness=codex", "harness=claude_code", 1)

	cases := []struct {
		name string
		body string
	}{
		{"valid-compound-plus-product-steering", "# prefix\n" + render.Content + "\n" + productSteeringBegin + "\nstale\n" + productSteeringEnd + "\n"},
		{"valid-compound-plus-orphan-end", "# prefix\n" + render.Content + "\n" + productSteeringEnd + "\n"},
		{"valid-compound-plus-orphan-begin", "# prefix\n" + render.Content + "\n" + productSteeringBegin + "\n"},
		{"valid-compound-plus-duplicate-markers", "# prefix\n" + render.Content + "\n" + productSteeringBegin + "\nstale A\n" + productSteeringEnd + "\nmid\n" + productSteeringBegin + "\nstale B\n" + productSteeringEnd + "\n"},
		{"valid-compound-plus-unknown-variant", "# prefix\n" + render.Content + "\n" + unknownVariantBegin + "\nstale\n" + productSteeringEnd + "\n"},
		{"foreign-compound-plus-product-steering", "# prefix\n" + foreign + "\n" + productSteeringBegin + "\nstale\n" + productSteeringEnd + "\n"},
		{"legacy-whole-file-plus-unknown-nested-variant", umutOSLegacyFirstLine + "\nlegacy active body\n\n" + unknownVariantBegin + "\nstale\n" + productSteeringEnd + "\n"},
		{"legacy-whole-file-plus-canonical-nested", umutOSLegacyFirstLine + "\nlegacy active body\n\n" + productSteeringBegin + "\nstale\n" + productSteeringEnd + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := writeTarget(t, root, "AGENTS.md", tc.body)
			refuseAndPreserve(t, render, target, tc.body, false)
			refuseAndPreserve(t, render, target, tc.body, true)
		})
	}
}

// Falsifier PS8: malformed product-steering shapes refuse under bootstrap
// alone too (no Compound marker present to pair with).
func TestBootstrapReplaceRefusesMalformedNestedMarkers(t *testing.T) {
	root := t.TempDir()
	render := mustRender(t, "codex", "malformed nested fixture", root)
	cases := []struct {
		name string
		body string
	}{
		{"duplicated", "# prefix\n" + productSteeringBegin + "\nstale A\n" + productSteeringEnd + "\nmiddle\n" + productSteeringBegin + "\nstale B\n" + productSteeringEnd + "\n# suffix\n"},
		{"reversed", "# prefix\n" + productSteeringEnd + "\nstale\n" + productSteeringBegin + "\n# suffix\n"},
		{"orphan-end", "# prefix\n" + productSteeringEnd + "\n# suffix\n"},
		{"orphan-begin", "# prefix\n" + productSteeringBegin + "\n# suffix\n"},
		{"unknown-variant", "# prefix\n" + unknownVariantBegin + "\nstale\n" + productSteeringEnd + "\n# suffix\n"},
		{"canonical-pair-plus-extra-unknown-begin", "# prefix\n" + productSteeringBegin + "\nstale\n" + productSteeringEnd + "\n" + unknownVariantBegin + "\n# suffix\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := writeTarget(t, root, "AGENTS.md", tc.body)
			refuseAndPreserve(t, render, target, tc.body, true)
		})
	}
}

// Falsifier PS9 (retention): an ordinary Compound-only file still updates
// normally, and an unambiguous legacy whole file still bootstraps.
func TestMaterializationRetainsCompoundOnlyUpdateAndWholeFileBootstrap(t *testing.T) {
	root := t.TempDir()
	render := mustRender(t, "codex", "ordinary update", root)
	target := writeTarget(t, root, "AGENTS.md", "before\n\n"+strings.Replace(render.Content, "single authority", "old authority", 1)+"after\n")
	report, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{})
	if err != nil {
		t.Fatalf("ordinary compound-only update refused: %v", err)
	}
	if !report.Changed || report.BootstrapTakeover {
		t.Fatalf("ordinary update report = %+v", report)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "before\n\n") || !strings.HasSuffix(string(got), "after\n") {
		t.Fatalf("ordinary update changed unmanaged text:\n%s", got)
	}
	if strings.Contains(string(got), "old authority") {
		t.Fatalf("ordinary update did not refresh the managed block:\n%s", got)
	}

	legacy := umutOSLegacyFirstLine + "\nlegacy active body\n"
	legacyTarget := writeTarget(t, root, "CLAUDE.md", legacy)
	legacyRender := mustRender(t, "claude_code", "whole-file bootstrap", root)
	legacyReport, err := MaterializeInstructionBlock(legacyRender, InstructionMaterializationOptions{BootstrapTakeover: true})
	if err != nil {
		t.Fatalf("unambiguous whole-file bootstrap refused: %v", err)
	}
	if !legacyReport.Changed || legacyReport.BootstrapKind != BootstrapKindLegacyWholeFile {
		t.Fatalf("whole-file bootstrap report = %+v", legacyReport)
	}
	gotLegacy, err := os.ReadFile(legacyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotLegacy), "legacy active body") || strings.Contains(string(gotLegacy), "umut-os") {
		t.Fatalf("whole-file bootstrap retained legacy body:\n%s", gotLegacy)
	}
}

// Falsifier PS6: a "vanished path" — the file that used to be the umut-os
// whole-file form now carries the nested product-steering block. The
// generated client must not retain any legacy runtime read instruction.
func TestBootstrapReplaceNestedProductSteeringSyntheticallyReplacesVanishedLegacyFile(t *testing.T) {
	root := t.TempDir()
	target := writeTarget(t, root, "AGENTS.md", "# prefix\n"+productSteeringBegin+"\nlegacy active body text\n"+productSteeringEnd+"\n# suffix\n")
	render := mustRender(t, "codex", "vanished-path fixture", root)
	report, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{BootstrapTakeover: true})
	if err != nil {
		t.Fatalf("vanished-path nested takeover refused: %v", err)
	}
	if !report.Changed || report.BootstrapKind != BootstrapKindNestedBlock {
		t.Fatalf("vanished-path report = %+v", report)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	for _, stale := range []string{"legacy active body text", "operating-system/umut-os", productSteeringFamilySignature} {
		if strings.Contains(body, stale) {
			t.Fatalf("vanished-path takeover retained %q:\n%s", stale, body)
		}
	}
	if !strings.Contains(body, "BEGIN COMPOUND GENERATED INSTRUCTIONS") {
		t.Fatalf("vanished-path takeover did not install the compiled Compound block:\n%s", body)
	}
}
