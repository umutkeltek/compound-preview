package main

import (
	"compound/internal/proofscope"
	"compound/internal/work"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// newProofCheckPresubmitRepo is a CLI-shaped module named "compound" so the
// real proofscope derivation runs: cmd/compound imports internal/work, which
// imports internal/landlease; internal/arch reads the repository root.
func newProofCheckPresubmitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	proofCheckDevelopmentGit(t, repo, "init", "-q")
	for name, body := range map[string]string{
		"go.mod":                        "module compound\n\ngo 1.23\n",
		"AGENTS.md":                     "# agents\n",
		"skills/compound-land/SKILL.md": "# land\n",
		"cmd/compound/main.go":          "package main\n\nimport _ \"compound/internal/work\"\n\nfunc main() {}\n",
		"internal/work/work.go":         "package work\n\nimport _ \"compound/internal/landlease\"\n",
		"internal/landlease/lease.go":   "package landlease\n",
		"internal/arch/arch.go":         "package arch\n",
		"internal/arch/arch_test.go":    "package arch\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestRoot(t *testing.T) { _, _, _, _ = runtime.Caller(0) }\n",
		"internal/policy/policy.go":     "package policy\n",
		"tools/gen.go":                  "//go:build ignore\n\npackage main\n",
		".gitignore":                    "private/\n",
	} {
		proofCheckDevelopmentWriteFile(t, filepath.Join(repo, name), body)
	}
	base := proofCheckPresubmitCommit(t, repo, "base")
	// Operator-private notes: a gitignored directory holding Go files that do
	// not even compile. A `./...` walk would reach them; the Git inventory
	// never does.
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "private/notes/notes.go"), "package notes\n\nthis is not Go\n")
	return repo, base
}

func proofCheckPresubmitCommit(t *testing.T, repo, message string) string {
	t.Helper()
	identity := []string{"-c", "user.name=Compound Test", "-c", "user.email=test@example.invalid"}
	proofCheckDevelopmentGit(t, repo, append(identity, "add", "-A", ".")...)
	proofCheckDevelopmentGit(t, repo, append(identity, "commit", "-qm", message)...)
	return strings.TrimSpace(proofCheckDevelopmentGit(t, repo, "rev-parse", "HEAD"))
}

func proofCheckPresubmitDryRun(t *testing.T, repo, base string) (proofCheckReport, string) {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "presubmit.json")
	stdout := captureStdout(t, func() {
		cmdProofCheck([]string{"--presubmit", "--dry-run", "--repo", repo, "--base", base, "--out", outPath})
	})
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --out report: %v\n%s", err, stdout)
	}
	var report proofCheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("parse --out report: %v\n%s", err, body)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	return report, lines[len(lines)-1]
}

func proofCheckPresubmitGoTests(report proofCheckReport) map[string][]string {
	runs := map[string][]string{}
	for _, step := range report.Steps {
		if strings.HasPrefix(step.Name, "go-test") {
			runs[step.Name] = step.Command
		}
	}
	return runs
}

