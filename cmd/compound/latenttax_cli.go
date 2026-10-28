package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/latenttax"
)

// cmdLatentTax is the front door for the one-week latent-tax rig — the read-only
// factorial evaluator. It delegates to runLatentTax, which returns an exit code
// (never calls os.Exit) so the fail-loud gates — an invalid arm config, a
// dose-mismatched filler arm — are exercised directly by tests.
func cmdLatentTax(args []string) {
	if code := runLatentTax(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runLatentTax(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, latentTaxUsage())
		return 2
	}
	switch args[0] {
	case "validate":
		return runLatentTaxValidate(args[1:], stdout, stderr)
	case "plan":
		return runLatentTaxPlan(args[1:], stdout, stderr)
	case "report":
		return runLatentTaxReport(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, latentTaxUsage())
		return 2
	}
}

func latentTaxUsage() string {
	return "usage:\n" +
		"  compound latent-tax validate --task-set <file> --arms <file> [--falsifiers <file>]\n" +
		"    validates the task set + factorial arm config (incl. the dose-matching gate) and,\n" +
		"    if given, the pre-registered falsifier registry. A filler arm whose token length\n" +
		"    diverges from its instruction arm beyond tolerance is a deterministic config error.\n" +
		"  compound latent-tax plan --task-set <file> --arms <file> [--home <dir>] [--out <file>] [--json]\n" +
		"    mints the arm-run matrix (one PROPOSED, ungraded run record per task x dose x arm) into a\n" +
		"    run-index JSONL the dispatcher consumes on CHEAP lanes. Executes nothing; binds no work_id.\n" +
		"  compound latent-tax report --run-index <file> [--task-set <file>] [--arms <file>]\n" +
		"                              [--falsifiers <file>] [--home <dir>] [--out <file>] [--json]\n" +
		"    DERIVES per-arm distributions (thrash from PolicyDecision denials, tokens/latency from\n" +
		"    route evidence, success from the recorded verdict) by READING the ledger for each run's\n" +
		"    work_id, evaluates the three pre-registered kill-criteria, and emits the report skeleton\n" +
		"    plus an adjudication REQUEST citing run receipts. It promotes nothing.\n" +
		"    --out points the report at a file the operator later moves under a reviews home; the\n" +
		"    default is $COMPOUND_HOME/reviews/latenttax/. run-index/report carry work ids — keep them\n" +
		"    under $COMPOUND_HOME, never the repo tree."
}

