package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/attention"
	"compound/internal/home"
	"compound/internal/work"
)

// The attention-consistency smoke was the single most expensive gate after go-test: 60s,
// nearly all of it one `compound attention --json` against the operator's live home (30-36s
// and climbing). It was also the gate least connected to the diff under test, for a reason
// that is easy to miss: the console resolves COMPOUND_BIN as
// $COMPOUND_BIN -> ~/.local/bin/compound -> PATH, so with none set the smoke exercised the
// INSTALLED binary. A Go change in the working tree could not fail it, and a stale install
// could.
//
// Pinning both the home and the binary fixes cost, meaning, and a flake class at once: the
// console's own projection timeout is 30s (runCompound in console/src/lib/compound.ts) while
// the live projection took 30-36s, so this gate lived one machine hiccup away from a red that
// no diff caused. 59.45s -> 0.27s measured.
//
// The smoke tolerates an empty projection on purpose ("or none for a live-empty projection"),
// which is exactly what makes a bare temp home dangerous here: the smoke would pass having
// compared two empty lists. So the gate proves the fixture is non-vacuous itself, before
// handing the home to the smoke.
func runProofCheckConsoleAttentionGate(repo string, step proofCheckStep) ([]byte, error) {
	return runProofCheckConsoleAttentionGateContext(context.Background(), repo, step)
}

func runProofCheckConsoleAttentionGateContext(ctx context.Context, repo string, step proofCheckStep) ([]byte, error) {
	exe, err := work.ProofRuntimeExecutable(ctx, "compound", proofCheckRuntimeExecutable)
	if err != nil {
		return nil, err
	}
	tmpHome, err := os.MkdirTemp("", "compound-proofcheck-attention-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpHome)
	now := time.Now().UTC()
	if err := home.Init(tmpHome, proofCheckWorkbenchActor, now); err != nil {
		return nil, fmt.Errorf("initialize proof-check attention home: %w", err)
	}
	// The workbench fixture is reused deliberately: its claimed-and-held envelopes are what
	// put groups in the needs_you band, so one fixture feeds both gates and cannot drift
	// into two different definitions of "work that needs you".
	if err := seedProofCheckWorkbenchHome(tmpHome, now); err != nil {
		return nil, fmt.Errorf("seed proof-check attention fixture: %w", err)
	}
	return runProofCheckConsoleAttentionInHomeContext(ctx, repo, tmpHome, exe, step)
}

func runProofCheckConsoleAttentionInHome(repo, homePath, binary string, step proofCheckStep) ([]byte, error) {
	return runProofCheckConsoleAttentionInHomeContext(context.Background(), repo, homePath, binary, step)
}

func runProofCheckConsoleAttentionInHomeContext(ctx context.Context, repo, homePath, binary string, step proofCheckStep) ([]byte, error) {
	evidence, err := proofCheckAttentionFixtureGroupsContext(ctx, homePath, binary)
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

// proofCheckAttentionFixtureGroups requires the fixture to put at least one group in the
// needs_you band. Without this the gate would be green on a home where the projection has
// nothing to project — the smoke's own emptiness allowance is a courtesy to a genuinely
// quiet operator, not a licence for a fixture to assert nothing.
func proofCheckAttentionFixtureGroups(homePath, binary string) ([]byte, error) {
	return proofCheckAttentionFixtureGroupsContext(context.Background(), homePath, binary)
}

func proofCheckAttentionFixtureGroupsContext(ctx context.Context, homePath, binary string) ([]byte, error) {
	cmd := proofCheckCommandContext(ctx, binary, "attention", "--json")
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("attention projection on the seeded home failed: %w", err)
	}
	var report attention.Report
	if err := json.Unmarshal(out, &report); err != nil {
		return out, fmt.Errorf("parse attention projection: %w", err)
	}
	if report.Counts.NeedsYouGroups == 0 {
		return out, fmt.Errorf("seeded home produced no needs_you attention groups: the smoke would compare two empty lists and pass having asserted nothing")
	}
	banded := 0
	for _, group := range report.Groups {
		if group.Band == "needs_you" {
			banded++
		}
	}
	if banded != report.Counts.NeedsYouGroups {
		return out, fmt.Errorf("attention counts.needs_you_groups = %d but %d groups carry the band", report.Counts.NeedsYouGroups, banded)
	}
	return []byte(fmt.Sprintf("fixture attention: needs_you_groups=%d\n", banded)), nil
}
