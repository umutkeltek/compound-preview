package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
)

func instructionContractForTest(harness, task string) envelope.EffectiveContract {
	in := Input{
		Harness:     harness,
		Cwd:         "/repo/example",
		Task:        task,
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	return Compile(in, DefaultPolicy(), fixedNow)
}

func TestRenderInstructionBlockCarriesStableAuthorityAndProjectRules(t *testing.T) {
	for _, tc := range []struct {
		harness string
		target  string
	}{
		{harness: "codex", target: "AGENTS.md"},
		{harness: "claude_code", target: "CLAUDE.md"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.target), []byte("# existing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			render, err := RenderInstructionBlock(instructionContractForTest(tc.harness, "first task"), root)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(render.TargetPath) != tc.target || !render.Drifted {
				t.Fatalf("render target/drift = %s/%t, want %s/true", render.TargetPath, render.Drifted, tc.target)
			}
			for _, want := range []string{
				"Compound is the authority for durable meaning",
				"Retrieval engines and legacy vector/fact stores are bridges/indexes",
				"rented executor",
				"cheapest sufficient lane",
				"AGENTS.md",
				"skills/README.md",
				"Task-specific intent, memory, capabilities, and checks stay in `compound orient` and hook-served ContextPacks",
			} {
				if !strings.Contains(render.Content, want) {
					t.Fatalf("render missing %q:\n%s", want, render.Content)
				}
			}

			// The precedence ladder is the block's conflict-resolution rule, and it is
			// the one part a reader cannot reconstruct from the other bullets: without
			// it, "Compound is the authority for durable meaning" and "verify before
			// asserting" are both present with nothing saying which governs when a
			// served claim and the code disagree. Each rung is pinned individually so a
			// future edit cannot quietly drop one and leave a ladder that still reads
			// like a ladder.
			for _, rung := range []string{
				"the operator's latest instruction in this session",
				"verified current reality",
				"served runtime state",
				"tracked static contract, including this block",
				"evidence, never a premise",
			} {
				if !strings.Contains(render.Content, rung) {
					t.Errorf("precedence ladder missing rung %q:\n%s", rung, render.Content)
				}
			}

			// Ordering is the content of the rule: the same five rungs in a different
			// order is a different, and in one case inverted, policy.
			ladder := []string{
				"operator's latest instruction",
				"verified current reality",
				"served runtime state",
				"tracked static contract",
				"recall, injected reminders, mined history",
			}
			prev := -1
			for _, rung := range ladder {
				at := strings.Index(render.Content, rung)
				if at < 0 {
					continue // absence already reported above
				}
				if at < prev {
					t.Errorf("precedence rung %q renders out of order; the ladder's order IS the policy", rung)
				}
				prev = at
			}

			// Self-demotion: the block must rank itself rather than claim supremacy.
			// An instruction surface that omits its own rank reads as outranking
			// everything that is not in it.
			if !strings.Contains(render.Content, "This block is rung 4 and does not outrank rungs 1-3") {
				t.Errorf("managed block does not state its own rank:\n%s", render.Content)
			}

			// Persistence-is-not-authority travels with the ladder: "keep going" is the
			// instruction most likely to be read as widening what is permitted, and the
			// ungoverned look-alike is the specific move it forbids.
			for _, want := range []string{
				"Persistence is not authority",
				"never widens the set of authorized actions",
				"never produce a similar-looking artifact by an ungoverned route",
			} {
				if !strings.Contains(render.Content, want) {
					t.Errorf("render missing %q:\n%s", want, render.Content)
				}
			}
		})
	}
}

