package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// The budget is a conserved allowance: preparation spends it, the admitted run
// inherits what is left, and it never goes negative.
func TestProofCheckExecutionBudgetConservesActiveOperation(t *testing.T) {
	t.Parallel()
	budget := work.NewProofExecutionBudget(10 * time.Minute)
	if got := budget.Remaining(); got != 10*time.Minute {
		t.Fatalf("fresh budget remaining = %v, want %v", got, 10*time.Minute)
	}
	budget.Charge(4 * time.Minute)
	if got := budget.Remaining(); got != 6*time.Minute {
		t.Fatalf("remaining after preparation = %v, want %v", got, 6*time.Minute)
	}
	budget.Charge(-time.Minute)
	if got := budget.Remaining(); got != 6*time.Minute {
		t.Fatalf("negative preparation changed the budget: %v", got)
	}
	budget.Charge(30 * time.Minute)
	if got := budget.Remaining(); got != 0 {
		t.Fatalf("overdrawn budget remaining = %v, want 0", got)
	}
	if work.NewProofExecutionBudget(0) != nil {
		t.Fatal("an omitted budget produced a budget object")
	}
}

// Queue waiting for the scarce proof resource is not active operation: it must
// not consume the budget. Pre-admission preparation is active operation: it
// must. The admitted run continues with the remainder, never a fresh budget.
func TestProofCheckAdmissionExecutionBudgetExcludesQueueWait(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-budget-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "first governed proof", "codex:alpha", now)
	second := createClaimedProofWork(t, store, "second governed proof", "codex:beta", now)

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
			Now:       time.Now,
		}, func() (proofCheckReport, error) {
			close(started)
			<-finish
			return realTimeProofCheckReport("/synthetic/repo-alpha"), nil
		})
		firstDone <- runErr
	}()
	<-started

	budget := work.NewProofExecutionBudget(10 * time.Minute)
	// Pre-admission preparation already spent four minutes of active operation.
	budget.Charge(4 * time.Minute)

	var secondRunnerStarts atomic.Int32
	var observedRemaining atomic.Int64
	var observedDeadline atomic.Int64
	var queuedRemaining atomic.Int64
	secondDone := make(chan error, 1)
	go func() {
		_, _, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
			HomePath:  homePath,
			WorkID:    second.ID,
			Actor:     "codex:beta",
			Workspace: "/synthetic/repo-beta",
			Command:   "compound proof-check --wait 30s --out /synthetic/report-beta.json",
			Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
			TTL:       time.Minute,
			Wait:      30 * time.Second,
			WaitProgress: proofBudgetWaitObserver(func(p []byte) (int, error) {
				queuedRemaining.CompareAndSwap(0, int64(budget.Remaining()))
				return len(p), nil
			}),
			Now:             time.Now,
			ExecutionBudget: budget,
		}, func(ctx context.Context) (proofCheckReport, error) {
			secondRunnerStarts.Add(1)
			observedRemaining.Store(int64(budget.Remaining()))
			if deadline, ok := ctx.Deadline(); ok {
				observedDeadline.Store(int64(deadline.UnixNano()))
			}
			return realTimeProofCheckReport("/synthetic/repo-beta"), nil
		})
		secondDone <- runErr
	}()

	// The queued battery must still be waiting for the resource.
	select {
	case err := <-secondDone:
		t.Fatalf("queued battery returned while the first held the resource: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if secondRunnerStarts.Load() != 0 {
		t.Fatal("queued battery started while the first held the resource")
	}

	close(finish)
	if err := <-firstDone; err != nil {
		t.Fatalf("first governed proof: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("queued governed proof: %v", err)
	}
	if secondRunnerStarts.Load() != 1 {
		t.Fatalf("queued runner started %d times, want 1", secondRunnerStarts.Load())
	}
	// The queue wait did not consume a single nanosecond of active budget, and
	// the four minutes of preparation were not forgiven.
	queued := time.Duration(queuedRemaining.Load())
	if queued <= 0 || queued > 6*time.Minute {
		t.Fatalf("missing or reset admission preparation allowance: %v", queued)
	}
	if got := time.Duration(observedRemaining.Load()); got != queued {
		t.Fatalf("queue consumed active allowance: before=%v after=%v", queued, got)
	}

	deadline := time.Unix(0, observedDeadline.Load())
	if observedDeadline.Load() == 0 {
		t.Fatal("the admitted run received no execution deadline")
	}
	if remaining := deadline.Sub(time.Now()); remaining < 5*time.Minute || remaining > 7*time.Minute {
		t.Fatalf("admitted run deadline leaves %v, want about %v", remaining, 6*time.Minute)
	}
}

// A run whose budget was already exhausted by preparation never starts a gate,
// never reports green, and closes as failed rather than verified.
func TestProofCheckAdmissionExecutionBudgetExpiryFailsWithoutVerification(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-budget-expiry-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "budget exhausted governed proof", "codex:alpha", now)

	before, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	budget := work.NewProofExecutionBudget(time.Minute)
	budget.Charge(time.Minute)

	var runnerStarts atomic.Int32
	report, completed, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:        homePath,
		WorkID:          item.ID,
		Actor:           "codex:alpha",
		Workspace:       "/synthetic/repo-alpha",
		Command:         "compound proof-check --out /synthetic/report-alpha.json",
		Profile:         syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		TTL:             time.Minute,
		Now:             time.Now,
		ExecutionBudget: budget,
	}, func(ctx context.Context) (proofCheckReport, error) {
		runnerStarts.Add(1)
		return realTimeProofCheckReport("/synthetic/repo-alpha"), nil
	})
	if runErr == nil {
		t.Fatal("an exhausted execution budget admitted the runner")
	}
	if runnerStarts.Load() != 0 {
		t.Fatalf("runner started %d times after the budget was exhausted", runnerStarts.Load())
	}
	if !strings.Contains(runErr.Error(), "execution budget") {
		t.Fatalf("failure reason %q does not name the execution budget", runErr)
	}
	if report.Status == "ok" {
		t.Fatal("an exhausted execution budget reported green")
	}
	if completed.VerificationRef != "" {
		t.Fatalf("an exhausted execution budget minted verification %q", completed.VerificationRef)
	}
	if completed.Result.Status == work.ProofResourceVerified {
		t.Fatalf("an exhausted execution budget recorded a verified proof: %+v", completed.Result)
	}
	status, statusErr := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if statusErr != nil || status.Held {
		t.Fatalf("expired preparation leaked scarce holder: %+v %v", status, statusErr)
	}
	after, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RunAttemptIDs) != len(before.RunAttemptIDs) {
		t.Fatalf("expired preparation minted attempt: before=%v after=%v", before.RunAttemptIDs, after.RunAttemptIDs)
	}

}

