package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/work"
)

const rustCanonicalProofCLISubEnv = "COMPOUND_RUST_CANONICAL_PROOF_CLI_SUB"
const unsupportedCanonicalProofCLISubEnv = "COMPOUND_UNSUPPORTED_CANONICAL_PROOF_CLI_SUB"

func TestResolveCanonicalProofPlanKeepsCompoundInventoryUnchanged(t *testing.T) {
	t.Parallel()
	repo := initCanonicalProofRepo(t, map[string]string{
		"go.mod":    "module compound\n\ngo 1.26.4\n",
		"README.md": "compound fixture\n",
	})
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	plan, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	want, err := proofCheckSteps(source.BaseSHA, source.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != work.CanonicalProofPlanCompoundV1 {
		t.Fatalf("plan id = %q, want %q", plan.ID, work.CanonicalProofPlanCompoundV1)
	}
	if !reflect.DeepEqual(plan.Steps, want) {
		t.Fatalf("Compound plan changed existing inventory\n got: %#v\nwant: %#v", plan.Steps, want)
	}
	profile, err := work.ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	publishes, err := work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(plan, profile, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	if !publishes {
		t.Fatal("exact admitted Compound profile must retain installed-runtime artifact publication")
	}
	mutated := plan
	mutated.Steps = append([]proofCheckStep(nil), plan.Steps...)
	mutated.Steps[0].Command = append(append([]string(nil), plan.Steps[0].Command...), "./unexpected")
	mutatedProfile, err := work.ProofResourceProfileForCanonicalPlan(mutated)
	if err != nil {
		t.Fatal(err)
	}
	publishes, err = work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(mutated, mutatedProfile, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	if publishes {
		t.Fatal("changed Compound inventory digest must not authorize installed-runtime artifact publication")
	}
}

func TestCanonicalProofInventoryDependencyIsCompoundOnlyAndNonEmpty(t *testing.T) {
	t.Parallel()
	compoundRepo := initCanonicalProofRepo(t, map[string]string{
		"go.mod": "module compound\n\ngo 1.26.4\n",
	})
	compoundSource, err := gitrepository.CaptureCleanSnapshot(compoundRepo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.ResolveCanonicalProofPlan(compoundRepo, compoundSource, nil); err == nil || !strings.Contains(err.Error(), "inventory is unavailable") {
		t.Fatalf("nil Compound inventory error = %v, want unavailable refusal", err)
	}
	emptyInventory := func(string, ...string) ([]work.ProofCheckStep, error) { return nil, nil }
	if _, err := work.ResolveCanonicalProofPlan(compoundRepo, compoundSource, emptyInventory); err == nil || !strings.Contains(err.Error(), "inventory is empty") {
		t.Fatalf("empty Compound inventory error = %v, want empty refusal", err)
	}

	rustRepo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\nresolver = \"2\"\n",
		"Cargo.lock": "# synthetic lock\nversion = 4\n",
	})
	rustSource, err := gitrepository.CaptureCleanSnapshot(rustRepo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	rustPlan, err := work.ResolveCanonicalProofPlan(rustRepo, rustSource, nil)
	if err != nil {
		t.Fatalf("Rust resolution unexpectedly required Compound inventory: %v", err)
	}
	if publishes, err := work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(rustPlan, work.ProofResourceProfile{}, nil); err != nil || publishes {
		t.Fatalf("Rust runtime-artifact authorization = (%v, %v), want (false, nil)", publishes, err)
	}
	executed, err := work.CanonicalProofPlanForExecution(
		work.ContextWithCanonicalProofPlan(t.Context(), rustPlan),
		rustRepo,
		rustSource,
		nil,
	)
	if err != nil || !reflect.DeepEqual(executed, rustPlan) {
		t.Fatalf("stored Rust execution plan = (%+v, %v), want original plan", executed, err)
	}
}

func TestResolveCanonicalProofPlanBuildsSourceBoundRustWorkspaceFloor(t *testing.T) {
	t.Parallel()
	repo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\nresolver = \"2\"\n",
		"Cargo.lock": "# synthetic lock\nversion = 4\n",
	})
	baseSHA := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	writeCanonicalProofFile(t, repo, "README.md", "candidate\n")
	runCommandGit(t, repo, "add", "README.md")
	runCommandGit(t, repo, "commit", "-m", "candidate")
	source, err := gitrepository.CaptureCleanSnapshot(repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	rangeRef := source.BaseSHA + "..." + source.HeadSHA
	want := []proofCheckStep{
		{Name: "rustfmt", Command: []string{"cargo", "fmt", "--all", "--", "--check"}, ProofRef: "cargo-fmt-all-check", CIPortable: true},
		{Name: "cargo-clippy", Command: []string{"cargo", "clippy", "--locked", "--workspace", "--all-targets", "--", "-D", "warnings"}, ProofRef: "cargo-clippy-locked-workspace-all-targets-deny-warnings", CIPortable: true},
		{Name: "cargo-test", Command: []string{"cargo", "test", "--locked", "--workspace"}, ProofRef: "cargo-test-locked-workspace", CIPortable: true},
		{Name: "cargo-build", Command: []string{"cargo", "build", "--locked", "--workspace"}, ProofRef: "cargo-build-locked-workspace", CIPortable: true},
		{Name: "diff-check", Command: []string{"git", "diff", "--check", rangeRef}, ProofRef: "diff-check", CIPortable: true},
		{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs changed in " + rangeRef + ">"}, ProofRef: "redact-public-clean"},
	}
	if plan.ID != work.CanonicalProofPlanRustWorkspaceV1 {
		t.Fatalf("plan id = %q, want %q", plan.ID, work.CanonicalProofPlanRustWorkspaceV1)
	}
	if !reflect.DeepEqual(plan.Steps, want) {
		t.Fatalf("Rust plan inventory\n got: %#v\nwant: %#v", plan.Steps, want)
	}

	profile, err := work.ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Kind != work.ProofResourceProfileCanonicalFull || profile.SourceSnapshot == nil ||
		!reflect.DeepEqual(*profile.SourceSnapshot, source) || profile.Digest == "" {
		t.Fatalf("Rust admission profile is not source-bound canonical proof: %+v", profile)
	}
	if len(profile.Gates) != len(want) {
		t.Fatalf("profile gates = %d, want %d", len(profile.Gates), len(want))
	}
	publishes, err := work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(plan, profile, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	if publishes {
		t.Fatal("Rust profile digest must not authorize Compound-only installed-runtime claims")
	}

	report, err := runProofCheckWithRangeContext(
		work.ContextWithCanonicalProofPlan(t.Context(), plan),
		repo,
		true,
		source.BaseSHA,
		source.HeadSHA,
		&source,
		false,
		false,
		time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceSnapshot == nil || !reflect.DeepEqual(*report.SourceSnapshot, source) {
		t.Fatalf("report source = %+v, want %+v", report.SourceSnapshot, source)
	}
	if len(report.Steps) != len(want) {
		t.Fatalf("report steps = %d, want %d", len(report.Steps), len(want))
	}
	for i := range want {
		if report.Steps[i].Name != want[i].Name ||
			!reflect.DeepEqual(report.Steps[i].Command, want[i].Command) ||
			report.Steps[i].ProofRef != want[i].ProofRef || report.Steps[i].Status != "planned" {
			t.Fatalf("report step %d = %+v, want planned identity %+v", i, report.Steps[i], want[i])
		}
	}
}

func TestResolveCanonicalProofPlanFailsClosedBeforeRustExecution(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name: "lockless Rust workspace",
			files: map[string]string{
				"Cargo.toml": "[workspace]\nmembers = []\n",
			},
			wantErr: "Cargo.lock",
		},
		{
			name: "mixed root manifests",
			files: map[string]string{
				"Cargo.toml": "[workspace]\nmembers = []\n",
				"Cargo.lock": "version = 4\n",
				"go.mod":     "module example.invalid/mixed\n\ngo 1.25\n",
			},
			wantErr: "mixed",
		},
		{
			name: "unsupported root",
			files: map[string]string{
				"README.md": "unsupported\n",
			},
			wantErr: "unsupported canonical proof repository",
		},
		{
			name: "generic Go module",
			files: map[string]string{
				"go.mod": "module example.invalid/generic\n\ngo 1.26.4\n",
			},
			wantErr: "module compound",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initCanonicalProofRepo(t, tc.files)
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("resolve error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveCanonicalProofPlanRejectsNonBlobRootMarkers(t *testing.T) {
	t.Parallel()
	t.Run("symlink", func(t *testing.T) {
		repo := initCanonicalProofRepo(t, map[string]string{
			"Cargo.lock": "version = 4\n",
			"manifest":   "[workspace]\nmembers = []\n",
		})
		if err := os.Symlink("manifest", filepath.Join(repo, "Cargo.toml")); err != nil {
			t.Fatal(err)
		}
		runCommandGit(t, repo, "add", "Cargo.toml")
		runCommandGit(t, repo, "commit", "-m", "symlink marker")
		source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil || !strings.Contains(err.Error(), "regular blob") {
			t.Fatalf("symlink marker error = %v, want regular blob refusal", err)
		}
	})

	t.Run("tree", func(t *testing.T) {
		repo := initCanonicalProofRepo(t, map[string]string{
			"Cargo.lock":            "version = 4\n",
			"Cargo.toml/member.txt": "not a manifest\n",
		})
		source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil || !strings.Contains(err.Error(), "regular blob") {
			t.Fatalf("tree marker error = %v, want regular blob refusal", err)
		}
	})
}

func TestRustNonCertifyingModesRefuseCompoundGoInventory(t *testing.T) {
	t.Parallel()
	repo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\n",
		"Cargo.lock": "version = 4\n",
	})
	if _, err := runProofCheck(repo, true, "", false, false, time.Now()); err == nil || !strings.Contains(err.Error(), "governed canonical proof") {
		t.Fatalf("Rust dry-run error = %v, want governed canonical proof refusal", err)
	}
	if _, err := runDevelopmentProofCheck(repo, true, "", time.Now()); err == nil || !strings.Contains(err.Error(), "governed canonical proof") {
		t.Fatalf("Rust development error = %v, want governed canonical proof refusal", err)
	}
}

func TestRustNonCertifyingModesRefusePhysicalMarkersWithoutGitHead(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeCanonicalProofFile(t, repo, "Cargo.toml", "[workspace]\nmembers = []\n")
	writeCanonicalProofFile(t, repo, "Cargo.lock", "version = 4\n")

	checks := []struct {
		name string
		run  func() error
	}{
		{
			name: "dry-run",
			run: func() error {
				_, err := runProofCheck(repo, true, "", false, false, time.Now())
				return err
			},
		},
		{
			name: "CI dry-run",
			run: func() error {
				_, err := runProofCheck(repo, true, "", true, false, time.Now())
				return err
			},
		},
		{
			name: "development",
			run: func() error {
				_, err := runDevelopmentProofCheck(repo, true, "", time.Now())
				return err
			},
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil || !strings.Contains(err.Error(), "governed canonical proof") {
				t.Fatalf("error = %v, want governed canonical proof refusal", err)
			}
		})
	}
}

func TestRustNonCertifyingModesRefuseCommittedMarkersMissingFromWorkingTree(t *testing.T) {
	t.Parallel()
	repo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\n",
		"Cargo.lock": "version = 4\n",
	})
	for _, marker := range []string{"Cargo.toml", "Cargo.lock"} {
		if err := os.Remove(filepath.Join(repo, marker)); err != nil {
			t.Fatal(err)
		}
	}

	for _, check := range []struct {
		name string
		run  func() error
	}{
		{
			name: "CI dry-run",
			run: func() error {
				_, err := runProofCheck(repo, true, "", true, false, time.Now())
				return err
			},
		},
		{
			name: "development",
			run: func() error {
				_, err := runDevelopmentProofCheck(repo, true, "", time.Now())
				return err
			},
		},
	} {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil || !strings.Contains(err.Error(), "governed canonical proof") {
				t.Fatalf("error = %v, want committed Rust marker refusal", err)
			}
		})
	}
}

