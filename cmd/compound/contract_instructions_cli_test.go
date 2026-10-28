package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/contract"
	"compound/internal/home"
)

const contractInstructionsChildEnv = "COMPOUND_TEST_CONTRACT_INSTRUCTIONS_CHILD"
const contractInstructionsMaterializeChildEnv = "COMPOUND_TEST_CONTRACT_INSTRUCTIONS_MATERIALIZE_CHILD"

func TestContractCompileInstructionsCheckFailsOnDriftAndPassesOnExactBlock(t *testing.T) {
	t.Parallel()
	if os.Getenv(contractInstructionsChildEnv) == "1" {
		cmdContract([]string{
			"compile",
			"--harness", "codex",
			"--cwd", os.Getenv("COMPOUND_TEST_INSTRUCTION_REPO"),
			"--task", "task-scoped text must not enter the static block",
			"--repo-kind", "control-plane",
			"--mode", "execute",
			"--instructions",
			"--check-instructions",
		})
		return
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repoRoot := t.TempDir()
	target := filepath.Join(repoRoot, "AGENTS.md")
	canonicalRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	canonicalTarget := filepath.Join(canonicalRepoRoot, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# unmanaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func() ([]byte, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestContractCompileInstructionsCheckFailsOnDriftAndPassesOnExactBlock$")
		cmd.Env = append(os.Environ(),
			contractInstructionsChildEnv+"=1",
			"COMPOUND_HOME="+homePath,
			"COMPOUND_TEST_INSTRUCTION_REPO="+repoRoot,
		)
		return cmd.CombinedOutput()
	}

	driftOutput, err := run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("drift check exit = %v, want 1; output:\n%s", err, driftOutput)
	}
	var drift contract.InstructionBlockRender
	if err := json.NewDecoder(bytes.NewReader(driftOutput)).Decode(&drift); err != nil {
		t.Fatalf("parse drift render: %v\n%s", err, driftOutput)
	}
	if !drift.Drifted || drift.Content == "" || drift.TargetPath != canonicalTarget {
		t.Fatalf("drift render = %+v", drift)
	}
	if err := os.WriteFile(target, []byte("# unmanaged\n\n"+drift.Content), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanOutput, err := run()
	if err != nil {
		t.Fatalf("exact block check failed: %v\n%s", err, cleanOutput)
	}
	var clean contract.InstructionBlockRender
	if err := json.NewDecoder(bytes.NewReader(cleanOutput)).Decode(&clean); err != nil {
		t.Fatalf("parse clean render: %v\n%s", err, cleanOutput)
	}
	if clean.Drifted || clean.ObservedSHA256 != clean.ContentSHA256 {
		t.Fatalf("clean render = %+v", clean)
	}
}

// Falsifier B5: `compound contract compile --instructions --preset <name>`
// prints the variant render, exits 0, does not touch the repo file, and
// `--check-instructions --preset <name>` is rejected (preset renders are
// not drift-checked).
func TestContractCompileInstructionsPresetVariant(t *testing.T) {
	t.Parallel()
	if os.Getenv(contractInstructionsChildEnv) == "1" {
		preset := os.Getenv("COMPOUND_TEST_INSTRUCTION_PRESET")
		negative := os.Getenv("COMPOUND_TEST_INSTRUCTION_NEGATIVE") == "1"
		args := []string{
			"compile",
			"--harness", "codex",
			"--cwd", os.Getenv("COMPOUND_TEST_INSTRUCTION_REPO"),
			"--task", "task-scoped text must not enter the static block",
			"--repo-kind", "control-plane",
			"--mode", "execute",
			"--instructions",
			"--preset", preset,
		}
		if negative {
			// Inject --check-instructions before --preset to exercise the
			// rejection path. (cmdContract only reads flags; arg order does
			// not matter for the parser.)
			args = append([]string{
				"compile",
				"--harness", "codex",
				"--cwd", os.Getenv("COMPOUND_TEST_INSTRUCTION_REPO"),
				"--task", "x",
				"--repo-kind", "control-plane",
				"--mode", "execute",
				"--instructions",
				"--check-instructions",
				"--preset", preset,
			}, []string{}...)
		}
		cmdContract(args)
		return
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repoRoot := t.TempDir()
	target := filepath.Join(repoRoot, "AGENTS.md")
	originalContents := "# unmanaged\n"
	if err := os.WriteFile(target, []byte(originalContents), 0o644); err != nil {
		t.Fatal(err)
	}

	runPreset := func(preset string, negative bool) ([]byte, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestContractCompileInstructionsPresetVariant$")
		env := append(os.Environ(),
			contractInstructionsChildEnv+"=1",
			"COMPOUND_HOME="+homePath,
			"COMPOUND_TEST_INSTRUCTION_REPO="+repoRoot,
			"COMPOUND_TEST_INSTRUCTION_PRESET="+preset,
		)
		if negative {
			env = append(env, "COMPOUND_TEST_INSTRUCTION_NEGATIVE=1")
		}
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	// Positive path: variant renders, exits 0, repo file untouched.
	output, err := runPreset("executor", false)
	if err != nil {
		t.Fatalf("preset render failed: %v\n%s", err, output)
	}
	var render contract.InstructionBlockRender
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&render); err != nil {
		t.Fatalf("parse preset render: %v\n%s", err, output)
	}
	if render.Drifted {
		t.Fatalf("preset render reports drifted=true; want false; %+v", render)
	}
	if !strings.Contains(render.Content, "preset="+"`executor`") {
		t.Fatalf("preset render body missing preset tag:\n%s", render.Content)
	}
	if !strings.Contains(render.Content, "Work only inside the dispatched envelope/worktree") {
		t.Fatalf("preset render missing executor charter:\n%s", render.Content)
	}
	foundNoTracked := false
	for _, d := range render.Details {
		if d == "preset variant render; no tracked surface" {
			foundNoTracked = true
			break
		}
	}
	if !foundNoTracked {
		t.Fatalf("preset render missing no-tracked-surface detail; got %v", render.Details)
	}

	// Verify the repo file was NOT touched.
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != originalContents {
		t.Fatalf("preset render mutated the repo file:\nbefore=%q\nafter=%q", originalContents, string(after))
	}

	// Negative path: --check-instructions --preset must os.Exit(2).
	out, runErr := runPreset("executor", true)
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("--check-instructions --preset not rejected: err=%v out=%s", runErr, out)
	}
	if !strings.Contains(string(out), "preset") {
		t.Fatalf("rejection stderr should mention preset; got: %s", out)
	}
}

func TestContractCompileMaterializesRecognizedBootstrapAndThenChecksClean(t *testing.T) {
	t.Parallel()
	if os.Getenv(contractInstructionsMaterializeChildEnv) == "1" {
		args := []string{
			"compile",
			"--harness", os.Getenv("COMPOUND_TEST_INSTRUCTION_HARNESS"),
			"--cwd", os.Getenv("COMPOUND_TEST_INSTRUCTION_REPO"),
			"--task", "installed static contract",
			"--repo-kind", "control-plane",
			"--mode", "execute",
			"--instructions",
		}
		switch os.Getenv("COMPOUND_TEST_INSTRUCTION_ACTION") {
		case "materialize":
			args = append(args, "--materialize-instructions")
			if os.Getenv("COMPOUND_TEST_INSTRUCTION_BOOTSTRAP") == "1" {
				args = append(args, "--bootstrap-replace")
			}
		case "check":
			args = append(args, "--check-instructions")
		}
		cmdContract(args)
		return
	}

	for _, tc := range []struct {
		harness string
		target  string
	}{
		{harness: "claude_code", target: "CLAUDE.md"},
		{harness: "codex", target: "AGENTS.md"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			repoRoot := t.TempDir()
			target := filepath.Join(repoRoot, tc.target)
			legacy := "<!-- GENERATED from operating-system/umut-os/spine/steering.config.ts; do NOT hand-edit. -->\nlegacy\n"
			if err := os.WriteFile(target, []byte(legacy), 0o640); err != nil {
				t.Fatal(err)
			}
			run := func(action string, bootstrap bool) ([]byte, error) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestContractCompileMaterializesRecognizedBootstrapAndThenChecksClean$")
				env := append(os.Environ(),
					contractInstructionsMaterializeChildEnv+"=1",
					"COMPOUND_HOME="+homePath,
					"COMPOUND_TEST_INSTRUCTION_REPO="+repoRoot,
					"COMPOUND_TEST_INSTRUCTION_HARNESS="+tc.harness,
					"COMPOUND_TEST_INSTRUCTION_ACTION="+action,
				)
				if bootstrap {
					env = append(env, "COMPOUND_TEST_INSTRUCTION_BOOTSTRAP=1")
				}
				cmd.Env = env
				return cmd.CombinedOutput()
			}

			refused, err := run("materialize", false)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("markerless materialization exit=%v, want 1; output=%s", err, refused)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != legacy {
				t.Fatalf("refused CLI materialization changed target: err=%v body=%q", err, got)
			}

			output, err := run("materialize", true)
			if err != nil {
				t.Fatalf("bootstrap materialization failed: %v\n%s", err, output)
			}
			var report contract.InstructionMaterializationReport
			if err := json.NewDecoder(bytes.NewReader(output)).Decode(&report); err != nil {
				t.Fatalf("parse materialization report: %v\n%s", err, output)
			}
			if !report.Changed || !report.BootstrapTakeover || filepath.Base(report.TargetPath) != tc.target {
				t.Fatalf("materialization report = %+v", report)
			}
			body, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "operating-system") || !strings.Contains(string(body), "Compound-generated contract") {
				t.Fatalf("materialized body retained legacy authority:\n%s", body)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("materialized mode=%v err=%v", info.Mode().Perm(), err)
			}

			checkOutput, err := run("check", false)
			if err != nil {
				t.Fatalf("post-materialization check failed: %v\n%s", err, checkOutput)
			}
			var render contract.InstructionBlockRender
			if err := json.NewDecoder(bytes.NewReader(checkOutput)).Decode(&render); err != nil {
				t.Fatalf("parse clean render: %v\n%s", err, checkOutput)
			}
			if render.Drifted || render.ObservedSHA256 != render.ContentSHA256 {
				t.Fatalf("post-materialization render = %+v", render)
			}
		})
	}
}
