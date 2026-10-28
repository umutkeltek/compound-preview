package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func proofCheckConsoleAttentionStep(t *testing.T) (repo string, step proofCheckStep) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..")), proofCheckStep{
		Name:          "console-attention-consistency-smoke",
		Command:       consoleBunSmokeCommand("scripts/attention-consistency-smoke.mjs"),
		Dir:           "console",
		SkipIfMissing: "bun",
		ProofRef:      "console-attention-consistency-smoke",
	}
}

// requireConsoleSmokeRuntime skips for the same reasons the gate itself skips: the smoke
// needs bun and console/node_modules, neither of which exists in a bare clone.
func requireConsoleSmokeRuntime(t *testing.T, repo string) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	if _, err := os.Stat(filepath.Join(repo, "console", "node_modules")); err != nil {
		t.Skip("console/node_modules absent")
	}
}

// TestProofCheckConsoleAttentionGateProvesFixtureIdentity is the positive case: pinned to a
// seeded home and the repo's own binary, the smoke proves Attention and Decide agree on a
// membership the fixture determines — no live COMPOUND_HOME, no installed binary.
func TestProofCheckConsoleAttentionGateProvesFixtureIdentity(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckConsoleAttentionStep(t)
	requireConsoleSmokeRuntime(t, repo)
	out, err := runProofCheckConsoleAttentionGateForBinary(t, repo, compoundTestBinary(t), step, false)
	if err != nil {
		t.Fatalf("attention gate failed on its own seeded home: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "needs_you_groups=") {
		t.Fatalf("gate did not record the fixture's attention evidence:\n%s", out)
	}
}

// TestProofCheckConsoleAttentionGateRefusesVacuousFixture is the control that makes this
// isolation safe. The smoke deliberately accepts an empty projection, so on an unseeded home
// it passes having compared two empty lists — the gate must refuse the home before the smoke
// ever sees it.
func TestProofCheckConsoleAttentionGateRefusesVacuousFixture(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckConsoleAttentionStep(t)
	requireConsoleSmokeRuntime(t, repo)
	out, err := runProofCheckConsoleAttentionGateForBinary(t, repo, compoundTestBinary(t), step, true)
	if err == nil {
		t.Fatalf("gate passed on an unseeded home:\n%s", out)
	}
	if !strings.Contains(err.Error(), "asserted nothing") {
		t.Fatalf("gate failed for the wrong reason: %v\n%s", err, out)
	}
}

// TestProofCheckConsoleAttentionSmokePassesOnAVacuousHome pins WHY the guard above exists: it
// is not defending against a hypothetical. Run directly against an unseeded home, the smoke
// itself is green. If this ever fails, the smoke grew its own emptiness check and the guard
// can be reconsidered — it must not be deleted on the assumption that it already did.
func TestProofCheckConsoleAttentionSmokePassesOnAVacuousHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("runs the console smoke")
	}
	repo, step := proofCheckConsoleAttentionStep(t)
	requireConsoleSmokeRuntime(t, repo)
	bare := proofCheckWorkbenchGateHome(t, true)
	cmd := exec.Command(step.Command[0], step.Command[1:]...)
	cmd.Dir = filepath.Join(repo, step.Dir)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+bare, "COMPOUND_BIN="+compoundTestBinary(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke no longer passes on an empty home — re-examine the gate's non-vacuity guard: %v\n%s", err, out)
	}
}

// runProofCheckConsoleAttentionGateForBinary mirrors runProofCheckConsoleAttentionGate with
// the binary injected: os.Executable() under `go test` is the test binary, which has no
// attention verb.
func runProofCheckConsoleAttentionGateForBinary(t *testing.T, repo, binary string, step proofCheckStep, bare bool) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := home.Init(dir, proofCheckWorkbenchActor, now); err != nil {
		t.Fatalf("home.Init: %v", err)
	}
	if !bare {
		if err := seedProofCheckWorkbenchHome(dir, now); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
	return runProofCheckConsoleAttentionInHome(repo, dir, binary, step)
}
