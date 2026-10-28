package authority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

func TestApplyWorktreeRetirementRemovesNonForceThenCASDeletesAndIsIdempotent(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	recorded := 0
	deps := WorktreeRetirementApplyDependencies{RecoveryInventoryPreflight: testRetirementRecoveryPreflight,
		Git: runner, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused},
		Evaluate: func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
			return policy.Evaluate(action, policy.DefaultConfig(), now), nil
		},
		RecordOutcome: func(outcome WorktreeRetirementActionOutcome, _ time.Time) (WorktreeRetirementActionProof, error) {
			recorded++
			if outcome.Outcome != "completed" || !outcome.Changed || outcome.Action.Capability != "source-control.worktree-retire" || outcome.Action.Action != "retire_redundant_git_surface" {
				t.Fatalf("action outcome = %+v", outcome)
			}
			return completeRetirementActionProof(outcome, "sha256:"+strings.Repeat("e", 64)), nil
		},
	}
	request := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	result, err := ApplyWorktreeRetirement(context.Background(), request, deps, time.Date(2026, 7, 15, 7, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !result.WorktreeRemoved || !result.RefDeleted || result.ReceiptID != "rec_retirement" || result.TransitionID != "tr_retirement" {
		t.Fatalf("apply result = %+v", result)
	}
	if _, err := os.Stat(fixture.worktree); !os.IsNotExist(err) {
		t.Fatalf("retired worktree still exists: %v", err)
	}
	if retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatal("candidate ref still exists")
	}
	if got := retirementGitOutput(t, fixture.repo, "rev-parse", "refs/heads/main"); got != fixture.mainTip {
		t.Fatalf("authority ref moved to %s", got)
	}
	if got := retirementGitOutput(t, fixture.repo, "rev-parse", "refs/remotes/origin/main"); got != fixture.mainTip {
		t.Fatalf("publication ref moved to %s", got)
	}
	assertRetirementMutationArgv(t, runner.calls, fixture)
	mutations := runner.mutationCount()

	replayed, err := ApplyWorktreeRetirement(context.Background(), request, deps, time.Date(2026, 7, 15, 7, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Status != "completed" || runner.mutationCount() != mutations || recorded != 1 {
		t.Fatalf("idempotent replay=%+v mutations=%d/%d recorded=%d", replayed, runner.mutationCount(), mutations, recorded)
	}
}

func TestApplyWorktreeRetirementResumesAfterPersistedWorktreeRemoval(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	crash := errors.New("injected crash after durable worktree removal")
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.AfterPhase = func(phase string) error {
		if phase == WorktreeRetirementPhaseWorktreeRemoved {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	if result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) || result.Phase != WorktreeRetirementPhaseWorktreeRemoved {
		t.Fatalf("crash result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(fixture.worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree removal not durable: %v", err)
	}
	if !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatal("ref deleted before durable phase boundary")
	}
	if records != 0 {
		t.Fatalf("crash incorrectly recorded terminal action: %d", records)
	}

	deps.AfterPhase = nil
	resumed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || !resumed.WorktreeRemoved || !resumed.RefDeleted || records != 1 {
		t.Fatalf("resumed result=%+v records=%d", resumed, records)
	}
	if runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 1 {
		t.Fatalf("resume repeated destructive phase: calls=%+v", runner.calls)
	}
}

func TestApplyWorktreeRetirementPersistsStartedPhaseBeforeWorktreeMutationAndRecoversPostSuccessCrash(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	plan, err := LoadWorktreeRetirementPlan(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(home, "reviews", WorktreeRetirementReviewArea, plan.ID+".retirement.pending.json")
	crash := errors.New("injected crash after worktree removal before completion persistence")
	runner := &phaseAssertingRetirementGit{delegate: ExecWorktreeRetirementGit{}, pendingPath: pendingPath, t: t}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.AfterGitMutation = func(startedPhase string) error {
		if startedPhase == WorktreeRetirementPhaseWorktreeRemovalStarted {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, crash) || result.Phase != WorktreeRetirementPhaseWorktreeRemovalStarted {
		t.Fatalf("post-success crash result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(fixture.worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree mutation did not happen before crash: %v", err)
	}
	if !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatal("ref deleted before worktree phase recovery")
	}
	pending, exists, err := loadWorktreeRetirementPending(pendingPath)
	if err != nil || !exists || pending.Phase != WorktreeRetirementPhaseWorktreeRemovalStarted {
		t.Fatalf("persisted pending=%+v exists=%v err=%v", pending, exists, err)
	}
	if runner.worktreeRemoveCount != 1 || runner.refDeleteCount != 0 || records != 0 {
		t.Fatalf("crash calls remove=%d delete=%d records=%d", runner.worktreeRemoveCount, runner.refDeleteCount, records)
	}

	deps.AfterGitMutation = nil
	resumed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || runner.worktreeRemoveCount != 1 || runner.refDeleteCount != 1 || records != 1 {
		t.Fatalf("resume result=%+v remove=%d delete=%d records=%d", resumed, runner.worktreeRemoveCount, runner.refDeleteCount, records)
	}
}

func TestApplyWorktreeRetirementPersistsStartedPhaseBeforeRefMutationAndRecoversPostSuccessCrash(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	plan, err := LoadWorktreeRetirementPlan(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(home, "reviews", WorktreeRetirementReviewArea, plan.ID+".retirement.pending.json")
	crash := errors.New("injected crash after ref deletion before completion persistence")
	runner := &phaseAssertingRetirementGit{delegate: ExecWorktreeRetirementGit{}, pendingPath: pendingPath, t: t}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.AfterGitMutation = func(startedPhase string) error {
		if startedPhase == WorktreeRetirementPhaseRefDeletionStarted {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, crash) || result.Phase != WorktreeRetirementPhaseRefDeletionStarted {
		t.Fatalf("post-success crash result=%+v err=%v", result, err)
	}
	if retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatal("ref mutation did not happen before crash")
	}
	pending, exists, err := loadWorktreeRetirementPending(pendingPath)
	if err != nil || !exists || pending.Phase != WorktreeRetirementPhaseRefDeletionStarted {
		t.Fatalf("persisted pending=%+v exists=%v err=%v", pending, exists, err)
	}
	if runner.worktreeRemoveCount != 1 || runner.refDeleteCount != 1 || records != 0 {
		t.Fatalf("crash calls remove=%d delete=%d records=%d", runner.worktreeRemoveCount, runner.refDeleteCount, records)
	}

	deps.AfterGitMutation = nil
	resumed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || runner.worktreeRemoveCount != 1 || runner.refDeleteCount != 1 || records != 1 {
		t.Fatalf("resume result=%+v remove=%d delete=%d records=%d", resumed, runner.worktreeRemoveCount, runner.refDeleteCount, records)
	}
}

func TestApplyWorktreeRetirementResumesPreparedIntentWithoutGitMutation(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	crash := errors.New("injected crash after durable prepared intent")
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.AfterPhase = func(phase string) error {
		if phase == WorktreeRetirementPhasePrepared {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	if result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) || result.Phase != WorktreeRetirementPhasePrepared {
		t.Fatalf("prepared crash result=%+v err=%v", result, err)
	}
	if runner.mutationCount() != 0 || !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatalf("prepared crash mutated Git: %+v", runner.calls)
	}
	deps.AfterPhase = nil
	resumed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || records != 1 || runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 1 {
		t.Fatalf("prepared resume result=%+v calls=%+v records=%d", resumed, runner.calls, records)
	}
}

func TestApplyWorktreeRetirementPendingOnlyResumeReevaluatesCurrentPolicyBeforeMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, outcome := range []string{"hold", "deny"} {
		t.Run(outcome, func(t *testing.T) {
			fixture := newRetirementGitFixture(t)
			home, paths, report := authorizedRetirementFixture(t, fixture)
			crash := errors.New("injected crash with prepared pending transaction")
			runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
			records := 0
			deps := successfulRetirementApplyDependencies(runner, &records)
			deps.AfterPhase = func(phase string) error {
				if phase == WorktreeRetirementPhasePrepared {
					return crash
				}
				return nil
			}
			req := WorktreeRetirementApplyRequest{
				HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
				Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
			}
			if result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) || result.Phase != WorktreeRetirementPhasePrepared {
				t.Fatalf("prepared crash result=%+v err=%v", result, err)
			}
			deps.AfterPhase = nil
			deps.Evaluate = func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
				decision := policy.Evaluate(action, policy.DefaultConfig(), now)
				decision.Outcome = outcome
				decision.Obligations = nil
				return decision, nil
			}
			_, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "policy") {
				t.Fatalf("resume policy error=%v", err)
			}
			if runner.mutationCount() != 0 || records != 0 || !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
				t.Fatalf("policy refusal mutated state calls=%+v records=%d", runner.calls, records)
			}
		})
	}
}

