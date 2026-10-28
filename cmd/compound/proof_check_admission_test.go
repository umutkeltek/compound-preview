package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
	"compound/internal/work"
)

const proofCheckTransportCLISubEnv = "COMPOUND_PROOF_TRANSPORT_CLI_SUB"
const proofCheckReleasePendingCLISubEnv = "COMPOUND_PROOF_RELEASE_PENDING_CLI_SUB"
const proofCheckReleasePendingTamperEnv = "COMPOUND_PROOF_RELEASE_PENDING_TAMPER_AFTER_CAPTURE"
const proofCheckReleasePendingTamperBody = "tampered after capture\n"
const proofCheckFailedCanonicalCLISubEnv = "COMPOUND_PROOF_FAILED_CANONICAL_CLI_SUB"
const proofCheckFailedReleaseCLISubEnv = "COMPOUND_PROOF_FAILED_RELEASE_CLI_SUB"
const proofCheckPostReportErrorCLISubEnv = "COMPOUND_PROOF_POST_REPORT_ERROR_CLI_SUB"
const proofCheckUnsafeTTLCLISubEnv = "COMPOUND_PROOF_UNSAFE_TTL_CLI_SUB"
const proofCheckMinimumTTLCLISubEnv = "COMPOUND_PROOF_MINIMUM_TTL_CLI_SUB"
const proofCheckMinimumTTLControllerSubEnv = "COMPOUND_PROOF_MINIMUM_TTL_CONTROLLER_SUB"
const proofCheckBarrierChildSubEnv = "COMPOUND_PROOF_BARRIER_CHILD_SUB"
const proofCheckArtifactPublishFailureSubEnv = "COMPOUND_PROOF_ARTIFACT_PUBLISH_FAILURE_SUB"
const proofCheckSignalForwardSubEnv = "COMPOUND_PROOF_SIGNAL_FORWARD_SUB"
const proofCheckMissingBaseSubEnv = "COMPOUND_PROOF_MISSING_BASE_SUB"
const proofCheckLegacyRecoverySubEnv = "COMPOUND_PROOF_LEGACY_RECOVERY_SUB"
const proofCheckLegacyRecoveryRealSubEnv = "COMPOUND_PROOF_LEGACY_RECOVERY_REAL_SUB"
const proofCheckLegacyRecoveryReportlessSubEnv = "COMPOUND_PROOF_LEGACY_RECOVERY_REPORTLESS_SUB"

func TestProofCheckCommandMissingBaseFailsBeforeReportOrRunner(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckMissingBaseSubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			context.Context,
			string,
			bool,
			gitrepository.Snapshot,
			bool,
			bool,
			time.Time,
		) (proofCheckReport, error) {
			if err := os.WriteFile(os.Getenv("COMPOUND_PROOF_MISSING_BASE_MARKER"), []byte("started\n"), 0o644); err != nil {
				return proofCheckReport{}, err
			}
			return proofCheckReport{}, errors.New("runner must not start")
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_MISSING_BASE_REPO"),
			"--work", "work_synthetic_missing_base",
			"--actor", "codex:missing-base",
			"--out", os.Getenv("COMPOUND_PROOF_MISSING_BASE_OUT"),
		})
		os.Exit(0)
	}

	outPath := filepath.Join(t.TempDir(), "proof.json")
	const prior = "prior report must survive\n"
	if err := os.WriteFile(outPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "runner-started")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandMissingBaseFailsBeforeReportOrRunner$")
	cmd.Env = append(
		os.Environ(),
		proofCheckMissingBaseSubEnv+"=1",
		"COMPOUND_PROOF_MISSING_BASE_REPO="+t.TempDir(),
		"COMPOUND_PROOF_MISSING_BASE_OUT="+outPath,
		"COMPOUND_PROOF_MISSING_BASE_MARKER="+marker,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("missing-base exit = %v, want 2; output=%s", err, out)
	}
	if !strings.Contains(string(out), "certifying proof-check requires explicit --base") {
		t.Fatalf("missing-base output = %q", out)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != prior {
		t.Fatalf("missing-base command changed prior report: %q", body)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-base command started runner: %v", err)
	}
}

