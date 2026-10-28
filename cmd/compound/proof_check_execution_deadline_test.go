package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
)

// A bounded deadline must cancel the WHOLE existing process group, not just the
// direct child: a nested descendant that keeps the stdout pipe open dies with
// the group, and no later command launches afterwards.
func TestProofCheckExecutionDeadlineCancelsNestedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	descendantPIDPath := filepath.Join(dir, "descendant.pid")
	laterMarkerPath := filepath.Join(dir, "later-command.txt")

	// A synthetic compiler-like child: the outer shell holds the stdout pipe
	// open while a nested descendant of its own keeps running.
	script := fmt.Sprintf("sh -c 'echo $$ > %s; sleep 300' & wait", descendantPIDPath)

	budget := work.NewProofExecutionBudget(3 * time.Second)
	ctx, cancel, budgetErr := budget.RunContext(context.Background())
	defer cancel()
	if budgetErr != nil {
		t.Fatal(budgetErr)
	}

	type outcome struct {
		output []byte
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		output, err := proofCheckCommandContext(ctx, "sh", "-c", script).CombinedOutput()
		done <- outcome{output: output, err: err}
	}()

	descendantPID := waitForProofCheckTestPID(t, descendantPIDPath)
	select {
	case result := <-done:
		if result.err == nil {
			t.Fatalf("the stalled group exited cleanly at the deadline: %s", result.output)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the nested process group was not cancelled at the deadline")
	}
	if cause := work.ProofExecutionBudgetCause(ctx); cause == nil {
		t.Fatal("the group was not cancelled by the bounded execution deadline")
	}
	// The nested descendant must be gone: a pipe timeout alone would leave it
	// running, and only the whole-group stop kills it.
	waitForProofCheckTestProcessExit(t, descendantPID)

	// A later command must not launch once the deadline has ended the run.
	later := proofCheckCommandContext(ctx, "sh", "-c", "touch "+laterMarkerPath)
	if _, err := later.CombinedOutput(); err == nil {
		t.Fatal("a later command launched after the execution deadline")
	}
	if _, err := os.Stat(laterMarkerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a later command ran after the execution deadline: %v", err)
	}
}

// The bounded deadline ends the run truthfully: the partial failed report and
// the exact reason survive in the authoritative evidence, and the verdict is
// never green.
func TestProofCheckExecutionDeadlineRetainsPartialFailedReport(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "report.json")
	custody, err := proofCheckCaptureOutPath(outPath)
	if err != nil {
		t.Fatal(err)
	}

	// One gate passed, then the bounded deadline ended the run.
	partial := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		ProfileKind:   work.ProofResourceProfileCanonicalFull,
		Status:        "ok",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Repo:          "/synthetic/repo",
		Steps: []proofCheckStep{
			{Name: "synthetic-passed-gate", ProofRef: "synthetic-proof-a", Status: "ok"},
		},
		ProofRefs: []string{"synthetic-proof-a"},
	}

	budget := work.NewProofExecutionBudget(200 * time.Millisecond)
	ctx, cancel, budgetErr := budget.RunContext(context.Background())
	defer cancel()
	if budgetErr != nil {
		t.Fatal(budgetErr)
	}
	<-ctx.Done()
	cause := work.ProofExecutionBudgetCause(ctx)
	if cause == nil {
		t.Fatal("the bounded deadline produced no cause")
	}

	report, runErr := proofCheckPublishDeadlineReport(outPath, partial, &custody, cause)
	if runErr == nil {
		t.Fatal("the bounded deadline reported success")
	}
	if !strings.Contains(runErr.Error(), "execution budget") {
		t.Fatalf("failure reason %q does not name the execution budget", runErr)
	}
	if report.Status != "failed" {
		t.Fatalf("the bounded deadline reported status %q, want failed", report.Status)
	}
	if verdict := proofCheckVerdictLine(report, outPath); !strings.HasPrefix(verdict, "battery: FAILED") {
		t.Fatalf("verdict %q is not a failed battery", verdict)
	}

	// The authoritative evidence file keeps the partial result and the reason.
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var published proofCheckReport
	if err := json.Unmarshal(body, &published); err != nil {
		t.Fatal(err)
	}
	if published.Status != "failed" {
		t.Fatalf("published report status = %q, want failed", published.Status)
	}
	if len(published.Steps) != 1 || published.Steps[0].Name != "synthetic-passed-gate" {
		t.Fatalf("published report lost the partial result: %+v", published.Steps)
	}
	if joined := strings.Join(published.Warnings, "\n"); !strings.Contains(joined, "execution budget") {
		t.Fatalf("published report does not retain the deadline reason: %+v", published.Warnings)
	}
}

// A short synthetic budget cancels a stalled profile preparation before the
// reservation exists and before any remote transport is launched.
func TestProofCheckExecutionBudgetCancelsStalledProfilePreparation(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-preparation-budget-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	createClaimedProofWork(t, store, "stalled preparation governed proof", "codex:alpha", now)

	routePath := filepath.Join(t.TempDir(), "route.json")
	writeProofCheckRemoteRoute(t, routePath)

	source := gitrepository.Snapshot{
		RepositoryFingerprint: strings.Repeat("1", 64),
		BaseSHA:               strings.Repeat("2", 40),
		HeadSHA:               strings.Repeat("3", 40),
		TreeSHA:               strings.Repeat("4", 40),
		Clean:                 true,
	}
	profile, err := work.NewProofResourceProfileWithSource(
		work.ProofResourceProfileCanonicalFull,
		"/synthetic/repo",
		source,
		[]work.ProofResourceGateProfile{{
			Name:     "go-test",
			ProofRef: "go-test-count-1-general-release",
			Command:  []string{"go", "test", "./..."},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	budget := work.NewProofExecutionBudget(300 * time.Millisecond)
	ctx, cancel := budget.PreparationContext(context.Background())
	defer cancel()

	var commands atomic.Int32
	_, sealErr := work.SealRemoteProofProfile(ctx, profile, routePath, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		commands.Add(1)
		if name == "ssh" {
			t.Error("a remote transport was launched before the bounded deadline")
		}
		// A synthetic stalled child: it holds the pipe open and never returns.
		return proofCheckCommandContext(ctx, "sh", "-c", "sleep 300")
	})
	if sealErr == nil {
		t.Fatal("the stalled profile preparation was not cancelled")
	}
	if cause := work.ProofExecutionBudgetCause(ctx); cause == nil {
		t.Fatalf("the preparation did not end at the bounded deadline: %v", sealErr)
	}
	if commands.Load() == 0 {
		t.Fatal("the preparation never reached the stalled probe")
	}
	// The refusal happened before the scarce proof resource was reserved.
	status, statusErr := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if statusErr == nil && status.Held {
		t.Fatalf("the cancelled preparation reserved the proof resource: %+v", status)
	}
}

// writeProofCheckRemoteRoute writes an exact sealed remote route descriptor for
// the synthetic preparation fixture.
func writeProofCheckRemoteRoute(t *testing.T, path string) {
	t.Helper()
	config, err := json.Marshal(map[string]string{
		"VERIFY_ROOT":     "/synthetic/remote-proof",
		"GOTOOLCHAIN_PIN": "go1.26.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	route := work.RemoteProofRoute{
		SchemaVersion:  "compound.remote_go_test_route.v0.1",
		Host:           "proof.example",
		RunnerPath:     "/synthetic/runner.py",
		RunnerSHA256:   strings.Repeat("5", 64),
		GoVersion:      "go1.26.0",
		ConfigBase64:   base64.StdEncoding.EncodeToString(config),
		TimeoutSeconds: 600,
	}
	body, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