func TestApplyWorktreeRetirementResumeRecordsCompletedTruthWithoutReauthorizingFinishedGitMutation(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	plan, err := LoadWorktreeRetirementPlan(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	crash := errors.New("crash after ref deletion before completed phase persistence")
	deps.AfterGitMutation = func(phase string) error {
		if phase == WorktreeRetirementPhaseRefDeletionStarted {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	first, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, crash) || first.Phase != WorktreeRetirementPhaseRefDeletionStarted {
		t.Fatalf("post-ref crash result=%+v err=%v", first, err)
	}
	pendingPath := filepath.Join(home, "reviews", WorktreeRetirementReviewArea, plan.ID+".retirement.pending.json")
	pending, exists, err := loadWorktreeRetirementPending(pendingPath)
	if err != nil || !exists {
		t.Fatalf("pending exists=%v err=%v", exists, err)
	}
	authorizedPolicyID := pending.Decision.ID
	mutations := runner.mutationCount()
	policyCalls := 0
	deps.AfterGitMutation = nil
	deps.Evaluate = func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
		policyCalls++
		decision := policy.Evaluate(action, policy.DefaultConfig(), now)
		decision.Outcome = "hold"
		decision.Obligations = nil
		return decision, nil
	}
	result, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.PolicyDecisionID != authorizedPolicyID || records != 1 {
		t.Fatalf("truth result=%+v authorized_policy=%s records=%d", result, authorizedPolicyID, records)
	}
	if policyCalls != 0 || runner.mutationCount() != mutations {
		t.Fatalf("truth recording reevaluated/mutated policy_calls=%d mutations=%d/%d", policyCalls, runner.mutationCount(), mutations)
	}
}

func TestApplyWorktreeRetirementRecordsPolicyDeniedPartialMutationAndCanRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	crash := errors.New("crash with prepared pending transaction")
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.AfterPhase = func(phase string) error {
		if phase == WorktreeRetirementPhasePrepared {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	if _, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) {
		t.Fatalf("prepared crash error=%v", err)
	}
	deps.AfterPhase = nil
	policyCalls := 0
	allowedPolicyID := ""
	deps.Evaluate = func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
		policyCalls++
		decision := policy.Evaluate(action, policy.DefaultConfig(), now)
		if policyCalls == 1 {
			allowedPolicyID = decision.ID
			return decision, nil
		}
		decision.Outcome = "deny"
		decision.Obligations = nil
		return decision, nil
	}
	partial, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "policy") {
		t.Fatalf("partial policy result=%+v err=%v", partial, err)
	}
	if partial.Status != "partial" || !partial.WorktreeRemoved || partial.RefDeleted || partial.PolicyDecisionID != allowedPolicyID || partial.ReceiptID == "" || partial.TransitionID == "" {
		t.Fatalf("partial result=%+v allowed_policy=%s", partial, allowedPolicyID)
	}
	if runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 0 || records != 1 || !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatalf("partial calls=%+v records=%d", runner.calls, records)
	}

	req.OwnerProof.WorkDigest = partial.WorkDigest
	deps.Evaluate = func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
		return policy.Evaluate(action, policy.DefaultConfig(), now), nil
	}
	completed, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 1 || records != 2 {
		t.Fatalf("retry result=%+v calls=%+v records=%d", completed, runner.calls, records)
	}
}

