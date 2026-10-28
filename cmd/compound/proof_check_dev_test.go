package main

import (
	"compound/internal/work"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runProofCheck(repoPath string, dryRun bool, baseRef string, ciOnly bool, ciConsole bool, now time.Time) (proofCheckReport, error) {
	return runProofCheckContext(context.Background(), repoPath, dryRun, baseRef, ciOnly, ciConsole, now)
}

func TestProofCheckDevelopmentPinsCanonicalFreshnessAndUsesGoCache(t *testing.T) {
	t.Parallel()
	canonical, err := proofCheckSteps("")
	if err != nil {
		t.Fatalf("canonical steps: %v", err)
	}
	for _, phase := range []struct{ name, mode string }{
		{"go-test", "general-release"},
		{"go-test-home", "home"},
	} {
		canonicalGo := proofCheckDevelopmentFindStep(t, canonical, phase.name)
		if want := []string{"go", "run", "./scripts/testbattery", phase.mode}; !reflect.DeepEqual(canonicalGo.Command, want) {
			t.Fatalf("canonical %s command = %v, want %v", phase.name, canonicalGo.Command, want)
		}
		if !proofCheckUsesGo(canonicalGo) {
			t.Fatalf("canonical %s driver lost GOTOOLCHAIN and proof Go-cache injection", phase.name)
		}
	}

	repo, _ := newProofCheckDevelopmentRepo(t)
	development, err := proofCheckDevelopmentSteps(repo, "")
	if err != nil {
		t.Fatalf("development steps: %v", err)
	}
	developmentGo := proofCheckDevelopmentFindStep(t, development, "go-test")
	if want := []string{"go", "test", "-short", "-timeout=10m", "./..."}; !reflect.DeepEqual(developmentGo.Command, want) {
		t.Fatalf("development go-test command = %v, want %v", developmentGo.Command, want)
	}
	for _, arg := range developmentGo.Command {
		if strings.HasPrefix(arg, "-count") {
			t.Fatalf("development go-test disables the Go test cache: %v", developmentGo.Command)
		}
	}
}

func TestProofCheckCanonicalReportOmitsDevelopmentProfileKind(t *testing.T) {
	t.Parallel()
	repo, _ := newProofCheckDevelopmentRepo(t)
	report, err := runProofCheck(repo, true, "", false, false, time.Now())
	if err != nil {
		t.Fatalf("canonical dry-run: %v", err)
	}
	// The certifying battery stamps canonical_full: admission refuses to certify a report whose
	// profile_kind is not exactly that (internal/work/proof_reservation.go). What must never
	// happen is a canonical report wearing the DEVELOPMENT label, which would let non-certifying
	// evidence pass as proof.
	if report.SchemaVersion != proofCheckSchemaVersion || report.ProfileKind != work.ProofResourceProfileCanonicalFull {
		t.Fatalf("canonical report schema/profile = %q/%q, want %q/%q", report.SchemaVersion, report.ProfileKind, proofCheckSchemaVersion, work.ProofResourceProfileCanonicalFull)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal canonical report: %v", err)
	}
	if strings.Contains(string(data), proofCheckProfileDevelopment) {
		t.Fatalf("canonical JSON carries the development profile label: %s", data)
	}
}

func TestProofCheckDevelopmentDryRunIsClearlyNonCertifying(t *testing.T) {
	repo, baseRef := newProofCheckDevelopmentRepo(t)
	out := captureStdout(t, func() {
		cmdProofCheck([]string{
			"--dev",
			"--dry-run",
			"--json",
			"--repo", repo,
			"--base", baseRef,
		})
	})
	var report proofCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse development dry-run: %v\n%s", err, out)
	}

	if proofCheckDevelopmentSchemaVersion != "compound.proof_check_development_report.v0.1" {
		t.Fatalf("development schema constant = %q", proofCheckDevelopmentSchemaVersion)
	}
	if report.SchemaVersion != proofCheckDevelopmentSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", report.SchemaVersion, proofCheckDevelopmentSchemaVersion)
	}
	if report.ProfileKind != proofCheckProfileDevelopment {
		t.Fatalf("profile_kind = %q, want %q", report.ProfileKind, proofCheckProfileDevelopment)
	}
	if !report.DryRun || report.Status != "planned" || report.BaseRef != baseRef {
		t.Fatalf("unexpected development report: %+v", report)
	}
	if report.CIOnly || report.CIConsole {
		t.Fatalf("development report claimed a canonical CI selector: %+v", report)
	}
	if report.ArtifactRef != "" {
		t.Fatalf("development report wrote canonical artifact ref %q", report.ArtifactRef)
	}

	required := map[string]bool{
		"go-test":             false,
		"gofmt":               false,
		"diff-check":          false,
		"architecture-doctor": false,
		"redact-public":       false,
	}
	for _, step := range report.Steps {
		if _, ok := required[step.Name]; ok {
			required[step.Name] = true
		}
		if step.Status != "planned" {
			t.Fatalf("development dry-run step %q status = %q, want planned", step.Name, step.Status)
		}
		if !strings.HasPrefix(step.ProofRef, "development:") {
			t.Fatalf("development step %q has certifying-looking ref %q", step.Name, step.ProofRef)
		}
		switch step.Name {
		case "profiles-check", "workbench-projection", "codex-pretooluse-native-gate", "memory-doctor":
			t.Fatalf("development profile included canonical mutable/live gate %q", step.Name)
		}
		if strings.HasPrefix(step.Name, "console-") && step.Name != "console-tsc" {
			t.Fatalf("development profile included canonical console smoke %q", step.Name)
		}
	}
	for name, found := range required {
		if !found {
			t.Fatalf("development profile missing %q", name)
		}
	}
	if len(report.ProofRefs) != len(report.Steps) {
		t.Fatalf("proof refs = %v, want one development ref per step", report.ProofRefs)
	}
	for _, ref := range report.ProofRefs {
		if !strings.HasPrefix(ref, "development:") {
			t.Fatalf("development report contains certifying-looking proof ref %q", ref)
		}
	}
}