func runLatentTaxValidate(args []string, stdout, stderr io.Writer) int {
	taskSetPath := flagValue(args, "--task-set")
	armsPath := flagValue(args, "--arms")
	if strings.TrimSpace(taskSetPath) == "" || strings.TrimSpace(armsPath) == "" {
		fmt.Fprintln(stderr, "usage: compound latent-tax validate --task-set <file> --arms <file> [--falsifiers <file>]")
		return 2
	}
	ts, err := latenttax.LoadTaskSet(taskSetPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ac, err := latenttax.LoadArmConfig(armsPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	regNote := "default"
	if fp := strings.TrimSpace(flagValue(args, "--falsifiers")); fp != "" {
		if _, err := latenttax.LoadFalsifierRegistry(fp); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		regNote = fp
	}
	doses := 0
	arms := 0
	for _, d := range ac.Doses {
		doses++
		arms += len(d.Arms)
	}
	fmt.Fprintf(stdout, "# compound latent-tax validate: OK\n")
	fmt.Fprintf(stdout, "task_set: %s (%d tasks)\n", ts.TaskSetID, len(ts.Tasks))
	fmt.Fprintf(stdout, "arms: %s (%d doses, %d arms; dose-matching gate passed)\n", ac.ConfigID, doses, arms)
	fmt.Fprintf(stdout, "falsifiers: %s\n", regNote)
	fmt.Fprintf(stdout, "planned run matrix: %d records\n", len(ts.Tasks)*arms)
	return 0
}

func runLatentTaxPlan(args []string, stdout, stderr io.Writer) int {
	taskSetPath := flagValue(args, "--task-set")
	armsPath := flagValue(args, "--arms")
	if strings.TrimSpace(taskSetPath) == "" || strings.TrimSpace(armsPath) == "" {
		fmt.Fprintln(stderr, "usage: compound latent-tax plan --task-set <file> --arms <file> [--home <dir>] [--out <file>] [--json]")
		return 2
	}
	ts, err := latenttax.LoadTaskSet(taskSetPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ac, err := latenttax.LoadArmConfig(armsPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now := time.Now()
	records, err := latenttax.PlanRuns(ts, ac, now.UTC().Format(time.RFC3339))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	data, err := latenttax.MarshalRunIndex(records)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	outPath, err := latentTaxOutPath(flagValue(args, "--out"), flagValue(args, "--home"), now, ts.TaskSetID+"-runs", ".jsonl")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := writeLatentTaxFile(outPath, data); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, records); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "# compound latent-tax plan: %s\n", ts.TaskSetID)
		fmt.Fprintf(stdout, "minted %d proposed run records (ungraded; dispatcher binds work_id on cheap lanes)\n", len(records))
		fmt.Fprintf(stdout, "run-index: %s\n", outPath)
	}
	return 0
}

func runLatentTaxReport(args []string, stdout, stderr io.Writer) int {
	runIndexPath := flagValue(args, "--run-index")
	if strings.TrimSpace(runIndexPath) == "" {
		fmt.Fprintln(stderr, "usage: compound latent-tax report --run-index <file> [--task-set <file>] [--arms <file>] [--falsifiers <file>] [--home <dir>] [--out <file>] [--json]")
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	runs, err := latenttax.LoadRunIndex(runIndexPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// task-set / arms are optional: they supply the report's identity ids and let the
	// dose-matching gate be re-asserted at report time. Absent, ids come from the runs.
	ts := latenttax.TaskSet{TaskSetID: taskSetIDFromRuns(runs)}
	if tp := strings.TrimSpace(flagValue(args, "--task-set")); tp != "" {
		loaded, err := latenttax.LoadTaskSet(tp)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		ts = loaded
	}
	var ac latenttax.ArmConfig
	if ap := strings.TrimSpace(flagValue(args, "--arms")); ap != "" {
		loaded, err := latenttax.LoadArmConfig(ap)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		ac = loaded
	}
	reg := latenttax.DefaultFalsifierRegistry()
	if fp := strings.TrimSpace(flagValue(args, "--falsifiers")); fp != "" {
		loaded, err := latenttax.LoadFalsifierRegistry(fp)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		reg = loaded
	}

	now := time.Now()
	report, err := latenttax.BuildReport(ts, ac, reg, runs, homePath, buildSHAOrDev(), now.UTC().Format(time.RFC3339))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	data, err := latenttax.MarshalReport(report)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	label := report.TaskSetID
	if strings.TrimSpace(label) == "" {
		label = "latenttax"
	}
	outPath, err := latentTaxOutPath(flagValue(args, "--out"), flagValue(args, "--home"), now, label+"-report", ".json")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := writeLatentTaxFile(outPath, data); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		printLatentTaxReport(stdout, report, outPath)
	}
	return 0
}

func printLatentTaxReport(w io.Writer, r latenttax.Report, outPath string) {
	fmt.Fprintf(w, "# compound latent-tax report: %s (build %s)\n", r.TaskSetID, r.BuildSHA)
	fmt.Fprintf(w, "runs: %d total, %d dispatched, %d graded, %d pending\n", r.RunCount, r.Dispatched, r.Graded, r.Pending)
	fmt.Fprintf(w, "registry: %s (preregistered %s)\n", r.RegistryID, r.PreregisteredAt)
	for _, a := range r.Arms {
		fmt.Fprintf(w, "  arm %-28s dose=%-2d n=%-2d success=%.3f thrash(mean=%.2f max=%.0f) tokens(mean=%.0f) latency_ms(mean=%.0f)\n",
			a.ArmKind, a.Dose, a.Runs, a.SuccessRate, a.Thrash.Mean, a.Thrash.Max, a.Tokens.Mean, a.LatencyMS.Mean)
	}
	fmt.Fprintln(w, "falsifiers (pre-registered kill-criteria):")
	for _, f := range r.Falsifiers {
		fmt.Fprintf(w, "  %-24s %-18s left=%.4f right=%.4f margin=%.4f\n", f.ID, strings.ToUpper(f.Verdict), f.Left, f.Right, f.Margin)
		fmt.Fprintf(w, "      %s\n", f.Detail)
	}
	fmt.Fprintln(w, "adjudication request (REQUEST only — the rig promotes nothing):")
	for _, p := range r.Adjudication.Proposed {
		fmt.Fprintf(w, "  %-24s suggests: %s\n", p.Criterion, p.Suggests)
	}
	if len(r.Adjudication.CitedReceipts) > 0 {
		fmt.Fprintf(w, "  cited receipts: %s\n", strings.Join(r.Adjudication.CitedReceipts, ", "))
	}
	fmt.Fprintf(w, "report: %s\n", outPath)
}

// latentTaxOutPath resolves the output file: an explicit --out wins; otherwise a
// timestamped file under $COMPOUND_HOME/reviews/latenttax/ (generated evidence under
// the home, never the repo tree).
func latentTaxOutPath(outFlag, homeFlag string, now time.Time, label, ext string) (string, error) {
	if s := strings.TrimSpace(outFlag); s != "" {
		return s, nil
	}
	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(homePath, "reviews", "latenttax")
	name := now.UTC().Format("20060102T150405Z") + "-" + slugifyBench(label) + ext
	return filepath.Join(dir, name), nil
}

func writeLatentTaxFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func taskSetIDFromRuns(runs []latenttax.RunRecord) string {
	for _, r := range runs {
		if strings.TrimSpace(r.TaskSetID) != "" {
			return r.TaskSetID
		}
	}
	return ""
}