func TestApplyWorktreeRetirementRetriesTransientWorktreeRemovalWithSelfCausedOwnerDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	injected := errors.New("transient worktree remove failure")
	runner := &failOnceWorktreeRemoveGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	first, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, injected) || first.Status != "failed" || first.WorkDigest == "" || first.WorktreeRemoved {
		t.Fatalf("first removal result=%+v err=%v", first, err)
	}
	if _, err := os.Stat(fixture.worktree); err != nil {
		t.Fatalf("failed removal changed worktree: %v", err)
	}
	req.OwnerProof.WorkDigest = first.WorkDigest
	second, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "completed" || runner.removeCalls != 2 || runner.successfulRemoveCalls != 1 || runner.refDeleteCalls != 1 || records != 2 {
		t.Fatalf("retry result=%+v remove=%d successful=%d delete=%d records=%d", second, runner.removeCalls, runner.successfulRemoveCalls, runner.refDeleteCalls, records)
	}
}

func TestApplyWorktreeRetirementCASDriftLeavesRefAndRecordsPartialOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	runner := &casDriftRetirementGit{delegate: ExecWorktreeRetirementGit{}, ref: fixture.candidateRef, replacement: fixture.mainTip}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	result, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}, deps, time.Now())
	if err == nil || result.Status != "partial" || !result.WorktreeRemoved || result.RefDeleted {
		t.Fatalf("CAS drift result=%+v err=%v", result, err)
	}
	if got := retirementGitOutput(t, fixture.repo, "rev-parse", fixture.candidateRef); got != fixture.mainTip {
		t.Fatalf("CAS drift deleted or rewrote ref to %s", got)
	}
	if records != 1 {
		t.Fatalf("partial outcome records=%d", records)
	}
}

