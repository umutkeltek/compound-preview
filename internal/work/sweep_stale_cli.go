// sweep_stale_cli.go owns the `compound work sweep-stale` verb. It is the
// work-state-machine counterpart to `compound run-attempt sweep`: where the
// run-attempt sweep terminalizes stuck RunAttempts (and explicitly does NOT
// touch the work state machine), sweep-stale advances stuck WorkEnvelopes to
// failed/cancelled and explicitly does NOT touch RunAttempts. The two never
// run on the same byte; each owns its organ.
//
// Spec: docs_private/plan/packets/2026-08-24-work-sweep-stale.md
//
// DRY-RUN IS THE DEFAULT. Mutation is experimental and unsupported in v0.1;
// both --yes and the explicit experimental flag are required to apply.
package work

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ExperimentalSweepStaleMutationsFlag is the explicit unsupported-v0.1
// admission token for stale-work mutation. Read-only planning does not need it.
const ExperimentalSweepStaleMutationsFlag = "--experimental-sweep-stale-mutations"

type sweepStaleCLIOptions struct {
	threshold time.Duration
	dryRun    bool
	jsonOut   bool
}

// SweepStaleCLIRequiresWritableStore classifies the already-tokenized command
// using the same parser as RunWorkSweepStaleCLI. Invalid and read-only requests
// always return false so cmdWork can reject them without materializing state.
func SweepStaleCLIRequiresWritableStore(args []string) bool {
	parsed, err := parseSweepStaleCLIArgs(args)
	return err == nil && !parsed.dryRun
}

func parseSweepStaleCLIArgs(args []string) (sweepStaleCLIOptions, error) {
	parsed := sweepStaleCLIOptions{threshold: DefaultSweepStaleThreshold, dryRun: true}
	var thresholdText string
	var thresholdProvided bool
	var dryRunRequested, applyRequested, experimental bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRunRequested = true
		case "--yes":
			applyRequested = true
		case ExperimentalSweepStaleMutationsFlag:
			experimental = true
		case "--json":
			parsed.jsonOut = true
		case "--threshold":
			if thresholdProvided {
				return sweepStaleCLIOptions{}, fmt.Errorf("--threshold may only be provided once")
			}
			if i+1 >= len(args) {
				return sweepStaleCLIOptions{}, fmt.Errorf("--threshold requires a value")
			}
			i++
			thresholdProvided = true
			thresholdText = args[i]
		default:
			return sweepStaleCLIOptions{}, fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	if thresholdProvided {
		threshold, err := time.ParseDuration(thresholdText)
		if err != nil {
			return sweepStaleCLIOptions{}, fmt.Errorf("--threshold %q is not a valid duration: %w", thresholdText, err)
		}
		if threshold <= 0 {
			return sweepStaleCLIOptions{}, fmt.Errorf("--threshold must be greater than zero, got %q", thresholdText)
		}
		parsed.threshold = threshold
	}
	if dryRunRequested && applyRequested {
		return sweepStaleCLIOptions{}, fmt.Errorf("--dry-run and --yes are mutually exclusive")
	}
	if applyRequested && !experimental {
		return sweepStaleCLIOptions{}, fmt.Errorf("--yes requires %s; stale reclaim mutation is experimental and unsupported in v0.1", ExperimentalSweepStaleMutationsFlag)
	}
	if experimental && !applyRequested {
		return sweepStaleCLIOptions{}, fmt.Errorf("%s requires --yes", ExperimentalSweepStaleMutationsFlag)
	}
	parsed.dryRun = !applyRequested
	return parsed, nil
}

// RunWorkSweepStaleCLI is the testable command edge for
// `compound work sweep-stale [--threshold D] [--dry-run | --yes
// --experimental-sweep-stale-mutations] [--json]`.
func RunWorkSweepStaleCLI(args []string, store *Store, homePath string, now time.Time, stdout, stderr io.Writer) int {
	if sweepStaleHasUsageFlag(args) {
		fmt.Fprintln(stdout, sweepStaleUsageText())
		return 0
	}
	parsed, err := parseSweepStaleCLIArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		fmt.Fprintln(stderr, sweepStaleUsageText())
		return 2
	}

	probe := NewDefaultSweepStaleProbe(homePath)
	// One coherent staleness definition per pass: the run-attempt liveness
	// probe uses the same window as the envelope threshold.
	probe.AttemptLivenessThreshold = parsed.threshold
	var report SweepStaleReport
	if parsed.dryRun {
		report, err = store.SweepStalePlan(parsed.threshold, probe, time.Time{})
	} else {
		report, err = store.SweepStaleApply(parsed.threshold, probe, now)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: work sweep-stale: %v\n", err)
		return 1
	}

	if parsed.jsonOut {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "error: encode sweep-stale: %v\n", err)
			return 1
		}
		return 0
	}

	mode := "dry-run"
	if !parsed.dryRun {
		mode = "applied"
	}
	fmt.Fprintf(stdout, "sweep-stale %s threshold=%s scanned=%d safe=%d reclaimed=%d skipped=%d unreadable=%d\n",
		mode, parsed.threshold, report.Scanned, report.Safe, report.Reclaimed, report.Skipped, report.Unreadable)
	if len(report.Candidates) > 0 {
		fmt.Fprintln(stdout, "candidates:")
		for _, candidate := range report.Candidates {
			status := "would-reclaim"
			if candidate.Reclaimed {
				status = "reclaimed"
			} else if candidate.ReclaimError != "" {
				status = "error:" + candidate.ReclaimError
			}
			fmt.Fprintf(stdout, "  %s state=%s -> %s age=%s %s\n",
				candidate.WorkID, candidate.State, candidate.TargetState, candidate.LastEventAge, status)
		}
	}
	if len(report.UnreadableIDs) > 0 {
		fmt.Fprintln(stdout, "unreadable (probe failure, fail-closed):")
		for _, id := range report.UnreadableIDs {
			fmt.Fprintf(stdout, "  %s\n", id)
		}
	}
	if !parsed.dryRun {
		for _, candidate := range report.Candidates {
			if candidate.ReclaimError != "" {
				return 1
			}
		}
	}
	return 0
}

func sweepStaleHasUsageFlag(args []string) bool {
	for _, argument := range args {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

func sweepStaleUsageText() string {
	return "usage: compound work sweep-stale [--threshold 72h] [--dry-run | --yes --experimental-sweep-stale-mutations] [--json]\n  default is dry-run (prints candidate set + reasons, exits 0).\n  --yes mutation is experimental, unsupported in v0.1, and requires --experimental-sweep-stale-mutations.\n  Never touches run_attempts (compound run-attempt sweep owns that)."
}
