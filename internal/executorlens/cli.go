package executorlens

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"compound/internal/home"
)

// RunCLI is the operator/orchestrator door onto the executor-receipt locator — the
// evidence-only collector that reads THIS repo's executor lanes' (Codex, Claude Code) receipts/events
// and turns them into behavioral-lens signals. It delegates to runCLI, which returns an exit
// code (never calls os.Exit) so the visibility gate and fail-loud paths are exercised by tests.
//
// The command body lives with its organ rather than in cmd/compound, matching the
// schedule.Run / browser.Run / lanematrix.Run pattern, so cmd/compound stays a thin
// dispatch layer under its ratcheting size budget. It is RunCLI and not Run because
// Run is already this package's collector entry point (Run(homePath, record, now)) —
// the CLI face is the caller of that, not a second spelling of it.
func RunCLI(args []string) {
	if code := runCLI(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func cliUsage() string {
	return "usage:\n" +
		"  compound executor-lens subjects [--home P] [--json]\n" +
		"    list the executor lanes (Codex, Claude Code) found in this home's receipts/events and how\n" +
		"    many hedging (public_safe) and agreement (operator_private) observations they yield — no writes\n" +
		"  compound executor-lens run [--home P] [--record] [--json]\n" +
		"    collect executor observations, compose them through the home registry (operator_private lenses\n" +
		"    run only if this home enables them — default off, stranger clone fail-closed), and emit the\n" +
		"    OUTWARD-safe public projection; --record appends every produced signal under $COMPOUND_HOME\n" +
		"the collector reads recorded envelope files under $COMPOUND_HOME/events; it makes NO provider calls\n" +
		"and the outward projection is public_safe only — operator_private executor signals never leave the home."
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || cliHasUsageFlag(args) {
		fmt.Fprintln(stderr, cliUsage())
		return 2
	}
	switch args[0] {
	case "subjects":
		return runCLISubjects(args[1:], stdout, stderr)
	case "run":
		return runCLIRun(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, cliUsage())
		return 2
	}
}

func runCLISubjects(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(cliFlagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	col, err := CollectObservations(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cliHasFlag(args, "--json") {
		if err := cliEncodeJSON(stdout, col); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# executor-lens subjects (%d executor lanes seen)\n", len(col.Subjects))
	for _, s := range col.Subjects {
		fmt.Fprintf(stdout, "- %s\n", s)
	}
	fmt.Fprintf(stdout, "hedging observations (public_safe): %d\n", len(col.Hedging))
	fmt.Fprintf(stdout, "agreement observations (operator_private): %d\n", len(col.Agreement))
	fmt.Fprintln(stdout, "note: observations become EVIDENCE signals only when composed (see `run`); nothing is gated or promoted.")
	return 0
}

func runCLIRun(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(cliFlagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	report, err := Run(homePath, cliHasFlag(args, "--record"), time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cliHasFlag(args, "--json") {
		if err := cliEncodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# executor-lens run (home=%s)\n", report.HomePath)
	fmt.Fprintf(stdout, "executor lanes: %d | hedging obs: %d | agreement obs: %d\n",
		len(report.Subjects), report.HedgingObs, report.AgreementObs)
	fmt.Fprintf(stdout, "agreement-asymmetry lens enabled this home: %v\n", report.AgreementEnabled)
	fmt.Fprintf(stdout, "composed signals: %d | recorded: %d\n", report.Composed, report.Recorded)
	fmt.Fprintln(stdout, "# public projection (public_safe only — operator_private stays under $COMPOUND_HOME)")
	for _, sig := range report.PublicSignals {
		fmt.Fprintf(stdout, "- lens=%s subject=%s value=%.3f confidence=%.2f visibility=%s\n",
			sig.LensID, sig.Subject, sig.Value, sig.Confidence, sig.Visibility)
	}
	return 0
}

// Flag helpers are package-local rather than imported, matching the organ-CLI
// pattern already used by internal/browser and internal/schedule: an organ owns
// its own small argv reading so moving a command out of cmd/compound does not
// require exporting cmd/compound's unexported helpers.

func cliHasUsageFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func cliHasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func cliFlagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func cliEncodeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