func TestApplyWorktreeRetirementRetriesPartialOnlyWhileExactRefRemains(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	injected := errors.New("transient update-ref failure")
	runner := &failOnceRefDeleteGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	first, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, injected) || first.Status != "partial" || !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
		t.Fatalf("partial first=%+v err=%v", first, err)
	}
	req.OwnerProof.WorkDigest = first.WorkDigest
	second, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "completed" || retirementGitRefExists(fixture.repo, fixture.candidateRef) || runner.removeCalls != 1 || runner.deleteCalls != 2 || records != 2 {
		t.Fatalf("partial retry=%+v remove=%d delete=%d records=%d", second, runner.removeCalls, runner.deleteCalls, records)
	}
}

func TestApplyWorktreeRetirementRefusesPolicyAndStateDriftBeforeMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name    string
		prepare func(*WorktreeRetirementApplyDependencies)
		want    string
	}{
		{name: "liveness drift", prepare: func(d *WorktreeRetirementApplyDependencies) {
			d.Liveness = staticRetirementLiveness{state: WorktreeLivenessActive}
		}, want: "drift"},
		{name: "policy hold", prepare: func(d *WorktreeRetirementApplyDependencies) {
			d.Evaluate = func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
				decision := policy.Evaluate(action, policy.DefaultConfig(), now)
				decision.Outcome = "hold"
				decision.Obligations = nil
				return decision, nil
			}
		}, want: "policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRetirementGitFixture(t)
			home, paths, report := authorizedRetirementFixture(t, fixture)
			runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
			records := 0
			deps := successfulRetirementApplyDependencies(runner, &records)
			tc.prepare(&deps)
			_, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
				HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
				Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
			}, deps, time.Now())
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if runner.mutationCount() != 0 || records != 0 || !retirementGitRefExists(fixture.repo, fixture.candidateRef) {
				t.Fatalf("refusal mutated state: calls=%+v records=%d", runner.calls, records)
			}
		})
	}
}

