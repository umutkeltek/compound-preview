package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/bench"
	"compound/internal/home"
)

// cmdBench is the evaluator organ front door. It delegates to runBench, which returns an exit
// code (never calls os.Exit) so the fail-loud gates — absent must-hit ids on `run`, a split
// regression on `compare` — are exercised directly by tests without killing the test process.
func cmdBench(args []string) {
	if code := runBench(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runBench(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, benchUsage())
		return 2
	}
	switch args[0] {
	case "run":
		return runBenchRun(args[1:], stdout, stderr)
	case "compare":
		return runBenchCompare(args[1:], stdout, stderr)
	case "convert":
		return runBenchConvert(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, benchUsage())
		return 2
	}
}

func benchUsage() string {
	return "usage:\n" +
		"  compound bench run --suite <file> [--home <dir>] [--json] [--allow-absent] [--suite-kind <kind>] [--no-actionable]\n" +
		"  compound bench compare --before <report.json> --after <report.json> [--json] [--allow-suite-mismatch]\n" +
		"  compound bench convert acceptance --in <acceptance.json> --out <suite.json> [--name <suite>] [--id-map v2|v1|none]\n" +
		"  compound bench convert golden --in <recall-golden.yaml> --out <suite.json> [--name <suite>]\n" +
		"    deterministic: same input bytes -> byte-identical suite.\n" +
		"    " + bench.SplitRuleDoc + "\n" +
		"    dropped (acceptance): required_policy_for, selected_rule, class_cases, task_spec routing — named with counts, never silent.\n" +
		"    --id-map (acceptance only, default v2): translates legacy resolver dotted ids to compound\n" +
		"      graph ids before scoring. v1 shapes: skill.agents.shared.<x> -> skill:<x>, recipe.recipes.\n" +
		"      canonical.<x>.md -> procedure:<x> (dots hyphenated), tool.<x> -> BOTH tool:<x> AND mcp:<x>.\n" +
		"      must_outrank tool-pair rule: a tool.<x> WINNER emits TWO OutrankAssertion entries (tool:<x> and\n" +
		"      mcp:<x>, same losers) — an absent one surfaces honestly via absent_from_graph rather than the\n" +
		"      converter guessing which is real. Forbidden/loser substring patterns translate under the same\n" +
		"      shapes when the pattern IS one of them (equal to, or prefixed by, one of the roots); anything\n" +
		"      else — unmapped-family-shaped patterns and ordinary free-text substrings alike — stays raw,\n" +
		"      tallied as a single unmapped_patterns total (not broken out by family: most patterns are free\n" +
		"      text, not ids). v2 = v1 + verified name-variant mappings from the 2026-07-09 Wave-2 parity\n" +
		"      adjudication: two more generic shapes (skill.codex.skills.<x> / skill.codex.superpowers.<x> ->\n" +
		"      skill:<x>) plus a small literal table for renames neither shape reaches (duplicate-name\n" +
		"      disambiguation, frontmatter-name drift, filler-word drift, one tool landed under a different\n" +
		"      node kind). v2 only ever ADDS matches v1 missed; it never changes a v1 match — v1 stays\n" +
		"      selectable and byte-stable. --id-map none reproduces the pre-id-map converter byte-for-byte\n" +
		"      (today's raw dotted-id suite). Every id/pattern the applied version does not translate is named\n" +
		"      + counted by family under \"unmapped\" — never silently dropped or silently mismapped; this is\n" +
		"      the true W5-J9 parity backlog.\n" +
		"    output carries private ids: write it only under $COMPOUND_HOME (e.g. ~/.compound/bench-suites/), never the repo tree."
}