func TestProofCheckPresubmitDerivesGatesFromTheDiff(t *testing.T) {
	repo, base := newProofCheckPresubmitRepo(t)
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "internal/landlease/lease.go"), "package landlease\n\nconst TTL = 1\n")
	head := proofCheckPresubmitCommit(t, repo, "landlease change")

	report, verdict := proofCheckPresubmitDryRun(t, repo, base)
	if report.SchemaVersion != work.ProofCheckPresubmitSchemaVersion || report.ProfileKind != work.ProofCheckProfilePresubmit {
		t.Fatalf("schema/profile = %q/%q", report.SchemaVersion, report.ProfileKind)
	}
	if report.ArtifactRef != "" || report.CIOnly || report.CIConsole {
		t.Fatalf("presubmit report claimed canonical evidence: %+v", report)
	}
	if report.Presubmit == nil || report.Presubmit.Head != head || report.BaseRef != base {
		t.Fatalf("presubmit derivation = %+v base=%q; want head %s", report.Presubmit, report.BaseRef, head)
	}
	var names []string
	for _, step := range report.Steps {
		names = append(names, step.Name)
		if step.Status != "planned" || !strings.HasPrefix(step.ProofRef, "presubmit:") {
			t.Fatalf("step %s status=%s proof_ref=%s", step.Name, step.Status, step.ProofRef)
		}
	}
	wantNames := []string{"go-build", "go-vet", "gofmt", "diff-check", "architecture-doctor", "go-test", "go-test-cli", "go-test-work", "redact-public"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("steps = %v; want %v", names, wantNames)
	}
	for i, mode := range []string{"build", "vet"} {
		if want := []string{"go", "run", "./scripts/testbattery", mode, "--source-head", head}; !reflect.DeepEqual(report.Steps[i].Command, want) {
			t.Fatalf("%s gate = %v; want the sealed Git-inventory gate %v", mode, report.Steps[i].Command, want)
		}
	}
	wantTests := map[string][]string{
		"go-test":      {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/internal/arch", "compound/internal/landlease"},
		"go-test-cli":  {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/cmd/compound"},
		"go-test-work": {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/internal/work"},
	}
	if got := proofCheckPresubmitGoTests(report); !reflect.DeepEqual(got, wantTests) {
		t.Fatalf("go-test runs = %v; want %v", got, wantTests)
	}
	if diffCheck := report.Steps[3].Command; !reflect.DeepEqual(diffCheck, []string{"git", "diff", "--check", base + "...HEAD"}) {
		t.Fatalf("diff-check = %v", diffCheck)
	}
	if !strings.HasPrefix(verdict, "presubmit: planned non_certifying=true scope=affected reason=diff packages=4 deferred=0 planned=9 total=9 out=") {
		t.Fatalf("verdict line = %q", verdict)
	}
}

func TestProofCheckPresubmitDocsOnlyRunsDoctrineReaders(t *testing.T) {
	repo, base := newProofCheckPresubmitRepo(t)
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "skills/compound-land/SKILL.md"), "# land\n\nPresubmit first.\n")
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "AGENTS.md"), "# agents\n\nProof discipline.\n")
	proofCheckPresubmitCommit(t, repo, "docs only")

	report, verdict := proofCheckPresubmitDryRun(t, repo, base)
	wantTests := map[string][]string{
		"go-test":                 {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/internal/arch"},
		"go-test-cli-readers-cli": {"go", "test", "-short", "-count=1", "-timeout=30m", "-run", proofscope.PresubmitCLIReaders, "compound/cmd/compound"},
	}
	if got := proofCheckPresubmitGoTests(report); !reflect.DeepEqual(got, wantTests) {
		t.Fatalf("go-test runs = %v; want %v", got, wantTests)
	}
	if !strings.Contains(verdict, " scope=affected reason=diff packages=2 ") {
		t.Fatalf("verdict line = %q", verdict)
	}
}

func TestProofCheckPresubmitRunsWholeModuleForModuleAndRemovedPackages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(t *testing.T, repo string)
		reason string
	}{
		{"go.mod", func(t *testing.T, repo string) {
			proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "go.mod"), "module compound\n\ngo 1.24\n")
		}, "module-files:go.mod"},
		{"removed package", func(t *testing.T, repo string) {
			if err := os.RemoveAll(filepath.Join(repo, "internal", "policy")); err != nil {
				t.Fatal(err)
			}
		}, "package-removed:internal/policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := newProofCheckPresubmitRepo(t)
			tc.edit(t, repo)
			proofCheckPresubmitCommit(t, repo, tc.name)
			report, verdict := proofCheckPresubmitDryRun(t, repo, base)
			// The whole Git inventory, named: never `./...`, never the
			// gitignored private/notes package, never the ignore-only tools dir.
			inventory := []string{"compound/cmd/compound", "compound/internal/arch", "compound/internal/landlease", "compound/internal/policy", "compound/internal/work"}
			if tc.name == "removed package" {
				inventory = slices.DeleteFunc(inventory, func(pkg string) bool { return pkg == "compound/internal/policy" })
			}
			general := slices.DeleteFunc(append([]string(nil), inventory...), func(pkg string) bool {
				return pkg == "compound/internal/work" || pkg == "compound/cmd/compound"
			})
			want := map[string][]string{
				"go-test":      append([]string{"go", "test", "-short", "-count=1", "-timeout=30m"}, general...),
				"go-test-cli":  {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/cmd/compound"},
				"go-test-work": {"go", "test", "-short", "-count=1", "-timeout=30m", "compound/internal/work"},
			}
			if got := proofCheckPresubmitGoTests(report); !reflect.DeepEqual(got, want) {
				t.Fatalf("go-test runs = %v; want %v", got, want)
			}
			if report.Presubmit == nil || !reflect.DeepEqual(report.Presubmit.NoGoFiles, []string{"compound/tools"}) {
				t.Fatalf("no_go_files = %+v; want the ignore-only tools dir", report.Presubmit)
			}
			if !strings.Contains(verdict, " scope=full reason="+tc.reason+" packages=all ") {
				t.Fatalf("verdict line = %q", verdict)
			}
		})
	}
}