func TestProofCheckDevelopmentTextOutputEndsWithDevVerdict(t *testing.T) {
	repo, baseRef := newProofCheckDevelopmentRepo(t)
	out := captureStdout(t, func() {
		cmdProofCheck([]string{
			"--dev",
			"--dry-run",
			"--repo", repo,
			"--base", baseRef,
		})
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "dev-check: planned ") ||
		!strings.Contains(last, "non_certifying=true") ||
		!strings.Contains(last, "planned=") ||
		!strings.Contains(last, "elapsed_ms=") {
		t.Fatalf("last development output line is not the non-certifying verdict: %q\n%s", last, out)
	}
}

func TestProofCheckDevelopmentRejectsCanonicalAndGovernanceFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "ci", args: []string{"--dev", "--ci"}},
		{name: "ci-console", args: []string{"--dev", "--ci-console"}},
		{name: "work", args: []string{"--dev", "--work", "work_example"}},
		{name: "actor", args: []string{"--dev", "--actor", "codex"}},
		{name: "ttl", args: []string{"--dev", "--ttl", "5m"}},
		{name: "steal", args: []string{"--dev", "--steal"}},
		{name: "reason", args: []string{"--dev", "--reason", "take over"}},
		{name: "work-equals", args: []string{"--dev", "--work=work_example"}},
		{name: "ci-equals", args: []string{"--dev", "--ci=true"}},
		{name: "dev-equals", args: []string{"--dev=true"}},
		{name: "out-equals", args: []string{"--dev", "--out=report.json"}},
		{name: "bare-out", args: []string{"--dev", "--out"}},
		{name: "out-swallow-flag", args: []string{"--dev", "--out", "--json"}},
		{name: "unknown", args: []string{"--dev", "--fast"}},
		{name: "positional", args: []string{"--dev", "unexpected"}},
		{name: "duplicate", args: []string{"--dev", "--base", "main", "--base", "HEAD"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateProofCheckDevelopmentArgs(tc.args); err == nil {
				t.Fatalf("development args %v accepted incompatible flag", tc.args)
			}
		})
	}
	if err := validateProofCheckDevelopmentArgs([]string{
		"--dev",
		"--dry-run",
		"--json",
		"--base", "main",
		"--out", "report.json",
	}); err != nil {
		t.Fatalf("valid development args rejected: %v", err)
	}
	if !proofCheckDevelopmentRequested([]string{"--dev=true"}) {
		t.Fatal("assignment-form --dev would bypass strict development argument validation")
	}
}

func TestProofCheckDevelopmentRuntimeArtifactIsRejected(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	report := proofCheckReport{
		SchemaVersion: proofCheckDevelopmentSchemaVersion,
		ProfileKind:   proofCheckProfileDevelopment,
		Status:        "ok",
		GeneratedAt:   time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Repo:          "/tmp/development-repo",
		ProofRefs:     []string{"development:go-test-short-cached-all"},
	}
	got, err := recordProofCheckRuntimeArtifact(homePath, report)
	if err == nil {
		t.Fatal("development report wrote canonical installed-runtime artifact")
	}
	if got.ArtifactRef != "" {
		t.Fatalf("development report gained artifact ref %q", got.ArtifactRef)
	}
	if _, statErr := os.Stat(filepath.Join(homePath, "artifacts")); !os.IsNotExist(statErr) {
		t.Fatalf("development artifact rejection mutated artifacts directory: %v", statErr)
	}
}