// runBenchConvert turns a real evaluation source into a bench suite deterministically. It is a pure
// file→file transform (no --home, no ledger read): read --in, convert, write byte-stable --out,
// and print the drop-accounting so no dropped assertion is silent.
func runBenchConvert(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, benchUsage())
		return 2
	}
	mode := args[0]
	inPath := flagValue(args[1:], "--in")
	outPath := flagValue(args[1:], "--out")
	if strings.TrimSpace(inPath) == "" || strings.TrimSpace(outPath) == "" {
		fmt.Fprintln(stderr, benchUsage())
		return 2
	}
	data, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	name := strings.TrimSpace(flagValue(args[1:], "--name"))
	var (
		suite       bench.Suite
		idMapReport bench.IDMapReport
	)
	switch mode {
	case "acceptance":
		if name == "" {
			name = "wave2-acceptance"
		}
		idMap := strings.TrimSpace(flagValue(args[1:], "--id-map"))
		suite, idMapReport, err = bench.ConvertAcceptance(data, name, idMap)
	case "golden":
		if name == "" {
			name = "recall-golden"
		}
		suite, err = bench.ConvertGolden(data, name)
	default:
		fmt.Fprintln(stderr, benchUsage())
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	out, err := bench.MarshalSuite(suite)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	printConvertSummary(stdout, mode, outPath, suite, idMapReport)
	return 0
}

func printConvertSummary(w io.Writer, mode, outPath string, suite bench.Suite, idMap bench.IDMapReport) {
	fmt.Fprintf(w, "# compound bench convert %s -> %s\n", mode, suite.Suite)
	fmt.Fprintf(w, "cases: %d\n", len(suite.Cases))
	held := map[string]int{}
	for _, c := range suite.Cases {
		held[c.Split]++
	}
	fmt.Fprintf(w, "splits: held_in=%d held_out=%d (%s)\n", held[bench.SplitHeldIn], held[bench.SplitHeldOut], bench.SplitRuleDoc)
	if len(suite.Dropped) > 0 {
		fmt.Fprintln(w, "dropped (not surfacing-expressible; named + counted, never silent):")
		for _, d := range suite.Dropped {
			fmt.Fprintf(w, "  %-32s %4d  %s\n", d.Field, d.Count, d.Reason)
		}
	}
	if idMap.Version != "" {
		fmt.Fprintf(w, "id-map: %s\n", idMap.Version)
	}
	if len(idMap.UnmappedIDFamilies) > 0 {
		fmt.Fprintln(w, "unmapped (no v1 canonical id mapping; raw id passthrough, named + counted, never silent):")
		for _, d := range idMap.UnmappedIDFamilies {
			fmt.Fprintf(w, "  %-32s %4d  %s\n", d.Field, d.Count, d.Reason)
		}
	}
	if idMap.UnmappedPatterns > 0 {
		fmt.Fprintf(w, "unmapped_patterns: %d (forbidden/must_outrank substrings with no known family prefix; single total, not per-family — see usage)\n", idMap.UnmappedPatterns)
	}
	fmt.Fprintf(w, "suite: %s\n", outPath)
}

