package main

import (
	"compound/internal/home"
	"compound/internal/work"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	proofCheckProfileDevelopment       = "development"
	proofCheckDevelopmentSchemaVersion = "compound.proof_check_development_report.v0.1"
)

// proofCheckDevelopmentRequested selects the non-certifying runner: --dev or --presubmit.
func proofCheckDevelopmentRequested(args []string) bool {
	return work.ProofCheckNonCertifyingMode(args) != ""
}

func validateProofCheckDevelopmentArgs(args []string) error {
	return work.ValidateProofCheckNonCertifyingArgs(args)
}

func cmdProofCheckDevelopment(args []string) {
	if err := validateProofCheckDevelopmentArgs(args); err != nil {
		fmt.Fprintln(os.Stderr, "proof-check:", err)
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	dryRun := hasFlag(args, "--dry-run")
	repoPath := flagValue(args, "--repo")
	baseRef := flagValue(args, "--base")
	outPath := strings.TrimSpace(flagValue(args, "--out"))
	var outTarget string
	var outCustody *proofCheckOutCustody
	if outPath != "" {
		target, custody, err := proofCheckPrepareDevelopmentOutPath(outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "proof-check: --out path not writable:", err)
			os.Exit(2)
		}
		outTarget = target
		outCustody = &custody
	}

	started := time.Now()
	ctx := work.ContextWithGateOutputReportPath(context.Background(), outTarget)
	report, err := runNonCertifyingProofCheckContext(ctx, repoPath, dryRun, baseRef, started, work.ProofCheckNonCertifyingMode(args))
	if err != nil {
		fmt.Fprintln(os.Stderr, "proof-check:", err)
		os.Exit(2)
	}
	// The development lane records into the SAME history file but under its own ProfileKind, so
	// the operator gets drift visibility on the fast lane too while its cached `-short` runs stay
	// structurally unable to contaminate the certifying lane's baseline.
	if !dryRun {
		homePath, homeErr := home.Resolve("")
		proofCheckAppendTimingWarnings(&report, homePath, homeErr, time.Since(started), time.Now())
	}
	if outPath != "" {
		if err := proofCheckWriteReportFileWithCustody(outTarget, report, *outCustody); err != nil {
			fmt.Fprintln(os.Stderr, "proof-check: writing --out file:", err)
			os.Exit(2)
		}
	}
	if jsonOut {
		printJSON(report)
	} else {
		printProofCheck(report)
	}
	if outPath != "" || !jsonOut {
		fmt.Println(proofCheckVerdictLine(report, outPath))
	}
	if report.Status != "ok" && !dryRun {
		os.Exit(1)
	}
}

func runDevelopmentProofCheck(repoPath string, dryRun bool, baseRef string, now time.Time) (proofCheckReport, error) {
	return runNonCertifyingProofCheck(repoPath, dryRun, baseRef, now, "--dev")
}

// runNonCertifyingProofCheck runs --dev, or --presubmit with its diff-derived gates.
func runNonCertifyingProofCheck(repoPath string, dryRun bool, baseRef string, now time.Time, mode string) (proofCheckReport, error) {
	return runNonCertifyingProofCheckContext(context.Background(), repoPath, dryRun, baseRef, now, mode)
}

func runNonCertifyingProofCheckContext(ctx context.Context, repoPath string, dryRun bool, baseRef string, now time.Time, mode string) (proofCheckReport, error) {
	repo, err := resolveProofCheckRepo(repoPath)
	if err != nil {
		return proofCheckReport{}, err
	}
	if err := work.RefuseRustInCompoundOnlyProofMode(repo, "development proof mode"); err != nil {
		return proofCheckReport{}, err
	}
	baseRef = strings.TrimSpace(baseRef)
	var steps []proofCheckStep
	var presubmit *work.ProofCheckPresubmit
	if mode == "--presubmit" {
		steps, presubmit, err = work.ProofCheckPresubmitSteps(ctx, repo, baseRef)
	} else {
		steps, err = proofCheckDevelopmentSteps(repo, baseRef)
	}
	if err != nil {
		return proofCheckReport{}, err
	}
	if mode == "--presubmit" && !dryRun {
		if err := work.ValidateConsoleProofPrerequisites(repo, steps); err != nil {
			return proofCheckReport{}, err
		}
	}
	report := proofCheckReport{
		Presubmit:     presubmit,
		SchemaVersion: proofCheckDevelopmentSchemaVersion,
		ProfileKind:   proofCheckProfileDevelopment,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Repo:          repo,
		BaseRef:       baseRef,
		DryRun:        dryRun,
		Steps:         make([]proofCheckStep, 0, len(steps)),
	}
	if presubmit != nil {
		report.SchemaVersion, report.ProfileKind = work.ProofCheckPresubmitSchemaVersion, work.ProofCheckProfilePresubmit
	}
	if dryRun {
		report.Status = "planned"
	}
	for _, step := range steps {
		result := step
		if dryRun {
			result.Status = "planned"
		} else {
			result = runProofCheckStepContext(ctx, repo, step, baseRef)
			if result.Status == "failed" {
				report.Status = "failed"
			}
		}
		if result.Status == "ok" || result.Status == "planned" {
			report.ProofRefs = append(report.ProofRefs, result.ProofRef)
		}
		report.Steps = append(report.Steps, result)
	}
	return report, nil
}