// A changed file under a gitignored directory never reaches the diff, and an
// edit there can never add its package to the presubmit scope.
func TestProofCheckPresubmitIgnoresGitignoredGoPackages(t *testing.T) {
	repo, base := newProofCheckPresubmitRepo(t)
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "private/notes/more.go"), "package notes\n")
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "AGENTS.md"), "# agents\n\nmore\n")
	proofCheckPresubmitCommit(t, repo, "docs plus ignored notes")
	report, _ := proofCheckPresubmitDryRun(t, repo, base)
	for name, command := range proofCheckPresubmitGoTests(report) {
		for _, arg := range command {
			if strings.Contains(arg, "private") || arg == "./..." {
				t.Fatalf("%s reaches %q: %v", name, arg, command)
			}
		}
	}
	if report.Presubmit.Plan.Scope.Mode != proofscope.Affected {
		t.Fatalf("scope = %+v; the ignored edit must not widen a docs-only diff", report.Presubmit.Plan.Scope)
	}
}

func TestProofCheckPresubmitRefusesMovingOrForeignBases(t *testing.T) {
	repo, base := newProofCheckPresubmitRepo(t)
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "internal/policy/policy.go"), "package policy\n\nconst P = 1\n")
	head := proofCheckPresubmitCommit(t, repo, "policy change")
	proofCheckDevelopmentGit(t, repo, "checkout", "-q", "-b", "side", base)
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "internal/arch/arch.go"), "package arch\n\nconst A = 1\n")
	side := proofCheckPresubmitCommit(t, repo, "side change")
	proofCheckDevelopmentGit(t, repo, "checkout", "-q", head)

	for _, tc := range []struct{ base, want string }{
		{"main", "full lowercase commit SHA"},
		{base[:12], "full lowercase commit SHA"},
		{strings.ToUpper(base), "full lowercase commit SHA"},
		{side, "is not an ancestor of HEAD"},
	} {
		if _, err := runNonCertifyingProofCheck(repo, true, tc.base, time.Now(), "--presubmit"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("base %q: err = %v; want %q", tc.base, err, tc.want)
		}
	}
	if _, err := runNonCertifyingProofCheck(repo, true, base, time.Now(), "--presubmit"); err != nil {
		t.Fatalf("exact ancestor base refused: %v", err)
	}
}

