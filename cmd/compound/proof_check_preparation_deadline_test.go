package main

import (
	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProofCheckPreparationCancellationBeforeAdmission(t *testing.T) {
	const helper = "COMPOUND_PREPARATION_DEADLINE_HELPER"
	if os.Getenv(helper) == "1" {
		cmdProofCheck([]string{"--repo", os.Getenv("PROOF_PREP_REPO"), "--base", "HEAD", "--work", os.Getenv("PROOF_PREP_WORK"), "--actor", "codex:prep-deadline", "--out", os.Getenv("PROOF_PREP_OUT"), "--execution-timeout", os.Getenv("PROOF_PREP_BUDGET")})
		os.Exit(0)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"deadline", "signal"} {
		t.Run(mode, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Now().UTC()
			if err := home.Init(homePath, "preparation-cancellation", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			item := createClaimedProofWork(t, store, "preparation cancellation", "codex:prep-deadline", now)
			before, err := store.Get(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			repo := initCompoundCommandGitRepo(t)
			bin := t.TempDir()
			pidPath := filepath.Join(t.TempDir(), "child.pid")
			// The actual committed-inventory Git command stalls with a nested child.
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"ls-tree\" ]; then\n sleep 300 &\n child=$!\n printf '%%s\\n' \"$child\" > \"$PROOF_PREP_PID\"\n wait \"$child\"\n exit $?\nfi\nexec '%s' \"$@\"\n", strings.ReplaceAll(realGit, "'", "'\\''"))
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			budget := "4s"
			if mode == "signal" {
				budget = "30s"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckPreparationCancellationBeforeAdmission$")
			cmd.Env = append(os.Environ(), helper+"=1", "COMPOUND_HOME="+homePath, "PROOF_PREP_REPO="+repo, "PROOF_PREP_WORK="+item.ID, "PROOF_PREP_OUT="+filepath.Join(t.TempDir(), "proof.json"), "PROOF_PREP_PID="+pidPath, "PROOF_PREP_BUDGET="+budget, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout = log
			cmd.Stderr = log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			pid := waitForProofCheckTestPID(t, pidPath)
			if mode == "signal" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled preparation exited green")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("preparation did not stop")
			}
			body, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			output := string(body)
			if mode == "deadline" {
				for _, want := range []string{"proof-check execution budget of " + budget, "pre-admission preparation", "queue waiting for the proof resource is excluded"} {
					if !strings.Contains(output, want) {
						t.Fatalf("deadline output missing %q: %s", want, output)
					}
				}
			} else if !strings.Contains(output, "terminated signal received") || strings.Contains(output, "proof-check execution budget") {
				t.Fatalf("signal incorrectly reported as deadline: %s", output)
			}

			waitForProofCheckTestProcessExit(t, pid)
			status, err := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
			if err != nil || status.Held {
				t.Fatalf("preparation reserved resource: %+v %v", status, err)
			}
			after, err := store.Get(item.ID)
			if err != nil || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) {
				t.Fatalf("preparation minted attempt: before=%+v after=%+v err=%v", before.RunAttemptIDs, after.RunAttemptIDs, err)
			}
			if t.Failed() {
				body, _ := os.ReadFile(log.Name())
				t.Log(string(body))
			}
		})
	}
}

func TestProofCheckDeadlineMissingOutRefusesBeforeRunner(t *testing.T) {
	const helper = "COMPOUND_DEADLINE_MISSING_OUT_HELPER"
	if os.Getenv(helper) == "1" {
		cmdProofCheck([]string{"--repo", os.Getenv("PROOF_PREP_REPO"), "--base", "HEAD", "--work", "work_synthetic", "--actor", "codex:prep-deadline", "--execution-timeout", "1s"})
		os.Exit(0)
	}
	homePath := t.TempDir()
	if err := home.Init(homePath, "deadline-missing-out", time.Now()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProofCheckDeadlineMissingOutRefusesBeforeRunner$")
	cmd.Env = append(os.Environ(), helper+"=1", "COMPOUND_HOME="+homePath, "PROOF_PREP_REPO="+t.TempDir())
	body, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(body), "requires --work ID, --actor ACTOR, and --out FILE") {
		t.Fatalf("omitted output refusal: %v %s", err, body)
	}
	status, err := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if err != nil || status.Held {
		t.Fatalf("missing output admitted resource: %+v %v", status, err)
	}
}

func TestProofCheckExecutionBudgetPreparationUsesRemainingAllowance(t *testing.T) {
	b := work.NewProofExecutionBudget(time.Minute)
	b.Charge(45 * time.Second)
	started := time.Now()
	ctx, cancel := b.PreparationContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Sub(started) > 16*time.Second || deadline.Sub(started) < 14*time.Second {
		t.Fatalf("preparation reset consumed allowance: deadline=%v", deadline)
	}
}

func TestProofCheckDeadlineReportPreservesExactOutputCustody(t *testing.T) {
	for _, stage := range []string{"before-publication", "after-own-publication", "own-publication"} {
		t.Run(stage, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "proof.json")
			if err := os.WriteFile(out, []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			}
			custody, err := proofCheckCaptureOutPath(out)
			if err != nil {
				t.Fatal(err)
			}
			defer custody.close()
			report := realTimeProofCheckReport("/synthetic/repo")
			if stage != "before-publication" {
				if err := proofCheckWriteReportFileWithCustody(out, report, custody); err != nil {
					t.Fatal(err)
				}
			}
			prior, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var foreign os.FileInfo
			if stage != "own-publication" {
				replacement := out + ".foreign"
				if err := os.WriteFile(replacement, prior, 0600); err != nil {
					t.Fatal(err)
				}
				foreign, err = os.Stat(replacement)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, out); err != nil {
					t.Fatal(err)
				}
			}
			cause := fmt.Errorf("execution budget expired")
			failed, err := proofCheckPublishDeadlineReport(out, report, &custody, cause)
			if stage == "own-publication" {
				if err != cause || failed.Status != "failed" {
					t.Fatalf("own publication cannot be rewritten: %+v %v", failed, err)
				}
				body, _ := os.ReadFile(out)
				if !strings.Contains(string(body), "\"status\": \"failed\"") {
					t.Fatalf("failed report missing: %s", body)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "outside this proof") {
					t.Fatalf("foreign same-byte replacement accepted: %v", err)
				}
				actual, statErr := os.Stat(out)
				body, readErr := os.ReadFile(out)
				if statErr != nil || readErr != nil || !os.SameFile(actual, foreign) || string(body) != string(prior) {
					t.Fatalf("foreign output modified: %v %v", statErr, readErr)
				}
			}
		})
	}
}