// A bounded deadline that ends the admitted run closes the real completion path
// as failed and non-verified, with the exact deadline reason as the cause.
func TestProofCheckAdmissionExecutionBudgetDeadlineFailsTheRun(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-budget-deadline-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "bounded deadline governed proof", "codex:alpha", now)
	out := filepath.Join(t.TempDir(), "partial.json")
	custody, err := proofCheckCaptureOutPath(out)
	if err != nil {
		t.Fatal(err)
	}
	budget := work.NewProofExecutionBudget(100 * time.Millisecond)
	report, completed, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath: homePath, WorkID: item.ID, Actor: "codex:alpha", Workspace: "/synthetic/repo-alpha",
		Command: "compound proof-check --out /synthetic/report-alpha.json",
		Profile: syntheticAdmissionProfile(t, "/synthetic/repo-alpha"), TTL: time.Minute,
		Now: time.Now, ExecutionBudget: budget, ReportPath: out, ReportCustody: &custody,
	}, func(ctx context.Context) (proofCheckReport, error) {
		<-ctx.Done()
		partial := realTimeProofCheckReport("/synthetic/repo-alpha")
		partial.Steps = partial.Steps[:1]
		return proofCheckPublishDeadlineReport(out, partial, &custody, work.ProofExecutionBudgetCause(ctx))
	})
	if runErr == nil || report.Status != "failed" || !completed.ResourceReleased || completed.VerificationRef != "" || completed.Result.Status == work.ProofResourceVerified {
		t.Fatalf("deadline did not finalize failed: report=%+v completed=%+v error=%v", report, completed, runErr)
	}
	ra, err := runattempt.Open(homePath).Get(completed.Result.RunAttemptID)
	if err != nil || ra.Status != runattempt.StatusFailed {
		t.Fatalf("original attempt not failed: %+v %v", ra, err)
	}
	status, err := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if err != nil || status.Held {
		t.Fatalf("deadline holder retained: %+v %v", status, err)
	}
	after, err := store.Get(item.ID)
	if err != nil || len(after.VerificationIDs) != 0 {
		t.Fatalf("deadline donated verification: %+v %v", after, err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var saved proofCheckReport
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "failed" || len(saved.Steps) != 1 {
		t.Fatalf("partial failure not retained: %+v", saved)
	}
}

func TestProofCheckAdmissionRejectsSuccessAfterExecutionDeadline(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "deadline-success-race", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "deadline race", "codex:alpha", now)
	_, completed, err := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath: homePath, WorkID: item.ID, Actor: "codex:alpha", Workspace: "/synthetic/repo-alpha",
		Command: "compound proof-check --fixture", Profile: syntheticAdmissionProfile(t, "/synthetic/repo-alpha"), TTL: time.Minute, Now: time.Now,
		ExecutionBudget: work.NewProofExecutionBudget(50 * time.Millisecond),
	}, func(ctx context.Context) (proofCheckReport, error) {
		<-ctx.Done()
		return realTimeProofCheckReport("/synthetic/repo-alpha"), nil
	})
	if err == nil || completed.VerificationRef != "" || !completed.ResourceReleased {
		t.Fatalf("expired runner was certified or leaked: %+v %v", completed, err)
	}
}

