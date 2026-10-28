package promotedriver

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/home"
)

// Usage is the operator-facing usage line for `compound memory promote-driver`.
// It lives HERE (not in cmd/compound) because this subcommand's CLI body lives
// in this organ: the cmd layer stays a thin dispatch, and the cmd/compound LOC
// budget ratchet (internal/arch.checkCmdCompoundBudget) only lets cmd/compound
// shrink.
const Usage = "usage: compound memory promote-driver [--confirm --work W] [--recover] [--dry-run] [--json] [--promote-limit N] [--scan-limit N] [--walk-scan-limit N] [--registry GLOB[,GLOB]... (plan-only)]  (supported apply is a frozen candidate-only pass; dry-run may preview experimental registry and lane-belief phases; apply requires --confirm plus a real maintenance WorkEnvelope id; --recover completes or abandons a frozen pass left over from a prior crash/rollback; receipts append to state/promote-driver/receipts.jsonl)"

// RunCLI runs the `compound memory promote-driver` subcommand over rest (the
// arguments after the subcommand name) and returns the process exit code: 2 on
// flag-validation failure, 1 on runtime error, 0 otherwise. The pass itself is
// one single-flight run of Run. Supported apply freezes exact candidate,
// claim, cursor, and operation preconditions; registry import and lane-belief
// mutation remain experimental and are preview-only here. Dry-run is the
// DEFAULT: a pass only mutates with --confirm, and the admission registry
// grades that flag Governed.
func RunCLI(rest []string) int {
	// Fail-closed flag validation (flag-validation ratchet, internal/flagaudit):
	// every first-party command path must refuse unknown flags instead of
	// silently dropping them. Pattern: internal/work.ValidateWorkListFlags.
	boolFlags := map[string]bool{"--confirm": true, "--recover": true, "--dry-run": true, "--json": true, "--help": true, "-h": true}
	valueFlags := map[string]bool{"--promote-limit": true, "--scan-limit": true, "--walk-scan-limit": true, "--registry": true, "--work": true}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if boolFlags[a] {
			continue
		}
		if valueFlags[a] {
			if i+1 >= len(rest) {
				fmt.Fprintf(os.Stderr, "error: flag %q requires a value\n%s\n", a, Usage)
				return 2
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n%s\n", a, Usage)
		} else {
			fmt.Fprintf(os.Stderr, "error: unknown argument %q\n%s\n", a, Usage)
		}
		return 2
	}
	has := func(name string) bool {
		for _, a := range rest {
			if a == name {
				return true
			}
		}
		return false
	}
	// Feature probe for callers (the autonomy heartbeat) that must detect a
	// stale binary without RUNNING a pass: --help prints usage and returns,
	// touching nothing.
	if has("--help") || has("-h") {
		fmt.Println(Usage)
		return 0
	}
	if has("--confirm") && has("--dry-run") {
		fmt.Fprintf(os.Stderr, "error: --confirm and --dry-run are contradictory; a pass is dry-run by default and --confirm applies it\n%s\n", Usage)
		return 2
	}
	if has("--recover") && has("--dry-run") {
		fmt.Fprintf(os.Stderr, "error: --recover mutates the journal (either completes or abandons a frozen pass); combine with --dry-run only after the recovery door is read-only\n%s\n", Usage)
		return 2
	}
	if has("--recover") && (has("--confirm") || has("--registry")) {
		fmt.Fprintf(os.Stderr, "error: --recover is its own mutation door; do not combine with --confirm or --registry (the recovery path picks its mode from the frozen pass's recorded intents)\n%s\n", Usage)
		return 2
	}
	if has("--confirm") && has("--registry") {
		fmt.Fprintf(os.Stderr, "error: --registry is plan-only; registry import mutation is experimental and excluded from supported promote-driver apply\n%s\n", Usage)
		return 2
	}
	if has("--confirm") && strings.TrimSpace(stringFlagValue(rest, "--work")) == "" {
		fmt.Fprintf(os.Stderr, "error: --confirm requires --work with the current maintenance WorkEnvelope id; autonomous schedulers must mint/claim that work before applying\n%s\n", Usage)
		return 2
	}
	intVals := map[string]int{}
	for _, name := range []string{"--promote-limit", "--scan-limit", "--walk-scan-limit"} {
		v, err := intFlagValue(rest, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n%s\n", err, Usage)
			return 2
		}
		intVals[name] = v
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		return 1
	}
	opts := Options{
		// The safe zero value is a plan: only --confirm (and not --dry-run)
		// opens the mutation door. The programmatic API inherits the same
		// posture — an Options{} caller can never mutate.
		//
		// The pair is also REFUSED rather than silently reconciled:
		// --confirm --dry-run used to quietly mean dry-run, which lets an
		// automation script believe it applied a pass it did not. A
		// contradictory mutation request is a usage error (exit 2), the same
		// posture as the flag-validation ratchet above.
		Apply:          (has("--confirm") || has("--recover")) && !has("--dry-run"),
		Recover:        has("--recover"),
		PromoteLimit:   intVals["--promote-limit"],
		SweepScanLimit: intVals["--scan-limit"],
		WalkScanLimit:  intVals["--walk-scan-limit"],
		WorkID:         stringFlagValue(rest, "--work"),
	}
	if raw := strings.TrimSpace(stringFlagValue(rest, "--registry")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				opts.RegistryPaths = append(opts.RegistryPaths, p)
			}
		}
	}
	report, err := Run(homePath, opts, time.Now())
	if has("--json") {
		printReportJSON(report)
	} else {
		renderReport(report)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// renderReport prints the human-readable pass summary to stdout.
func renderReport(report Report) {
	mode := "apply"
	if report.DryRun {
		mode = "dry-run"
	}
	if report.Locked {
		fmt.Printf("# memory promote-driver: another pass holds the lock; did nothing (pass %s)\n", report.PassID)
		return
	}
	fmt.Printf("# memory promote-driver pass %s (%s)\n", report.PassID, mode)
	if report.Stage != "" {
		fmt.Printf("stage:      %s\n", report.Stage)
	}
	if len(report.ReconciledPassIDs) > 0 {
		fmt.Printf("recovered:  %s\n", strings.Join(report.ReconciledPassIDs, ","))
	}
	if report.Intent != nil {
		fmt.Printf("intent:     %d candidate(s), %d operation(s)\n", len(report.Intent.CandidateIDs), len(report.Intent.Operations))
	}
	fmt.Printf("promote:    %d promoted, %d deferred (limit %d)\n", report.Promote.Promoted, len(report.Promote.Deferred), report.Promote.Limit)
	for _, p := range report.Promote.Promotions {
		fmt.Printf("  - %s basis=%s (%s)\n", p.CandidateID, p.Basis, p.Action)
	}
	fmt.Printf("dedupe:     %d group(s), %d collapsed\n", report.Dedupe.Groups, report.Dedupe.Collapsed)
	if report.Dedupe.Skipped != "" {
		fmt.Printf("  (skipped: %s)\n", report.Dedupe.Skipped)
	}
	for _, src := range report.Imports.Sources {
		fmt.Printf("import:     %s -> %s", src.Path, src.Action)
		if len(src.ClaimIDs) > 0 {
			fmt.Printf(" (%d claims)", len(src.ClaimIDs))
		}
		if src.Error != "" {
			fmt.Printf(" error=%s", src.Error)
		}
		fmt.Println()
	}
	lb := report.LaneBeliefs
	fmt.Printf("lanebeliefs: fold +%d obs, promote %d, cool %d\n", lb.Fold.NewObservationCount, lb.Promote.PromotedCount, lb.Cool.CooledCount)
	if lb.Error != "" {
		fmt.Printf("  (error: %s)\n", lb.Error)
	}
	fmt.Printf("sweep:      walked %d, decoded %d, corrupt %d, adjudicated %d, skipped %d, deferred %d, failed %d, walked_all=%t\n",
		report.Sweep.Walked,
		report.Sweep.Decoded,
		report.Sweep.Corrupt,
		report.Sweep.Adjudicated,
		report.Sweep.Skipped,
		report.Sweep.Deferred,
		report.Sweep.Failed,
		report.Sweep.WalkedAll,
	)
	var verdicts []string
	for v, n := range report.Sweep.VerdictCounts {
		verdicts = append(verdicts, fmt.Sprintf("%s=%d", v, n))
	}
	sort.Strings(verdicts)
	if len(verdicts) > 0 {
		fmt.Printf("  verdicts: %s\n", strings.Join(verdicts, " "))
	}
	if report.Sweep.NextCursor != "" {
		fmt.Printf("  next cursor: %s (re-run to continue)\n", cursorDisplay(report.Sweep.NextCursor))
	}
	if report.PendingAfter != nil {
		fmt.Printf("pending:    %d (reconciled this pass)\n", *report.PendingAfter)
	}
	if report.ReceiptPath != "" {
		fmt.Printf("receipt:    %s\n", report.ReceiptPath)
	}
	for _, e := range report.Promote.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	for _, e := range report.Sweep.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
}

// printReportJSON renders the report as indented JSON on stdout (the cmd-layer
// printJSON equivalent, kept here so the CLI body stays in this organ).
func printReportJSON(report Report) {
	out, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return
	}
	fmt.Println(string(out))
}

// stringFlagValue returns the value following the first occurrence of name in
// args, or "" when the flag is absent or trailing.
func stringFlagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// intFlagValue parses `name <int>` from args. Absent → 0; non-integer or
// negative → error (the repo's parse-error convention; RunCLI exits 2).
func intFlagValue(args []string, name string) (int, error) {
	raw := stringFlagValue(args, name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be non-negative", name)
	}
	return n, nil
}