func TestApplyWorktreeRetirementResumeBindsExactSelfCausedOwnerDigest(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	injected := errors.New("transient update-ref failure")
	runner := &failOnceRefDeleteGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
	records := 0
	firstDigest := "sha256:" + strings.Repeat("e", 64)
	secondDigest := "sha256:" + strings.Repeat("f", 64)
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.RecordOutcome = func(outcome WorktreeRetirementActionOutcome, _ time.Time) (WorktreeRetirementActionProof, error) {
		records++
		digest := firstDigest
		if records == 2 {
			digest = secondDigest
		}
		return completeRetirementActionProof(outcome, digest), nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	first, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now())
	if !errors.Is(err, injected) || first.Status != "partial" || first.WorkDigest != firstDigest || first.LeaseWorkSHA256 != req.OwnerProof.LeaseWorkSHA256 {
		t.Fatalf("partial result=%+v err=%v", first, err)
	}
	req.OwnerProof.WorkDigest = firstDigest
	second, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "completed" || second.WorkDigest != secondDigest || runner.removeCalls != 1 || runner.deleteCalls != 2 || records != 2 {
		t.Fatalf("resumed result=%+v remove=%d delete=%d records=%d", second, runner.removeCalls, runner.deleteCalls, records)
	}
}

func TestApplyWorktreeRetirementResumeRefusesUnboundOwnerDigestOrLease(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*WorktreeRetirementOwnerProof)
		want   string
	}{
		{name: "original work digest after self outcome", mutate: func(proof *WorktreeRetirementOwnerProof) {}, want: "work digest"},
		{name: "unrelated work digest", mutate: func(proof *WorktreeRetirementOwnerProof) { proof.WorkDigest = "sha256:" + strings.Repeat("a", 64) }, want: "work digest"},
		{name: "lease digest drift", mutate: func(proof *WorktreeRetirementOwnerProof) {
			proof.WorkDigest = "sha256:" + strings.Repeat("e", 64)
			proof.LeaseWorkSHA256 = strings.Repeat("a", 64)
		}, want: "lease"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRetirementGitFixture(t)
			home, paths, report := authorizedRetirementFixture(t, fixture)
			injected := errors.New("transient update-ref failure")
			runner := &failOnceRefDeleteGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
			records := 0
			deps := successfulRetirementApplyDependencies(runner, &records)
			req := WorktreeRetirementApplyRequest{
				HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
				Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
			}
			if _, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, injected) {
				t.Fatalf("first error=%v", err)
			}
			tc.mutate(&req.OwnerProof)
			beforeRemove, beforeDelete := runner.removeCalls, runner.deleteCalls
			_, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("resume error=%v want %q", err, tc.want)
			}
			if runner.removeCalls != beforeRemove || runner.deleteCalls != beforeDelete || records != 1 {
				t.Fatalf("refused resume mutated state remove=%d/%d delete=%d/%d records=%d", runner.removeCalls, beforeRemove, runner.deleteCalls, beforeDelete, records)
			}
		})
	}
}

