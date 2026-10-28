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

func proofCheckConsoleAgentsStep(t *testing.T) (repo string, step proofCheckStep) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..")), proofCheckStep{
		Name:          "console-agents-consistency-smoke",
		Command:       consoleSmokeCommand("scripts/agents-consistency-smoke.mjs"),
		Dir:           "console",
		SkipIfMissing: "compound",
		ProofRef:      "console-agents-consistency-smoke",
	}
}

// TestProofCheckConsoleAgentsGateProvesFixtureLeaseIdentity is the positive case: the lease
// populations the smoke reconciles come from a fixture, so the gate no longer depends on the
// operator happening to have leases, nor on the installed binary.
func TestProofCheckConsoleAgentsGateProvesFixtureLeaseIdentity(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckConsoleAgentsStep(t)
	requireConsoleSmokeRuntime(t, repo)
	out, err := runProofCheckConsoleAgentsGateForBinary(t, repo, compoundTestBinary(t), step, false)
	if err != nil {
		t.Fatalf("agents gate failed on its own seeded home: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fixture executors: leases=") {
		t.Fatalf("gate did not record the fixture's lease evidence:\n%s", out)
	}
}

// TestProofCheckConsoleAgentsGateRefusesLeaselessHome is the control: with no leases the
// smoke's cross-door reconciliation compares empty sets and passes, so the gate must refuse
// the home first.
func TestProofCheckConsoleAgentsGateRefusesLeaselessHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckConsoleAgentsStep(t)
	requireConsoleSmokeRuntime(t, repo)
	out, err := runProofCheckConsoleAgentsGateForBinary(t, repo, compoundTestBinary(t), step, true)
	if err == nil {
		t.Fatalf("gate passed on a leaseless home:\n%s", out)
	}
	if !strings.Contains(err.Error(), "asserting nothing") {
		t.Fatalf("gate failed for the wrong reason: %v\n%s", err, out)
	}
}

// TestProofCheckConsoleAgentsSmokePassesOnALeaselessHome pins why that guard is needed: the
// smoke is green with nothing to reconcile. Same contract as its attention sibling — if this
// ever fails, the smoke grew its own emptiness check; do not delete the guard on the
// assumption that it already had one.
func TestProofCheckConsoleAgentsSmokePassesOnALeaselessHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("runs the console smoke")
	}
	repo, step := proofCheckConsoleAgentsStep(t)
	requireConsoleSmokeRuntime(t, repo)
	bare := proofCheckWorkbenchGateHome(t, true)
	cmd := exec.Command(step.Command[0], step.Command[1:]...)
	cmd.Dir = filepath.Join(repo, step.Dir)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+bare, "COMPOUND_BIN="+compoundTestBinary(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke no longer passes on a leaseless home — re-examine the gate's guard: %v\n%s", err, out)
	}
}

func runProofCheckConsoleAgentsGateForBinary(t *testing.T, repo, binary string, step proofCheckStep, bare bool) ([]byte, error) {
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
	return runProofCheckConsoleAgentsInHome(repo, dir, binary, step)
}
