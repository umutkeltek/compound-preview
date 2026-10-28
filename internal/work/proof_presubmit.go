package work

import (
	"bytes"
	"compound/internal/proofscope"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// The presubmit is the minutes-scale pre-landing gate (skills/compound-land
// §1). Like --dev it is non-certifying: it writes no installed-runtime proof
// artifact and never satisfies a governed close. The canonical battery runs
// after the land.
const (
	ProofCheckProfilePresubmit       = "presubmit"
	ProofCheckPresubmitSchemaVersion = "compound.proof_check_presubmit_report.v0.1"
	// ProofCheckPresubmitTestTimeout bounds each package of a presubmit go-test run.
	ProofCheckPresubmitTestTimeout = "-timeout=30m"
)

var presubmitFullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ProofCheckPresubmit records how the gate derived its go-test scope.
type ProofCheckPresubmit struct {
	Head string                   `json:"head"`
	Plan proofscope.PresubmitPlan `json:"plan"`
	// NoGoFiles are inventory directories go list resolves to no buildable
	// or test files (for example only `//go:build ignore` files); go test
	// would refuse them, so presubmit drops them.
	NoGoFiles []string `json:"no_go_files,omitempty"`
}

// ProofCheckNonCertifyingMode returns "--dev" or "--presubmit" when argv
// selects one of the non-certifying runners, and "" otherwise.
func ProofCheckNonCertifyingMode(args []string) string {
	mode := ""
	for _, arg := range args {
		switch {
		case arg == "--presubmit" || strings.HasPrefix(arg, "--presubmit="):
			return "--presubmit"
		case arg == "--dev" || strings.HasPrefix(arg, "--dev="):
			mode = "--dev"
		}
	}
	return mode
}

// ValidateProofCheckNonCertifyingArgs is the strict argv contract of
// `proof-check --dev` and `proof-check --presubmit`: no governance or
// canonical selector, each flag at most once, values as separate tokens.
func ValidateProofCheckNonCertifyingArgs(args []string) error {
	mode := ProofCheckNonCertifyingMode(args)
	if mode == "" {
		mode = "--dev"
	}
	governanceFlags := map[string]bool{
		"--ci":                true,
		"--ci-console":        true,
		"--work":              true,
		"--actor":             true,
		"--ttl":               true,
		"--steal":             true,
		"--reason":            true,
		"--execution-timeout": true,
	}
	booleanFlags := map[string]bool{
		"--dev":       true,
		"--presubmit": true,
		"--json":      true,
		"--dry-run":   true,
	}
	valueFlags := map[string]bool{
		"--repo": true,
		"--base": true,
		"--out":  true,
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		flag := arg
		if eq := strings.IndexByte(flag, '='); eq >= 0 {
			flag = flag[:eq]
		}
		if governanceFlags[flag] {
			return fmt.Errorf("%s cannot be combined with %s", mode, flag)
		}
		if booleanFlags[flag] {
			if arg != flag {
				return fmt.Errorf("%s does not accept a value", flag)
			}
			if seen[flag] {
				return fmt.Errorf("%s may be provided only once", flag)
			}
			seen[flag] = true
			continue
		}
		if valueFlags[flag] {
			if arg != flag {
				return fmt.Errorf("%s requires a separate value", flag)
			}
			if seen[flag] {
				return fmt.Errorf("%s may be provided only once", flag)
			}
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
				return fmt.Errorf("%s requires a non-empty value", flag)
			}
			seen[flag] = true
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unsupported %s flag %s", mode, arg)
		}
		return fmt.Errorf("unexpected %s argument %q", mode, arg)
	}
	if seen["--dev"] && seen["--presubmit"] {
		return fmt.Errorf("--dev and --presubmit are mutually exclusive")
	}
	if seen["--presubmit"] && !seen["--base"] {
		return fmt.Errorf("--presubmit requires --base <full commit SHA>")
	}
	return nil
}

