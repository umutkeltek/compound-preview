package main

import (
	"fmt"
	"os"
	"time"

	"compound/internal/home"
	"compound/internal/jobprobe"
)

func jobProbeUsage() string {
	return "usage: compound jobprobe collect [--json]"
}

// cmdJobProbe runs the job-roster probe-refresh collector: it re-observes the
// current liveness of declared probe targets (files under COMPOUND_HOME) and
// re-imports the observed state onto the job-roster authority, so
// `compound director propose --dry-run` derives job_health from observed state
// rather than the blind imported snapshot.
func cmdJobProbe(args []string) {
	if len(args) == 0 || hasHelpFlag(args) {
		fmt.Fprintln(os.Stderr, jobProbeUsage())
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	if args[0] != "collect" {
		fmt.Fprintln(os.Stderr, jobProbeUsage())
		os.Exit(2)
	}
	jsonOut := false
	for _, arg := range args[1:] {
		switch arg {
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintln(os.Stderr, jobProbeUsage())
			os.Exit(2)
		}
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	report, err := jobprobe.Collect(homePath, jobprobe.NewSystemProbeRunner(), time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobprobe collect:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("jobprobe collect: targets=%d probed=%d alive=%d dead=%d failure_refreshed=%d\n",
		report.TargetCount, report.ProbedCount, report.AliveCount, report.DeadCount, report.Refresh.FailureRefreshedCount)
	for _, warning := range report.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
}
