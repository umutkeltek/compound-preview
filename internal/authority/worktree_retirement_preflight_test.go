package authority

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorktreeRetirementRequiresRecoveryInventoryCallback(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	_, err := BuildWorktreeRetirementPlan(context.Background(), retirementPlanRequest(fixture), WorktreeRetirementDependencies{Git: runner}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "preflight is required") || len(runner.calls) != 0 {
		t.Fatalf("missing planning callback: err=%v Git calls=%+v", err, runner.calls)
	}
	home, paths, report := authorizedRetirementFixture(t, fixture)
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.RecoveryInventoryPreflight = nil
	_, err = ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}, deps, time.Now())
	if err == nil || !strings.Contains(err.Error(), "preflight is required") || runner.mutationCount() != 0 || records != 0 {
		t.Fatalf("missing apply callback: err=%v mutations=%d records=%d", err, runner.mutationCount(), records)
	}
}

func TestWorktreeRetirementPreflightsBeforeNewAndResumedEffects(t *testing.T) {
	for _, phase := range []string{"new", WorktreeRetirementPhasePrepared, WorktreeRetirementPhaseWorktreeRemoved, WorktreeRetirementPhaseWorktreeRemovalStarted} {
		t.Run(phase, func(t *testing.T) {
			fixture := newRetirementGitFixture(t)
			home, paths, report := authorizedRetirementFixture(t, fixture)
			runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
			records := 0
			deps := successfulRetirementApplyDependencies(runner, &records)
			req := WorktreeRetirementApplyRequest{HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath, Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip)}
			if phase != "new" {
				crash := errors.New("fixture phase interruption")
				interrupt := func(current string) error {
					if current == phase {
						return crash
					}
					return nil
				}
				deps.AfterPhase, deps.AfterGitMutation = interrupt, interrupt
				if _, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) {
					t.Fatalf("phase interruption: %v", err)
				}
				deps.AfterPhase, deps.AfterGitMutation = nil, nil
			}
			before := runner.mutationCount()
			refusal := errors.New("custody owner rejects current recovery inventory")
			calls := 0
			deps.RecoveryInventoryPreflight = func(WorktreeRetirementPlan) error { calls++; return refusal }
			if _, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute)); !errors.Is(err, refusal) {
				t.Fatalf("preflight refusal: %v", err)
			}
			if calls != 1 || runner.mutationCount() != before || records != 0 {
				t.Fatalf("refusal calls=%d mutations=%d/%d records=%d", calls, runner.mutationCount(), before, records)
			}
		})
	}
}

func TestWorktreeRetirementSettledReplayDoesNotReadmitRecoveryCapacity(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	// Reproduce a pre-upgrade completed retirement whose correctly hashed
	// recovery file was already too large for capture when effects settled.
	file, err := os.Create(filepath.Join(t.TempDir(), "legacy-oversize.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	request := retirementPlanRequest(fixture)
	request.RecoveryRefs = []string{fmt.Sprintf("sha256-file:%x:%s", hash.Sum(nil), path)}
	plan, err := BuildWorktreeRetirementPlan(context.Background(), request, WorktreeRetirementDependencies{Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused}, RecoveryInventoryPreflight: testRetirementRecoveryPreflight}, time.Now())
	if err != nil || !plan.Eligible {
		t.Fatalf("legacy plan=%+v err=%v", plan, err)
	}
	home := t.TempDir()
	paths, err := RecordWorktreeRetirementPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review, err := LoadWorktreeRetirementReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	review.Decision, review.Rationale, review.ReviewedBy, review.ReviewedAt = "authorize_retirement", "synthetic historical retirement", "operator:test", time.Now().UTC().Format(time.RFC3339Nano)
	report, err := RecordWorktreeRetirementReview(home, review, paths.ReviewYAMLPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	req := WorktreeRetirementApplyRequest{HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath, Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip)}
	completed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := runner.mutationCount()
	for _, missing := range []bool{false, true} {
		deps.RecoveryInventoryPreflight = nil
		if !missing {
			deps.RecoveryInventoryPreflight = func(WorktreeRetirementPlan) error {
				t.Fatal("settled replay invoked new capacity admission")
				return errors.New("uncustodiable")
			}
		}
		replayed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !replayed.Replayed || replayed.Status != "completed" || replayed.ReceiptID != completed.ReceiptID || replayed.ResultDigest != completed.ResultDigest || runner.mutationCount() != before || records != 1 {
			t.Fatalf("settled replay=%+v err=%v mutations=%d/%d records=%d", replayed, err, runner.mutationCount(), before, records)
		}
	}
}
