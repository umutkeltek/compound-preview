package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

// The retained failed proof route restores the original owned report through the EXISTING
// --out custody: the same rooted, CAS-guarded write the battery uses. A recovery that wrote
// the report by another route would not be the same evidence projection.
func TestProofCheckRestoreRetainedReportUsesTheExistingOutCustody(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schema_version":"compound.proof_check_report.v0.6","status":"failed","steps":[]}`)
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "failed-proof.json")
	report, err := proofCheckRestoreRetainedReport(homePath, ref, outPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "failed" {
		t.Fatalf("the restored report is not the retained failed verdict: %+v", report)
	}
	published, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, body) {
		t.Fatal("the restored --out bytes differ from the immutable owned artifact")
	}
	if _, err := proofCheckRestoreRetainedReport(homePath, ref, outPath); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, body) {
		t.Fatal("a repeated restore changed the restored --out bytes")
	}
}

// The command boundary detects the EXACT recovery tokens only. Both spellings of the retained
// route must be recognised, and a longer token that merely shares the prefix is not a recovery
// flag: it must never silently enter the recovery route.
func TestProofCheckRecoveryFlagDetectsExactTokensOnly(t *testing.T) {
	for name, testCase := range map[string]struct {
		args []string
		want bool
	}{
		"spaced":            {[]string{"--resume-retained", "pfr_x"}, true},
		"equals":            {[]string{"--resume-retained=pfr_x"}, true},
		"equals_empty":      {[]string{"--resume-retained="}, true},
		"near_miss_prefix":  {[]string{"--resume-retained-extra", "pfr_x"}, false},
		"near_miss_suffix":  {[]string{"--resume_retained", "pfr_x"}, false},
		"unrelated":         {[]string{"--out", "x.json"}, false},
		"quiescent_spaced":  {[]string{"--reconcile-quiescent", "pfr_x"}, true},
		"quiescent_equals":  {[]string{"--reconcile-quiescent=pfr_x"}, true},
		"quiescent_nearmis": {[]string{"--reconcile-quiescent-extra"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			flag := "--resume-retained"
			if strings.Contains(name, "quiescent") {
				flag = "--reconcile-quiescent"
			}
			if got := work.HasProofRecoveryFlag(testCase.args, flag); got != testCase.want {
				t.Fatalf("recovery token detection for %v = %v, want %v", testCase.args, got, testCase.want)
			}
		})
	}
}

// The retained command boundary is proven through the REAL cmdProofCheck, not through a helper:
// the parent re-execs the test binary with an env toggle whose child arm enters cmdProofCheck
// with the exact arguments. The observed exit code and output therefore come from the production
// selector and dispatch — removing the routing from cmdProofCheck fails these tests.
//
// The child arm runs from an ORPHANED working directory: it enters its own temporary working
// directory and then removes it, so the retained route is exercised without a current source
// checkout and without a resolvable current directory. The route reads its home and its output
// from ABSOLUTE paths, so neither can depend on the missing directory.
const proofCheckRetainedCLIChildEnv = "COMPOUND_PROOFCHECK_RETAINED_CLI_CHILD"

// TestProofCheckRetainedRecoveryChildEntry is the child arm of the retained command-boundary
// tests. It enters cmdProofCheck itself, so the parent's assertions observe the production
// command, not a test-local substitute.
func TestProofCheckRetainedRecoveryChildEntry(t *testing.T) {
	raw := os.Getenv(proofCheckRetainedCLIChildEnv)
	if raw == "" {
		t.Skip("parent-only arm")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	// The recovery route resolves its home and its output from absolute paths, so the missing
	// working directory cannot change the resolution of either.
	for index := 0; index < len(args); index++ {
		name, value := args[index], ""
		switch {
		case strings.HasPrefix(name, "--home="), strings.HasPrefix(name, "--out="):
			value = name[strings.Index(name, "=")+1:]
		case name == "--home", name == "--out":
			if index+1 >= len(args) {
				t.Fatalf("the retained route argument %q has no value", name)
			}
			index++
			value = args[index]
		default:
			continue
		}
		if !filepath.IsAbs(value) {
			t.Fatalf("the retained route argument %s is not absolute: %q", name, value)
		}
	}
	// The child enters its OWN temporary working directory and then removes it. The retained route
	// must not need a current source checkout, and it must not need a current directory either.
	orphan, err := os.MkdirTemp("", "compound-retained-orphan-cwd")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(orphan); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	// The working directory is really UNAVAILABLE: its recorded path no longer exists, and the
	// current working directory does not resolve to an existing directory any more.
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Fatalf("the orphaned working directory still exists at %q: %v", orphan, statErr)
	}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		if _, statErr := os.Stat(wd); !os.IsNotExist(statErr) {
			t.Fatalf("the current working directory is still reachable at %q: %v", wd, statErr)
		}
	} else if !errors.Is(wdErr, os.ErrNotExist) {
		t.Fatalf("the current working directory could not be checked: %v", wdErr)
	}
	cmdProofCheck(args)
	// A route that returns instead of exiting is still an observed exit code.
	os.Exit(0)
}

