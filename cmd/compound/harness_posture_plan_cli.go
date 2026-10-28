package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"compound/internal/harness"
	"compound/internal/hookplan"
)

func runHarnessPosturePlan(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: compound harness posture-plan --harness <name> [--json]"
	valueFlags := map[string]bool{"--harness": true}
	boolFlags := map[string]bool{"--json": true}
	if err := validateKnownFlags(args, valueFlags, boolFlags); err != nil {
		fmt.Fprintln(stderr, "harness posture-plan:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := rejectFlagUsedAsValue(args, valueFlags); err != nil {
		fmt.Fprintln(stderr, "harness posture-plan:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	harnessName := valueFlag(args, "--harness")
	if harnessName == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	compiled, err := hookplan.CompileDefaultPosture(hookplan.DefaultPostureRequest{
		Harness:         harnessName,
		HandlerRef:      "compound",
		HandlerRevision: "binary:" + buildSHAOrDev(),
		Registry:        harness.DefaultRegistry(),
	})
	if err != nil {
		fmt.Fprintln(stderr, "harness posture-plan:", err)
		return 1
	}

	if hasFlag(args, "--json") {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(compiled); err != nil {
			fmt.Fprintln(stderr, "harness posture-plan:", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "# harness posture-plan %s\n\n", harnessName)
	fmt.Fprintf(stdout, "- family: %s\n", compiled.CompiledFrom.HarnessFamily)
	fmt.Fprintf(stdout, "- surface: %s\n", compiled.CompiledFrom.HarnessSurface)
	fmt.Fprintf(stdout, "- attestation_revision: %s\n", compiled.CompiledFrom.AttestationRevision)
	fmt.Fprintf(stdout, "- mutation_ready: %t\n", compiled.MutationReady)
	for _, intention := range compiled.Intentions {
		fmt.Fprintf(
			stdout,
			"- %s: %s enforcement=%s native=%s",
			intention.Intention,
			intention.Disposition,
			intention.EnforcementStrength,
			strings.Join(intention.NativeEvents, ","),
		)
		if intention.Reason != "" {
			fmt.Fprintf(stdout, " reason=%q", intention.Reason)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}