func TestProofCheckCommandLegacyRecoveryBypassesBaseWithoutRunningBattery(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckLegacyRecoverySubEnv) == "1" {
		proofCheckLegacyRecoverer = func(
			_ string,
			_ string,
			in work.ProofResourceLegacyRecoveryInput,
			_ time.Time,
		) (work.ProofResourceLegacyRecoveryResult, bool, error) {
			body, err := json.Marshal(in)
			if err != nil {
				return work.ProofResourceLegacyRecoveryResult{}, true, err
			}
			if err := os.WriteFile(os.Getenv("COMPOUND_PROOF_LEGACY_RECOVERY_MARKER"), body, 0o644); err != nil {
				return work.ProofResourceLegacyRecoveryResult{}, true, err
			}
			return work.ProofResourceLegacyRecoveryResult{
				SchemaVersion:        work.ProofResourceLegacyRecoverySchemaVersion,
				ResourceReleased:     true,
				FreshBatteryRequired: true,
			}, true, nil
		}
		proofCheckCanonicalRunner = func(
			context.Context,
			string,
			bool,
			gitrepository.Snapshot,
			bool,
			bool,
			time.Time,
		) (proofCheckReport, error) {
			_ = os.WriteFile(os.Getenv("COMPOUND_PROOF_LEGACY_RUNNER_MARKER"), []byte("runner-started"), 0o644)
			return proofCheckReport{}, errors.New("legacy recovery must not run battery")
		}
		cmdProofCheck([]string{
			"--recover-legacy-retained",
			"--repo", os.Getenv("COMPOUND_PROOF_LEGACY_REPO"),
			"--work", "work_legacy_recovery",
			"--actor", "codex:legacy-recovery",
			"--out", os.Getenv("COMPOUND_PROOF_LEGACY_OUT"),
		})
		os.Exit(0)
	}

	repoPath := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "legacy-proof.json")
	const prior = "legacy report remains untouched\n"
	if err := os.WriteFile(outPath, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	recoveryMarker := filepath.Join(t.TempDir(), "recovery.json")
	runnerMarker := filepath.Join(t.TempDir(), "runner-started")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandLegacyRecoveryBypassesBaseWithoutRunningBattery$")
	cmd.Env = append(
		os.Environ(),
		proofCheckLegacyRecoverySubEnv+"=1",
		"COMPOUND_HOME="+t.TempDir(),
		"COMPOUND_PROOF_LEGACY_REPO="+repoPath,
		"COMPOUND_PROOF_LEGACY_OUT="+outPath,
		"COMPOUND_PROOF_LEGACY_RECOVERY_MARKER="+recoveryMarker,
		"COMPOUND_PROOF_LEGACY_RUNNER_MARKER="+runnerMarker,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("legacy recovery exit = %v, want 2; output=%s", err, out)
	}
	if !strings.Contains(string(out), "fresh source-bound v0.3 battery required") {
		t.Fatalf("legacy recovery output = %q", out)
	}
	if _, err := os.Stat(runnerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy recovery started battery runner: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != prior {
		t.Fatalf("legacy recovery rewrote retained report: %q", body)
	}
	markerBody, err := os.ReadFile(recoveryMarker)
	if err != nil {
		t.Fatalf("legacy recovery was not called: %v; output=%s", err, out)
	}
	var got work.ProofResourceLegacyRecoveryInput
	if err := json.Unmarshal(markerBody, &got); err != nil {
		t.Fatal(err)
	}
	resolvedRepo, err := filepath.Abs(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOut, err := filepath.Abs(outPath)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand, err := work.ProofCheckInvocationIdentity([]string{
		"--repo", repoPath,
		"--work", "work_legacy_recovery",
		"--actor", "codex:legacy-recovery",
		"--out", resolvedOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != "codex:legacy-recovery" ||
		got.Workspace != resolvedRepo ||
		got.Command != wantCommand {
		t.Fatalf("legacy recovery identity = %+v, want workspace=%q command=%q", got, resolvedRepo, wantCommand)
	}
}

func TestProofCheckLegacyRecoveryRejectsAllRunnerModes(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--dev", "--timings", "--dry-run", "--ci", "--ci-console"} {
		t.Run(flag, func(t *testing.T) {
			err := proofCheckLegacyRecoveryModeError([]string{
				"--recover-legacy-retained",
				flag,
			})
			if err == nil {
				t.Fatalf("legacy recovery accepted runner mode %s", flag)
			}
		})
	}
	if err := proofCheckLegacyRecoveryModeError([]string{"--recover-legacy-retained"}); err != nil {
		t.Fatalf("legacy recovery-only mode rejected: %v", err)
	}
}

func TestProofCheckCommandLegacyRecoveryReleasesRealRetainedHolderWithoutVerification(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckLegacyRecoveryRealSubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			context.Context,
			string,
			bool,
			gitrepository.Snapshot,
			bool,
			bool,
			time.Time,
		) (proofCheckReport, error) {
			_ = os.WriteFile(os.Getenv("COMPOUND_PROOF_LEGACY_REAL_RUNNER"), []byte("runner-started"), 0o644)
			return proofCheckReport{}, errors.New("legacy recovery must not run battery")
		}
		cmdProofCheck([]string{
			"--recover-legacy-retained",
			"--repo", os.Getenv("COMPOUND_PROOF_LEGACY_REAL_REPO"),
			"--work", os.Getenv("COMPOUND_PROOF_LEGACY_REAL_WORK"),
			"--actor", "codex:legacy-real",
			"--out", os.Getenv("COMPOUND_PROOF_LEGACY_REAL_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := home.Init(homePath, "proof-legacy-recovery-real-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:legacy-real"
	w := createClaimedProofWork(t, store, "real legacy recovery-only release", actor, now)
	repoPath := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "legacy-real.json")
	const prior = "retained legacy report bytes\n"
	if err := os.WriteFile(outPath, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	resolvedOut, err := filepath.Abs(outPath)
	if err != nil {
		t.Fatal(err)
	}
	commandIdentity, err := work.ProofCheckInvocationIdentity([]string{
		"--repo", repoPath,
		"--work", w.ID,
		"--actor", actor,
		"--out", resolvedOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(w.ID, work.ProofResourceReserveInput{
		Actor:     actor,
		TTL:       10 * time.Minute,
		Workspace: repoPath,
		Command:   commandIdentity,
		Profile:   syntheticAdmissionProfile(t, repoPath),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ExecutionBarrier == nil {
		t.Fatal("legacy setup did not return execution barrier")
	}
	if err := reserved.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	report := successfulProofCheckReport()
	report.SchemaVersion = "compound.proof_check_report.v0.2"
	report.GeneratedAt = now.Add(2 * time.Second).Format(time.RFC3339)
	report.Repo = repoPath
	report.SourceSnapshot = nil
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(homePath, "state")
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, completionErr := store.CompleteProofResource(w.ID, work.ProofResourceCompleteInput{
		Actor:             actor,
		ReservationID:     reserved.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, now.Add(3*time.Second))
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if completionErr == nil {
		t.Fatal("legacy setup did not retain completion journal")
	}
	// The recovery target is the pre-v0.3 crash window: the journal and holder
	// survived, but no verification was promoted to the work envelope.
	legacyWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyWork.VerificationIDs = nil
	legacyWorkBody, err := json.MarshalIndent(legacyWork, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyWorkBody = append(legacyWorkBody, '\n')
	if err := os.WriteFile(
		filepath.Join(homePath, "work", "work", w.ID+".json"),
		legacyWorkBody,
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	runnerMarker := filepath.Join(t.TempDir(), "runner-started")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandLegacyRecoveryReleasesRealRetainedHolderWithoutVerification$")
	cmd.Env = append(
		os.Environ(),
		proofCheckLegacyRecoveryRealSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_LEGACY_REAL_REPO="+repoPath,
		"COMPOUND_PROOF_LEGACY_REAL_WORK="+w.ID,
		"COMPOUND_PROOF_LEGACY_REAL_OUT="+outPath,
		"COMPOUND_PROOF_LEGACY_REAL_RUNNER="+runnerMarker,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("real legacy recovery exit = %v, want 2; output=%s", err, out)
	}
	if !strings.Contains(string(out), "fresh source-bound v0.3 battery required") {
		t.Fatalf("real legacy recovery output = %q", out)
	}
	if _, err := os.Stat(runnerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real legacy recovery started battery runner: %v", err)
	}
	status, err := landlease.Status(homePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Held || status.Holder != "" {
		t.Fatalf("real legacy recovery retained holder: %+v", status)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 0 {
		t.Fatalf("real legacy recovery emitted verification: %v", current.VerificationIDs)
	}
	afterOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOut) != prior {
		t.Fatalf("real legacy recovery rewrote report bytes: %q", afterOut)
	}
}

func TestProofCheckCommandLegacyRecoveryReleasesReportlessFailureWithoutRunningBattery(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckLegacyRecoveryReportlessSubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			context.Context,
			string,
			bool,
			gitrepository.Snapshot,
			bool,
			bool,
			time.Time,
		) (proofCheckReport, error) {
			_ = os.WriteFile(
				os.Getenv("COMPOUND_PROOF_LEGACY_REPORTLESS_RUNNER"),
				[]byte("runner-started"),
				0o644,
			)
			return proofCheckReport{}, errors.New("reportless legacy recovery must not run battery")
		}
		cmdProofCheck([]string{
			"--recover-legacy-retained",
			"--repo", os.Getenv("COMPOUND_PROOF_LEGACY_REPORTLESS_REPO"),
			"--work", os.Getenv("COMPOUND_PROOF_LEGACY_REPORTLESS_WORK"),
			"--actor", "codex:legacy-reportless",
			"--out", os.Getenv("COMPOUND_PROOF_LEGACY_REPORTLESS_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := home.Init(homePath, "proof-legacy-reportless-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:legacy-reportless"
	w := createClaimedProofWork(t, store, "real reportless legacy recovery", actor, now)
	repoPath := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "legacy-reportless.json")
	const prior = "no report was published\n"
	if err := os.WriteFile(outPath, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	resolvedOut, err := filepath.Abs(outPath)
	if err != nil {
		t.Fatal(err)
	}
	commandIdentity, err := work.ProofCheckInvocationIdentity([]string{
		"--repo", repoPath,
		"--work", w.ID,
		"--actor", actor,
		"--out", resolvedOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(w.ID, work.ProofResourceReserveInput{
		Actor:     actor,
		TTL:       10 * time.Minute,
		Workspace: repoPath,
		Command:   commandIdentity,
		Profile:   syntheticAdmissionProfile(t, repoPath),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ExecutionBarrier == nil {
		t.Fatal("reportless legacy setup omitted execution barrier")
	}
	if err := reserved.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(homePath, "state")
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, completionErr := store.CompleteProofResource(w.ID, work.ProofResourceCompleteInput{
		Actor:             actor,
		ReservationID:     reserved.Reservation.ID,
		ProducerExitKnown: false,
		FailureReason:     "proof runner infrastructure failed before report publication",
	}, now.Add(3*time.Second))
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if completionErr == nil {
		t.Fatal("reportless legacy setup did not retain completion journal")
	}
	legacyWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyWork.VerificationIDs = nil
	legacyWorkBody, err := json.MarshalIndent(legacyWork, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyWorkBody = append(legacyWorkBody, '\n')
	if err := os.WriteFile(
		filepath.Join(homePath, "work", "work", w.ID+".json"),
		legacyWorkBody,
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	runnerMarker := filepath.Join(t.TempDir(), "runner-started")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestProofCheckCommandLegacyRecoveryReleasesReportlessFailureWithoutRunningBattery$",
	)
	cmd.Env = append(
		os.Environ(),
		proofCheckLegacyRecoveryReportlessSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_LEGACY_REPORTLESS_REPO="+repoPath,
		"COMPOUND_PROOF_LEGACY_REPORTLESS_WORK="+w.ID,
		"COMPOUND_PROOF_LEGACY_REPORTLESS_OUT="+outPath,
		"COMPOUND_PROOF_LEGACY_REPORTLESS_RUNNER="+runnerMarker,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("reportless legacy recovery exit = %v, want 2; output=%s", err, out)
	}
	if !strings.Contains(string(out), "fresh source-bound v0.3 battery required") {
		t.Fatalf("reportless legacy recovery output = %q", out)
	}
	if _, err := os.Stat(runnerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reportless legacy recovery started battery runner: %v", err)
	}
	status, err := landlease.Status(homePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Held || status.Holder != "" {
		t.Fatalf("reportless legacy recovery retained holder: %+v", status)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 0 {
		t.Fatalf("reportless legacy recovery emitted verification: %v", current.VerificationIDs)
	}
	afterOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOut) != prior {
		t.Fatalf("reportless legacy recovery rewrote output bytes: %q", afterOut)
	}
}

func TestProofCheckCommandSignalCancelsAndWaitsForWholeGateGroup(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckSignalForwardSubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			ctx context.Context,
			repoPath string,
			dryRun bool,
			source gitrepository.Snapshot,
			ciOnly bool,
			ciConsole bool,
			now time.Time,
		) (proofCheckReport, error) {
			cmd := proofCheckCommandContext(
				ctx,
				"sh",
				"-c",
				`sleep 30 & child=$!; printf '%s\n' "$child" > "$COMPOUND_PROOF_SIGNAL_PID"; wait "$child"`,
			)
			cmd.Env = os.Environ()
			return proofCheckReport{}, cmd.Run()
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_SIGNAL_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_SIGNAL_WORK"),
			"--actor", "codex:signal-forward",
			"--out", os.Getenv("COMPOUND_PROOF_SIGNAL_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 45, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-signal-forward-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "signal cancels proof group", "codex:signal-forward", now)
	repoPath := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "proof.json")
	pidPath := filepath.Join(t.TempDir(), "gate-child.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandSignalCancelsAndWaitsForWholeGateGroup$")
	cmd.Env = append(
		os.Environ(),
		proofCheckSignalForwardSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_SIGNAL_REPO="+repoPath,
		"COMPOUND_PROOF_SIGNAL_WORK="+w.ID,
		"COMPOUND_PROOF_SIGNAL_OUT="+outPath,
		"COMPOUND_PROOF_SIGNAL_PID="+pidPath,
	)
	// A file, not a pipe: the gate's backgrounded sleep inherits these fds.
	// On failure, show why the child never reached its gate (Woodpecker 584).
	childLog, err := os.Create(filepath.Join(t.TempDir(), "proof-check-child.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = childLog.Close()
		if t.Failed() {
			output, _ := os.ReadFile(childLog.Name())
			t.Logf("proof-check child output:\n%s", output)
		}
	})
	cmd.Stdout, cmd.Stderr = childLog, childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	gatePID := waitForProofCheckGatePID(t, pidPath, 5*time.Second)
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("signal-interrupted proof-check exited green")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(gatePID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate descendant pid %d remained after proof-check signal handling: %v", gatePID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWaitForProofCheckGatePIDWaitsForCompletePublication(t *testing.T) {
	t.Parallel()
	pidPath := filepath.Join(t.TempDir(), "gate-child.pid")
	if err := os.WriteFile(pidPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		writeDone <- os.WriteFile(pidPath, []byte("4242\n"), 0o600)
	}()

	if got := waitForProofCheckGatePID(t, pidPath, time.Second); got != 4242 {
		t.Fatalf("published pid = %d, want 4242", got)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func waitForProofCheckGatePID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		rawPID, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(rawPID)), "%d", &pid); scanErr == nil && pid > 0 {
				return pid
			} else if scanErr != nil {
				lastErr = fmt.Errorf("parse pid %q: %w", rawPID, scanErr)
			} else {
				lastErr = fmt.Errorf("published pid %d is not positive", pid)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read gate child pid %s: %v", path, err)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for parseable gate child pid at %s: %v", path, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProofCheckCommandInstalledRuntimePublicationFailureIsNonGreen(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckArtifactPublishFailureSubEnv) == "1" {
		proofCheckCanonicalRunner = func(
			ctx context.Context,
			repoPath string,
			dryRun bool,
			source gitrepository.Snapshot,
			ciOnly bool,
			ciConsole bool,
			now time.Time,
		) (proofCheckReport, error) {
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			return report, err
		}
		proofCheckRuntimeArtifactRecorder = func(string, proofCheckReport) (proofCheckReport, error) {
			return proofCheckReport{}, errors.New("synthetic installed-runtime publication fault")
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_ARTIFACT_FAILURE_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_ARTIFACT_FAILURE_WORK"),
			"--actor", "codex:artifact-publication",
			"--out", os.Getenv("COMPOUND_PROOF_ARTIFACT_FAILURE_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-artifact-publication-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "runtime artifact publication failure", "codex:artifact-publication", now)
	repoPath := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "proof.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandInstalledRuntimePublicationFailureIsNonGreen$")
	cmd.Env = append(
		os.Environ(),
		proofCheckArtifactPublishFailureSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_ARTIFACT_FAILURE_REPO="+repoPath,
		"COMPOUND_PROOF_ARTIFACT_FAILURE_WORK="+w.ID,
		"COMPOUND_PROOF_ARTIFACT_FAILURE_OUT="+outPath,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("publication failure exit = %v, want 2; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "battery: FAILED post_report_error=true ") {
		t.Fatalf("publication failure last stdout = %q, want non-green post-report verdict; stdout=%q", last, stdout.String())
	}
	if got := strings.Count(stdout.String(), "battery:"); got != 1 {
		t.Fatalf("publication failure verdict count = %d, want one; stdout=%q", got, stdout.String())
	}
}

func TestProofCheckAdmissionSweepAndExpiryCannotBypassLiveProcessTree(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckBarrierChildSubEnv) == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-sweep-barrier-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "swept proof predecessor", "codex:sweep-alpha", now)
	second := createClaimedProofWork(t, store, "sweep proof successor", "codex:sweep-beta", now.Add(time.Second))

	childStarted := make(chan *exec.Cmd, 1)
	firstDone := make(chan error, 1)
	go func() {
		_, _, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
			HomePath:  homePath,
			WorkID:    first.ID,
			Actor:     "codex:sweep-alpha",
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/sweep-alpha.json",
			Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
			TTL:       minimumProofCheckReservationTTL,
			Now:       func() time.Time { return now.Add(2 * time.Second) },
		}, func(ctx context.Context) (proofCheckReport, error) {
			cmd := proofCheckCommandContext(ctx, os.Args[0], "-test.run=^TestProofCheckAdmissionSweepAndExpiryCannotBypassLiveProcessTree$")
			cmd.Env = append(os.Environ(), proofCheckBarrierChildSubEnv+"=1")
			if err := cmd.Start(); err != nil {
				return proofCheckReport{}, err
			}
			childStarted <- cmd
			return proofCheckReport{}, cmd.Wait()
		})
		firstDone <- runErr
	}()
	child := <-childStarted
	t.Cleanup(func() {
		if child.Process != nil {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-firstDone:
		case <-time.After(5 * time.Second):
		}
	})

	firstWork, err := store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	proofRunID := firstWork.RunAttemptIDs[len(firstWork.RunAttemptIDs)-1]
	attempts := runattempt.Open(homePath)
	report, err := attempts.SweepStaleWithProbe(
		time.Second,
		now.Add(time.Minute),
		buildDelegateLivenessProbe(homePath),
	)
	if err != nil {
		t.Fatal(err)
	}
	swept, err := attempts.Get(proofRunID)
	if err != nil {
		t.Fatal(err)
	}
	if swept.Status != runattempt.StatusRunning || report.Protected != 1 {
		t.Fatalf("live proof sweep = status %q report %+v, want running and protected", swept.Status, report)
	}

	var successorStarts atomic.Int32
	_, _, err = runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    second.ID,
		Actor:     "codex:sweep-beta",
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/sweep-beta.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
		TTL:       minimumProofCheckReservationTTL,
		Now:       func() time.Time { return now.Add(minimumProofCheckReservationTTL + 3*time.Second) },
	}, func(context.Context) (proofCheckReport, error) {
		successorStarts.Add(1)
		return successfulProofCheckReport(), nil
	})
	if err == nil {
		t.Fatal("sweep and TTL expiry admitted successor while predecessor process tree was alive")
	}
	if successorStarts.Load() != 0 {
		t.Fatalf("successor runner starts = %d, want zero before predecessor group death", successorStarts.Load())
	}
}

func TestProofCheckGateProcessInheritsExecutionBarrier(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckBarrierChildSubEnv) == "1" {
		time.Sleep(250 * time.Millisecond)
		os.Exit(0)
	}
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	_, barrier, err := landlease.AcquireWithExecutionBarrierPrepared(
		homePath,
		"proof:pfr_gate:ratt_20260724T130000Z_aaaaaaaa:work_gate:codex-gate",
		time.Second,
		now,
		func(landlease.AcquireOutcome) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := proofCheckContextWithExecutionBarrier(context.Background(), barrier)
	cmd := proofCheckCommandContext(ctx, os.Args[0], "-test.run=^TestProofCheckGateProcessInheritsExecutionBarrier$")
	cmd.Env = append(os.Environ(), proofCheckBarrierChildSubEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := landlease.Acquire(homePath, "generic-landing", time.Minute, now.Add(2*time.Second)); err == nil {
		t.Fatal("gate subprocess did not inherit execution barrier after proof supervisor exited")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := landlease.Acquire(homePath, "generic-landing", time.Minute, now.Add(2*time.Second)); err != nil {
		t.Fatalf("barrier remained after full gate process exit: %v", err)
	}
}

func TestProofCheckAdmissionCancelsLateAsyncCommandBeforeBarrierRelease(t *testing.T) {
	homePath := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "proof-report.json")
	markerPath := filepath.Join(t.TempDir(), "late-command-started")
	now := time.Date(2026, 7, 24, 13, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-late-command-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "cancel late proof command before attestation", "codex:late-command", now)
	input := proofCheckAdmissionInput{
		HomePath:   homePath,
		WorkID:     item.ID,
		Actor:      "codex:late-command",
		Workspace:  "/synthetic/repo-alpha",
		Command:    "compound proof-check --out " + outPath,
		Profile:    syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		ReportPath: outPath,
		TTL:        time.Minute,
		Now:        func() time.Time { return now.Add(time.Second) },
	}

	lateDone := make(chan error, 1)
	var hookErr error
	var heldBeforeBarrierClose bool
	originalAfterCancel := proofCheckAfterRunnerContextCancel
	t.Cleanup(func() { proofCheckAfterRunnerContextCancel = originalAfterCancel })
	proofCheckAfterRunnerContextCancel = func() {
		lateErr := <-lateDone
		if !errors.Is(lateErr, context.Canceled) {
			hookErr = fmt.Errorf("late command error = %v, want context cancellation", lateErr)
			return
		}
		status, err := landlease.Status(homePath, input.Now())
		if err != nil {
			hookErr = err
			return
		}
		heldBeforeBarrierClose = status.Held && status.Holder != ""
	}

	report, completed, err := runProofCheckWithAdmissionContext(input, func(ctx context.Context) (proofCheckReport, error) {
		go func() {
			<-ctx.Done()
			cmd := proofCheckCommandContext(
				ctx,
				"sh",
				"-c",
				`printf started > "$1"`,
				"proof-late-command",
				markerPath,
			)
			lateDone <- cmd.Run()
		}()
		report := successfulProofCheckReport()
		report.GeneratedAt = now.Add(2 * time.Second).Format(time.RFC3339)
		if err := proofCheckWriteReportFile(outPath, report); err != nil {
			return proofCheckReport{}, err
		}
		return report, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !heldBeforeBarrierClose {
		t.Fatal("completion released or stopped holding before the cancelled async command attempt completed")
	}
	if report.Status != "ok" || !completed.ResourceReleased || completed.VerificationRef == "" {
		t.Fatalf("governed completion = report:%+v result:%+v", report, completed)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late cancelled command created marker: %v", err)
	}
}

func TestProofCheckAdmissionRejectsSecondBeforeRunnerStarts(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-admission-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "first governed proof", "codex:alpha", now)
	second := createClaimedProofWork(t, store, "second governed proof", "codex:beta", now.Add(time.Second))

	started := make(chan struct{})
	finish := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, _, runErr := runProofCheckWithAdmission(proofCheckAdmissionInput{
			HomePath:  homePath,
			WorkID:    first.ID,
			Actor:     "codex:alpha",
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
			Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
			TTL:       time.Minute,
			Now:       func() time.Time { return now.Add(2 * time.Second) },
		}, func() (proofCheckReport, error) {
			close(started)
			<-finish
			return successfulProofCheckReport(), nil
		})
		firstDone <- runErr
	}()
	<-started

	var secondRunnerStarts atomic.Int32
	_, _, err = runProofCheckWithAdmission(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    second.ID,
		Actor:     "codex:beta",
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
		TTL:       time.Minute,
		Now:       func() time.Time { return now.Add(3 * time.Second) },
	}, func() (proofCheckReport, error) {
		secondRunnerStarts.Add(1)
		return successfulProofCheckReport(), nil
	})
	var held *work.ProofResourceHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second run error = %v, want proof resource held", err)
	}
	if secondRunnerStarts.Load() != 0 {
		t.Fatalf("conflicting runner started %d times before admission", secondRunnerStarts.Load())
	}

	close(finish)
	if err := <-firstDone; err != nil {
		t.Fatalf("first governed proof: %v", err)
	}
}

func TestProofCheckAdmissionStealCancelsPredecessorProcessBeforeSuccessorStarts(t *testing.T) {
	t.Parallel()
	testProofCheckAdmissionStealCancelsPredecessor(t, false, false)
}

func TestProofCheckAdmissionStealWithDelayedAdmissionPreservesProcessReadinessBudget(t *testing.T) {
	t.Parallel()
	testProofCheckAdmissionStealCancelsPredecessor(t, true, false)
}

func TestProofCheckAdmissionSlowFinalizationPreservesProcessTerminationBudget(t *testing.T) {
	t.Parallel()
	testProofCheckAdmissionStealCancelsPredecessor(t, false, true)
}

func testProofCheckAdmissionStealCancelsPredecessor(t *testing.T, delayAdmission, holdFinalization bool) {
	t.Helper()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-steal-cancellation-test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte(`standing_grants:
  - id: operator_proof_resource_steal
    capability: proof.resource
    mutation_class: security
    reversibility: manual
    blast_radius: repo
    expires_at: "`+time.Now().UTC().Add(365*24*time.Hour).Format(time.RFC3339)+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "cancellable predecessor proof", "codex:cancel-first", now)
	second := createClaimedProofWork(t, store, "successor waits for process death", "codex:cancel-second", now.Add(time.Millisecond))
	firstProfile := syntheticAdmissionProfile(t, "/synthetic/repo-alpha")
	secondProfile := syntheticAdmissionProfile(t, "/synthetic/repo-beta")
	startedPath := filepath.Join(t.TempDir(), "predecessor-started")
	terminatedPath := filepath.Join(t.TempDir(), "predecessor-terminated")
	// Admission is setup, bounded by the test's existing deadline. The
	// process-readiness timer is armed only after exec reports a real start.
	deadline, bounded := t.Deadline()
	if !bounded {
		deadline = time.Now().Add(minimumProofCheckReservationTTL)
	}
	testCtx, cancel := context.WithDeadline(context.Background(), deadline)
	firstFinished := make(chan struct{})
	processStarted := make(chan struct{})
	processTerminated := make(chan error, 1)
	completionGate := make(chan struct{})
	gateReleased := false
	releaseCompletion := func() {
		if !gateReleased {
			close(completionGate)
			gateReleased = true
		}
	}
	defer releaseCompletion()
	firstDone := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-firstFinished:
		case <-time.After(minimumProofCheckReservationTTL):
			t.Error("predecessor admission/runner did not join after test cancellation")
		}
	})
	var delayed atomic.Bool
	admissionNow := func() time.Time {
		if delayAdmission && !delayed.Swap(true) {
			// Delay real admission, not the subprocess, beyond its unchanged
			// three-second readiness window.
			timer := time.NewTimer(3*time.Second + 50*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-testCtx.Done():
			}
		}
		return time.Now()
	}
	go func() {
		defer close(firstFinished)
		_, _, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
			Context:   testCtx,
			HomePath:  homePath,
			WorkID:    first.ID,
			Actor:     "codex:cancel-first",
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
			Profile:   firstProfile,
			TTL:       30 * time.Second,
			Now:       admissionNow,
		}, func(ctx context.Context) (proofCheckReport, error) {
			cmd := proofCheckCommandContext(ctx, "sh", "-c", "printf started > \"$1\"; while :; do sleep 1; done", "proof-predecessor", startedPath)
			if err := cmd.Start(); err != nil {
				return proofCheckReport{}, err
			}
			close(processStarted)
			runErr := cmd.Wait()
			sentinelErr := os.WriteFile(terminatedPath, []byte("terminated\n"), 0o644)
			processTerminated <- sentinelErr
			if holdFinalization {
				<-completionGate
			}
			return proofCheckReport{}, errors.Join(runErr, sentinelErr)
		})
		firstDone <- runErr
	}()
	if err := waitForProofCheckProcessReady(testCtx, processStarted, firstDone, startedPath, 3*time.Second); err != nil {
		t.Fatal(err)
	}

	var successorStarts atomic.Int32
	_, _, secondErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:    homePath,
		WorkID:      second.ID,
		Actor:       "codex:cancel-second",
		Workspace:   "/synthetic/repo-beta",
		Command:     "compound proof-check --out /synthetic/report-beta.json --steal",
		Profile:     secondProfile,
		TTL:         30 * time.Second,
		Steal:       true,
		StealReason: "operator requested takeover after observing the predecessor process",
		Now:         time.Now,
	}, func(context.Context) (proofCheckReport, error) {
		successorStarts.Add(1)
		if _, err := os.Stat(terminatedPath); err != nil {
			return proofCheckReport{}, fmt.Errorf("successor overlapped predecessor process: %w", err)
		}
		return proofCheckReport{}, errors.New("synthetic successor stops after admission-order assertion")
	})
	if secondErr == nil {
		t.Fatal("synthetic successor unexpectedly completed")
	}
	if successorStarts.Load() != 0 {
		status, _ := landlease.Status(homePath, time.Now())
		t.Fatalf("successor runner starts = %d, want zero until predecessor termination; admission error=%v status=%+v", successorStarts.Load(), secondErr, status)
	}
	// Process lifetime and durable audit completion have separate budgets.
	// Keep the process termination bound unchanged even when finalization is slow.
	select {
	case processErr := <-processTerminated:
		if processErr != nil {
			t.Fatalf("record predecessor termination: %v", processErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("predecessor runner did not terminate after lease loss")
	}
	if holdFinalization {
		select {
		case <-firstFinished:
			t.Fatal("completion gate failed to hold finalization")
		default:
		}
	}
	releaseCompletion()
	completionCtx, stopCompletion := context.WithTimeout(testCtx, minimumProofCheckReservationTTL)
	defer stopCompletion()
	select {
	case <-firstFinished:
	case <-completionCtx.Done():
		t.Fatal("predecessor process exited but governed finalization did not finish")
	}
	firstErr := <-firstDone
	if firstErr == nil || !strings.Contains(firstErr.Error(), "heartbeat") {
		t.Fatalf("predecessor completion error = %v, want audited holder-loss heartbeat failure", firstErr)
	}
	if _, err := os.Stat(terminatedPath); err != nil {
		t.Fatalf("predecessor process termination sentinel missing: %v", err)
	}
	_, _, retryErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:    homePath,
		WorkID:      second.ID,
		Actor:       "codex:cancel-second",
		Workspace:   "/synthetic/repo-beta",
		Command:     "compound proof-check --out /synthetic/report-beta.json --steal",
		Profile:     secondProfile,
		TTL:         30 * time.Second,
		Steal:       true,
		StealReason: "operator requested takeover after observing the predecessor process",
		Now:         time.Now,
	}, func(context.Context) (proofCheckReport, error) {
		successorStarts.Add(1)
		if _, err := os.Stat(terminatedPath); err != nil {
			return proofCheckReport{}, fmt.Errorf("successor overlapped predecessor process: %w", err)
		}
		return proofCheckReport{}, errors.New("synthetic successor stops after admission-order assertion")
	})
	if retryErr == nil {
		t.Fatal("synthetic successor retry unexpectedly completed")
	}
	if successorStarts.Load() != 1 {
		t.Fatalf("successor retry runner starts = %d, want one after predecessor group termination; err=%v", successorStarts.Load(), retryErr)
	}
}

func TestProofCheckAdmissionExpiryRequiresGovernedTerminationEvidenceBeforeRunner(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-expiry-termination-test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte(`standing_grants:
  - id: operator_proof_resource_steal
    capability: proof.resource
    mutation_class: security
    reversibility: manual
    blast_radius: repo
    expires_at: "`+time.Now().UTC().Add(365*24*time.Hour).Format(time.RFC3339)+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "expired stopped proof", "codex:expiry-first", now)
	second := createClaimedProofWork(t, store, "governed stale recovery", "codex:expiry-second", now.Add(time.Millisecond))
	firstProfile := syntheticAdmissionProfile(t, "/synthetic/repo-alpha")
	secondProfile := syntheticAdmissionProfile(t, "/synthetic/repo-beta")
	acquired, err := store.ReserveProofResource(first.ID, work.ProofResourceReserveInput{
		Actor:     "codex:expiry-first",
		TTL:       minimumProofCheckReservationTTL,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
		Profile:   firstProfile,
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	var runnerStarts atomic.Int32
	input := proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    second.ID,
		Actor:     "codex:expiry-second",
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
		Profile:   secondProfile,
		TTL:       minimumProofCheckReservationTTL,
		Now:       func() time.Time { return now.Add(minimumProofCheckReservationTTL + 3*time.Second) },
	}
	_, _, err = runProofCheckWithAdmissionContext(input, func(context.Context) (proofCheckReport, error) {
		runnerStarts.Add(1)
		return proofCheckReport{}, errors.New("synthetic post-admission stop")
	})
	if err == nil {
		t.Fatalf("expired predecessor admission error = %v, want execution barrier", err)
	}
	if runnerStarts.Load() != 0 {
		t.Fatalf("expired predecessor started successor runner %d times", runnerStarts.Load())
	}

	if err := acquired.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	input.Command += " --steal --terminated-run " + acquired.Reservation.RunAttemptID
	input.Steal = true
	input.StealReason = "predecessor process group was terminated and waited"
	input.TerminatedRunAttemptID = acquired.Reservation.RunAttemptID
	input.Now = func() time.Time { return now.Add(minimumProofCheckReservationTTL + 5*time.Second) }
	_, _, err = runProofCheckWithAdmissionContext(input, func(context.Context) (proofCheckReport, error) {
		runnerStarts.Add(1)
		return proofCheckReport{}, errors.New("synthetic post-admission stop")
	})
	if err == nil || !strings.Contains(err.Error(), "synthetic post-admission stop") {
		t.Fatalf("governed termination recovery error = %v, want post-admission sentinel", err)
	}
	if runnerStarts.Load() != 1 {
		t.Fatalf("successor runner starts after governed termination = %d, want one", runnerStarts.Load())
	}
}

func TestProofCheckConflictPreflightPreservesOutTarget(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-conflict-out-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "proof conflict holder", "codex:out-alpha", now)
	second := createClaimedProofWork(t, store, "proof conflict waiter", "codex:out-beta", now.Add(time.Second))
	if _, err := store.ReserveProofResource(first.ID, work.ProofResourceReserveInput{
		Actor:     "codex:out-alpha",
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct {
		name     string
		existing []byte
	}{
		{name: "nonexistent"},
		{name: "existing", existing: []byte("previous proof report remains authoritative\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "proof.json")
			if tc.existing != nil {
				if err := os.WriteFile(outPath, tc.existing, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := proofCheckValidateOutPath(outPath); err != nil {
				t.Fatalf("preflight --out: %v", err)
			}
			var runnerStarts atomic.Int32
			_, _, err := runProofCheckWithAdmission(proofCheckAdmissionInput{
				HomePath:   homePath,
				WorkID:     second.ID,
				Actor:      "codex:out-beta",
				Workspace:  "/synthetic/repo-beta",
				Command:    "compound proof-check --out " + outPath,
				Profile:    syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
				ReportPath: outPath,
				TTL:        time.Minute,
				Now:        func() time.Time { return now.Add(time.Duration(3+i) * time.Second) },
			}, func() (proofCheckReport, error) {
				runnerStarts.Add(1)
				return successfulProofCheckReport(), nil
			})
			var held *work.ProofResourceHeldError
			if !errors.As(err, &held) {
				t.Fatalf("conflicting proof = %v, want held refusal", err)
			}
			if runnerStarts.Load() != 0 {
				t.Fatalf("conflicting runner started %d times", runnerStarts.Load())
			}
			got, readErr := os.ReadFile(outPath)
			if tc.existing == nil {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("conflict created nonexistent --out: err=%v bytes=%q", readErr, got)
				}
			} else if readErr != nil || !bytes.Equal(got, tc.existing) {
				t.Fatalf("conflict changed existing --out: err=%v got=%q want=%q", readErr, got, tc.existing)
			}
		})
	}
}

func TestProofCheckAdmissionCompletionErrorLeavesNoRunningAttempt(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-terminal-return-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "completion error terminalizes run", "codex:terminal-return", now)
	workDir := filepath.Join(homePath, "work")
	_, _, runErr := runProofCheckWithAdmission(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    w.ID,
		Actor:     "codex:terminal-return",
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		TTL:       time.Minute,
		Now:       func() time.Time { return now.Add(2 * time.Second) },
	}, func() (proofCheckReport, error) {
		if err := os.Chmod(workDir, 0o555); err != nil {
			return proofCheckReport{}, err
		}
		return successfulProofCheckReport(), nil
	})
	if err := os.Chmod(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runErr == nil {
		t.Fatal("completion persistence fault unexpectedly succeeded")
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RunAttemptIDs) == 0 {
		t.Fatal("completion fault omitted attached ratt_*")
	}
	ra, err := runattempt.Open(homePath).Get(after.RunAttemptIDs[len(after.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(ra.Status) {
		t.Fatalf("runProofCheckWithAdmission returned error with %s still %q", ra.ID, ra.Status)
	}
}

func TestProofCheckAdmissionSurfacesCommittedButRetainedLease(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-retained-release-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "retained release is not plain green", "codex:retained-release", now)
	successorWork := createClaimedProofWork(t, store, "successor after public recovery", "codex:successor", now.Add(time.Second))
	stateDir := filepath.Join(homePath, "state")
	outPath := filepath.Join(t.TempDir(), "proof.json")
	currentNow := now.Add(2 * time.Second)
	runInput := proofCheckAdmissionInput{
		HomePath:   homePath,
		WorkID:     w.ID,
		Actor:      "codex:retained-release",
		Workspace:  "/synthetic/repo-alpha",
		Command:    "compound proof-check --out " + outPath,
		Profile:    syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		ReportPath: outPath,
		TTL:        time.Minute,
		Now:        func() time.Time { return currentNow },
	}
	var runnerCalls atomic.Int32

	report, completed, runErr := runProofCheckWithAdmission(runInput, func() (proofCheckReport, error) {
		runnerCalls.Add(1)
		produced := successfulProofCheckReport()
		if err := proofCheckWriteReportFile(outPath, produced); err != nil {
			return proofCheckReport{}, err
		}
		if err := os.Chmod(stateDir, 0o555); err != nil {
			return proofCheckReport{}, err
		}
		return produced, nil
	})
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runErr == nil {
		t.Fatalf("retained lease returned plain success with report status %q", report.Status)
	}
	var releasePending *work.ProofResourceReleasePendingError
	if !errors.As(runErr, &releasePending) {
		t.Fatalf("admission error = %v, want typed release-pending signal", runErr)
	}
	if completed.VerificationRef == "" ||
		completed.ResourceReleased ||
		!completed.RecoveryJournalRetained {
		t.Fatalf("admission result did not expose retained release: %+v", completed)
	}
	status, err := landlease.Status(homePath, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held {
		t.Fatal("admission boundary reported retained release but lease was not observable")
	}

	beforeResumeOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeResumeWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentNow = now.Add(4 * time.Second)
	resumedReport, resumed, resumeErr := runProofCheckWithAdmission(runInput, func() (proofCheckReport, error) {
		runnerCalls.Add(1)
		return proofCheckReport{}, errors.New("proof runner must not restart during retained-release recovery")
	})
	if resumeErr != nil {
		t.Fatalf("same public command did not recover retained release: %v", resumeErr)
	}
	if runnerCalls.Load() != 1 {
		t.Fatalf("retained-release recovery restarted proof runner; calls=%d", runnerCalls.Load())
	}
	if !reflect.DeepEqual(resumedReport, report) {
		t.Fatalf("resumed report differs from prior owned report:\n got=%+v\nwant=%+v", resumedReport, report)
	}
	if resumed.Result.ID != completed.Result.ID ||
		resumed.Result.RunAttemptID != completed.Result.RunAttemptID ||
		resumed.VerificationRef != completed.VerificationRef ||
		!resumed.ResourceReleased ||
		resumed.RecoveryJournalRetained {
		t.Fatalf("resumed result did not truthfully finalize prior completion: first=%+v resumed=%+v", completed, resumed)
	}
	afterResumeOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterResumeOut, beforeResumeOut) {
		t.Fatal("retained-release recovery rewrote the prior owned --out")
	}
	status, err = landlease.Status(homePath, currentNow)
	if err != nil {
		t.Fatal(err)
	}
	if status.Held {
		t.Fatalf("public recovery retained scarce holder: %+v", status)
	}
	pendingEntries, err := os.ReadDir(filepath.Join(homePath, "work", "proof-resource-completions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingEntries) != 0 {
		t.Fatalf("public recovery retained completion journal entries: %v", pendingEntries)
	}
	finalWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalWork.RunAttemptIDs) != len(beforeResumeWork.RunAttemptIDs) {
		t.Fatalf("public recovery minted a new RunAttempt: before=%v after=%v", beforeResumeWork.RunAttemptIDs, finalWork.RunAttemptIDs)
	}
	attempts, err := runattempt.Open(homePath).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.WorkID != w.ID {
			continue
		}
		if !containsString(finalWork.RunAttemptIDs, attempt.ID) {
			t.Fatalf("recovery attempt %s is not reachable from Work: %v", attempt.ID, finalWork.RunAttemptIDs)
		}
		if !runattempt.IsTerminal(attempt.Status) {
			t.Fatalf("recovery attempt %s remained %q", attempt.ID, attempt.Status)
		}
	}
	successor, err := store.ReserveProofResource(successorWork.ID, work.ProofResourceReserveInput{
		Actor:     "codex:successor",
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
	}, currentNow.Add(time.Second))
	if err != nil {
		t.Fatalf("successor remained wedged after public recovery: %v", err)
	}
	if successor.Reservation.Status != work.ProofResourceAcquired {
		t.Fatalf("successor reservation = %+v, want acquired", successor.Reservation)
	}
}

func TestProofCheckCommandReleasePendingVerdictAndExactRecovery(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckReleasePendingCLISubEnv) == "1" {
		if os.Getenv(proofCheckReleasePendingTamperEnv) == "1" {
			proofCheckBeforeRetainedOutMatch = func(path string) {
				if err := os.WriteFile(path, []byte(proofCheckReleasePendingTamperBody), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		realTimingRecorder := proofCheckTimingHistoryRecorder
		proofCheckTimingHistoryRecorder = func(
			homePath string,
			report proofCheckReport,
			wall time.Duration,
			now time.Time,
		) (string, error) {
			if markerPath := os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_TIMINGS"); markerPath != "" {
				marker, err := os.OpenFile(
					markerPath,
					os.O_CREATE|os.O_APPEND|os.O_WRONLY,
					0o600,
				)
				if err != nil {
					return "", err
				}
				if _, err := marker.WriteString("timing-recorded\n"); err != nil {
					_ = marker.Close()
					return "", err
				}
				if err := marker.Close(); err != nil {
					return "", err
				}
			}
			return realTimingRecorder(homePath, report, wall, now)
		}
		proofCheckCanonicalRunner = func(_ context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			marker, err := os.OpenFile(os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_RUNS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return proofCheckReport{}, err
			}
			if _, err := marker.WriteString("runner-started\n"); err != nil {
				_ = marker.Close()
				return proofCheckReport{}, err
			}
			if err := marker.Close(); err != nil {
				return proofCheckReport{}, err
			}
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			if err != nil {
				return proofCheckReport{}, err
			}
			if err := os.Chmod(filepath.Join(os.Getenv("COMPOUND_HOME"), "state"), 0o555); err != nil {
				return proofCheckReport{}, err
			}
			return report, nil
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_REPO"),
			"--base", os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_BASE"),
			"--work", os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_WORK"),
			"--actor", "codex:release-pending-cli",
			"--out", os.Getenv("COMPOUND_PROOF_RELEASE_PENDING_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-release-pending-cli-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "real CLI release-pending recovery", "codex:release-pending-cli", now)
	repo := initCompoundCommandGitRepo(t)
	proofBaseSHA := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	const proofBaseRef = "refs/heads/proof-base"
	runCommandGit(t, repo, "branch", "proof-base", proofBaseSHA)
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "candidate.txt")
	runCommandGit(t, repo, "commit", "-m", "candidate")
	outPath := filepath.Join(t.TempDir(), "proof.json")
	runnerMarker := filepath.Join(t.TempDir(), "runner.log")
	timingMarker := filepath.Join(t.TempDir(), "timings.log")
	stateDir := filepath.Join(homePath, "state")
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	runCommand := func() (stdout, stderr string, err error) {
		command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandReleasePendingVerdictAndExactRecovery$")
		command.Env = append(os.Environ(),
			proofCheckReleasePendingCLISubEnv+"=1",
			"COMPOUND_HOME="+homePath,
			"COMPOUND_PROOF_RELEASE_PENDING_REPO="+repo,
			"COMPOUND_PROOF_RELEASE_PENDING_BASE="+proofBaseRef,
			"COMPOUND_PROOF_RELEASE_PENDING_WORK="+w.ID,
			"COMPOUND_PROOF_RELEASE_PENDING_OUT="+outPath,
			"COMPOUND_PROOF_RELEASE_PENDING_RUNS="+runnerMarker,
			"COMPOUND_PROOF_RELEASE_PENDING_TIMINGS="+timingMarker,
		)
		var stdoutBuf bytes.Buffer
		var stderrBuf bytes.Buffer
		command.Stdout = &stdoutBuf
		command.Stderr = &stderrBuf
		err = command.Run()
		return stdoutBuf.String(), stderrBuf.String(), err
	}

	firstStdout, firstStderr, firstErr := runCommand()
	var firstExit *exec.ExitError
	if !errors.As(firstErr, &firstExit) || firstExit.ExitCode() != 2 {
		t.Fatalf("release-pending command exit = %v, want 2; stdout=%q stderr=%q", firstErr, firstStdout, firstStderr)
	}
	firstLines := strings.Split(strings.TrimSpace(firstStdout), "\n")
	firstLast := firstLines[len(firstLines)-1]
	if !strings.HasPrefix(firstLast, "battery: FAILED ") ||
		!strings.Contains(firstLast, "release_pending=true") ||
		!strings.HasSuffix(firstLast, "out="+outPath) {
		t.Fatalf("release-pending last stdout line = %q, want explicit FAILED verdict; stdout:\n%s", firstLast, firstStdout)
	}
	if strings.Contains(firstStdout, "battery: ok") {
		t.Fatalf("release-pending command printed a green verdict:\n%s", firstStdout)
	}
	if got := strings.Count(firstStdout, "battery:"); got != 1 {
		t.Fatalf("release-pending stdout verdict count = %d, want exactly one:\n%s", got, firstStdout)
	}
	if !strings.Contains(firstStderr, "release remains pending") {
		t.Fatalf("release-pending stderr omitted recovery reason:\n%s", firstStderr)
	}
	firstOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("release-pending command did not preserve --out: %v", err)
	}
	var firstReport proofCheckReport
	if err := json.Unmarshal(firstOut, &firstReport); err != nil {
		t.Fatalf("release-pending --out is not a report: %v", err)
	}
	if firstReport.Status != "ok" {
		t.Fatalf("authoritative report status = %q, want prior gate result ok", firstReport.Status)
	}
	runtimeArtifacts, err := filepath.Glob(filepath.Join(homePath, "artifacts", "refs", "proof-check-installed-runtime-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runtimeArtifacts) != 0 {
		t.Fatalf("release-pending proof published installed-runtime artifact before governed release: %v", runtimeArtifacts)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runCommandGit(t, repo, "branch", "-f", "proof-base", "HEAD")
	mismatchStdout, mismatchStderr, mismatchErr := runCommand()
	if mismatchErr == nil {
		t.Fatalf("moved proof base reused retained battery; stdout=%q stderr=%q", mismatchStdout, mismatchStderr)
	}
	runsAfterMismatch, err := os.ReadFile(runnerMarker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runsAfterMismatch), "runner-started"); got != 1 {
		t.Fatalf("moved proof base restarted prior runner; starts=%d marker=%q", got, runsAfterMismatch)
	}
	afterMismatchOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterMismatchOut, firstOut) {
		t.Fatal("moved proof base rewrote retained owned report")
	}
	runCommandGit(t, repo, "branch", "-f", "proof-base", proofBaseSHA)
	outParent := filepath.Dir(outPath)
	if err := os.Chmod(outParent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outParent, 0o700) })
	secondStdout, secondStderr, secondErr := runCommand()
	if err := os.Chmod(outParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if secondErr != nil {
		t.Fatalf("exact CLI recovery failed: %v; stdout=%q stderr=%q", secondErr, secondStdout, secondStderr)
	}
	secondLines := strings.Split(strings.TrimSpace(secondStdout), "\n")
	secondLast := secondLines[len(secondLines)-1]
	if !strings.HasPrefix(secondLast, "battery: ok ") || !strings.HasSuffix(secondLast, "out="+outPath) {
		t.Fatalf("recovered last stdout line = %q, want truthful prior ok verdict; stdout:\n%s", secondLast, secondStdout)
	}
	if got := strings.Count(secondStdout, "battery:"); got != 1 {
		t.Fatalf("recovered stdout verdict count = %d, want exactly one:\n%s", got, secondStdout)
	}
	if secondStderr != "" {
		t.Fatalf("exact recovery wrote unexpected stderr:\n%s", secondStderr)
	}
	runnerRuns, err := os.ReadFile(runnerMarker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runnerRuns), "runner-started"); got != 1 {
		t.Fatalf("exact CLI recovery runner starts = %d, want one total; marker=%q", got, runnerRuns)
	}
	timingRuns, err := os.ReadFile(timingMarker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(timingRuns), "timing-recorded"); got != 1 {
		t.Fatalf("exact CLI recovery fabricated timing sample; records=%d marker=%q", got, timingRuns)
	}
	secondOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondOut, firstOut) {
		t.Fatal("exact CLI recovery rewrote the prior owned report")
	}
	runtimeArtifacts, err = filepath.Glob(filepath.Join(homePath, "artifacts", "refs", "proof-check-installed-runtime-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runtimeArtifacts) != 1 {
		t.Fatalf("successful governed recovery published %d installed-runtime artifacts, want exactly one: %v", len(runtimeArtifacts), runtimeArtifacts)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{
		To:    "running",
		Actor: "codex:release-pending-cli",
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, work.CloseInput{
		Outcome:  "completed",
		MoveType: "audit_only",
		Actor:    "codex:release-pending-cli",
		Summary:  "successful governed proof release",
	}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "synthetic held valve keeps adjacent proof projection visible",
		State:            "held",
	}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	afterRecoveryWorkbench, err := store.Workbench(now.Add(5*time.Second), work.WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	adjacent := afterRecoveryWorkbench.DispatchNext.AdjacentProgress
	if adjacent == nil {
		t.Fatalf("successful governed recovery omitted adjacent Workbench projection: %+v", afterRecoveryWorkbench.DispatchNext)
	}
	if _, ok := workbenchAdjacentActionByID(adjacent.ConsumedNonPacketActions, "proof-check-installed-runtime"); !ok {
		t.Fatalf("successful governed recovery did not consume installed-runtime action: %+v", adjacent)
	}
}

func TestProofCheckCommandReleasePendingRecoveryRestoresOwnedOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		mutate             func(t *testing.T, path string)
		readOnly           bool
		tamperAfterCapture bool
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tampered",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("tampered proof output\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "missing read-only parent",
			readOnly: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "tampered read-only parent",
			readOnly: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("tampered proof output\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:               "same-inode tamper after capture",
			tamperAfterCapture: true,
			mutate:             func(*testing.T, string) {},
		},
		{
			name:               "same-inode tamper after capture read-only parent",
			readOnly:           true,
			tamperAfterCapture: true,
			mutate:             func(*testing.T, string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			homePath := t.TempDir()
			now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
			if err := home.Init(homePath, "proof-release-pending-restore-test", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w := createClaimedProofWork(t, store, "restore retained proof output", "codex:release-pending-cli", now)
			repo := initCompoundCommandGitRepo(t)
			outPath := filepath.Join(t.TempDir(), "proof.json")
			runnerMarker := filepath.Join(t.TempDir(), "runner.log")
			stateDir := filepath.Join(homePath, "state")
			t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

			runCommand := func() (stdout, stderr string, err error) {
				tamperAfterCapture := "0"
				if tc.tamperAfterCapture {
					tamperAfterCapture = "1"
				}
				command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandReleasePendingVerdictAndExactRecovery$")
				command.Env = append(
					os.Environ(),
					proofCheckReleasePendingCLISubEnv+"=1",
					"COMPOUND_HOME="+homePath,
					"COMPOUND_PROOF_RELEASE_PENDING_REPO="+repo,
					"COMPOUND_PROOF_RELEASE_PENDING_BASE=HEAD",
					"COMPOUND_PROOF_RELEASE_PENDING_WORK="+w.ID,
					"COMPOUND_PROOF_RELEASE_PENDING_OUT="+outPath,
					"COMPOUND_PROOF_RELEASE_PENDING_RUNS="+runnerMarker,
					proofCheckReleasePendingTamperEnv+"="+tamperAfterCapture,
				)
				var stdoutBuf bytes.Buffer
				var stderrBuf bytes.Buffer
				command.Stdout = &stdoutBuf
				command.Stderr = &stderrBuf
				err = command.Run()
				return stdoutBuf.String(), stderrBuf.String(), err
			}

			firstStdout, firstStderr, firstErr := runCommand()
			var firstExit *exec.ExitError
			if !errors.As(firstErr, &firstExit) || firstExit.ExitCode() != 2 {
				t.Fatalf("release-pending command exit = %v, want 2; stdout=%q stderr=%q", firstErr, firstStdout, firstStderr)
			}
			firstOut, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, outPath)

			var beforeRetryOut []byte
			var beforeRetryErr error
			outParent := filepath.Dir(outPath)
			if tc.readOnly {
				beforeRetryOut, beforeRetryErr = os.ReadFile(outPath)
				if beforeRetryErr != nil && !errors.Is(beforeRetryErr, os.ErrNotExist) {
					t.Fatal(beforeRetryErr)
				}
				if err := os.Chmod(outParent, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(outParent, 0o700) })
			}

			secondStdout, secondStderr, secondErr := runCommand()
			if tc.readOnly {
				if err := os.Chmod(outParent, 0o700); err != nil {
					t.Fatal(err)
				}
				var secondExit *exec.ExitError
				if !errors.As(secondErr, &secondExit) || secondExit.ExitCode() != 2 {
					t.Fatalf("read-only restore recovery exit = %v, want 2; stdout=%q stderr=%q", secondErr, secondStdout, secondStderr)
				}
				if strings.Contains(secondStdout, "battery: ok") {
					t.Fatalf("read-only restore recovery printed a green verdict:\n%s", secondStdout)
				}
				runnerRuns, err := os.ReadFile(runnerMarker)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(string(runnerRuns), "runner-started"); got != 1 {
					t.Fatalf("read-only restore recovery runner starts = %d, want one total", got)
				}
				status, err := landlease.Status(homePath, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if !status.Held || status.Holder == "" {
					t.Fatalf("read-only restore recovery released the pending holder: %+v", status)
				}
				afterRetryOut, afterRetryErr := os.ReadFile(outPath)
				if errors.Is(beforeRetryErr, os.ErrNotExist) {
					if !errors.Is(afterRetryErr, os.ErrNotExist) {
						t.Fatalf("read-only restore recovery rewrote missing output: err=%v bytes=%q", afterRetryErr, afterRetryOut)
					}
				} else {
					if afterRetryErr != nil {
						t.Fatal(afterRetryErr)
					}
					wantAfterRetry := beforeRetryOut
					if tc.tamperAfterCapture {
						wantAfterRetry = []byte(proofCheckReleasePendingTamperBody)
					}
					if !bytes.Equal(afterRetryOut, wantAfterRetry) {
						t.Fatalf("read-only restore recovery rewrote tampered output: got=%q want=%q", afterRetryOut, wantAfterRetry)
					}
				}
				return
			}
			if secondErr != nil {
				t.Fatalf("exact restore recovery failed: %v; stdout=%q stderr=%q", secondErr, secondStdout, secondStderr)
			}
			secondLines := strings.Split(strings.TrimSpace(secondStdout), "\n")
			secondLast := secondLines[len(secondLines)-1]
			if !strings.HasPrefix(secondLast, "battery: ok ") ||
				!strings.HasSuffix(secondLast, "out="+outPath) ||
				strings.Count(secondStdout, "battery:") != 1 {
				t.Fatalf("restored recovery verdict = %q; stdout:\n%s", secondLast, secondStdout)
			}
			if secondStderr != "" {
				t.Fatalf("restored recovery wrote unexpected stderr:\n%s", secondStderr)
			}
			restoredOut, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("recovery returned green with missing --out: %v", err)
			}
			if !bytes.Equal(restoredOut, firstOut) {
				t.Fatalf("recovery returned green with non-owned --out: got=%q want immutable report", restoredOut)
			}
			runnerRuns, err := os.ReadFile(runnerMarker)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(runnerRuns), "runner-started"); got != 1 {
				t.Fatalf("restore recovery runner starts = %d, want one total", got)
			}
			status, err := landlease.Status(homePath, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if status.Held {
				t.Fatalf("restore recovery retained lease: %+v", status)
			}
		})
	}
}

func TestProofCheckCommandCanonicalFailedBatteryReleasesAdmission(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckFailedCanonicalCLISubEnv) == "1" {
		proofCheckCanonicalRunner = func(_ context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			if err != nil {
				return proofCheckReport{}, err
			}
			report.Status = "failed"
			report.Steps[0].Status = "failed"
			report.Steps[0].ExitCode = 1
			report.Steps[0].OutputTail = "injected canonical gate failed"
			report.ProofRefs = append([]string(nil), report.ProofRefs[1:]...)
			return report, nil
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_FAILED_CANONICAL_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_FAILED_CANONICAL_WORK"),
			"--actor", "codex:failed-canonical-cli",
			"--out", os.Getenv("COMPOUND_PROOF_FAILED_CANONICAL_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-failed-canonical-cli-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "real CLI complete failed proof", "codex:failed-canonical-cli", now)
	successorWork := createClaimedProofWork(t, store, "proof after real CLI failure", "codex:successor", now.Add(time.Second))
	repo := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "proof.json")

	command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandCanonicalFailedBatteryReleasesAdmission$")
	command.Env = append(
		os.Environ(),
		proofCheckFailedCanonicalCLISubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_FAILED_CANONICAL_REPO="+repo,
		"COMPOUND_PROOF_FAILED_CANONICAL_WORK="+w.ID,
		"COMPOUND_PROOF_FAILED_CANONICAL_OUT="+outPath,
	)
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	command.Stdout = &stdoutBuf
	command.Stderr = &stderrBuf
	runErr := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("failed canonical command exit = %v, want 1; stdout=%q stderr=%q", runErr, stdoutBuf.String(), stderrBuf.String())
	}
	if stderrBuf.Len() != 0 {
		t.Fatalf("complete failed battery wrote unexpected stderr:\n%s", stderrBuf.String())
	}
	stdout := stdoutBuf.String()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "battery: FAILED ") ||
		!strings.Contains(last, "failed=1") ||
		!strings.HasSuffix(last, "out="+outPath) {
		t.Fatalf("failed canonical last stdout line = %q, want truthful FAILED verdict; stdout:\n%s", last, stdout)
	}
	if got := strings.Count(stdout, "battery:"); got != 1 {
		t.Fatalf("failed canonical stdout verdict count = %d, want exactly one:\n%s", got, stdout)
	}

	reportBody, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("failed canonical command did not preserve --out: %v", err)
	}
	var report proofCheckReport
	if err := json.Unmarshal(reportBody, &report); err != nil {
		t.Fatalf("failed canonical --out is not a full report: %v", err)
	}
	expectedSteps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	failedGates := 0
	skippedGates := 0
	for _, step := range report.Steps {
		switch step.Status {
		case "failed":
			failedGates++
		case "skipped":
			skippedGates++
		}
	}
	if report.Status != "failed" ||
		len(report.Steps) != len(expectedSteps) ||
		failedGates != 1 ||
		skippedGates != 0 ||
		len(report.ProofRefs) != len(expectedSteps)-1 {
		t.Fatalf("failed canonical --out = %+v, want complete red report", report)
	}

	finalWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalWork.VerificationIDs) != 0 {
		t.Fatalf("failed canonical report entered verification_ids: %v", finalWork.VerificationIDs)
	}
	for _, attemptID := range finalWork.RunAttemptIDs {
		attempt, err := runattempt.Open(homePath).Get(attemptID)
		if err != nil {
			t.Fatal(err)
		}
		if !runattempt.IsTerminal(attempt.Status) {
			t.Fatalf("failed canonical attempt %s remained %q", attempt.ID, attempt.Status)
		}
	}
	pending, err := os.ReadDir(filepath.Join(homePath, "work", "proof-resource-completions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("failed canonical command retained completion journal: %v", pending)
	}
	lease, err := landlease.Status(homePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Held {
		t.Fatalf("failed canonical command retained scarce lease: %+v", lease)
	}

	successor, err := store.ReserveProofResource(successorWork.ID, work.ProofResourceReserveInput{
		Actor:     "codex:successor",
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("successor remained blocked after complete failed battery: %v", err)
	}
	if successor.Reservation.Status != work.ProofResourceAcquired {
		t.Fatalf("successor reservation = %+v, want acquired", successor.Reservation)
	}
}

func TestProofCheckCommandFailedBatteryReleasePendingAndExactRecovery(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckFailedReleaseCLISubEnv) == "1" {
		proofCheckCanonicalRunner = func(_ context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			marker, err := os.OpenFile(os.Getenv("COMPOUND_PROOF_FAILED_RELEASE_RUNS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return proofCheckReport{}, err
			}
			if _, err := marker.WriteString("runner-started\n"); err != nil {
				_ = marker.Close()
				return proofCheckReport{}, err
			}
			if err := marker.Close(); err != nil {
				return proofCheckReport{}, err
			}
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			if err != nil {
				return proofCheckReport{}, err
			}
			report.Status = "failed"
			report.Steps[0].Status = "failed"
			report.Steps[0].ExitCode = 1
			report.Steps[0].OutputTail = "injected canonical gate failed"
			report.ProofRefs = append([]string(nil), report.ProofRefs[1:]...)
			if err := os.Chmod(filepath.Join(os.Getenv("COMPOUND_HOME"), "state"), 0o555); err != nil {
				return proofCheckReport{}, err
			}
			return report, nil
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_FAILED_RELEASE_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_FAILED_RELEASE_WORK"),
			"--actor", "codex:failed-release-cli",
			"--out", os.Getenv("COMPOUND_PROOF_FAILED_RELEASE_OUT"),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-failed-release-cli-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "failed battery retained release", "codex:failed-release-cli", now)
	repo := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "proof.json")
	runnerMarker := filepath.Join(t.TempDir(), "runner.log")
	stateDir := filepath.Join(homePath, "state")
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	runCommand := func() (stdout, stderr string, err error) {
		command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandFailedBatteryReleasePendingAndExactRecovery$")
		command.Env = append(os.Environ(),
			proofCheckFailedReleaseCLISubEnv+"=1",
			"COMPOUND_HOME="+homePath,
			"COMPOUND_PROOF_FAILED_RELEASE_REPO="+repo,
			"COMPOUND_PROOF_FAILED_RELEASE_WORK="+w.ID,
			"COMPOUND_PROOF_FAILED_RELEASE_OUT="+outPath,
			"COMPOUND_PROOF_FAILED_RELEASE_RUNS="+runnerMarker,
		)
		var stdoutBuf bytes.Buffer
		var stderrBuf bytes.Buffer
		command.Stdout = &stdoutBuf
		command.Stderr = &stderrBuf
		err = command.Run()
		return stdoutBuf.String(), stderrBuf.String(), err
	}

	firstStdout, firstStderr, firstErr := runCommand()
	var firstExit *exec.ExitError
	if !errors.As(firstErr, &firstExit) || firstExit.ExitCode() != 2 {
		t.Fatalf("failed release-pending exit = %v, want 2; stdout=%q stderr=%q", firstErr, firstStdout, firstStderr)
	}
	firstLines := strings.Split(strings.TrimSpace(firstStdout), "\n")
	firstLast := firstLines[len(firstLines)-1]
	if !strings.HasPrefix(firstLast, "battery: FAILED ") ||
		!strings.Contains(firstLast, "release_pending=true") ||
		!strings.Contains(firstLast, "failed=1") ||
		!strings.HasSuffix(firstLast, "out="+outPath) {
		t.Fatalf("failed release-pending last stdout line = %q; stdout:\n%s", firstLast, firstStdout)
	}
	if got := strings.Count(firstStdout, "battery:"); got != 1 {
		t.Fatalf("failed release-pending verdict count = %d, want one:\n%s", got, firstStdout)
	}
	if !strings.Contains(firstStderr, "release remains pending") {
		t.Fatalf("failed release-pending stderr omitted recovery reason:\n%s", firstStderr)
	}
	firstOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var firstReport proofCheckReport
	if err := json.Unmarshal(firstOut, &firstReport); err != nil {
		t.Fatal(err)
	}
	if firstReport.Status != "failed" {
		t.Fatalf("failed release-pending report status = %q", firstReport.Status)
	}
	firstWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstWork.VerificationIDs) != 0 {
		t.Fatalf("red release-pending battery certified verification: %v", firstWork.VerificationIDs)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	secondStdout, secondStderr, secondErr := runCommand()
	var secondExit *exec.ExitError
	if !errors.As(secondErr, &secondExit) || secondExit.ExitCode() != 1 {
		t.Fatalf("failed exact recovery exit = %v, want 1; stdout=%q stderr=%q", secondErr, secondStdout, secondStderr)
	}
	secondLines := strings.Split(strings.TrimSpace(secondStdout), "\n")
	secondLast := secondLines[len(secondLines)-1]
	if !strings.HasPrefix(secondLast, "battery: FAILED ") ||
		strings.Contains(secondLast, "release_pending=true") ||
		!strings.Contains(secondLast, "failed=1") ||
		!strings.HasSuffix(secondLast, "out="+outPath) {
		t.Fatalf("failed exact recovery last stdout line = %q; stdout:\n%s", secondLast, secondStdout)
	}
	if secondStderr != "" {
		t.Fatalf("failed exact recovery wrote stderr:\n%s", secondStderr)
	}
	runnerRuns, err := os.ReadFile(runnerMarker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runnerRuns), "runner-started"); got != 1 {
		t.Fatalf("failed exact recovery runner starts = %d, want one total", got)
	}
	secondOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondOut, firstOut) {
		t.Fatal("failed exact recovery changed immutable report")
	}
	status, err := landlease.Status(homePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if status.Held {
		t.Fatalf("failed exact recovery retained scarce lease: %+v", status)
	}
	finalWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalWork.VerificationIDs) != 0 {
		t.Fatalf("recovered red battery certified verification: %v", finalWork.VerificationIDs)
	}
}

func TestProofCheckCommandPostReportFailuresAlwaysPrintFailedVerdict(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckPostReportErrorCLISubEnv) == "1" {
		proofCheckCanonicalRunner = func(ctx context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			if err != nil {
				return proofCheckReport{}, err
			}
			switch os.Getenv("COMPOUND_PROOF_POST_REPORT_ERROR_CASE") {
			case "validation":
				report.Steps[0].Command = []string{"go", "test", "./synthetic-tampered-gate"}
			case "heartbeat":
				if err := os.Chmod(filepath.Join(os.Getenv("COMPOUND_HOME"), "state"), 0o555); err != nil {
					return proofCheckReport{}, err
				}
				select {
				case <-ctx.Done():
				case <-time.After(minimumProofCheckReservationTTL):
					return proofCheckReport{}, errors.New("heartbeat cancellation did not reach runner")
				}
			default:
				return proofCheckReport{}, errors.New("unknown injected post-report error case")
			}
			return report, nil
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_POST_REPORT_ERROR_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_POST_REPORT_ERROR_WORK"),
			"--actor", "codex:post-report-error-cli",
			"--out", os.Getenv("COMPOUND_PROOF_POST_REPORT_ERROR_OUT"),
			"--ttl", minimumProofCheckReservationTTL.String(),
		})
		os.Exit(0)
	}

	for _, testCase := range []string{"validation", "heartbeat"} {
		t.Run(testCase, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Now().UTC()
			if err := home.Init(homePath, "proof-post-report-error-test", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w := createClaimedProofWork(t, store, "post-report error verdict", "codex:post-report-error-cli", now)
			repo := initCompoundCommandGitRepo(t)
			outPath := filepath.Join(t.TempDir(), "proof.json")
			stateDir := filepath.Join(homePath, "state")
			t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })
			command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandPostReportFailuresAlwaysPrintFailedVerdict$")
			command.Env = append(os.Environ(),
				proofCheckPostReportErrorCLISubEnv+"=1",
				"COMPOUND_HOME="+homePath,
				"COMPOUND_PROOF_POST_REPORT_ERROR_CASE="+testCase,
				"COMPOUND_PROOF_POST_REPORT_ERROR_REPO="+repo,
				"COMPOUND_PROOF_POST_REPORT_ERROR_WORK="+w.ID,
				"COMPOUND_PROOF_POST_REPORT_ERROR_OUT="+outPath,
			)
			var stdoutBuf bytes.Buffer
			var stderrBuf bytes.Buffer
			command.Stdout = &stdoutBuf
			command.Stderr = &stderrBuf
			err = command.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("%s post-report exit = %v, want 2; stdout=%q stderr=%q", testCase, err, stdoutBuf.String(), stderrBuf.String())
			}
			stdout := stdoutBuf.String()
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, "battery: FAILED ") ||
				!strings.HasSuffix(last, "out="+outPath) ||
				strings.Contains(stdout, "battery: ok") {
				t.Fatalf("%s post-report last stdout line = %q; stdout:\n%s", testCase, last, stdout)
			}
			if got := strings.Count(stdout, "battery:"); got != 1 {
				t.Fatalf("%s post-report verdict count = %d, want one:\n%s", testCase, got, stdout)
			}
			body, readErr := os.ReadFile(outPath)
			if readErr != nil {
				t.Fatalf("%s post-report failure omitted durable --out: %v", testCase, readErr)
			}
			var report proofCheckReport
			if err := json.Unmarshal(body, &report); err != nil || report.SchemaVersion == "" {
				t.Fatalf("%s post-report --out is not a complete report: err=%v report=%+v", testCase, err, report)
			}
		})
	}
}

func TestProofCheckCommandRejectsUnsafeTTLBeforeAdmission(t *testing.T) {
	t.Parallel()
	if os.Getenv(proofCheckUnsafeTTLCLISubEnv) == "1" {
		proofCheckCanonicalRunner = func(_ context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			if err := os.WriteFile(os.Getenv("COMPOUND_PROOF_UNSAFE_TTL_RUNNER"), []byte("runner-started\n"), 0o644); err != nil {
				return proofCheckReport{}, err
			}
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			return report, err
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_UNSAFE_TTL_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_UNSAFE_TTL_WORK"),
			"--actor", "codex:unsafe-ttl-cli",
			"--out", os.Getenv("COMPOUND_PROOF_UNSAFE_TTL_OUT"),
			"--ttl", "3s",
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-unsafe-ttl-cli-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "unsafe proof ttl refused", "codex:unsafe-ttl-cli", now)
	before, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo := initCompoundCommandGitRepo(t)
	outPath := filepath.Join(t.TempDir(), "proof.json")
	runnerMarker := filepath.Join(t.TempDir(), "runner.log")
	command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandRejectsUnsafeTTLBeforeAdmission$")
	command.Env = append(
		os.Environ(),
		proofCheckUnsafeTTLCLISubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_UNSAFE_TTL_REPO="+repo,
		"COMPOUND_PROOF_UNSAFE_TTL_WORK="+w.ID,
		"COMPOUND_PROOF_UNSAFE_TTL_OUT="+outPath,
		"COMPOUND_PROOF_UNSAFE_TTL_RUNNER="+runnerMarker,
	)
	output, runErr := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("unsafe TTL command exit = %v, want usage 2; output:\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "--ttl") || strings.Contains(string(output), "battery:") {
		t.Fatalf("unsafe TTL refusal = %q, want usage reason without fabricated battery", output)
	}
	if _, err := os.Stat(runnerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe TTL started runner: %v", err)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe TTL created --out: %v", err)
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.RunAttemptIDs, before.RunAttemptIDs) {
		t.Fatalf("unsafe TTL minted RunAttempt: before=%v after=%v", before.RunAttemptIDs, after.RunAttemptIDs)
	}
}

func TestScopedProofHeartbeatAndSweepKeepReservedHolder(t *testing.T) {
	homePath, _, _, _ := scopedLandingCLIFixture(t) // publishes the real cutover marker
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const actor = "codex:scoped-proof-heartbeat"
	item := createClaimedProofWork(t, store, "scoped proof heartbeat", actor, now)
	reserved, err := store.ReserveProofResource(item.ID, work.ProofResourceReserveInput{
		Actor: actor, TTL: time.Minute, Workspace: "/synthetic/proof-repo",
		Command: "compound proof-check --out /synthetic/scoped-proof.json",
		Profile: syntheticAdmissionProfile(t, "/synthetic/proof-repo"),
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ExecutionBarrier == nil {
		t.Fatal("scoped reservation did not return its execution barrier")
	}
	t.Cleanup(func() { _ = reserved.ExecutionBarrier.Close() })

	var ticks, cancelled atomic.Int32
	stop, heartbeatResult := startProofCheckReservationHeartbeat(store, proofCheckAdmissionInput{
		HomePath: homePath, WorkID: item.ID, Actor: actor, TTL: time.Minute,
		Now: func() time.Time {
			ticks.Add(1)
			return time.Now().UTC()
		},
	}, reserved.Reservation, func() { cancelled.Add(1) })
	t.Cleanup(stop)
	deadline := time.Now().Add(2 * time.Second)
	for ticks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ticks.Load() == 0 {
		t.Fatal("scoped proof holder watcher did not tick")
	}
	if err := heartbeatResult(); err != nil || cancelled.Load() != 0 {
		t.Fatalf("scoped proof watcher lost its holder: err=%v cancelled=%d", err, cancelled.Load())
	}
	status, err := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if err != nil || !status.Held || status.Holder != reserved.Reservation.Holder {
		t.Fatalf("scoped proof holder after watcher tick: %+v %v", status, err)
	}
	probe := &work.DefaultSweepStaleProbe{Home: homePath, Now: time.Now}
	if held, err := probe.LeaseHeld(item.ID); err != nil || !held {
		t.Fatalf("sweep lost scoped proof reservation: held=%v err=%v", held, err)
	}
	attempt, err := runattempt.Open(homePath).Get(reserved.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := buildDelegateLivenessProbeWithRunner(homePath, newDelegateCLIFakeRunner())(attempt); err != nil || !live {
		t.Fatalf("CLI sweep probe lost scoped proof attempt: live=%v err=%v", live, err)
	}
}

func TestProofCheckCommandMinimumTTLHeartbeatPreventsConcurrentRunner(t *testing.T) {
	// This test gives the child three seconds to reach its runner. Keep the
	// paired runners isolated from unrelated package tests so that deadline
	// measures heartbeat admission rather than host scheduler contention.
	if os.Getenv(proofCheckMinimumTTLCLISubEnv) == "1" {
		if path := os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_ENTERED"); path != "" {
			if err := os.WriteFile(path, []byte("process-entered\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		role := os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_ROLE")
		parentLifetime, err := newTestSubprocessParentLifetime(2 * minimumProofCheckReservationTTL)
		if err != nil {
			t.Fatal(err)
		}
		proofCheckCanonicalRunner = func(_ context.Context, repoPath string, _ bool, source gitrepository.Snapshot, _ bool, _ bool, now time.Time) (proofCheckReport, error) {
			if role == "first" {
				if err := os.WriteFile(os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_FIRST_RUNNER"), []byte("runner-started\n"), 0o644); err != nil {
					return proofCheckReport{}, err
				}
				releasePath := os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_RELEASE")
				for {
					if err := parentLifetime.check(); err != nil {
						return proofCheckReport{}, err
					}
					if _, err := os.Stat(releasePath); err == nil {
						break
					} else if !errors.Is(err, os.ErrNotExist) {
						return proofCheckReport{}, err
					}
					time.Sleep(20 * time.Millisecond)
				}
			} else {
				if err := os.WriteFile(os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_SECOND_RUNNER"), []byte("runner-started\n"), 0o644); err != nil {
					return proofCheckReport{}, err
				}
			}
			report, err := injectedCanonicalProofCheckReport(repoPath, source.BaseSHA, now)
			report.SourceSnapshot = &source
			return report, err
		}
		cmdProofCheck([]string{
			"--repo", os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_REPO"),
			"--base", "HEAD",
			"--work", os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_WORK"),
			"--actor", os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_ACTOR"),
			"--out", os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_OUT"),
			"--ttl", minimumProofCheckReservationTTL.String(),
		})
		os.Exit(0)
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-minimum-ttl-cli-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	firstWork := createClaimedProofWork(t, store, "minimum TTL slow proof", "codex:min-ttl-first", now)
	secondWork := createClaimedProofWork(t, store, "minimum TTL competitor", "codex:min-ttl-second", now.Add(time.Second))
	repo := initCompoundCommandGitRepo(t)
	firstOut := filepath.Join(t.TempDir(), "first-proof.json")
	secondOut := filepath.Join(t.TempDir(), "second-proof.json")
	firstMarker := filepath.Join(t.TempDir(), "first-runner.log")
	firstEntered := filepath.Join(t.TempDir(), "first-entered")
	secondMarker := filepath.Join(t.TempDir(), "second-runner.log")
	releasePath := filepath.Join(t.TempDir(), "release")

	newCommand := func(role, workID, actor, outPath string) *exec.Cmd {
		command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandMinimumTTLHeartbeatPreventsConcurrentRunner$")
		command.Env = append(
			os.Environ(),
			proofCheckMinimumTTLCLISubEnv+"=1",
			"COMPOUND_HOME="+homePath,
			"COMPOUND_PROOF_MINIMUM_TTL_ROLE="+role,
			"COMPOUND_PROOF_MINIMUM_TTL_REPO="+repo,
			"COMPOUND_PROOF_MINIMUM_TTL_WORK="+workID,
			"COMPOUND_PROOF_MINIMUM_TTL_ACTOR="+actor,
			"COMPOUND_PROOF_MINIMUM_TTL_OUT="+outPath,
			"COMPOUND_PROOF_MINIMUM_TTL_FIRST_RUNNER="+firstMarker,
			"COMPOUND_PROOF_MINIMUM_TTL_SECOND_RUNNER="+secondMarker,
			"COMPOUND_PROOF_MINIMUM_TTL_RELEASE="+releasePath,
			testSubprocessParentPIDEnv+"="+fmt.Sprint(os.Getpid()),
		)
		return command
	}

	firstCommand := newCommand("first", firstWork.ID, "codex:min-ttl-first", firstOut)
	firstCommand.Env = append(firstCommand.Env, "COMPOUND_PROOF_MINIMUM_TTL_ENTERED="+firstEntered)
	var firstOutput bytes.Buffer
	firstCommand.Stdout = &firstOutput
	firstCommand.Stderr = &firstOutput
	if err := firstCommand.Start(); err != nil {
		t.Fatal(err)
	}
	// One Wait owns process reaping and drains both output streams. Read the
	// buffer only after firstDone closes, including failure and cleanup paths.
	firstDone := make(chan struct{})
	var firstErr error
	go func() {
		firstErr = firstCommand.Wait()
		close(firstDone)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, []byte("release\n"), 0o644)
		select {
		case <-firstDone:
		default:
			_ = firstCommand.Process.Kill()
		}
		<-firstDone
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-firstDone:
			t.Fatalf("minimum TTL first process exited before runner: %v; output:\n%s", firstErr, firstOutput.String())
		default:
		}
		if _, err := os.Stat(firstMarker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			_, enteredErr := os.Stat(firstEntered)
			_ = firstCommand.Process.Kill()
			<-firstDone
			t.Fatalf("minimum TTL first runner did not start; process_entered=%t entry_error=%v exit=%v; output:\n%s", enteredErr == nil, enteredErr, firstErr, firstOutput.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(minimumProofCheckReservationTTL + 500*time.Millisecond)
	secondOutput, secondErr := newCommand("second", secondWork.ID, "codex:min-ttl-second", secondOut).CombinedOutput()
	var secondExit *exec.ExitError
	if !errors.As(secondErr, &secondExit) || secondExit.ExitCode() != 2 {
		t.Fatalf("minimum TTL competitor exit = %v, want admission refusal; output:\n%s", secondErr, secondOutput)
	}
	if _, err := os.Stat(secondMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("minimum TTL competitor runner started: %v", err)
	}
	if err := os.WriteFile(releasePath, []byte("release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	<-firstDone
	if firstErr != nil {
		t.Fatalf("minimum TTL first proof failed: %v; output:\n%s", firstErr, firstOutput.String())
	}
}

func TestProofCheckMinimumTTLChildExitsWhenControllerDies(t *testing.T) {
	if os.Getenv(proofCheckMinimumTTLControllerSubEnv) == "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestProofCheckCommandMinimumTTLHeartbeatPreventsConcurrentRunner$")
		command.Env = append(
			os.Environ(),
			proofCheckMinimumTTLCLISubEnv+"=1",
			"COMPOUND_PROOF_MINIMUM_TTL_ROLE=first",
			testSubprocessParentPIDEnv+"="+fmt.Sprint(os.Getpid()),
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if err := publishTestPIDFile(os.Getenv("COMPOUND_PROOF_MINIMUM_TTL_CHILD_PID"), command.Process.Pid); err != nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatalf("minimum TTL child failed: %v", err)
		}
		return
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-minimum-ttl-parent-lifetime-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w := createClaimedProofWork(t, store, "minimum TTL child follows controller lifetime", "codex:min-ttl-parent-lifetime", now)
	repo := initCompoundCommandGitRepo(t)
	root := t.TempDir()
	outPath := filepath.Join(root, "proof.json")
	firstMarker := filepath.Join(root, "runner-started")
	secondMarker := filepath.Join(root, "second-runner-started")
	releasePath := filepath.Join(root, "release")
	childPIDPath := filepath.Join(root, "child.pid")

	controller := exec.Command(os.Args[0], "-test.run=^TestProofCheckMinimumTTLChildExitsWhenControllerDies$")
	controller.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	controller.Env = append(
		os.Environ(),
		proofCheckMinimumTTLControllerSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PROOF_MINIMUM_TTL_REPO="+repo,
		"COMPOUND_PROOF_MINIMUM_TTL_WORK="+w.ID,
		"COMPOUND_PROOF_MINIMUM_TTL_ACTOR=codex:min-ttl-parent-lifetime",
		"COMPOUND_PROOF_MINIMUM_TTL_OUT="+outPath,
		"COMPOUND_PROOF_MINIMUM_TTL_FIRST_RUNNER="+firstMarker,
		"COMPOUND_PROOF_MINIMUM_TTL_SECOND_RUNNER="+secondMarker,
		"COMPOUND_PROOF_MINIMUM_TTL_RELEASE="+releasePath,
		"COMPOUND_PROOF_MINIMUM_TTL_CHILD_PID="+childPIDPath,
	)
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	controllerReapStarted := false
	var controllerGroup *testProcessGroup
	controllerPGID := controller.Process.Pid
	controllerGroup, err = captureLiveTestProcessGroup(controllerPGID, controller.Process.Pid)
	if err != nil {
		killPinnedTestProcessGroupAndReap(t, controller, controllerPGID, time.Second)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupTestProcessGroup(t, controllerGroup, time.Second)
		if !controllerReapStarted {
			killAndReapTestCommand(t, controller, time.Second)
			controllerReapStarted = true
		}
	})

	childPID := waitForProofCheckGatePID(t, childPIDPath, 5*time.Second)
	waitForTestPath(t, firstMarker, 5*time.Second)
	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("get minimum TTL child process group: %v", err)
	}
	if childPGID != controllerPGID {
		t.Fatalf("minimum TTL child process group = %d, want controller group %d", childPGID, controllerPGID)
	}
	if _, _, err := controllerGroup.status(); err != nil {
		t.Fatalf("capture minimum TTL child group membership: %v", err)
	}

	killAndReapTestCommand(t, controller, time.Second)
	controllerReapStarted = true
	waitForTestProcessGroupQuiescence(t, controllerGroup, 2*time.Second)
}

func TestProofCheckAdmissionLeavesLightweightModesUnreserved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		dryRun    bool
		ciOnly    bool
		ciConsole bool
		want      bool
	}{
		{name: "full", want: true},
		{name: "dry-run", dryRun: true},
		{name: "ci", ciOnly: true},
		{name: "ci-console", ciConsole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := proofCheckNeedsAdmission(tc.dryRun, tc.ciOnly, tc.ciConsole); got != tc.want {
				t.Fatalf("proofCheckNeedsAdmission = %t, want %t", got, tc.want)
			}
		})
	}
}

func createClaimedProofWork(t *testing.T, store *work.Store, predicate, actor string, now time.Time) workEnvelope {
	t.Helper()
	w, err := store.Create(work.CreateInput{
		Owner:            "proof-admission",
		Actor:            actor,
		SuccessPredicate: predicate,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	w, err = store.Claim(w.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return workEnvelope{ID: w.ID}
}

type workEnvelope struct {
	ID string
}

func successfulProofCheckReport() proofCheckReport {
	return proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		ProfileKind:   work.ProofResourceProfileCanonicalFull,
		Status:        "ok",
		GeneratedAt:   "2026-07-24T12:00:03Z",
		Repo:          "/synthetic/repo-alpha",
		ProofRefs:     []string{"synthetic-proof-a", "synthetic-proof-b"},
		Steps: []proofCheckStep{
			{
				Name:     "synthetic-gate-a",
				Command:  []string{"go", "test", "./internal/synthetic-a"},
				ProofRef: "synthetic-proof-a",
				Status:   "ok",
			},
			{
				Name:     "synthetic-gate-b",
				Command:  []string{"go", "test", "./internal/synthetic-b"},
				ProofRef: "synthetic-proof-b",
				Status:   "ok",
			},
		},
	}
}

func injectedCanonicalProofCheckReport(repo, baseRef string, now time.Time) (proofCheckReport, error) {
	headRef := ""
	headCmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "HEAD^{commit}")
	if out, headErr := headCmd.Output(); headErr == nil {
		headRef = strings.TrimSpace(string(out))
	}
	steps, err := proofCheckSteps(baseRef, headRef)
	if err != nil {
		return proofCheckReport{}, err
	}
	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		ProfileKind:   work.ProofResourceProfileCanonicalFull,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339Nano),
		Repo:          repo,
		BaseRef:       baseRef,
		ProofRefs:     make([]string, 0, len(steps)),
		Steps:         make([]proofCheckStep, 0, len(steps)),
	}
	for _, step := range steps {
		step.Status = "ok"
		step.DurationMS = 1
		step.OutputTail = "injected canonical gate passed"
		report.ProofRefs = append(report.ProofRefs, step.ProofRef)
		report.Steps = append(report.Steps, step)
	}
	return report, nil
}

func syntheticAdmissionProfile(t *testing.T, repo string) work.ProofResourceProfile {
	t.Helper()
	profile, err := work.NewProofResourceProfile(work.ProofResourceProfileCanonicalFull, repo, "", []work.ProofResourceGateProfile{
		{
			Name:     "synthetic-gate-a",
			Command:  []string{"go", "test", "./internal/synthetic-a"},
			ProofRef: "synthetic-proof-a",
		},
		{
			Name:     "synthetic-gate-b",
			Command:  []string{"go", "test", "./internal/synthetic-b"},
			ProofRef: "synthetic-proof-b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// A certifying battery must stay runnable by any harness or model invoked as a capability.
// Agents run proof-check with stdout redirected to a log, and a scheduled merge queue has no
// controlling terminal at all, so requiring an interactive stdout would make the certifying
// path human-only and leave every automated lane on the non-certifying --dev profile. Custody
// of the --out report under the still-held lease is the proof; the stdout verdict line is a
// convenience for whoever is watching. This test fails if a stdout-is-a-terminal admission
// gate is reintroduced: it pins stdout to a pipe (never a char device) and requires the
// governed run to be admitted, execute its runner exactly once, and record its verification.
func TestProofCheckAdmissionAdmitsNonTerminalStdoutForAgentInvocation(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	realStdout := os.Stdout
	os.Stdout = writeEnd
	t.Cleanup(func() {
		os.Stdout = realStdout
		writeEnd.Close()
	})
	if info, statErr := os.Stdout.Stat(); statErr != nil {
		t.Fatal(statErr)
	} else if info.Mode()&os.ModeCharDevice != 0 {
		t.Fatal("stdout must not be a character device for this test to falsify a terminal gate")
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-admission-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "agent-invoked governed proof", "agent:claude-code", now)

	var runnerStarts atomic.Int32
	report, completed, err := runProofCheckWithAdmission(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    item.ID,
		Actor:     "agent:claude-code",
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-agent.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		TTL:       time.Minute,
		Now:       func() time.Time { return now.Add(time.Second) },
	}, func() (proofCheckReport, error) {
		runnerStarts.Add(1)
		return successfulProofCheckReport(), nil
	})
	if err != nil {
		t.Fatalf("governed proof with non-terminal stdout = %v, want admitted", err)
	}
	if runnerStarts.Load() != 1 {
		t.Fatalf("runner starts = %d, want exactly one", runnerStarts.Load())
	}
	if report.Status != "ok" {
		t.Fatalf("report status = %q, want ok", report.Status)
	}
	if completed.VerificationRef == "" {
		t.Fatal("completed verification ref is empty; custody is the proof and must be recorded")
	}
}

func TestProofCheckTimingWarningOwnedBeforeGovernedRedCompletion(t *testing.T) {
	homePath := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "proof-report.json")
	now := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-timing-custody-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "timing warning must precede custody", "codex:timing-custody", now)
	custody, err := proofCheckPrepareOutPath(outPath)
	if err != nil {
		t.Fatal(err)
	}

	originalRecorder := proofCheckTimingHistoryRecorder
	t.Cleanup(func() { proofCheckTimingHistoryRecorder = originalRecorder })
	const timingWarning = "proof-check timing history was corrupt and has been reset"
	proofCheckTimingHistoryRecorder = func(
		string,
		proofCheckReport,
		time.Duration,
		time.Time,
	) (string, error) {
		return timingWarning, nil
	}

	report, completed, err := runProofCheckWithAdmission(proofCheckAdmissionInput{
		HomePath:      homePath,
		WorkID:        item.ID,
		Actor:         "codex:timing-custody",
		Workspace:     "/synthetic/repo-alpha",
		Command:       "compound proof-check --json --out " + outPath,
		Profile:       syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		ReportPath:    outPath,
		ReportCustody: &custody,
		TTL:           time.Minute,
		Now:           func() time.Time { return now.Add(time.Second) },
	}, func() (proofCheckReport, error) {
		produced := successfulProofCheckReport()
		produced.GeneratedAt = now.Add(2 * time.Second).Format(time.RFC3339)
		produced.Status = "failed"
		produced.Steps[0].Status = "failed"
		produced.Steps[0].ExitCode = 1
		produced.ProofRefs = produced.ProofRefs[1:]
		proofCheckAppendTimingWarnings(
			&produced,
			homePath,
			nil,
			time.Minute,
			now.Add(time.Minute),
		)
		if err := proofCheckWriteReportFileWithCustody(outPath, produced, custody); err != nil {
			return proofCheckReport{}, err
		}
		return produced, nil
	})
	if err != nil {
		t.Fatalf("governed red completion: %v", err)
	}
	if report.Status != "failed" || completed.Result.ProducerExitCode != 1 {
		t.Fatalf("red completion = report:%+v result:%+v", report, completed.Result)
	}
	if !reflect.DeepEqual(report.Warnings, []string{timingWarning}) {
		t.Fatalf("in-memory warnings = %v, want %q", report.Warnings, timingWarning)
	}

	decode := func(label string, body []byte) proofCheckReport {
		t.Helper()
		var got proofCheckReport
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode %s: %v\n%s", label, err, body)
		}
		return got
	}
	outBody, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode("--out", outBody); !reflect.DeepEqual(got, report) {
		t.Fatalf("--out report diverged:\ngot:  %+v\nwant: %+v", got, report)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	artifactBody, err := artifacts.Get(completed.Result.ReportArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode("stored artifact", artifactBody); !reflect.DeepEqual(got, report) {
		t.Fatalf("stored artifact diverged:\ngot:  %+v\nwant: %+v", got, report)
	}

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realStdout := os.Stdout
	os.Stdout = writeEnd
	printJSON(report)
	_ = writeEnd.Close()
	os.Stdout = realStdout
	jsonStdout, err := io.ReadAll(readEnd)
	_ = readEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := decode("--json stdout", jsonStdout); !reflect.DeepEqual(got, report) {
		t.Fatalf("--json stdout diverged:\ngot:  %+v\nwant: %+v", got, report)
	}
}

func TestProofCheckCompletionRetainsHolderUntilBackgroundDescendantExits(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "proof-report.json")
	childDir := t.TempDir()
	childReady := filepath.Join(childDir, "ready")
	childRelease := filepath.Join(childDir, "release")
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "proof-orphan-descendant-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "background descendants must exit before proof completion", "codex:orphan-descendant", now)
	input := proofCheckAdmissionInput{
		HomePath:   homePath,
		WorkID:     item.ID,
		Actor:      "codex:orphan-descendant",
		Workspace:  "/synthetic/repo-alpha",
		Command:    "compound proof-check --out " + outPath,
		Profile:    syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		ReportPath: outPath,
		TTL:        time.Minute,
		Now:        func() time.Time { return now.Add(time.Second) },
	}

	var runnerStarts atomic.Int32
	var gate *exec.Cmd
	report, completed, firstErr := runProofCheckWithAdmissionContext(input, func(ctx context.Context) (proofCheckReport, error) {
		runnerStarts.Add(1)
		// The gate process exits cleanly after spawning a background descendant.
		// exec.Cmd has finished waiting, so later context cancellation cannot kill
		// that descendant; only the inherited barrier can prove it is still alive.
		// Keep the descendant alive until the retained-holder assertion below.
		// A fixed sleep can expire while admission/report persistence is slow.
		gate = proofCheckCommandContext(ctx, "sh", "-c", `( : > "$1"; while [ ! -e "$2" ]; do sleep 0.01; done ) &`, "descendant-fixture", childReady, childRelease)
		if err := gate.Start(); err != nil {
			return proofCheckReport{}, err
		}
		t.Cleanup(func() { _ = syscall.Kill(-gate.Process.Pid, syscall.SIGKILL) })
		if err := gate.Wait(); err != nil {
			return proofCheckReport{}, err
		}
		readyDeadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(childReady); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return proofCheckReport{}, err
			}
			if time.Now().After(readyDeadline) {
				return proofCheckReport{}, errors.New("background descendant did not become ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		report := successfulProofCheckReport()
		report.GeneratedAt = now.Add(2 * time.Second).Format(time.RFC3339)
		if err := proofCheckWriteReportFile(outPath, report); err != nil {
			return proofCheckReport{}, err
		}
		return report, nil
	})
	if firstErr == nil {
		t.Fatalf("proof completion released while descendant still held the execution barrier: completed=%+v report=%+v", completed, report)
	}
	var executionPending *landlease.ExecutionPendingError
	if !errors.As(firstErr, &executionPending) {
		t.Fatalf("first completion error = %v, want ExecutionPendingError", firstErr)
	}
	t.Logf("first completion retained holder: %v", firstErr)
	status, err := landlease.Status(homePath, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held {
		t.Fatalf("failed-closed completion did not retain the proof holder: %+v", status)
	}
	if gate == nil || gate.Process == nil {
		t.Fatal("runner did not start the gate process")
	}
	if err := os.WriteFile(childRelease, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending, err := landlease.ExecutionPending(homePath, status.Holder)
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background descendant did not release execution barrier before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	resumedReport, resumed, retryErr := runProofCheckWithAdmissionContext(input, func(context.Context) (proofCheckReport, error) {
		runnerStarts.Add(1)
		return proofCheckReport{}, errors.New("exact completion retry reran proof gates")
	})
	if retryErr != nil {
		t.Fatalf("exact retry after descendant exit did not complete: %v", retryErr)
	}
	if runnerStarts.Load() != 1 {
		t.Fatalf("runner starts = %d, want one across retained-result recovery", runnerStarts.Load())
	}
	if resumedReport.Status != "ok" || resumed.VerificationRef == "" || !resumed.ResourceReleased {
		t.Fatalf("resumed completion = report:%+v result:%+v", resumedReport, resumed)
	}
}

// proofCheckGoCacheFixture isolates the per-worktree Go cache root so a
// fixture never creates directories under the operator's real user cache.
// work.ProofGoCache resolves its root through os.UserCacheDir, so without
// HOME/XDG_CACHE_HOME set here the fixture writes into the real cache and
// waits on a filesystem it does not own. It returns the fixture home and the
// cache root inside it, and proves the isolation BEFORE any cache is made.
func proofCheckGoCacheFixture(t *testing.T) (home string, cacheBase string) {
	t.Helper()
	home = t.TempDir()
	// The fixture home becomes the process HOME, so make it inert for Go
	// telemetry the same way the package's other disposable homes are made
	// inert: a telemetry counter file written after the child exits would race
	// t.TempDir's RemoveAll and fail the parent with "directory not empty".
	disableGoTelemetryIn(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "local-cache"))
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("resolve isolated user cache dir: %v", err)
	}
	if !strings.HasPrefix(cacheBase, home+string(filepath.Separator)) {
		t.Fatalf("user cache dir %q is not inside the fixture home %q", cacheBase, home)
	}
	return home, cacheBase
}

func TestProofCheckGoCacheIsPerWorktree(t *testing.T) {
	_, cacheBase := proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	first, firstOK := work.ProofGoCache(t.TempDir())
	second, secondOK := work.ProofGoCache(t.TempDir())
	if !firstOK || !secondOK {
		t.Fatal("no cache chosen for a real worktree")
	}
	if first == second {
		t.Fatalf("two worktrees share one cache (%s); sibling lanes can still evict each other", first)
	}
	for _, cache := range []string{first, second} {
		if !strings.HasPrefix(cache, cacheBase+string(filepath.Separator)) {
			t.Fatalf("cache %s escaped the isolated fixture cache root %s", cache, cacheBase)
		}
	}
}

func TestProofCheckGoCacheIsStableForOneWorktree(t *testing.T) {
	_, cacheBase := proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	worktree := t.TempDir()
	first, firstOK := work.ProofGoCache(worktree)
	second, secondOK := work.ProofGoCache(worktree)
	if !firstOK || !secondOK {
		t.Fatal("no cache chosen for a real worktree")
	}
	if first != second {
		t.Fatalf("same worktree got two caches (%s vs %s); repeated batteries would start cold", first, second)
	}
	for _, cache := range []string{first, second} {
		if !strings.HasPrefix(cache, cacheBase+string(filepath.Separator)) {
			t.Fatalf("cache %s escaped the isolated fixture cache root %s", cache, cacheBase)
		}
	}
}

func TestProofCheckGoCacheLivesOutsideWorktree(t *testing.T) {
	_, cacheBase := proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	worktree := t.TempDir()
	cache, ok := work.ProofGoCache(worktree)
	if !ok {
		t.Fatal("no cache chosen for a real worktree")
	}
	if !strings.HasPrefix(cache, cacheBase+string(filepath.Separator)) {
		t.Fatalf("cache %s escaped the isolated fixture cache root %s", cache, cacheBase)
	}
	relative, err := filepath.Rel(worktree, cache)
	if err == nil && !strings.HasPrefix(relative, "..") {
		t.Fatalf("cache %s is inside worktree %s; the public redact gate will scan it", cache, worktree)
	}
}

func TestProofCheckGoCacheHonorsSharedOptOut(t *testing.T) {
	proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "1")
	if _, ok := work.ProofGoCache(t.TempDir()); ok {
		t.Fatal("shared-cache opt-out ignored")
	}
}

// Path selection is insufficient proof: this fails if runProofCheckStep stops injecting the
// per-worktree cache even while every proofCheckGoCache unit test remains green.
func TestRunProofCheckStepInjectsWorktreeGoCache(t *testing.T) {
	_, cacheBase := proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	repo := initCompoundCommandGitRepo(t)
	want, ok := work.ProofGoCache(repo)
	if !ok {
		t.Fatal("no cache chosen for a real worktree")
	}
	if !strings.HasPrefix(want, cacheBase+string(filepath.Separator)) {
		t.Fatalf("cache %s escaped the isolated fixture cache root %s", want, cacheBase)
	}

	binDir := t.TempDir()
	fakeGo := filepath.Join(binDir, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nprintf '%s\\n' \"$GOCACHE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOCACHE", "shared-parent-cache")

	got := runProofCheckStep(repo, proofCheckStep{
		Name:     "go-cache-wiring",
		Command:  []string{"go"},
		ProofRef: "go-cache-wiring",
	}, "")
	if got.Status != "ok" {
		t.Fatalf("fake Go gate failed: %+v", got)
	}
	if strings.TrimSpace(got.OutputTail) != want {
		t.Fatalf("Go gate saw GOCACHE %q, want per-worktree cache %q", got.OutputTail, want)
	}
}

func TestRunProofCheckBunToolsInjectsWorktreeGoCache(t *testing.T) {
	_, cacheBase := proofCheckGoCacheFixture(t)
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	repo := t.TempDir()
	toolsDir := filepath.Join(repo, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "cache.test.ts"), []byte("test fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, ok := work.ProofGoCache(repo)
	if !ok {
		t.Fatal("no cache chosen for a real worktree")
	}
	if !strings.HasPrefix(want, cacheBase+string(filepath.Separator)) {
		t.Fatalf("cache %s escaped the isolated fixture cache root %s", want, cacheBase)
	}

	binDir := t.TempDir()
	fakeBun := filepath.Join(binDir, "bun")
	if err := os.WriteFile(fakeBun, []byte("#!/bin/sh\nprintf '%s\\n' \"$GOCACHE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOCACHE", "shared-parent-cache")

	output, err := runProofCheckBunTools(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("Bun tool children saw GOCACHE %q, want per-worktree cache %q", got, want)
	}
}

func TestPrepareProofCheckRuntimeExecutableSurvivesTemporarySourceRemoval(t *testing.T) {
	source := filepath.Join(t.TempDir(), "compound")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf 'staged-candidate\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalResolver := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(logical string) (string, error) {
		if logical != "compound" {
			return logical, nil
		}
		return source, nil
	}
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	ctx, cleanup, err := work.PinProofRuntimeExecutable(context.Background(), proofCheckRuntimeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	staged, err := work.ProofRuntimeExecutable(ctx, "compound", proofCheckRuntimeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if staged == source {
		t.Fatal("temporary candidate executable was not staged")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(staged).CombinedOutput()
	if err != nil {
		t.Fatalf("staged candidate did not survive source removal: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "staged-candidate" {
		t.Fatalf("staged candidate output = %q", got)
	}
}

func TestRetainedV05ProofReportRestoresExactOwnedBytes(t *testing.T) {
	root := t.TempDir()
	if err := home.Init(root, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("{\"schema_version\":\"compound.proof_check_report.v0.5\",\"status\":\"ok\",\"steps\":[]}\n")
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "retained.json")
	custody, err := proofCheckCaptureOutPath(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := restoreOwnedProofCheckReport(root, ref, path, &custody)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || string(restored) != string(body) || report.SchemaVersion != "compound.proof_check_report.v0.5" {
		t.Fatalf("legacy retained report changed or refused: %+v %s %v", report, restored, err)
	}
}