// proofCheckRetainedCLI runs the real cmdProofCheck in a child arm and reports its exit code and
// its separated stdout and stderr.
func proofCheckRetainedCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckRetainedRecoveryChildEntry$")
	cmd.Env = append(os.Environ(), proofCheckRetainedCLIChildEnv+"="+string(encoded))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("the retained recovery child arm did not run: %v", runErr)
	}
	return code, stdout.String(), stderr.String()
}

// The real command boundary refuses a retained recovery that matches nothing. It never writes
// the --out path, never restores a report and never admits a battery: the refusal happens
// before any source capture, so a moved or missing checkout cannot change the outcome.
func TestProofCheckRetainedRecoveryCommandBoundaryRefusesWithoutCaptureOrAdmission(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The original source checkout moved: it no longer exists at its recorded path.
	movedCheckout := filepath.Join(t.TempDir(), "moved-source-checkout")
	if err := os.MkdirAll(movedCheckout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(movedCheckout); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "never-written.json")
	for name, args := range map[string][]string{
		"spaced": {
			"--resume-retained", "pfr_synthetic",
			"--work", "work_synthetic",
			"--actor", "codex:proof-alpha",
			"--expected-pending-sha256", strings.Repeat("a", 64),
			"--out", outPath,
			"--home", homePath,
		},
		"equals": {
			"--resume-retained=pfr_synthetic",
			"--work", "work_synthetic",
			"--actor", "codex:proof-alpha",
			"--expected-pending-sha256", strings.Repeat("a", 64),
			"--out", outPath,
			"--home", homePath,
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := proofCheckRetainedCLI(t, args...)
			if code != 2 {
				t.Fatalf("a retained recovery that matches nothing must refuse: %d (%s)", code, stdout)
			}
			if !strings.Contains(stderr, "no exact retained failed proof completion") &&
				!strings.Contains(stderr, "read retained proof reservation") {
				t.Fatalf("the refusal did not name the missing retained completion: %q", stderr)
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatal("a refused retained recovery wrote the --out path")
			}
			if stdout != "" {
				t.Fatalf("a refused retained recovery published output: %q", stdout)
			}
			if strings.Contains(stdout, "battery:") || strings.Contains(stdout, "- step:") {
				t.Fatalf("the refused retained recovery published a battery report: %q", stdout)
			}
		})
	}
	// No work was created and no proof resource was admitted by the refused route.
	entries, err := os.ReadDir(filepath.Join(homePath, "work", "work"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("a refused retained recovery created work: %d entries", len(entries))
	}
	// The recovery route takes no checkout at all: a source argument is not even a defined
	// option, so a moved or missing source checkout can neither be captured nor block it.
	code, stdout, stderr := proofCheckRetainedCLI(t,
		"--resume-retained", "pfr_synthetic",
		"--work", "work_synthetic",
		"--actor", "codex:proof-alpha",
		"--expected-pending-sha256", strings.Repeat("a", 64),
		"--out", filepath.Join(t.TempDir(), "never-written.json"),
		"--home", homePath,
		"--repo", movedCheckout,
	)
	if code != 2 || !strings.Contains(stderr, "not defined") {
		t.Fatalf("the recovery route accepted a source checkout argument: %d (%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("the refused retained recovery published output: %q", stdout)
	}
}

// Two recovery routes in one invocation are contradictory. The command boundary refuses them
// before it captures anything.
func TestProofCheckConflictingRecoveryFlagsRefuse(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "never-written.json")
	code, stdout, stderr := proofCheckRetainedCLI(t,
		"--resume-retained", "pfr_synthetic",
		"--reconcile-quiescent", "pfr_synthetic",
		"--work", "work_synthetic",
		"--actor", "codex:proof-alpha",
		"--expected-pending-sha256", strings.Repeat("a", 64),
		"--out", outPath,
		"--home", homePath,
	)
	if code != 2 {
		t.Fatalf("conflicting recovery flags must refuse: %d (%s)", code, stdout)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("the refusal did not name the conflicting recovery routes: %q", stderr)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("a refused conflicting invocation wrote the --out path")
	}
}