func TestProofCheckPresubmitArgsAreStrict(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, args := range [][]string{
		{"--presubmit"},
		{"--presubmit", "--dev", "--base", sha},
		{"--presubmit", "--base", sha, "--work", "work_example"},
		{"--presubmit", "--base", sha, "--ci"},
		{"--presubmit=true", "--base", sha},
		{"--presubmit", "--presubmit", "--base", sha},
	} {
		if err := validateProofCheckDevelopmentArgs(args); err == nil {
			t.Fatalf("presubmit args %v accepted", args)
		}
	}
	if err := validateProofCheckDevelopmentArgs([]string{"--presubmit", "--base", sha, "--out", "r.json", "--json", "--dry-run"}); err != nil {
		t.Fatalf("valid presubmit args rejected: %v", err)
	}
	if !proofCheckDevelopmentRequested([]string{"--presubmit=true"}) || !work.ProofCheckLightweightMode([]string{"--presubmit"}) {
		t.Fatal("assignment-form or lightweight detection would let --presubmit reach the certifying path")
	}
	if _, err := work.ParseProofCheckRouteOptions([]string{"--presubmit", "--redact-review", "act_example"}); err == nil {
		t.Fatal("reviewed redaction accepted in a presubmit")
	}
}

func TestProofCheckPresubmitIsolatesSharedIOWithoutDroppingTests(t *testing.T) {
	const home = "compound/internal/home"
	const release = "compound/internal/release"
	const cli = "compound/cmd/compound"
	const core = "compound/internal/work"
	const hook = "compound/internal/hook"
	const general = "compound/internal/arch"
	tests := []struct {
		name     string
		packages []string
		selector string
		want     [][]string
	}{
		{"mixed", []string{home, core, release, hook, cli, general}, "", [][]string{{general}, {cli}, {core}, {release}, {hook}, {home}}},
		{"empty", nil, "", nil},
		{"hook-only", []string{hook}, "", [][]string{{hook}}},
		{"hook-absent", []string{home, core, release, cli, general}, "", [][]string{{general}, {cli}, {core}, {release}, {home}}},
		{"exact-hook-path-only", []string{"compound/internal/hookplan", hook}, "", [][]string{{"compound/internal/hookplan"}, {hook}}},
		{"hook-selection-preserved", []string{home, hook, general}, "^TestSelected$", [][]string{{general}, {hook}, {home}}},
		{"home-only", []string{home}, "", [][]string{{home}}},
		{"release-only", []string{release}, "", [][]string{{release}}},
		{"work-only", []string{core}, "", [][]string{{core}}},
		{"cli-only", []string{cli}, "", [][]string{{cli}}},
		{"general-only", []string{general}, "", [][]string{{general}}},
		{"selection-preserved", []string{home, cli}, "Selected", [][]string{{cli}, {home}}},
		{"work-selection-preserved", []string{home, core, cli}, "Selected", [][]string{{cli}, {core}, {home}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := proofscope.PresubmitPlan{Runs: []proofscope.PresubmitRun{{Packages: tt.packages, Run: tt.selector}}}
			steps := work.ProofCheckPresubmitGates(strings.Repeat("a", 40), strings.Repeat("b", 40), "compound", plan, nil)
			var got [][]string
			for _, step := range steps {
				if len(step.Command) > 1 && step.Command[0] == "go" && step.Command[1] == "test" {
					got = append(got, step.Command)
				}
			}
			var want [][]string
			for _, packages := range tt.want {
				command := []string{"go", "test", "-short", "-count=1", "-timeout=30m"}
				if tt.selector != "" {
					command = append(command, "-run", tt.selector)
				}
				want = append(want, append(command, packages...))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("shared IO phases must preserve complete scope, selectors and deadlines: got %v; want %v", got, want)
			}
		})
	}
}