func TestRustfmtFailureFactsCompareAcrossSealedRepositoryRoots(t *testing.T) {
	t.Parallel()
	makeStep := func(extraBaselineFailure bool) proofCheckStep {
		script := `printf 'Diff in %s/crates/sb-bodylog/src/lib.rs:3043:\n context\n-old line\n+new line\n' "$PWD"`
		if extraBaselineFailure {
			script += `; printf 'Diff in %s/crates/sb-bodylog/src/reclaim.rs:12:\n other context\n-old import\n+new import\n' "$PWD"`
		}
		script += `; exit 1`
		return proofCheckStep{
			Name:     "rustfmt",
			Command:  []string{"sh", "-c", script},
			ProofRef: "cargo-fmt-all-check",
		}
	}

	candidate := runProofCheckStep(t.TempDir(), makeStep(false), "")
	baseline := runProofCheckStep(t.TempDir(), makeStep(true), "")
	if candidate.Status != "failed" || baseline.Status != "failed" {
		t.Fatalf("rustfmt controls did not fail: candidate=%+v baseline=%+v", candidate, baseline)
	}
	if !proofFailureFactsSubset(candidate.FailureFacts, baseline.FailureFacts) {
		t.Fatalf("same relative rustfmt failure did not compare across roots:\ncandidate=%v\nbaseline=%v", candidate.FailureFacts, baseline.FailureFacts)
	}

	changedBody := makeStep(false)
	changedBody.Command[2] = strings.Replace(changedBody.Command[2], "+new line", "+candidate-only line", 1)
	changed := runProofCheckStep(t.TempDir(), changedBody, "")
	if proofFailureFactsSubset(changed.FailureFacts, baseline.FailureFacts) {
		t.Fatalf("candidate-only rustfmt diff collapsed into the baseline facts: %v", changed.FailureFacts)
	}
	for _, change := range []struct {
		name, old, new string
	}{
		{name: "relative path", old: "src/lib.rs", new: "src/other.rs"},
		{name: "line", old: "lib.rs:3043", new: "lib.rs:3044"},
	} {
		t.Run("changed "+change.name, func(t *testing.T) {
			changedHeader := makeStep(false)
			changedHeader.Command[2] = strings.Replace(changedHeader.Command[2], change.old, change.new, 1)
			result := runProofCheckStep(t.TempDir(), changedHeader, "")
			if proofFailureFactsSubset(result.FailureFacts, baseline.FailureFacts) {
				t.Fatalf("changed rustfmt %s collapsed into baseline facts: %v", change.name, result.FailureFacts)
			}
		})
	}

	outOfRoot := makeStep(false)
	outOfRoot.Command[2] = strings.Replace(outOfRoot.Command[2], "%s/crates", "%s/../outside", 1)
	outsideCandidate := runProofCheckStep(t.TempDir(), outOfRoot, "")
	outsideBaseline := runProofCheckStep(t.TempDir(), outOfRoot, "")
	if proofFailureFactsSubset(outsideCandidate.FailureFacts, outsideBaseline.FailureFacts) {
		t.Fatalf("out-of-root rustfmt header was normalized: candidate=%v baseline=%v", outsideCandidate.FailureFacts, outsideBaseline.FailureFacts)
	}

	t.Run("physical path emitted for logical repository root", func(t *testing.T) {
		physicalRepo := t.TempDir()
		logicalRepo := filepath.Join(t.TempDir(), "repo")
		if err := os.Symlink(physicalRepo, logicalRepo); err != nil {
			t.Skipf("symlink fixture unavailable: %v", err)
		}
		physicalStep := makeStep(false)
		physicalStep.Command[2] = strings.Replace(physicalStep.Command[2], `"$PWD"`, `"$(pwd -P)"`, 1)
		throughLogicalRoot := runProofCheckStep(logicalRepo, physicalStep, "")
		directPhysicalRoot := runProofCheckStep(physicalRepo, physicalStep, "")
		if !reflect.DeepEqual(throughLogicalRoot.FailureFacts, directPhysicalRoot.FailureFacts) {
			t.Fatalf("logical/physical repository aliases produced different rustfmt facts:\nlogical=%v\nphysical=%v", throughLogicalRoot.FailureFacts, directPhysicalRoot.FailureFacts)
		}
	})

	for _, oneSided := range []struct {
		name, body string
	}{
		{name: "insertion-only block", body: "+added newline\n"},
		{name: "deletion-only block", body: "-removed blank line\n"},
	} {
		t.Run(oneSided.name, func(t *testing.T) {
			step := makeStep(false)
			step.Command[2] = `printf 'Diff in %s/crates/one-sided.rs:7:\n` + oneSided.body + `' "$PWD"; exit 1`
			candidate := runProofCheckStep(t.TempDir(), step, "")
			baseline := runProofCheckStep(t.TempDir(), step, "")
			if !reflect.DeepEqual(candidate.FailureFacts, baseline.FailureFacts) {
				t.Fatalf("valid %s remained checkout-path-sensitive:\ncandidate=%v\nbaseline=%v", oneSided.name, candidate.FailureFacts, baseline.FailureFacts)
			}
		})
	}
}