func TestInstructionBlockIgnoresTaskTextAndDetectsOnlyManagedBlockDrift(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	first, err := RenderInstructionBlock(instructionContractForTest("codex", "private task alpha"), root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderInstructionBlock(instructionContractForTest("codex", "unrelated task beta"), root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Content != second.Content || first.ContentSHA256 != second.ContentSHA256 {
		t.Fatalf("task text leaked into static block:\nfirst=%s\nsecond=%s", first.Content, second.Content)
	}
	if strings.Contains(first.Content, "private task alpha") || strings.Contains(first.Content, "unrelated task beta") {
		t.Fatalf("task-scoped text appeared in static block: %s", first.Content)
	}

	if err := os.WriteFile(target, []byte("unmanaged before\n\n"+first.Content+"\nunmanaged after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err := RenderInstructionBlock(instructionContractForTest("codex", "third task"), root)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Drifted {
		t.Fatalf("unmanaged text caused drift: %+v", clean)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	driftedBody := strings.Replace(string(body), "single authority", "one authority", 1)
	if err := os.WriteFile(target, []byte(driftedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	drifted, err := RenderInstructionBlock(instructionContractForTest("codex", "third task"), root)
	if err != nil {
		t.Fatal(err)
	}
	if !drifted.Drifted || drifted.ObservedSHA256 == drifted.ContentSHA256 {
		t.Fatalf("managed-block mutation did not drift: %+v", drifted)
	}
}

func TestTrackedHarnessInstructionBlocksMatchCompiler(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	for _, harness := range []string{"codex", "claude_code"} {
		render, err := RenderInstructionBlock(instructionContractForTest(harness, "static parity proof"), repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		if render.Drifted {
			t.Fatalf("%s tracked instruction block drifted: %+v", harness, render)
		}
	}
}

func TestMaterializeInstructionBlockRequiresExplicitRecognizedBootstrapTakeover(t *testing.T) {
	for _, tc := range []struct {
		harness string
		target  string
	}{
		{harness: "claude_code", target: "CLAUDE.md"},
		{harness: "codex", target: "AGENTS.md"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, tc.target)
			legacy := "<!-- GENERATED from operating-system/umut-os/spine/steering.config.ts; do NOT hand-edit. -->\n\n# Legacy authority\n"
			if err := os.WriteFile(target, []byte(legacy), 0o640); err != nil {
				t.Fatal(err)
			}
			render, err := RenderInstructionBlock(instructionContractForTest(tc.harness, "bootstrap"), root)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{}); err == nil {
				t.Fatal("markerless legacy target materialized without explicit bootstrap takeover")
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != legacy {
				t.Fatalf("refused bootstrap changed target: err=%v body=%q", err, got)
			}

			report, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{BootstrapTakeover: true})
			if err != nil {
				t.Fatal(err)
			}
			if !report.Changed || !report.BootstrapTakeover || report.BeforeSHA256 == report.AfterSHA256 {
				t.Fatalf("bootstrap report = %+v", report)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != strings.TrimRight(render.Content, "\n")+"\n" {
				t.Fatalf("bootstrap body = %q, want managed content only", got)
			}
			if strings.Contains(string(got), "operating-system") || strings.Contains(string(got), "Legacy authority") {
				t.Fatalf("bootstrap retained legacy active body:\n%s", got)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("bootstrap mode = %v err=%v, want 0640", info.Mode().Perm(), err)
			}

			cleanRender, err := RenderInstructionBlock(instructionContractForTest(tc.harness, "bootstrap"), root)
			if err != nil {
				t.Fatal(err)
			}
			second, err := MaterializeInstructionBlock(cleanRender, InstructionMaterializationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if second.Changed || second.BootstrapTakeover {
				t.Fatalf("idempotent materialization = %+v", second)
			}
		})
	}
}

func TestMaterializeInstructionBlockPreservesUnmanagedTextAndRefusesUnsafeTargets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	render, err := RenderInstructionBlock(instructionContractForTest("codex", "managed update"), root)
	if err != nil {
		t.Fatal(err)
	}
	initial := "before\n\n" + strings.Replace(render.Content, "single authority", "old authority", 1) + "after\n"
	if err := os.WriteFile(target, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	render, err = RenderInstructionBlock(instructionContractForTest("codex", "managed update"), root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := MaterializeInstructionBlock(render, InstructionMaterializationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.BootstrapTakeover {
		t.Fatalf("managed update report = %+v", report)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "before\n\n") || !strings.HasSuffix(string(got), "after\n") {
		t.Fatalf("managed update changed unmanaged text:\n%s", got)
	}

	duplicate := render.Content + render.Content
	if err := os.WriteFile(target, []byte(duplicate), 0o644); err != nil {
		t.Fatal(err)
	}
	duplicateRender, err := RenderInstructionBlock(instructionContractForTest("codex", "duplicate"), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeInstructionBlock(duplicateRender, InstructionMaterializationOptions{}); err == nil {
		t.Fatal("duplicate markers materialized")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != duplicate {
		t.Fatalf("duplicate-marker refusal changed target: err=%v body=%q", err, got)
	}

	preset, err := RenderInstructionBlockVariant(instructionContractForTest("codex", "preset"), root, "executor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeInstructionBlock(preset, InstructionMaterializationOptions{}); err == nil {
		t.Fatal("preset render materialized")
	}

	realTarget := filepath.Join(root, "real-AGENTS.md")
	if err := os.WriteFile(realTarget, []byte(render.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTarget, target); err != nil {
		t.Fatal(err)
	}
	symlinkRender := render
	symlinkRender.TargetPath = target
	if _, err := MaterializeInstructionBlock(symlinkRender, InstructionMaterializationOptions{}); err == nil {
		t.Fatal("symlink target materialized")
	}
}

func TestMaterializeInstructionBlockRefusesPreimageRace(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	render, err := RenderInstructionBlock(instructionContractForTest("codex", "race"), root)
	if err != nil {
		t.Fatal(err)
	}
	initial := "before\n" + strings.Replace(render.Content, "single authority", "old authority", 1)
	if err := os.WriteFile(target, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	render, err = RenderInstructionBlock(instructionContractForTest("codex", "race"), root)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("concurrent owner\n")
	fsOps := defaultInstructionMaterializeFS()
	fsOps.beforeCommit = func() {
		if err := os.WriteFile(target, concurrent, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := materializeInstructionBlockWithFS(render, InstructionMaterializationOptions{}, fsOps); err == nil ||
		!strings.Contains(err.Error(), "changed since preflight") {
		t.Fatalf("preimage race error = %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(concurrent) {
		t.Fatalf("preimage race overwrote concurrent owner: err=%v body=%q", err, got)
	}
}