func TestProofCheckPresubmitHookPhaseRetainsPriorGatesAndFilteredMembership(t *testing.T) {
	for _, selector := range []string{"", "^TestSelected$"} {
		t.Run("selector="+selector, func(t *testing.T) {
			packages := []string{"compound/internal/arch", "compound/cmd/compound", "compound/internal/work", "compound/internal/release", "compound/internal/home"}
			plan := proofscope.PresubmitPlan{Runs: []proofscope.PresubmitRun{{Packages: packages, Run: selector}}}
			gates := func(plan proofscope.PresubmitPlan) []work.ProofCheckStep {
				return work.ProofCheckPresubmitGates(strings.Repeat("a", 40), strings.Repeat("b", 40), "compound", plan, []string{"console/fixture.ts"})
			}
			prior := gates(plan)
			plan.Runs[0].Packages = append(append([]string(nil), packages...), "compound/internal/hook")
			current := gates(plan)
			var retained []work.ProofCheckStep
			seen := map[string]int{}
			hookCount := 0
			for _, step := range current {
				if step.Command[0] == "go" && step.Command[1] == "test" {
					wantPrefix := []string{"go", "test", "-short", "-count=1", "-timeout=30m"}
					packageOffset := len(wantPrefix)
					if selector != "" {
						wantPrefix = append(wantPrefix, "-run", selector)
						packageOffset = len(wantPrefix)
					}
					if !reflect.DeepEqual(step.Command[:packageOffset], wantPrefix) {
						t.Fatalf("changed flags/filter: %v", step.Command)
					}
					for _, pkg := range step.Command[packageOffset:] {
						seen[pkg]++
					}
					if slices.Contains(step.Command[packageOffset:], "compound/internal/hook") {
						hookCount++
						name, ref := "go-test-hook", "presubmit:go-test-short-affected-hook"
						if selector != "" {
							name, ref = "go-test-cli-readers-hook", "presubmit:go-test-short-cli-readers-hook"
						}
						if step.Name != name || step.ProofRef != ref || !reflect.DeepEqual(step.Command[packageOffset:], []string{"compound/internal/hook"}) {
							t.Fatalf("hook phase = %+v; want exact isolated name/ref/membership", step)
						}
						continue
					}
				}
				retained = append(retained, step)
			}
			if hookCount != 1 || len(current) != len(prior)+1 || !reflect.DeepEqual(retained, prior) {
				t.Fatalf("prior gates changed or hook not singleton: hooks=%d\nprior=%v\ncurrent=%v", hookCount, prior, current)
			}
			if len(seen) != len(packages)+1 {
				t.Fatalf("coverage = %v", seen)
			}
			for _, pkg := range plan.Runs[0].Packages {
				if seen[pkg] != 1 {
					t.Fatalf("%s count = %d", pkg, seen[pkg])
				}
			}
		})
	}
}

// These runner tests block ordinary gates at the runtime resolver and direct
// gofmt/redact calls with explicit failing command stubs.
// Scope derivation still reads a real committed diff, so readiness is checked
// against the selected inventory rather than a hand-built list of steps.
func proofCheckPresubmitObserveGateResolution(t *testing.T) (*int, string) {
	t.Helper()
	gateLog := filepath.Join(t.TempDir(), "gate-commands.log")
	bin := t.TempDir()
	for _, name := range []string{"gofmt", "redact"} {
		body := "#!/bin/sh\nprintf '%s\\n' \"$0\" >> \"$COMPOUND_TEST_GATE_LOG\"\nprintf '%s\\n' 'presubmit test direct gate blocked' >&2\nexit 97\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COMPOUND_TEST_GATE_LOG", gateLog)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := proofCheckRuntimeExecutable
	calls := new(int)
	proofCheckRuntimeExecutable = func(string) (string, error) {
		*calls = *calls + 1
		return "", errors.New("presubmit test gate resolution reached")
	}
	t.Cleanup(func() { proofCheckRuntimeExecutable = previous })
	return calls, gateLog
}