func TestProofCheckDevelopmentOutCannotOverwriteCompoundHomeArtifact(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	proofCheckDevelopmentWriteFile(
		t,
		filepath.Join(homePath, "manifest.yaml"),
		"schema_version: compound.home.v0.1\nhome_kind: global\n",
	)
	target := filepath.Join(homePath, "artifacts", "refs", "proof-check-installed-runtime-20260724.md")
	proofCheckDevelopmentWriteFile(t, target, "canonical sentinel\n")

	if err := proofCheckValidateDevelopmentOutPath(target); err == nil {
		t.Fatal("development --out accepted canonical Compound artifact path")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read protected artifact: %v", err)
	}
	if string(got) != "canonical sentinel\n" {
		t.Fatalf("development --out validation mutated canonical artifact: %q", got)
	}
}

func TestProofCheckDevelopmentOutRejectsSymlinkEscapes(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	proofCheckDevelopmentWriteFile(
		t,
		filepath.Join(homePath, "manifest.yaml"),
		"schema_version: compound.home.v0.1\nhome_kind: global\n",
	)
	canonical := filepath.Join(homePath, "artifacts", "refs", "proof-check-installed-runtime-20260724.md")
	proofCheckDevelopmentWriteFile(t, canonical, "canonical sentinel\n")

	outside := t.TempDir()
	fileLink := filepath.Join(outside, "report.json")
	if err := os.Symlink(canonical, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := proofCheckValidateDevelopmentOutPath(fileLink); err == nil {
		t.Fatal("development --out accepted a file symlink to canonical evidence")
	}

	dirLink := filepath.Join(outside, "runtime-refs")
	if err := os.Symlink(filepath.Dir(canonical), dirLink); err != nil {
		t.Fatal(err)
	}
	if err := proofCheckValidateDevelopmentOutPath(filepath.Join(dirLink, "report.json")); err == nil {
		t.Fatal("development --out accepted a directory symlink into canonical evidence")
	}
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read protected artifact: %v", err)
	}
	if string(got) != "canonical sentinel\n" {
		t.Fatalf("symlink validation mutated canonical artifact: %q", got)
	}
}