func TestProofCheckExecutionPlanRetainsCancellation(t *testing.T) {
	repo := initCompoundCommandGitRepo(t)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		ctx = gitrepository.ContextWithCommandFactory(ctx, proofCheckCommandContext)
		if stored {
			ctx = work.ContextWithCanonicalProofPlan(ctx, work.CanonicalProofPlan{Repo: repo, Source: source})
		}
		cancel()
		_, err := work.CanonicalProofPlanForExecution(ctx, repo, source, proofCheckSteps)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("execution plan lost cancellation (stored=%v): %v", stored, err)
		}
	}
}
func TestProofCheckExecutionDeadlineStopsBeforeNextGate(t *testing.T) {
	// Use a stable synthetic runtime to isolate the gate execution deadline.
	originalRuntimeExecutable := proofCheckRuntimeExecutable
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalRuntimeExecutable })
	proofCheckRuntimeExecutable = func(logical string) (string, error) {
		if logical == "compound" {
			return "/bin/sh", nil
		}
		return originalRuntimeExecutable(logical)
	}
	repo := initCompoundCommandGitRepo(t)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	plan := work.CanonicalProofPlan{Repo: repo, Source: source, Steps: []work.ProofCheckStep{
		{Name: "stalled-synthetic-gate", Command: []string{"sh", "-c", "sleep 300"}, ProofRef: "synthetic-first"},
		{Name: "must-not-start", Command: []string{"sh", "-c", "exit 0"}, ProofRef: "synthetic-second"},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = gitrepository.ContextWithCommandFactory(ctx, proofCheckCommandContext)
	ctx = work.ContextWithCanonicalProofPlan(ctx, plan)
	report, err := runProofCheckWithRangeContext(ctx, repo, false, source.BaseSHA, source.HeadSHA, &source, false, false, time.Now())
	if err == nil || report.Status != "failed" || len(report.Steps) != 1 || report.Steps[0].Name != "stalled-synthetic-gate" {
		t.Fatalf("cancelled loop entered later setup or lost partial report: %+v %v", report, err)
	}
}

func TestProofCheckDeadlineInitializesEmptyFailedReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "partial.json")
	custody, err := proofCheckCaptureOutPath(out)
	if err != nil {
		t.Fatal(err)
	}
	cause := fmt.Errorf("execution budget expired during admitted setup")
	report, err := proofCheckPublishDeadlineReport(out, proofCheckReport{}, &custody, cause)
	if err != cause || report.SchemaVersion != proofCheckSchemaVersion || report.ProfileKind != work.ProofResourceProfileCanonicalFull || report.Status != "failed" || len(report.Steps) != 0 || len(report.ProofRefs) != 0 {
		t.Fatalf("empty setup failure lacks truthful report metadata: %+v %v", report, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, report.GeneratedAt); err != nil {
		t.Fatal(err)
	}
}
