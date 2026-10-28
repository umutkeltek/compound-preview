package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"compound/internal/harness"
	"compound/internal/work"
)

// `compound harness capability-drift` answers a question the registry could not:
// does this harness actually have the capabilities we route work on?
//
// The registry is a declaration. For lane-backed harnesses nothing ever compared
// it to the lane, and it over-claimed in the way that hurts: hooks, skills and
// MCP all declared true over a lane whose settings registered no hooks and whose
// launcher passed --no-mcp --no-skills. Work routed there could not report back,
// and the failure looked like silence rather than an error.
//
// Exit code is nonzero only for OVER-claims. An under-claim wastes a capability;
// an over-claim routes work into a hole. Ranking them the same way trains the
// operator to ignore the check, which is how the original mismatch survived.
const laneWrapperArtifactKind = "wrapper"

func cmdHarnessCapabilityDrift(args []string) {
	jsonOut := false
	only := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--harness":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: compound harness capability-drift [--harness NAME] [--json]")
				os.Exit(2)
			}
			i++
			only = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\nusage: compound harness capability-drift [--harness NAME] [--json]\n", args[i])
			os.Exit(2)
		}
	}

	registry := harness.DefaultRegistry()
	names := make([]string, 0, len(registry.Harnesses))
	for name := range registry.Harnesses {
		if only != "" && name != only {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintf(os.Stderr, "no registered harness matches %q\n", only)
		os.Exit(2)
	}

	type row struct {
		Harness string                         `json:"harness"`
		Status  string                         `json:"status"`
		Reason  string                         `json:"reason,omitempty"`
		Report  *harness.CapabilityDriftReport `json:"report,omitempty"`
	}
	rows := make([]row, 0, len(names))
	overclaimed := 0
	for _, name := range names {
		declared := registry.Harnesses[name].Capabilities
		// A harness name like `claude_minimax` carries its lane in the suffix; the
		// resolver owns the spelling conventions.
		lane := strings.TrimPrefix(strings.ReplaceAll(name, "_", "-"), "claude-")
		artifacts, err := work.ResolveLaneArtifacts(name, lane)
		if err != nil {
			// Unknown is not clean. A harness whose real files cannot be located
			// is exactly the case that hid this bug, so it must not read as pass.
			rows = append(rows, row{Harness: name, Status: "unknown", Reason: err.Error()})
			continue
		}
		observed, obsErr := harness.ObserveLaneCapabilities(
			artifacts[work.SessionStoreSettingsArtifactKind], artifacts[laneWrapperArtifactKind],
		)
		if obsErr != nil {
			rows = append(rows, row{Harness: name, Status: "unknown", Reason: obsErr.Error()})
			continue
		}
		report := harness.CapabilityDrift(name, declared, observed)
		status := "clean"
		if report.Overclaims > 0 {
			status = "overclaim"
			overclaimed++
		} else if report.Drifted {
			status = "underclaim"
		}
		rows = append(rows, row{Harness: name, Status: status, Report: &report})
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"schema_version": "compound.harness_capability_drift.v0.1",
			"overclaimed":    overclaimed,
			"harnesses":      rows,
		})
	} else {
		fmt.Println("# harness capability drift")
		for _, r := range rows {
			fmt.Printf("- %s: %s", r.Harness, r.Status)
			if r.Reason != "" {
				fmt.Printf(" (%s)", r.Reason)
			}
			fmt.Println()
			if r.Report == nil {
				continue
			}
			for _, m := range r.Report.Mismatches {
				kind := "underclaims"
				if m.Overclaimed {
					kind = "OVERCLAIMS"
				}
				fmt.Printf("    %s %s: declared=%v observed=%v\n", kind, m.Capability, m.Declared, m.Observed)
			}
			for _, e := range r.Report.Evidence {
				fmt.Printf("    evidence: %s\n", e)
			}
		}
	}
	if overclaimed > 0 {
		os.Exit(1)
	}
}