func runBenchRun(args []string, stdout, stderr io.Writer) int {
	suitePath := flagValue(args, "--suite")
	if strings.TrimSpace(suitePath) == "" {
		fmt.Fprintln(stderr, "usage: compound bench run --suite <file> [--home <dir>] [--json] [--allow-absent] [--suite-kind <kind>] [--no-actionable]")
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	suite, err := bench.LoadSuite(suitePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// --no-actionable marks this run as synthetic/diagnostic: the report still persists and scores,
	// but the director will not mine it for harness-edit candidates (prevents fixture noise becoming
	// suggested work). Absent the flag, the report stays actionable (back-compat).
	var actionable *bool
	if hasFlag(args, "--no-actionable") {
		v := false
		actionable = &v
	}
	now := time.Now()
	report, err := bench.Run(bench.Options{
		Home:       homePath,
		Suite:      suite,
		BuildSHA:   buildSHAOrDev(),
		Now:        now,
		SuiteKind:  flagValue(args, "--suite-kind"),
		Actionable: actionable,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	artifactPath, err := writeBenchReport(homePath, report, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	jsonOut := hasFlag(args, "--json")
	if jsonOut {
		if err := encodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		printBenchSummary(stdout, report, artifactPath)
	}

	// Fail-loud gate: a must-hit expected id that is absent from the graph/ledger entirely is a
	// parity gap, not a tuning miss. Exit nonzero unless the operator opts into --allow-absent.
	if report.HasAbsent() && !hasFlag(args, "--allow-absent") {
		fmt.Fprintf(stderr, "bench: %d expected id(s) absent from graph/ledger (parity gap): %s\n",
			len(report.AbsentFromGraph), strings.Join(report.AbsentFromGraph, ", "))
		fmt.Fprintln(stderr, "pass --allow-absent to record the gap without failing the run")
		return 1
	}
	return 0
}

func runBenchCompare(args []string, stdout, stderr io.Writer) int {
	beforePath := flagValue(args, "--before")
	afterPath := flagValue(args, "--after")
	if strings.TrimSpace(beforePath) == "" || strings.TrimSpace(afterPath) == "" {
		fmt.Fprintln(stderr, "usage: compound bench compare --before <report.json> --after <report.json> [--json] [--allow-suite-mismatch]")
		return 2
	}
	before, err := loadBenchReport(beforePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	after, err := loadBenchReport(afterPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cmp := bench.Compare(before, after)
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, cmp); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		bench.WriteCompareSummary(stdout, cmp)
	}
	if len(cmp.InputErrors) > 0 {
		fmt.Fprintf(stderr, "bench compare: non-certifying reports: %s\n", strings.Join(cmp.InputErrors, "; "))
		return 1
	}
	if cmp.InputMismatch {
		fmt.Fprintln(stderr, "bench compare: evaluation inputs or case coverage differ; cannot certify")
		return 1
	}
	// A suite mismatch is an apples-to-oranges comparison and must fail the gate. Accept already
	// encodes this (it is false on mismatch); the exit code enforces it too unless the operator
	// explicitly opts in to record the delta without gating on it (--allow-suite-mismatch).
	if cmp.SuiteMismatch && !hasFlag(args, "--allow-suite-mismatch") {
		fmt.Fprintf(stderr, "bench compare: before suite %q != after suite %q; comparison is apples-to-oranges and cannot be accepted (pass --allow-suite-mismatch to record the delta without gating on it)\n",
			cmp.BeforeSuite, cmp.AfterSuite)
		return 1
	}
	if cmp.Regressed {
		fmt.Fprintf(stderr, "bench compare: regression on splits [%s], lanes [%s] (hit@5/MRR lower or assertion violations higher)\n",
			strings.Join(cmp.RegressedSplit, ", "), strings.Join(cmp.RegressedLanes, ", "))
		return 1
	}
	return 0
}

// writeBenchReport persists the report to $COMPOUND_HOME/reviews/bench/<ts>-<suite>.json —
// generated evidence under COMPOUND_HOME, never the repo tree.
func writeBenchReport(homePath string, report bench.Report, now time.Time) (string, error) {
	dir := filepath.Join(homePath, "reviews", "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := now.UTC().Format("20060102T150405Z") + "-" + slugifyBench(report.Suite) + ".json"
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func loadBenchReport(path string) (bench.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bench.Report{}, err
	}
	var r bench.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return bench.Report{}, fmt.Errorf("parse report %s: %w", path, err)
	}
	return r, nil
}

func encodeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

func printBenchSummary(w io.Writer, report bench.Report, artifactPath string) {
	fmt.Fprintf(w, "# compound bench %s (build %s, graph %s)\n", report.Suite, report.BuildSHA, report.GraphHash)
	fmt.Fprintf(w, "cases: %d scored, %d skipped\n", report.ScoredCount, report.SkippedCount)
	for _, split := range []string{bench.SplitHeldIn, bench.SplitHeldOut} {
		m := report.Splits[split]
		fmt.Fprintf(w, "  %-9s n=%-3d hit@1=%.3f hit@5=%.3f mrr=%.3f\n", split, m.Cases, m.Hit1, m.Hit5, m.MRR)
	}
	fmt.Fprintf(w, "  %-9s n=%-3d hit@1=%.3f hit@5=%.3f mrr=%.3f\n", "overall", report.Overall.Cases, report.Overall.Hit1, report.Overall.Hit5, report.Overall.MRR)
	if len(report.AbsentFromGraph) > 0 {
		fmt.Fprintf(w, "absent_from_graph (%d): %s\n", len(report.AbsentFromGraph), strings.Join(report.AbsentFromGraph, ", "))
	}
	fmt.Fprintf(w, "report: %s\n", artifactPath)
}

func slugifyBench(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "suite"
	}
	return out
}
