// Package readiness is the read-only evaluation seam shared by
// `compound work delegate --dry-run` and `compound harness readiness`
// (R3B 2026-09-04). The detailed readiness logic still lives in
// cmd/compound/work_delegate_readiness.go and is installed into
// work.DelegateHarnessReadiness via init(). This package wraps that
// seam so the two CLI faces do not duplicate request construction or
// error handling, and so future read-only readiness consumers (the
// doctor harness section, the installer pre-flight, dashboards) call
// the same evaluation.
package readiness

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"compound/internal/work"
)

// EvaluateForHarness builds the minimum request that exercises every
// readiness detail evaluateDelegateHarnessReadiness inspects (permission
// authority + posture, blocking-hook plan + installation, instruction
// pack + context pack, activation argv) for the named harness, and
// returns the result the execute-time guard would compute.
//
// The result is exactly what `work delegate --dry-run` and the
// execute-time refusal would see — same seam, same evidence contract,
// same Details vocabulary. The workID is optional: an empty value is
// what `compound harness readiness` passes; a non-empty value matches
// what an execute advance sees once the envelope is claimed.
func EvaluateForHarness(homePath, harnessName, workspace, workID string, now time.Time) (work.DelegateHarnessReadinessResult, error) {
	if strings.TrimSpace(harnessName) == "" {
		return work.DelegateHarnessReadinessResult{}, errors.New("readiness: harness is required")
	}
	if work.DelegateHarnessReadiness == nil {
		return work.DelegateHarnessReadinessResult{}, errors.New("readiness: guard is not installed (set in cmd/compound composition root)")
	}
	if work.DelegateHarnessResolver == nil {
		return work.DelegateHarnessReadinessResult{}, errors.New("readiness: harness resolver is not installed")
	}
	resolved, err := work.DelegateHarnessResolver(work.DelegateHarnessResolveRequest{HomePath: homePath, Harness: strings.TrimSpace(harnessName), Now: now})
	if err != nil {
		return work.DelegateHarnessReadinessResult{Details: []string{"adapter_preflight: " + err.Error()}}, nil
	}
	req := work.DelegateHarnessReadinessRequest{
		HomePath:               homePath,
		WorkID:                 strings.TrimSpace(workID),
		Workspace:              workspace,
		Harness:                resolved.Harness,
		Command:                resolved.Command,
		LaneID:                 resolved.LaneID,
		RequestedModelID:       resolved.RequestedModelID,
		RequestedProviderRoute: resolved.RequestedProviderRoute,
		Now:                    now,
	}
	if req.WorkID != "" {
		store, err := work.OpenReadOnly(homePath)
		if err != nil {
			return work.DelegateHarnessReadinessResult{}, fmt.Errorf("readiness: open work: %w", err)
		}
		envelope, err := store.Get(req.WorkID)
		if err != nil {
			return work.DelegateHarnessReadinessResult{}, fmt.Errorf("readiness: read work: %w", err)
		}
		req.ContextPackID = envelope.IDs.ContextPackID
	}
	return work.DelegateHarnessReadiness(req)
}

// RepairCommandsForHarness returns one DoThis line per unique unmet
// readiness detail category for the named harness. This matches the
// repair vocabulary the lane-readiness refusal (R3B 2026-09-04) renders,
// so the doctor harness section and the new `compound harness readiness`
// face can both surface actionable repair commands without recomputing
// the readiness evaluation themselves.
func RepairCommandsForHarness(harnessName, workspace string, details []string) []string {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "claude"
	}
	out := []string{
		"compound harness readiness --harness " + harnessName,
		"compound work delegate --dry-run",
	}
	seen := map[string]bool{}
	for _, d := range details {
		var line string
		switch {
		case strings.HasPrefix(d, "permission_authority") || strings.HasPrefix(d, "permission_posture"):
			line = "compound harness permission-mode --harness " + harnessName + "    # permission authority / posture"
		case strings.HasPrefix(d, "blocking_hook") || strings.HasPrefix(d, "mutation-covering"):
			line = "compound hook install --harness " + harnessName + "             # blocking-hook binding"
		case strings.HasPrefix(d, "instruction_pack"):
			line = "compound harness instruction-pack --harness " + harnessName + " # instruction pack / context"
		case strings.HasPrefix(d, "activation_argv"):
			line = "compound harness readiness --harness " + harnessName + " --workspace \"" + strings.TrimSpace(workspace) + "\"  # argv-policy"
		default:
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}
