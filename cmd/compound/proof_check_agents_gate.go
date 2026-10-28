package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

// The agents-consistency smoke is the fourth gate with the live-home shape, and the cheapest
// to fix now that the pattern exists: 13.4s against the operator's live home, 0.22s against a
// seeded one. It resolved its binary as `COMPOUND_BIN || "compound"`, so like the attention
// smoke it was running whatever was INSTALLED, not the tree under test.
//
// What it proves is worth keeping: one lease population, two doors. `home doctor`'s
// operational_hygiene.stale_lease_count and `executors`' counts.stale_leases must agree, the
// per-executor splits must sum to each door's count, and the rendered band count must equal an
// independent recompute from raw lease ages. On an empty home every one of those is 0 == 0.
//
// The workbench fixture supplies exactly the disjoint-populations split the smoke reconciles:
// one ACTIVE lease (claimed, still running) and one RESIDUE lease (a lease whose work is
// terminal). The residue half used to fall out of closing a claimed envelope; since I1 a close
// releases its own lease in the same publish, so the fixture plants it deliberately
// (WriteResidualLeaseForTerminalWork) instead of inheriting it from a behavior that no longer
// exists. That also makes the fixture home's `home doctor` report operational_hygiene.ok
// = false — correct, and not something this gate asserts on.
func runProofCheckConsoleAgentsGate(repo string, step proofCheckStep) ([]byte, error) {
	return runProofCheckConsoleAgentsGateContext(context.Background(), repo, step)
}

func runProofCheckConsoleAgentsGateContext(ctx context.Context, repo string, step proofCheckStep) ([]byte, error) {
	exe, err := work.ProofRuntimeExecutable(ctx, "compound", proofCheckRuntimeExecutable)
	if err != nil {
		return nil, err
	}
	tmpHome, err := os.MkdirTemp("", "compound-proofcheck-agents-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpHome)
	now := time.Now().UTC()
	if err := home.Init(tmpHome, proofCheckWorkbenchActor, now); err != nil {
		return nil, fmt.Errorf("initialize proof-check agents home: %w", err)
	}
	if err := seedProofCheckWorkbenchHome(tmpHome, now); err != nil {
		return nil, fmt.Errorf("seed proof-check agents fixture: %w", err)
	}
	return runProofCheckConsoleAgentsInHomeContext(ctx, repo, tmpHome, exe, step)
}

func runProofCheckConsoleAgentsInHome(repo, homePath, binary string, step proofCheckStep) ([]byte, error) {
	return runProofCheckConsoleAgentsInHomeContext(context.Background(), repo, homePath, binary, step)
}

func runProofCheckConsoleAgentsInHomeContext(ctx context.Context, repo, homePath, binary string, step proofCheckStep) ([]byte, error) {
	evidence, err := proofCheckAgentsFixtureLeasesContext(ctx, homePath, binary)
	if err != nil {
		return evidence, err
	}
	executable, err := work.ProofRuntimeExecutable(ctx, step.Command[0], proofCheckRuntimeExecutable)
	if err != nil {
		return evidence, err
	}
	cmd := proofCheckCommandContext(ctx, executable, step.Command[1:]...)
	cmd.Dir = filepath.Join(repo, step.Dir)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "COMPOUND_BIN="+binary)
	output, err := cmd.CombinedOutput()
	return append(evidence, output...), err
}

// proofCheckAgentsFixtureLeases requires the fixture to carry leases in BOTH populations the
// smoke reconciles. Without active leases the sum-to-door-count assertions are 0 == 0; without
// a residue lease the two doors are compared while both are empty, which is the same vacuous
// green isolation would otherwise have bought.
func proofCheckAgentsFixtureLeases(homePath, binary string) ([]byte, error) {
	return proofCheckAgentsFixtureLeasesContext(context.Background(), homePath, binary)
}

func proofCheckAgentsFixtureLeasesContext(ctx context.Context, homePath, binary string) ([]byte, error) {
	cmd := proofCheckCommandContext(ctx, binary, "executors", "--json")
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("executors projection on the seeded home failed: %w", err)
	}
	var report struct {
		Counts struct {
			Leases      int `json:"leases"`
			StaleLeases int `json:"stale_leases"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		return out, fmt.Errorf("parse executors projection: %w", err)
	}
	if report.Counts.Leases == 0 || report.Counts.StaleLeases == 0 {
		return out, fmt.Errorf("seeded home carries leases=%d residue=%d: the smoke reconciles two lease populations and would compare empty sets, asserting nothing",
			report.Counts.Leases, report.Counts.StaleLeases)
	}
	return []byte(fmt.Sprintf("fixture executors: leases=%d residue=%d\n", report.Counts.Leases, report.Counts.StaleLeases)), nil
}