func proofCheckValidateDevelopmentOutPath(path string) error {
	_, custody, err := proofCheckPrepareDevelopmentOutPath(path)
	if err != nil {
		return err
	}
	return custody.close()
}

func proofCheckPrepareDevelopmentOutPath(path string) (string, proofCheckOutCustody, error) {
	target, err := proofCheckDevelopmentResolvedOutPath(path)
	if err != nil {
		return "", proofCheckOutCustody{}, err
	}
	custody, err := proofCheckPrepareOutPath(target)
	if err != nil {
		return "", proofCheckOutCustody{}, err
	}
	return target, custody, nil
}

func proofCheckDevelopmentResolvedOutPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("development report target cannot be a symlink")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(abs))
	for _, candidate := range []string{abs, resolved} {
		if err := proofCheckDevelopmentRejectRuntimePath(candidate); err != nil {
			return "", err
		}
	}
	return resolved, nil
}

func proofCheckDevelopmentRejectRuntimePath(path string) error {
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "artifacts" && parts[i+1] == "refs" {
			return fmt.Errorf("development report cannot target a canonical artifacts/refs directory")
		}
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		manifest, err := home.LoadManifest(dir)
		if err == nil && manifest.SchemaVersion == home.SchemaVersion {
			return fmt.Errorf("development report cannot be written under COMPOUND_HOME %s", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return nil
}

func proofCheckWriteDevelopmentReportFile(path string, report proofCheckReport) error {
	target, err := proofCheckDevelopmentResolvedOutPath(path)
	if err != nil {
		return err
	}
	return proofCheckWriteReportFile(target, report)
}

func proofCheckDevelopmentSteps(repo, baseRef string) ([]proofCheckStep, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	baseRef = strings.TrimSpace(baseRef)
	redactCommand := []string{"redact", "--repo-visibility", "public", "--from-file", "<changed files>"}
	if baseRef != "" {
		redactCommand = []string{"redact", "--repo-visibility", "public", "--from-file", "<files changed in " + baseRef + "...HEAD>"}
	}
	steps := []proofCheckStep{
		{
			Name:     "go-test",
			Command:  []string{"go", "test", "-short", "-timeout=10m", "./..."},
			ProofRef: "development:go-test-short-cached-all",
		},
		{
			Name:     "gofmt",
			Command:  []string{"gofmt", "-l", "<tracked *.go>"},
			ProofRef: "development:gofmt-check",
		},
		{
			Name:     "diff-check",
			Command:  []string{"git", "diff", "--check"},
			ProofRef: "development:diff-check",
		},
		{
			Name:     "architecture-doctor",
			Command:  []string{exe, "architecture", "doctor", "--strict"},
			ProofRef: "development:architecture-doctor-strict",
		},
	}

	// Console relevance includes deletions and both sides of renames. Moving a
	// console file away can break imports even though redact scans only extant bodies.
	changedFiles, err := proofCheckChangedPaths(repo, baseRef, "ACMDR")
	if err != nil {
		return nil, err
	}
	for _, path := range changedFiles {
		if path == "console" || strings.HasPrefix(path, "console/") {
			steps = append(steps, proofCheckStep{
				Name:     "console-tsc",
				Command:  []string{"./node_modules/.bin/tsc", "--noEmit"},
				Dir:      "console",
				ProofRef: "development:console-tsc-noemit",
			})
			break
		}
	}
	steps = append(steps, proofCheckStep{
		Name:     "redact-public",
		Command:  redactCommand,
		ProofRef: "development:redact-public-clean",
	})
	return steps, nil
}