func TestRustfmtFailureFactsPreserveExactDiffBlocks(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, script string) proofCheckStep {
		t.Helper()
		return runProofCheckStep(t.TempDir(), proofCheckStep{
			Name:     "rustfmt",
			Command:  []string{"sh", "-c", script + `; exit 1`},
			ProofRef: "cargo-fmt-all-check",
		}, "")
	}
	oneBlock := func(body string) string {
		return `printf 'Diff in %s/crates/sb-bodylog/src/lib.rs:3043:\n` + body + `' "$PWD"`
	}

	baseline := run(t, oneBlock(` context\n-old line\n+new line\n`))
	for _, change := range []struct {
		name, script string
	}{
		{
			name:   "indentation-only body change",
			script: oneBlock(`  context\n-old line\n+new line\n`),
		},
		{
			name:   "reordered body lines",
			script: oneBlock(`-old line\n context\n+new line\n`),
		},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := run(t, change.script)
			if proofFailureFactsSubset(candidate.FailureFacts, baseline.FailureFacts) {
				t.Fatalf("%s collapsed into baseline facts: candidate=%v baseline=%v", change.name, candidate.FailureFacts, baseline.FailureFacts)
			}
		})
	}

	baselineTwoBlocks := run(t,
		`printf 'Diff in %s/crates/a.rs:10:\n context a\n-old a\n+new a\n' "$PWD"; `+
			`printf 'Diff in %s/crates/b.rs:20:\n context b\n-old b\n+new b\n' "$PWD"`,
	)
	swappedBodies := run(t,
		`printf 'Diff in %s/crates/a.rs:10:\n context b\n-old b\n+new b\n' "$PWD"; `+
			`printf 'Diff in %s/crates/b.rs:20:\n context a\n-old a\n+new a\n' "$PWD"`,
	)
	if proofFailureFactsSubset(swappedBodies.FailureFacts, baselineTwoBlocks.FailureFacts) {
		t.Fatalf("rustfmt bodies moved between file headers collapsed: candidate=%v baseline=%v", swappedBodies.FailureFacts, baselineTwoBlocks.FailureFacts)
	}

	preambleCandidate := run(t, `printf 'warning from %s\n' "$PWD"; `+oneBlock(` context\n-old line\n+new line\n`))
	preambleBaseline := run(t, `printf 'warning from %s\n' "$PWD"; `+oneBlock(` context\n-old line\n+new line\n`))
	if proofFailureFactsSubset(preambleCandidate.FailureFacts, preambleBaseline.FailureFacts) {
		t.Fatalf("unrecognized rustfmt preamble was normalized: candidate=%v baseline=%v", preambleCandidate.FailureFacts, preambleBaseline.FailureFacts)
	}
}