// Without the option the governed run keeps the unbounded behavior: the runner
// context carries no deadline at all.
func TestProofCheckAdmissionWithoutExecutionBudgetKeepsUnboundedRun(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-unbounded-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item := createClaimedProofWork(t, store, "unbounded governed proof", "codex:alpha", now)

	var deadlineSeen atomic.Bool
	report, completed, runErr := runProofCheckWithAdmissionContext(proofCheckAdmissionInput{
		HomePath:  homePath,
		WorkID:    item.ID,
		Actor:     "codex:alpha",
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
		Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
		TTL:       time.Minute,
		Now:       time.Now,
	}, func(ctx context.Context) (proofCheckReport, error) {
		if _, ok := ctx.Deadline(); ok {
			deadlineSeen.Store(true)
		}
		return realTimeProofCheckReport("/synthetic/repo-alpha"), nil
	})
	if runErr != nil {
		t.Fatalf("unbounded governed proof: %v", runErr)
	}
	if deadlineSeen.Load() {
		t.Fatal("an unbounded governed run received an execution deadline")
	}
	if report.Status != "ok" || completed.VerificationRef == "" {
		t.Fatalf("unbounded governed proof lost its ordinary success: %+v %+v", report.Status, completed)
	}
}

func TestProofCheckExecutionBudgetExpiryReportsConsumedLimit(t *testing.T) {
	b := work.NewProofExecutionBudget(20 * time.Millisecond)
	ctx, cancel, err := b.RunContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-ctx.Done()
	cause := work.ProofExecutionBudgetCause(ctx)
	if cause == nil || strings.Contains(cause.Error(), "after 0s") {
		t.Fatalf("deadline consumption is false: %v", cause)
	}
}

type proofBudgetWaitObserver func([]byte) (int, error)

func (f proofBudgetWaitObserver) Write(p []byte) (int, error) { return f(p) }
