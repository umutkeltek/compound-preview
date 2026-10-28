package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/director"
)

const harnessEditReviewUsage = "usage: compound authority harness-edit-review <card.json> --decision whitelist|reject --rationale <text> [--actor <id>] [--json]\n  whitelist opens the surface for edits and requires --actor with operator provenance (e.g. operator:<id>)"

// runAuthorityHarnessEditReview parses `compound authority harness-edit-review
// <card> --decision whitelist|reject --rationale <text> [--actor <id>] [--json]`
// and records the operator decision. It mirrors the authority job-health-review
// shape: parse-then-record, print the JSON report, exit non-zero (writing
// nothing) on a usage or validation refusal. --rationale is required precisely
// because the console-projected argv never carries one — the operator supplies
// it, and a missing rationale must never fall through to a placeholder.
func runAuthorityHarnessEditReview(args []string, homePath string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, harnessEditReviewUsage)
		os.Exit(2)
	}
	cardPath := args[1]
	flags := args[2:]
	decision := flagValue(flags, "--decision")
	rationale := flagValue(flags, "--rationale")
	actor := flagValue(flags, "--actor")
	if strings.TrimSpace(decision) == "" || strings.TrimSpace(rationale) == "" {
		fmt.Fprintln(os.Stderr, harnessEditReviewUsage)
		os.Exit(2)
	}
	report, err := director.RecordHarnessEditReview(homePath, director.HarnessEditReviewInput{
		CardPath:   cardPath,
		Decision:   decision,
		Rationale:  rationale,
		ReviewedBy: actor,
	}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "authority harness-edit-review:", err)
		os.Exit(1)
	}
	printJSON(report)
}