func proofFailureFactsSubset(candidate, baseline []string) bool {
	known := make(map[string]bool, len(baseline))
	for _, fact := range baseline {
		known[fact] = true
	}
	for _, fact := range candidate {
		if !known[fact] {
			return false
		}
	}
	return len(candidate) > 0
}

func TestResolveCanonicalProofPlanReadsCommittedMarkersOnly(t *testing.T) {
	t.Parallel()
	repo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\n",
		"Cargo.lock": "version = 4\n",
	})
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "Cargo.toml")); err != nil {
		t.Fatal(err)
	}
	writeCanonicalProofFile(t, repo, "go.mod", "module example.invalid/uncommitted\n\ngo 1.25\n")

	plan, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != work.CanonicalProofPlanRustWorkspaceV1 {
		t.Fatalf("uncommitted marker drift changed sealed plan to %q", plan.ID)
	}
}

func TestRustCanonicalProofPublishesOwnedVerificationWithoutCompoundRuntimeArtifact(t *testing.T) {
	t.Parallel()
	if os.Getenv(rustCanonicalProofCLISubEnv) == "1" {
		proofCheckRuntimeExecutable = func(logical string) (string, error) {
			if logical == "cargo" {
				return os.Getenv("COMPOUND_RUST_FAKE_CARGO"), nil
			}
			return work.ResolveProofRuntimeCommand(logical)
		}
		proofCheckRuntimeArtifactRecorder = func(string, proofCheckReport) (proofCheckReport, error) {
			if err := os.WriteFile(os.Getenv("COMPOUND_RUST_RUNTIME_ARTIFACT_MARKER"), []byte("called\n"), 0o600); err != nil {
				return proofCheckReport{}, err
			}
			return proofCheckReport{}, errors.New("Rust proof attempted Compound runtime artifact publication")
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_RUST_PROOF_REPO"),
			"--base", os.Getenv("COMPOUND_RUST_PROOF_BASE"),
			"--work", os.Getenv("COMPOUND_RUST_PROOF_WORK"),
			"--actor", "codex:rust-proof-test",
			"--out", os.Getenv("COMPOUND_RUST_PROOF_OUT"),
		})
		os.Exit(0)
	}

	repo := initCanonicalProofRepo(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = []\n",
		"Cargo.lock": "version = 4\n",
	})
	baseSHA := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	writeCanonicalProofFile(t, repo, "README.md", "candidate public bytes\n")
	runCommandGit(t, repo, "add", "README.md")
	runCommandGit(t, repo, "commit", "-m", "candidate")
	source, err := gitrepository.CaptureCleanSnapshot(repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	homePath := t.TempDir()
	now := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "rust-canonical-proof-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex:rust-proof-test",
		Actor:            "codex:rust-proof-test",
		SuccessPredicate: "Rust proof earns owned verification and lands",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repo,
			RepositoryFingerprint: source.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       source.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	w, err = store.Claim(w.ID, w.Owner, now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "rust-proof.json")
	artifactMarker := filepath.Join(t.TempDir(), "runtime-artifact-called")
	cargoLog := filepath.Join(t.TempDir(), "cargo.log")
	redactLog := filepath.Join(t.TempDir(), "redact.log")
	fakeBin := t.TempDir()
	fakeCargo := filepath.Join(fakeBin, "cargo")
	if err := os.WriteFile(fakeCargo, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COMPOUND_RUST_CARGO_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "redact"), []byte("#!/bin/sh\ncat >> \"$COMPOUND_RUST_REDACT_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRustCanonicalProofPublishesOwnedVerificationWithoutCompoundRuntimeArtifact$")
	cmd.Env = append(
		os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		rustCanonicalProofCLISubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_RUST_PROOF_REPO="+repo,
		"COMPOUND_RUST_PROOF_BASE="+baseSHA,
		"COMPOUND_RUST_PROOF_WORK="+w.ID,
		"COMPOUND_RUST_PROOF_OUT="+outPath,
		"COMPOUND_RUST_RUNTIME_ARTIFACT_MARKER="+artifactMarker,
		"COMPOUND_RUST_FAKE_CARGO="+fakeCargo,
		"COMPOUND_RUST_CARGO_LOG="+cargoLog,
		"COMPOUND_RUST_REDACT_LOG="+redactLog,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("Rust proof command: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(artifactMarker); !os.IsNotExist(err) {
		t.Fatalf("Compound runtime artifact recorder was called for Rust proof: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var report proofCheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.Steps) != 6 {
		t.Fatalf("Rust proof report = status %q steps %d", report.Status, len(report.Steps))
	}
	cargoCalls, err := os.ReadFile(cargoLog)
	if err != nil {
		t.Fatal(err)
	}
	wantCargoCalls := strings.Join([]string{
		"fmt --all -- --check",
		"clippy --locked --workspace --all-targets -- -D warnings",
		"test --locked --workspace",
		"build --locked --workspace",
		"",
	}, "\n")
	if string(cargoCalls) != wantCargoCalls {
		t.Fatalf("Cargo calls = %q, want %q", cargoCalls, wantCargoCalls)
	}
	redactedBody, err := os.ReadFile(redactLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(redactedBody) != "candidate public bytes\n" {
		t.Fatalf("redact input = %q, want committed candidate blob", redactedBody)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 1 || !strings.HasPrefix(current.VerificationIDs[0], "proof-check:pfrs_") {
		t.Fatalf("Rust proof verification_ids = %v", current.VerificationIDs)
	}
	current, err = store.Advance(w.ID, work.AdvanceInput{
		To:         current.State,
		Actor:      w.Owner,
		ArtifactID: "commit:" + source.HeadSHA,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	landed, err := store.LandCheck(w.ID, work.LandCheckOptions{
		CandidateTipSHA: source.HeadSHA,
		Actor:           w.Owner,
		Record:          true,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if landed.Status != "landed" || landed.ProofVerificationRef != current.VerificationIDs[0] || landed.ProofHeadSHA != source.HeadSHA {
		t.Fatalf("Rust proof land-check = %+v", landed)
	}
}

func TestUnsupportedCanonicalProofFailsBeforeReservationOrRunner(t *testing.T) {
	t.Parallel()
	if os.Getenv(unsupportedCanonicalProofCLISubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			context.Context,
			string,
			bool,
			gitrepository.Snapshot,
			bool,
			bool,
			time.Time,
		) (proofCheckReport, error) {
			if err := os.WriteFile(os.Getenv("COMPOUND_UNSUPPORTED_RUNNER_MARKER"), []byte("called\n"), 0o600); err != nil {
				return proofCheckReport{}, err
			}
			return proofCheckReport{}, errors.New("unsupported repository reached runner")
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_UNSUPPORTED_PROOF_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_UNSUPPORTED_PROOF_WORK"),
			"--actor", "codex:unsupported-proof-test",
			"--out", os.Getenv("COMPOUND_UNSUPPORTED_PROOF_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 8, 4, 9, 15, 0, 0, time.UTC)
	if err := home.Init(homePath, "unsupported-canonical-proof-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "unsupported proof is inert", "codex:unsupported-proof-test", now)
	before, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo := initCanonicalProofRepo(t, map[string]string{"README.md": "unsupported\n"})
	outPath := filepath.Join(t.TempDir(), "proof.json")
	const priorReport = "prior report survives\n"
	if err := os.WriteFile(outPath, []byte(priorReport), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerMarker := filepath.Join(t.TempDir(), "runner-called")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnsupportedCanonicalProofFailsBeforeReservationOrRunner$")
	cmd.Env = append(
		os.Environ(),
		unsupportedCanonicalProofCLISubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_UNSUPPORTED_PROOF_REPO="+repo,
		"COMPOUND_UNSUPPORTED_PROOF_WORK="+w.ID,
		"COMPOUND_UNSUPPORTED_PROOF_OUT="+outPath,
		"COMPOUND_UNSUPPORTED_RUNNER_MARKER="+runnerMarker,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("unsupported proof exit = %v, want 2; output=%s", err, out)
	}
	if !strings.Contains(string(out), "unsupported canonical proof repository") {
		t.Fatalf("unsupported proof output = %q", out)
	}
	if _, err := os.Stat(runnerMarker); !os.IsNotExist(err) {
		t.Fatalf("unsupported proof reached runner: %v", err)
	}
	if body, err := os.ReadFile(outPath); err != nil || string(body) != priorReport {
		t.Fatalf("unsupported proof changed prior --out: body=%q err=%v", body, err)
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("unsupported proof mutated WorkEnvelope\n before=%+v\n after=%+v", before, after)
	}
}

func initCanonicalProofRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	runCommandGit(t, repo, "init", "-b", "main")
	runCommandGit(t, repo, "config", "user.email", "proof@example.invalid")
	runCommandGit(t, repo, "config", "user.name", "Proof Fixture")
	for name, body := range files {
		writeCanonicalProofFile(t, repo, name, body)
	}
	runCommandGit(t, repo, "add", "--", ".")
	runCommandGit(t, repo, "commit", "-m", "fixture")
	return repo
}

func writeCanonicalProofFile(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