func TestProofCheckPresubmitConsolePrerequisitesBeforeGates(t *testing.T) {
	for _, tc := range []struct{ name, shape, reason string }{
		{"missing dependencies", "missing", "console_deps_missing"},
		{"non-directory dependencies", "file", "console_deps_missing"},
		{"missing compiler", "directory", "console_compiler_missing"},
		{"non-executable compiler", "compiler", "console_compiler_unavailable"},
		{"non-file compiler", "compiler-directory", "console_compiler_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := newProofCheckPresubmitRepo(t)
			proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "console", "fixture.ts"), "export const value = 1;\n")
			proofCheckPresubmitCommit(t, repo, "console change")
			modules := filepath.Join(repo, "console", "node_modules")
			switch tc.shape {
			case "file":
				proofCheckDevelopmentWriteFile(t, modules, "not a directory")
			case "directory", "compiler", "compiler-directory":
				bin := filepath.Join(modules, ".bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.shape == "compiler" {
					proofCheckDevelopmentWriteFile(t, filepath.Join(bin, "tsc"), "#!/bin/sh\nexit 0\n")
					if err := os.Chmod(filepath.Join(bin, "tsc"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else if tc.shape == "compiler-directory" {
					if err := os.Mkdir(filepath.Join(bin, "tsc"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}

			calls, gateLog := proofCheckPresubmitObserveGateResolution(t)
			report, err := runNonCertifyingProofCheckContext(context.Background(), repo, false, base, time.Now(), "--presubmit")
			if err == nil || !strings.Contains(err.Error(), "proof prerequisites: "+tc.reason+": gate=console-tsc;") {
				t.Fatalf("want named setup refusal %s, got report=%+v err=%v", tc.reason, report, err)
			}
			if _, statErr := os.Stat(gateLog); !os.IsNotExist(statErr) {
				t.Fatalf("unready presubmit invoked a direct gate: stat=%v", statErr)
			}
			if *calls != 0 || !reflect.DeepEqual(report, proofCheckReport{}) {
				t.Fatalf("unready presubmit reached a gate or constructed gate evidence: resolutions=%d report=%+v", *calls, report)
			}
		})
	}
}

func TestProofCheckPresubmitConsolePrerequisitesRespectModeAndSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected bool
		ready    bool
		dryRun   bool
		mode     string
	}{
		{name: "ready compiler", selected: true, ready: true, mode: "--presubmit"},
		{name: "unselected console", mode: "--presubmit"},
		{name: "dry-run without dependencies", selected: true, dryRun: true, mode: "--presubmit"},
		{name: "development without dependencies", selected: true, mode: "--dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := newProofCheckPresubmitRepo(t)
			path, body := "internal/policy/policy.go", "package policy\n\nconst P = 1\n"
			if tc.selected {
				path, body = "console/fixture.ts", "export const value = 1;\n"
			}
			proofCheckDevelopmentWriteFile(t, filepath.Join(repo, path), body)
			proofCheckPresubmitCommit(t, repo, tc.name)
			if tc.ready {
				compiler := filepath.Join(repo, "console", "node_modules", ".bin", "tsc")
				proofCheckDevelopmentWriteFile(t, compiler, "#!/bin/sh\nexit 0\n")
				if err := os.Chmod(compiler, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			calls, gateLog := proofCheckPresubmitObserveGateResolution(t)
			report, err := runNonCertifyingProofCheckContext(context.Background(), repo, tc.dryRun, base, time.Now(), tc.mode)
			if err != nil {
				t.Fatalf("admitted runner returned a setup error: %v", err)
			}
			var consoleSelected bool
			for _, step := range report.Steps {
				consoleSelected = consoleSelected || step.Name == "console-tsc"
				if tc.dryRun && step.Status != "planned" {
					t.Fatalf("dry-run executed a gate: %+v", step)
				}
			}
			if consoleSelected != tc.selected {
				t.Fatalf("console gate selected=%v; want %v", consoleSelected, tc.selected)
			}
			if tc.dryRun {
				if _, statErr := os.Stat(gateLog); !os.IsNotExist(statErr) {
					t.Fatalf("dry-run invoked a direct gate: stat=%v", statErr)
				}
				if *calls != 0 || report.Status != "planned" || report.ProfileKind != work.ProofCheckProfilePresubmit {
					t.Fatalf("dry-run changed readiness or inventory behavior: resolutions=%d report=%+v", *calls, report)
				}
			} else if *calls == 0 || report.Status != "failed" || len(report.Steps) == 0 || !strings.Contains(report.Steps[0].OutputTail, "presubmit test gate resolution reached") {
				t.Fatalf("admitted runner did not reach its normal first gate: resolutions=%d report=%+v", *calls, report)
			}
			if tc.mode == "--dev" && report.ProfileKind != proofCheckProfileDevelopment {
				t.Fatalf("development profile changed: %q", report.ProfileKind)
			}
		})
	}
}