func TestProofCheckDevelopmentOutAtomicallyReplacesHardlinkWithoutMutatingSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	proofCheckDevelopmentWriteFile(t, source, "source sentinel\n")
	out := filepath.Join(dir, "report.json")
	if err := os.Link(source, out); err != nil {
		t.Fatal(err)
	}
	if err := proofCheckValidateDevelopmentOutPath(out); err != nil {
		t.Fatalf("validate ordinary development output: %v", err)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "source sentinel\n" {
		t.Fatalf("validation truncated existing output: got=%q err=%v", got, err)
	}

	report := proofCheckReport{
		SchemaVersion: proofCheckDevelopmentSchemaVersion,
		ProfileKind:   proofCheckProfileDevelopment,
		Status:        "ok",
		GeneratedAt:   time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Repo:          "/tmp/development-repo",
	}
	if err := proofCheckWriteDevelopmentReportFile(out, report); err != nil {
		t.Fatalf("write development report: %v", err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "source sentinel\n" {
		t.Fatalf("atomic output write mutated hardlink source: got=%q err=%v", got, err)
	}
	var gotReport proofCheckReport
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &gotReport); err != nil {
		t.Fatalf("parse development output: %v", err)
	}
	if gotReport.ProfileKind != proofCheckProfileDevelopment {
		t.Fatalf("written profile_kind = %q", gotReport.ProfileKind)
	}
}

func TestProofCheckDevelopmentOutUsesPrivateModeCustody(t *testing.T) {
	t.Parallel()
	report := proofCheckReport{
		SchemaVersion: proofCheckDevelopmentSchemaVersion,
		ProfileKind:   proofCheckProfileDevelopment,
		Status:        "ok",
		GeneratedAt:   time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Repo:          "/tmp/development-repo",
		ProofRefs:     []string{},
		Steps:         []proofCheckStep{},
	}
	for _, tc := range []struct {
		name     string
		existing bool
		mode     os.FileMode
	}{
		{name: "new report", mode: 0o600},
		{name: "existing private report", existing: true, mode: 0o600},
		{name: "existing group-readable report", existing: true, mode: 0o640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if tc.existing {
				if err := os.WriteFile(path, []byte("prior evidence\n"), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := proofCheckWriteDevelopmentReportFile(path, report); err != nil {
				t.Fatalf("write development report: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.mode {
				t.Fatalf("published mode = %04o, want %04o", got, tc.mode)
			}
		})
	}
}

func TestProofCheckDevelopmentConsoleTSCRunsWhenFileRenamedOut(t *testing.T) {
	t.Parallel()
	repo, _ := newProofCheckDevelopmentRepo(t)
	steps, err := proofCheckDevelopmentSteps(repo, "")
	if err != nil {
		t.Fatalf("Go-only development steps: %v", err)
	}
	if proofCheckDevelopmentHasStep(steps, "console-tsc") {
		t.Fatal("Go-only change scheduled console TypeScript check")
	}

	consolePath := filepath.Join(repo, "console", "view.ts")
	proofCheckDevelopmentWriteFile(t, consolePath, "export const ready = true\n")
	proofCheckDevelopmentGit(t, repo, "add", "console/view.ts")
	proofCheckDevelopmentGit(
		t,
		repo,
		"-c", "user.name=Compound Test",
		"-c", "user.email=test@example.invalid",
		"commit", "-qm", "console base",
	)
	baseRef := strings.TrimSpace(proofCheckDevelopmentGit(t, repo, "rev-parse", "HEAD"))

	if err := os.MkdirAll(filepath.Join(repo, "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	proofCheckDevelopmentGit(t, repo, "mv", "console/view.ts", "moved/view.ts")
	steps, err = proofCheckDevelopmentSteps(repo, baseRef)
	if err != nil {
		t.Fatalf("renamed-out console development steps: %v", err)
	}
	if !proofCheckDevelopmentHasStep(steps, "console-tsc") {
		t.Fatal("console file renamed out of console did not schedule TypeScript check")
	}
}

func TestProofCheckChangedFilesScansRenamedTargetNotDeletedSource(t *testing.T) {
	t.Parallel()
	repo, baseRef := newProofCheckDevelopmentRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	proofCheckDevelopmentGit(t, repo, "mv", "value.go", "moved/value.go")
	proofCheckDevelopmentWriteFile(
		t,
		filepath.Join(repo, "moved", "value.go"),
		"package proofcheckdev\n\nconst Value = 2\n",
	)

	files, err := proofCheckChangedFiles(repo, baseRef)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if !proofCheckDevelopmentStringSliceContains(files, "moved/value.go") {
		t.Fatalf("renamed target missing from redact inputs: %v", files)
	}
	if proofCheckDevelopmentStringSliceContains(files, "value.go") {
		t.Fatalf("deleted rename source would be handed to redact: %v", files)
	}
}

func TestProofCheckChangedFilesDropsBaseRangePathDeletedLocally(t *testing.T) {
	t.Parallel()
	repo, baseRef := newProofCheckDevelopmentRepo(t)
	proofCheckDevelopmentWriteFile(
		t,
		filepath.Join(repo, "value.go"),
		"package proofcheckdev\n\nconst Value = 2\n",
	)
	proofCheckDevelopmentGit(t, repo, "add", "value.go")
	proofCheckDevelopmentGit(
		t,
		repo,
		"-c", "user.name=Compound Test",
		"-c", "user.email=test@example.invalid",
		"commit", "-qm", "change value",
	)
	proofCheckDevelopmentGit(t, repo, "rm", "-q", "value.go")

	files, err := proofCheckChangedFiles(repo, baseRef)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if proofCheckDevelopmentStringSliceContains(files, "value.go") {
		t.Fatalf("locally deleted base-range path would be handed to redact: %v", files)
	}
}

func newProofCheckDevelopmentRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	proofCheckDevelopmentGit(t, repo, "init", "-q")
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "go.mod"), "module example.invalid/proofcheck-dev\n\ngo 1.23\n")
	proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "value.go"), "package proofcheckdev\n\nconst Value = 1\n")
	proofCheckDevelopmentGit(
		t,
		repo,
		"-c", "user.name=Compound Test",
		"-c", "user.email=test@example.invalid",
		"add", ".",
	)
	proofCheckDevelopmentGit(
		t,
		repo,
		"-c", "user.name=Compound Test",
		"-c", "user.email=test@example.invalid",
		"commit", "-qm", "base",
	)
	baseRef := strings.TrimSpace(proofCheckDevelopmentGit(t, repo, "rev-parse", "HEAD"))
	return repo, baseRef
}

func proofCheckDevelopmentGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func proofCheckDevelopmentWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func proofCheckDevelopmentFindStep(t *testing.T, steps []proofCheckStep, name string) proofCheckStep {
	t.Helper()
	for _, step := range steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("step %q not found in %v", name, steps)
	return proofCheckStep{}
}

func proofCheckDevelopmentHasStep(steps []proofCheckStep, name string) bool {
	for _, step := range steps {
		if step.Name == name {
			return true
		}
	}
	return false
}

func proofCheckDevelopmentStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
