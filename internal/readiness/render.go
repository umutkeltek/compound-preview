package readiness

import (
	"encoding/json"
	"fmt"
	"io"

	"compound/internal/work"
)

// Render writes the readiness evaluation to stdout in the same shape the
// CLI face (`compound harness readiness`) has produced since R3B
// 2026-09-04. It returns the exit code the caller should use: 0 when
// Ready=true, 1 when Ready=false (with details + repair commands), and 1
// when JSON encoding itself fails. The text shape matches the original
// face exactly so existing operators, dashboards, and the doctor harness
// section consume byte-identical output.
//
// Both this Render and the JSON shape are kept in lockstep with the
// integration tests in cmd/compound/harness_readiness_cli_test.go; the
// tests pin "status: ready/unready", the detail lines, the self
// referential `compound harness readiness --harness <name>` repair line,
// and the per-category repair vocabulary the lane-readiness refusal also
// surfaces.
func Render(stdout, stderr io.Writer, harnessName, workspace string, result work.DelegateHarnessReadinessResult, jsonOut bool) int {
	repairCmds := RepairCommandsForHarness(harnessName, workspace, result.Details)
	if jsonOut {
		rep := struct {
			Harness        string   `json:"harness"`
			Workspace      string   `json:"workspace,omitempty"`
			Ready          bool     `json:"ready"`
			Details        []string `json:"details,omitempty"`
			RepairCommands []string `json:"repair_commands,omitempty"`
		}{
			Harness:        harnessName,
			Workspace:      workspace,
			Ready:          result.Ready,
			Details:        result.Details,
			RepairCommands: repairCmds,
		}
		if err := json.NewEncoder(stdout).Encode(rep); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if !result.Ready {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# harness readiness\n")
	fmt.Fprintf(stdout, "- harness: %s\n", harnessName)
	if workspace != "" {
		fmt.Fprintf(stdout, "- workspace: %s\n", workspace)
	}
	fmt.Fprintf(stdout, "- status: %s\n", map[bool]string{true: "ready", false: "unready"}[result.Ready])
	for _, detail := range result.Details {
		fmt.Fprintf(stdout, "- detail: %s\n", detail)
	}
	if !result.Ready {
		fmt.Fprintln(stdout, "- repair:")
		for _, cmd := range repairCmds {
			fmt.Fprintf(stdout, "    %s\n", cmd)
		}
	}
	if !result.Ready {
		return 1
	}
	return 0
}
