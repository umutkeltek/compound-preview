package work

import (
	"strings"
	"testing"
	"time"
)

func TestDispatchSurfacesExpectedBaseAndWorkerLandingContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 21, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, base := initLandingRepo(t)
	declaredDelivery := localGitDelivery(repo, base)
	callerFingerprint := strings.Repeat("f", 64)
	declaredDelivery.RepositoryFingerprint = callerFingerprint
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "source-controlled work lands on main",
		GitDelivery:      declaredDelivery,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if w.GitDelivery == nil || w.GitDelivery.RepositoryFingerprint == "" || w.GitDelivery.RepositoryFingerprint == callerFingerprint {
		t.Fatalf("created work did not bind derived repository fingerprint: %+v", w.GitDelivery)
	}

	plan, err := store.DispatchPlan(now.Add(time.Minute), DispatchPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selected == nil || plan.Selected.WorkID != w.ID {
		t.Fatalf("dispatch did not select source-controlled work: %+v", plan.Selected)
	}
	if plan.Selected.ExpectedBaseSHA != base || plan.Selected.RepositoryFingerprint != w.GitDelivery.RepositoryFingerprint {
		t.Fatalf("dispatch source-control identity = %+v, want base %q and fingerprint %q", plan.Selected, base, w.GitDelivery.RepositoryFingerprint)
	}

	claimed, err := store.DispatchClaim(now.Add(2*time.Minute), DispatchPlanOptions{Actor: "codex", WorkID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## SOURCE CONTROL CONTRACT",
		"target_worktree_root: " + w.GitDelivery.TargetWorktreeRoot,
		"target_local_ref: refs/heads/main",
		"expected_base_sha: " + base,
		"repository_fingerprint: " + w.GitDelivery.RepositoryFingerprint,
		"publication_mode: local_only",
		"commit:<full-sha>",
		"compound work land-check",
	} {
		if !strings.Contains(claimed.WorkerPromptMD, want) {
			t.Fatalf("worker prompt missing %q:\n%s", want, claimed.WorkerPromptMD)
		}
	}
}

func TestPreFingerprintGitDeliveryIsVisibleButNotDispatchableBeforeBackfill(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 5, 0, 0, 0, time.UTC)
	store, legacy, _, _ := preFingerprintGitDeliveryFixture(t, now, "ready", "")
	report, err := store.Workbench(now.Add(time.Minute), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, legacy.ID)
	if item.GitDeliveryBound || item.LandingStatus != "repository_identity_migration_required" || !strings.Contains(item.NextAction, "operator") {
		t.Errorf("pre-fingerprint workbench item must expose migration debt: %+v", item)
	}

	plan, err := store.DispatchPlan(now.Add(2*time.Minute), DispatchPlanOptions{Actor: "codex", WorkID: legacy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selected != nil || len(plan.Candidates) != 1 || plan.Candidates[0].Action != "review" || len(plan.Candidates[0].Command) != 0 {
		t.Errorf("pre-fingerprint work must not be claimable from dispatch: %+v", plan)
	}
	if _, err := store.attachDispatchPrompt(legacy.ID, "codex", now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Errorf("direct prompt attachment before migration error = %v", err)
	}
	if _, err := store.ClaimPlan(legacy.ID, now.Add(4*time.Minute), ClaimPlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Errorf("claim plan before migration error = %v", err)
	}
	if _, err := store.Claim(legacy.ID, "codex", now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Errorf("claim before migration error = %v", err)
	}
}

func TestWorkbenchMakesCodeWithoutSourceBoundProofAwaitingProof(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 21, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, base := initLandingRepo(t)
	tip := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "verified code cannot disappear outside main",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{
		To:             "verifying",
		Actor:          "codex",
		ArtifactID:     "commit:" + tip,
		VerificationID: "check:focused-tests",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := store.Workbench(now.Add(4*time.Minute), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Open) != 1 {
		t.Fatalf("open work = %d, want 1", len(report.Open))
	}
	item := report.Open[0]
	if item.ExpectedBaseSHA != base || item.RepositoryFingerprint != w.GitDelivery.RepositoryFingerprint || item.CandidateTipSHA != tip {
		t.Fatalf("source-control identity missing from workbench: %+v", item)
	}
	if item.LandingStatus != "awaiting_proof" ||
		!strings.Contains(item.NextAction, "fresh canonical certifying proof") ||
		strings.Contains(item.NextAction, "fast-forward") {
		t.Fatalf("unproved code must be directly visible as awaiting proof: %+v", item)
	}
}
