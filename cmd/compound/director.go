package main

import (
	"fmt"
	"os"

	"compound/internal/director"
	"compound/internal/home"
)

func directorUsage() string {
	return "usage: compound director propose [--dry-run|--apply] --json"
}

func cmdDirector(args []string) {
	if len(args) == 0 || hasHelpFlag(args) {
		fmt.Fprintln(os.Stderr, directorUsage())
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	if args[0] != "propose" {
		fmt.Fprintln(os.Stderr, directorUsage())
		os.Exit(2)
	}
	jsonOut := false
	apply := false
	dryRun := false
	for _, arg := range args[1:] {
		switch arg {
		case "--json":
			jsonOut = true
		case "--apply":
			apply = true
		case "--dry-run":
			dryRun = true
		default:
			fmt.Fprintln(os.Stderr, directorUsage())
			os.Exit(2)
		}
	}
	if apply && dryRun {
		fmt.Fprintln(os.Stderr, "director propose: choose --dry-run or --apply, not both")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	report, err := director.Propose(homePath, director.Options{Apply: apply})
	if err != nil {
		fmt.Fprintln(os.Stderr, "director propose:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("suggested work mode=%s proposals=%d suppressed=%d skipped=%d\n", report.Mode, report.ProposalCount, report.SuppressedDuplicateCount, report.SkippedCount)
	if report.ResultPath != "" {
		fmt.Printf("result: %s\n", report.ResultPath)
	}
}
