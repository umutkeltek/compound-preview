package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/distiller"
	"compound/internal/mem"
)

// cmdMemoryConcur implements `compound memory concur`: it sends the SAME content
// to N independent model lanes and admits an action on an existing claim only when
// they CONCUR; one dissent parks the claim (flags it needs_review). It is the CLI
// face of the mem concurrence gate (internal/mem/concurrence.go). The model calls
// are rented through distiller.NewConcurrenceRunner (env-configured per lane); mem
// owns the reduction and the park-on-dissent invariant.
//
// usage: compound memory concur --claim <claim_id> --content "<text>"
//
//	--lanes zai,minimax,qwen [--threshold N] [--id <probe_id>] [--json]
//
// threshold defaults to the number of lanes (unanimous concurrence — the
// conservative "be sure" default). Per-lane endpoints come from
// COMPOUND_CONCUR_<LANE>_BASE_URL/_MODEL/_API_KEY; an unconfigured lane abstains
// rather than failing the whole sweep. Exit code is 0 only when the certainty gate
// passes, so a caller can branch on it (the same convention as memory doctor).
func cmdMemoryConcur(store *mem.Store, rest []string) {
	claimID := strings.TrimSpace(valueFlag(rest, "--claim"))
	content := strings.TrimSpace(valueFlag(rest, "--content"))
	lanesCSV := strings.TrimSpace(valueFlag(rest, "--lanes"))
	if claimID == "" || content == "" || lanesCSV == "" {
		fmt.Fprintln(os.Stderr, memoryUsage("concur"))
		os.Exit(2)
	}
	var lanes []string
	for _, l := range strings.Split(lanesCSV, ",") {
		if l = strings.TrimSpace(l); l != "" {
			lanes = append(lanes, l)
		}
	}
	if len(lanes) == 0 {
		fmt.Fprintln(os.Stderr, memoryUsage("concur"))
		os.Exit(2)
	}
	threshold := len(lanes)
	if v := strings.TrimSpace(valueFlag(rest, "--threshold")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "error: --threshold must be a positive integer")
			os.Exit(2)
		}
		threshold = n
	}
	probeID := strings.TrimSpace(valueFlag(rest, "--id"))
	if probeID == "" {
		probeID = "cli"
	}

	probe := mem.ConcurrenceProbe{
		SchemaVersion: mem.ConcurrenceProbeSchemaVersion,
		ID:            probeID,
		ClaimID:       claimID,
		Content:       content,
		Lanes:         lanes,
		Threshold:     threshold,
	}
	report, err := store.RunConcurrence(probe, distiller.NewConcurrenceRunner(), time.Now())
	die(err)

	if hasFlag(rest, "--json") {
		printJSON(report)
		return
	}
	fmt.Println("# memory concur")
	fmt.Printf("- claim: %s\n", report.ClaimID)
	fmt.Printf("- lanes: %s (%d independent)\n", strings.Join(lanes, ","), report.IndependentLaneCount)
	fmt.Printf("- decision: %s\n", report.Decision)
	fmt.Printf("- certainty_gate: %t\n", report.CertaintyGate)
	fmt.Printf("- agreement: %d/%d (threshold %d)\n", report.IndependentAgreementCount, report.IndependentLaneCount, report.Threshold)
	if len(report.Dissenters) > 0 {
		fmt.Printf("- dissenters: %s\n", strings.Join(report.Dissenters, ","))
	}
	if len(report.Abstainers) > 0 {
		fmt.Printf("- abstainers: %s\n", strings.Join(report.Abstainers, ","))
	}
	fmt.Printf("- report: %s\n", report.ReportPath)
	fmt.Printf("- reason: %s\n", report.Reason)
	if !report.CertaintyGate {
		os.Exit(1)
	}
}
