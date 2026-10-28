package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

func TestConsoleHomeOwningGatesSurviveRuntimeSourceRemoval(t *testing.T) {
	// Refuse the old path before it can re-exec this Go test binary without a
	// test filter. The structural regression diagnoses that unsafe bypass.
	for _, name := range []string{"proof_check_console_attention_gate.go", "proof_check_agents_gate.go"} {
		body, err := os.ReadFile(filepath.Join(testRepoRoot, "cmd", "compound", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "os.Executable(") {
			t.Fatalf("%s bypasses the pinned runtime; refusing recursive test execution", name)
		}
	}
	requireConsoleSmokeRuntime(t, testRepoRoot)
	body, err := os.ReadFile(compoundTestBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "compound")
	if err := os.WriteFile(source, body, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx, cleanup, err := work.PinProofRuntimeExecutable(ctx, func(string) (string, error) { return source, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pinned, err := work.ProofRuntimeExecutable(ctx, "compound", proofCheckRuntimeExecutable)
	if err != nil || pinned == source {
		t.Fatalf("runtime was not copied into proof custody: path=%q err=%v", pinned, err)
	}
	// Remove only the disposable source, never the shared test binary. Both
	// the fixture precheck and COMPOUND_BIN consumer must use the pinned copy.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, step := range steps {
		if step.Name != "console-attention-consistency-smoke" && step.Name != "console-agents-consistency-smoke" {
			continue
		}
		ran++
		t.Run(step.Name, func(t *testing.T) {
			result := runProofCheckStepContext(ctx, testRepoRoot, step, "")
			if result.Status != "ok" {
				t.Fatalf("gate did not survive runtime source removal: status=%s output=%s", result.Status, result.OutputTail)
			}
			if !strings.Contains(result.OutputTail, "fixture ") {
				t.Fatalf("gate lacked nonempty fixture evidence: %s", result.OutputTail)
			}
		})
	}
	if ran != 2 {
		t.Fatalf("ran %d console home-owning gates, want 2", ran)
	}
}
