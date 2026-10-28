package mem

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// RunCauseConsumptionCLI owns the cause-consumption command behavior while cmd/compound
// remains a thin process-boundary adapter. The report is read-only unless --record is present.
// The clock is read once for the report and a second time only for record publication.
func RunCauseConsumptionCLI(homePath string, args []string, stdout, stderr io.Writer, now func() time.Time) error {
	if now == nil {
		return fmt.Errorf("cause consumption clock is required")
	}
	report, err := BuildCauseConsumptionReport(homePath, now())
	if err != nil {
		return err
	}
	if causeConsumptionCLIHasFlag(args, "--record") {
		path, err := WriteCauseConsumptionRun(homePath, report, now())
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "recorded: %s\n", path); err != nil {
			return err
		}
	}
	if causeConsumptionCLIHasFlag(args, "--json") {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(body))
		return err
	}
	return writeCauseConsumptionSummary(stdout, report)
}

func causeConsumptionCLIHasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func writeCauseConsumptionSummary(writer io.Writer, report CauseConsumptionReport) error {
	var output strings.Builder
	fmt.Fprintln(&output, "# memory cause consumption")
	fmt.Fprintf(&output, "- generated_at: %s\n", report.GeneratedAt)
	fmt.Fprintf(&output, "- coverage: %s\n", report.CoverageNote)
	fmt.Fprintf(&output, "- transitions_scanned: %d  with_cause: %d\n", report.TransitionsScanned, report.TransitionsWithCause)
	fmt.Fprintln(&output, "- ref_tier_counts:")
	for _, tier := range causeConsumptionTiers {
		fmt.Fprintf(&output, "    %-16s %d\n", tier, report.RefTierCounts[tier])
	}
	fmt.Fprintf(&output, "- motion_from_unpromoted_ratio: %.2f (%s)\n", report.ThesisRatio, report.ThesisNote)
	fmt.Fprintf(&output, "- stranger dealt->caused: %d/%d dealt (%s)\n", report.DealtCausedCount, report.DealtCount, report.DealtCausedNote)
	if len(report.Transitions) == 0 {
		fmt.Fprintln(&output, "- transitions: (none observed)")
	} else {
		fmt.Fprintln(&output, "- transitions:")
		for _, transition := range report.Transitions {
			fmt.Fprintf(
				&output,
				"    %-20s cause=%-10s refs=%d  work=%s\n",
				transition.TransitionID,
				transition.DeltaCause,
				len(transition.CausedByRefs),
				transition.WorkID,
			)
		}
	}
	_, err := io.WriteString(writer, output.String())
	return err
}