// ProofCheckPresubmitSteps derives the presubmit gates for HEAD against an
// exact ancestor base commit. The running compound binary serves
// `architecture doctor`.
func ProofCheckPresubmitSteps(ctx context.Context, repo, base string) ([]ProofCheckStep, *ProofCheckPresubmit, error) {
	compoundExe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	base = strings.TrimSpace(base)
	if !presubmitFullSHA.MatchString(base) {
		return nil, nil, fmt.Errorf("--presubmit --base must be a full lowercase commit SHA, captured with git rev-parse --verify; got %q", base)
	}
	head, err := presubmitGit(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil, nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if _, err := presubmitGit(ctx, repo, "merge-base", "--is-ancestor", base, head); err != nil {
		return nil, nil, fmt.Errorf("--base %s is not an ancestor of HEAD %s; pass the commit this branch forked from", base, head)
	}
	scope, err := proofscope.DeriveGit(ctx, repo, base, head)
	if err != nil {
		return nil, nil, err
	}
	changed, err := presubmitGit(ctx, repo, "diff", "--name-only", "-z", "--no-renames", base, head, "--")
	if err != nil {
		return nil, nil, err
	}
	// The package universe is the Git tree at head, never a `./...` walk:
	// gitignored directories (operator-private notes with .go files) must not
	// enter the presubmit.
	tree, err := presubmitGit(ctx, repo, "ls-tree", "-r", "-z", "--name-only", "--full-tree", head)
	if err != nil {
		return nil, nil, err
	}
	derivation := &ProofCheckPresubmit{
		Head: head,
		Plan: proofscope.Presubmit(scope, proofscope.InventoryPackages(strings.Split(tree, "\x00"))),
	}
	if derivation.NoGoFiles, err = presubmitDropNoGoFiles(ctx, repo, &derivation.Plan); err != nil {
		return nil, nil, err
	}
	return ProofCheckPresubmitGates(base, head, compoundExe, derivation.Plan, strings.Split(changed, "\x00")), derivation, nil
}

// presubmitDropNoGoFiles keeps only packages go list resolves to Go or test
// files, as the testbattery source inventory does.
func presubmitDropNoGoFiles(ctx context.Context, repo string, plan *proofscope.PresubmitPlan) ([]string, error) {
	seen := map[string]bool{}
	args := []string{"list", "-e", "-f", "{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}"}
	for _, run := range plan.Runs {
		for _, pkg := range run.Packages {
			if !seen[pkg] {
				seen[pkg] = true
				args = append(args, "./"+strings.TrimPrefix(strings.TrimPrefix(pkg, "compound"), "/"))
			}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("presubmit derived no Go packages to test")
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list presubmit packages: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	resolved := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			resolved[line] = true
		}
	}
	var dropped []string
	for i, run := range plan.Runs {
		kept := run.Packages[:0:0]
		for _, pkg := range run.Packages {
			if resolved[pkg] {
				kept = append(kept, pkg)
			} else if !slices.Contains(dropped, pkg) {
				dropped = append(dropped, pkg)
			}
		}
		plan.Runs[i].Packages = kept
	}
	runs := plan.Runs[:0]
	for _, run := range plan.Runs {
		if len(run.Packages) > 0 {
			runs = append(runs, run)
		}
	}
	plan.Runs = runs
	return dropped, nil
}

// ProofCheckPresubmitGates is the ordered gate list for one plan: build and
// vet of the Git source inventory sealed to the head commit (testbattery
// refuses a dirty or drifted tree), format/whitespace/architecture, the
// derived go-test runs, the console type check when console changed, and the
// public redaction of the changed files.
func ProofCheckPresubmitGates(base, head, compoundExe string, plan proofscope.PresubmitPlan, changed []string) []ProofCheckStep {
	steps := []ProofCheckStep{
		{Name: "go-build", Command: []string{"go", "run", "./scripts/testbattery", "build", "--source-head", head}, ProofRef: "presubmit:go-build-inventory"},
		{Name: "go-vet", Command: []string{"go", "run", "./scripts/testbattery", "vet", "--source-head", head}, ProofRef: "presubmit:go-vet-inventory"},
		{Name: "gofmt", Command: []string{"gofmt", "-l", "<tracked *.go>"}, ProofRef: "presubmit:gofmt-check"},
		{Name: "diff-check", Command: []string{"git", "diff", "--check", base + "...HEAD"}, ProofRef: "presubmit:diff-check"},
		{Name: "architecture-doctor", Command: []string{compoundExe, "architecture", "doctor", "--strict"}, ProofRef: "presubmit:architecture-doctor-strict"},
	}
	for _, run := range plan.Runs {
		for _, phase := range proofscope.TestPackagePhases(run.Packages) {
			name, ref := "go-test", "presubmit:go-test-short-affected"
			if run.Run != "" {
				name, ref = "go-test-cli-readers", "presubmit:go-test-short-cli-readers"
			}
			if phase.Name != "general" {
				name += "-" + phase.Name
				ref += "-" + phase.Name
			}
			command := []string{"go", "test", "-short", "-count=1", ProofCheckPresubmitTestTimeout}
			if run.Run != "" {
				command = append(command, "-run", run.Run)
			}
			steps = append(steps, ProofCheckStep{Name: name, Command: append(command, phase.Packages...), ProofRef: ref})
		}
	}
	for _, path := range changed {
		if path == "console" || strings.HasPrefix(path, "console/") {
			steps = append(steps, ProofCheckStep{
				Name:     "console-tsc",
				Command:  []string{"./node_modules/.bin/tsc", "--noEmit"},
				Dir:      "console",
				ProofRef: "presubmit:console-tsc-noemit",
			})
			break
		}
	}
	return append(steps, ProofCheckStep{
		Name:     "redact-public",
		Command:  []string{"redact", "--repo-visibility", "public", "--from-file", "<files changed in " + base + "...HEAD>"},
		ProofRef: "presubmit:redact-public-clean",
	})
}

// ProofCheckPresubmitVerdictLine is the presubmit's single final stdout line.
func ProofCheckPresubmitVerdictLine(report ProofCheckReport, path string) string {
	passed, skipped, failed, planned := 0, 0, 0, 0
	firstFailed := ""
	var elapsedMS int64
	for _, step := range report.Steps {
		elapsedMS += step.DurationMS
		switch step.Status {
		case "ok":
			passed++
		case "skipped":
			skipped++
		case "failed":
			failed++
			if firstFailed == "" {
				firstFailed = step.Name
			}
		case "planned":
			planned++
		}
	}
	scope := "scope=unknown"
	if report.Presubmit != nil {
		plan := report.Presubmit.Plan
		packages := 0
		for _, run := range plan.Runs {
			packages += len(run.Packages)
		}
		scope = fmt.Sprintf("scope=%s reason=%s packages=%d deferred=%d", plan.Scope.Mode, plan.Scope.Reason, packages, len(plan.Deferred))
		if plan.Scope.Mode != proofscope.Affected {
			scope = fmt.Sprintf("scope=%s reason=%s packages=all", plan.Scope.Mode, plan.Scope.Reason)
		}
	}
	out := ""
	if strings.TrimSpace(path) != "" {
		out = " out=" + path
	}
	total := len(report.Steps)
	switch {
	case report.DryRun:
		return fmt.Sprintf("presubmit: planned non_certifying=true %s planned=%d total=%d%s", scope, planned, total, out)
	case report.Status == "ok":
		return fmt.Sprintf("presubmit: ok non_certifying=true %s passed=%d skipped=%d failed=%d total=%d elapsed_ms=%d%s", scope, passed, skipped, failed, total, elapsedMS, out)
	}
	return fmt.Sprintf("presubmit: FAILED non_certifying=true %s passed=%d skipped=%d failed=%d total=%d elapsed_ms=%d first_failed=%s%s", scope, passed, skipped, failed, total, elapsedMS, firstFailed, out)
}

func presubmitGit(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSuffix(string(out), "\x00"), nil
}