func TestApplyWorktreeRetirementFailureOutcomeProofMustBeCompleteBeforeResultPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*WorktreeRetirementActionProof)
	}{
		{name: "action mismatch", mutate: func(proof *WorktreeRetirementActionProof) { proof.ActionIntentID = "act_wrong" }},
		{name: "policy mismatch", mutate: func(proof *WorktreeRetirementActionProof) { proof.PolicyDecisionID = "pol_wrong" }},
		{name: "missing receipt", mutate: func(proof *WorktreeRetirementActionProof) { proof.ReceiptID = "" }},
		{name: "missing transition", mutate: func(proof *WorktreeRetirementActionProof) { proof.TransitionID = "" }},
		{name: "missing lease digest", mutate: func(proof *WorktreeRetirementActionProof) { proof.LeaseWorkSHA256 = "" }},
		{name: "missing work digest", mutate: func(proof *WorktreeRetirementActionProof) { proof.WorkDigest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRetirementGitFixture(t)
			home, paths, report := authorizedRetirementFixture(t, fixture)
			plan, err := LoadWorktreeRetirementPlan(paths.PlanPath)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("transient update-ref failure")
			runner := &failOnceRefDeleteGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
			deps := successfulRetirementApplyDependencies(runner, new(int))
			deps.RecordOutcome = func(outcome WorktreeRetirementActionOutcome, _ time.Time) (WorktreeRetirementActionProof, error) {
				proof := completeRetirementActionProof(outcome, "sha256:"+strings.Repeat("e", 64))
				tc.mutate(&proof)
				return proof, nil
			}
			result, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
				HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
				Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
			}, deps, time.Now())
			if err == nil || !strings.Contains(err.Error(), "outcome proof chain incomplete") {
				t.Fatalf("invalid proof result=%+v err=%v", result, err)
			}
			resultPath := filepath.Join(home, "reviews", WorktreeRetirementReviewArea, plan.ID+".retirement.result.json")
			if _, statErr := os.Stat(resultPath); !os.IsNotExist(statErr) {
				t.Fatalf("invalid proof persisted result: %v", statErr)
			}
		})
	}
}

func TestApplyWorktreeRetirementReobservesRecoveryProofBeforeMutation(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	resolver := &controlledRetirementRecoveryResolver{delegate: SHA256FileWorktreeRetirementRecoveryResolver{}, fail: true}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.RecoveryResolver = resolver
	_, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}, deps, time.Now())
	if err == nil || !strings.Contains(err.Error(), "recovery evidence") {
		t.Fatalf("recovery refusal error=%v", err)
	}
	if runner.mutationCount() != 0 || records != 0 || resolver.calls == 0 {
		t.Fatalf("recovery refusal mutated state calls=%+v records=%d resolver=%d", runner.calls, records, resolver.calls)
	}
}

func TestApplyWorktreeRetirementReobservesRecoveryProofAtFinalBoundary(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	resolver := &controlledRetirementRecoveryResolver{delegate: SHA256FileWorktreeRetirementRecoveryResolver{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.RecoveryResolver = resolver
	finalBoundaryReached := false
	deps.AfterPhase = func(phase string) error {
		if phase == WorktreeRetirementPhaseRefDeleted {
			finalBoundaryReached = true
			resolver.fail = true
		}
		return nil
	}
	result, err := ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}, deps, time.Now())
	if err == nil || !strings.Contains(err.Error(), "recovery evidence") || result.Status != "partial" {
		t.Fatalf("final recovery result=%+v err=%v", result, err)
	}
	if !finalBoundaryReached || runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 1 || records != 1 || resolver.calls < 2 {
		t.Fatalf("final recovery calls=%+v records=%d resolver=%d", runner.calls, records, resolver.calls)
	}
}

func TestApplyWorktreeRetirementReobservesRecoveryProofBeforeResume(t *testing.T) {
	fixture := newRetirementGitFixture(t)
	home, paths, report := authorizedRetirementFixture(t, fixture)
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	resolver := &controlledRetirementRecoveryResolver{delegate: SHA256FileWorktreeRetirementRecoveryResolver{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	deps.RecoveryResolver = resolver
	crash := errors.New("post-worktree success crash")
	deps.AfterGitMutation = func(phase string) error {
		if phase == WorktreeRetirementPhaseWorktreeRemovalStarted {
			return crash
		}
		return nil
	}
	req := WorktreeRetirementApplyRequest{
		HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath,
		Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: validRetirementSourceProof(fixture.candidateTip),
	}
	if _, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now()); !errors.Is(err, crash) {
		t.Fatalf("first error=%v", err)
	}
	resolver.fail = true
	deps.AfterGitMutation = nil
	_, err := ApplyWorktreeRetirement(context.Background(), req, deps, time.Now().Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "recovery evidence") {
		t.Fatalf("resume recovery error=%v", err)
	}
	if runner.worktreeRemoveCount() != 1 || runner.refDeleteCount() != 0 || records != 0 {
		t.Fatalf("resume recovery mutated state calls=%+v records=%d", runner.calls, records)
	}
}

type retirementGitCall struct {
	repo string
	args []string
}

type recordingRetirementGit struct {
	delegate WorktreeRetirementGit
	calls    []retirementGitCall
}

type casDriftRetirementGit struct {
	delegate    WorktreeRetirementGit
	ref         string
	replacement string
	drifted     bool
}

func (r *casDriftRetirementGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if !r.drifted && len(args) >= 2 && args[0] == "update-ref" && args[1] == "-d" {
		r.drifted = true
		if _, err := r.delegate.Run(ctx, repo, "update-ref", r.ref, r.replacement); err != nil {
			return "", err
		}
	}
	return r.delegate.Run(ctx, repo, args...)
}

type failOnceRefDeleteGit struct {
	delegate    WorktreeRetirementGit
	err         error
	failed      bool
	removeCalls int
	deleteCalls int
}

type failOnceWorktreeRemoveGit struct {
	delegate              WorktreeRetirementGit
	err                   error
	failed                bool
	removeCalls           int
	successfulRemoveCalls int
	refDeleteCalls        int
}

func (r *failOnceWorktreeRemoveGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		r.removeCalls++
		if !r.failed {
			r.failed = true
			return "", r.err
		}
		r.successfulRemoveCalls++
	}
	if len(args) >= 2 && args[0] == "update-ref" && args[1] == "-d" {
		r.refDeleteCalls++
	}
	return r.delegate.Run(ctx, repo, args...)
}

type phaseAssertingRetirementGit struct {
	delegate            WorktreeRetirementGit
	pendingPath         string
	t                   *testing.T
	worktreeRemoveCount int
	refDeleteCount      int
}

type controlledRetirementRecoveryResolver struct {
	delegate      WorktreeRetirementRecoveryResolver
	fail          bool
	calls         int
	mutationCount func() int
}

func (r *controlledRetirementRecoveryResolver) Resolve(ctx context.Context, reference string) (WorktreeRetirementRecoveryProof, error) {
	r.calls++
	if r.fail || (r.mutationCount != nil && r.mutationCount() > 0) {
		return WorktreeRetirementRecoveryProof{}, errors.New("recovery evidence unavailable")
	}
	return r.delegate.Resolve(ctx, reference)
}

func (r *phaseAssertingRetirementGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		r.assertPendingPhase(WorktreeRetirementPhaseWorktreeRemovalStarted)
		r.worktreeRemoveCount++
	}
	if len(args) >= 2 && args[0] == "update-ref" && args[1] == "-d" {
		r.assertPendingPhase(WorktreeRetirementPhaseRefDeletionStarted)
		r.refDeleteCount++
	}
	return r.delegate.Run(ctx, repo, args...)
}

func (r *phaseAssertingRetirementGit) assertPendingPhase(want string) {
	r.t.Helper()
	pending, exists, err := loadWorktreeRetirementPending(r.pendingPath)
	if err != nil || !exists || pending.Phase != want {
		r.t.Fatalf("pending before Git mutation=%+v exists=%v err=%v want phase=%s", pending, exists, err, want)
	}
}

func (r *failOnceRefDeleteGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		r.removeCalls++
	}
	if len(args) >= 2 && args[0] == "update-ref" && args[1] == "-d" {
		r.deleteCalls++
		if !r.failed {
			r.failed = true
			return "", r.err
		}
	}
	return r.delegate.Run(ctx, repo, args...)
}

func (r *recordingRetirementGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	r.calls = append(r.calls, retirementGitCall{repo: repo, args: append([]string{}, args...)})
	return r.delegate.Run(ctx, repo, args...)
}

func (r *recordingRetirementGit) mutationCount() int {
	return r.worktreeRemoveCount() + r.refDeleteCount()
}
func (r *recordingRetirementGit) worktreeRemoveCount() int {
	n := 0
	for _, call := range r.calls {
		if len(call.args) >= 2 && call.args[0] == "worktree" && call.args[1] == "remove" {
			n++
		}
	}
	return n
}
func (r *recordingRetirementGit) refDeleteCount() int {
	n := 0
	for _, call := range r.calls {
		if len(call.args) >= 2 && call.args[0] == "update-ref" && call.args[1] == "-d" {
			n++
		}
	}
	return n
}

func authorizedRetirementFixture(t *testing.T, fixture retirementGitFixture) (string, WorktreeRetirementPlanPaths, WorktreeRetirementReviewReport) {
	return authorizedRetirementRequestFixture(t, retirementPlanRequest(fixture))
}

func authorizedRetirementRequestFixture(t *testing.T, req WorktreeRetirementPlanRequest) (string, WorktreeRetirementPlanPaths, WorktreeRetirementReviewReport) {
	t.Helper()
	plan, err := BuildWorktreeRetirementPlan(context.Background(), req, WorktreeRetirementDependencies{RecoveryInventoryPreflight: testRetirementRecoveryPreflight,
		Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused},
	}, time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
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
	review.Decision = "authorize_retirement"
	review.Rationale = "retire exact redundant Git surfaces"
	review.ReviewedBy = "operator:test"
	review.ReviewedAt = "2026-07-15T07:01:00Z"
	report, err := RecordWorktreeRetirementReview(home, review, paths.ReviewYAMLPath, time.Date(2026, 7, 15, 7, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return home, paths, report
}

func successfulRetirementApplyDependencies(runner WorktreeRetirementGit, records *int) WorktreeRetirementApplyDependencies {
	return WorktreeRetirementApplyDependencies{RecoveryInventoryPreflight: testRetirementRecoveryPreflight,
		Git: runner, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused},
		Evaluate: func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
			return policy.Evaluate(action, policy.DefaultConfig(), now), nil
		},
		RecordOutcome: func(outcome WorktreeRetirementActionOutcome, _ time.Time) (WorktreeRetirementActionProof, error) {
			*records++
			return completeRetirementActionProof(outcome, "sha256:"+strings.Repeat("e", 64)), nil
		},
	}
}

func completeRetirementActionProof(outcome WorktreeRetirementActionOutcome, workDigest string) WorktreeRetirementActionProof {
	return WorktreeRetirementActionProof{
		ActionIntentID: outcome.Action.ID, PolicyDecisionID: outcome.Decision.ID,
		ReceiptID: "rec_retirement", TransitionID: "tr_retirement",
		LeaseWorkSHA256: validRetirementOwnerProof().LeaseWorkSHA256,
		WorkDigest:      workDigest,
	}
}

func retirementGitRefExists(repo, ref string) bool {
	cmd := ExecWorktreeRetirementGit{}
	_, err := cmd.Run(context.Background(), repo, "rev-parse", "--verify", ref)
	return err == nil
}

func assertRetirementMutationArgv(t *testing.T, calls []retirementGitCall, fixture retirementGitFixture) {
	t.Helper()
	var remove, delete bool
	for _, call := range calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "--force") {
			t.Fatalf("dangerous retirement argv: git %s", joined)
		}
		if len(call.args) > 0 {
			for _, forbidden := range []string{"reset", "checkout", "clean", "stash", "merge", "rebase", "push", "install"} {
				if call.args[0] == forbidden {
					t.Fatalf("dangerous retirement argv: git %s", joined)
				}
			}
		}
		if len(call.args) > 1 && call.args[0] == "worktree" && call.args[1] == "unlock" {
			t.Fatalf("dangerous retirement argv: git %s", joined)
		}
		if len(call.args) == 3 && call.args[0] == "worktree" && call.args[1] == "remove" && call.args[2] == fixture.worktree {
			remove = true
		}
		if len(call.args) == 4 && call.args[0] == "update-ref" && call.args[1] == "-d" && call.args[2] == fixture.candidateRef && call.args[3] == fixture.candidateTip {
			delete = true
		}
	}
	if !remove || !delete {
		t.Fatalf("missing non-force/CAS mutation argv: %+v", calls)
	}
}
