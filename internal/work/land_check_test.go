package work

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestLandCheckClassifiesReadyLandedAndStaleLineage(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	t.Run("ready then landed", func(t *testing.T) {
		repo, base, candidate := sharedLandingReadyRepo(t)

		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		w, err := store.Create(CreateInput{
			Owner:            "codex",
			SuccessPredicate: "candidate reaches main",
			GitDelivery:      localGitDelivery(repo, base),
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
		completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, candidate, now.Add(30*time.Second+time.Second))

		ready, err := store.LandCheck(w.ID, LandCheckOptions{
			RepoPath:        repo,
			CandidateTipSHA: candidate,
			TargetRef:       "main",
			Record:          true,
		}, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if ready.Status != "ready_to_land" || ready.CandidateContainedInTarget || !ready.ExpectedBaseAncestorOfCandidate {
			t.Fatalf("ready report = %+v", ready)
		}
		if ready.EvidenceRef == "" || ready.ProofPath == "" {
			t.Fatalf("recorded check missing durable proof identity: %+v", ready)
		}
		if _, err := os.Stat(ready.ProofPath); err != nil {
			t.Fatalf("recorded proof missing: %v", err)
		}

		landingGit(t, repo, "merge", "--ff-only", candidate)
		completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(90*time.Second))
		landed, err := store.LandCheck(w.ID, LandCheckOptions{
			RepoPath:        repo,
			CandidateTipSHA: candidate,
			TargetRef:       "main",
			Record:          true,
		}, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if landed.Status != "landed" || !landed.CandidateContainedInTarget || landed.TargetSHA != candidate {
			t.Fatalf("landed report = %+v", landed)
		}
	})

	t.Run("main moved", func(t *testing.T) {
		repo, base, candidate := sharedLandingReadyRepo(t)
		writeLandingFile(t, filepath.Join(repo, "main.txt"), "main moved\n")
		landingGit(t, repo, "add", "main.txt")
		landingGit(t, repo, "commit", "-m", "main moved")

		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "stale candidate is explicit", GitDelivery: localGitDelivery(repo, base)}, now)
		if err != nil {
			t.Fatal(err)
		}
		attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
		report, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if report.Status != "rebase_required" || report.NextAction == "" {
			t.Fatalf("stale report = %+v", report)
		}
	})
}

func TestLandCheckFailsClosedForDirtyTargetAndMovedSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "target remains exact", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, candidate, now.Add(30*time.Second+time.Second))

	_, err = store.LandCheck(w.ID, LandCheckOptions{
		RepoPath:        repo,
		CandidateTipSHA: candidate,
		TargetRef:       "main",
		ExpectedMainSHA: strings.Repeat("f", 40),
	}, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "not delivery authority") {
		t.Fatalf("caller expected-main snapshot must not replace bound authority, got %v", err)
	}

	// Unrelated WIP in the target worktree must NOT fabricate a blocked status:
	// the candidate simply is not on main yet (ready_to_land), and CleanTarget
	// still honestly reports the whole-tree dirt.
	writeLandingFile(t, filepath.Join(repo, "dirty.txt"), "uncommitted\n")
	tolerated, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if tolerated.Status != "ready_to_land" || tolerated.CleanTarget {
		t.Fatalf("unrelated dirt must not gate an untouched landing: %+v", tolerated)
	}

	// A real uncommitted change to a file the envelope OWNS still fails closed.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "conflicting local edit\n")
	report, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "dirty_target" || report.CleanTarget {
		t.Fatalf("expected dirty_target for owned-path dirt, got %+v", report)
	}
}

func TestCompletedCodeWorkRequiresCurrentLandedProof(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "close only after containment", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(150*time.Second))

	if _, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "not landed"}, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "land-check") {
		t.Fatalf("completed without landing proof err = %v", err)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "running" {
		t.Fatalf("failed gate mutated work state to %q", current.State)
	}

	ready, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "still not landed", ProofRefs: []string{ready.EvidenceRef}}, now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "not landed") {
		t.Fatalf("ready-only proof close err = %v", err)
	}

	landingGit(t, repo, "merge", "--ff-only", candidate)
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(5*time.Minute+time.Second))
	landed, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "candidate is on main", ProofRefs: []string{landed.EvidenceRef}}, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Work.State != "completed" || !contains(closed.Receipt.ProofRefs, landed.EvidenceRef) {
		t.Fatalf("closed result = %+v", closed)
	}
	bench, err := store.Workbench(now.Add(8*time.Minute), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	terminal := findWorkbenchItem(t, bench.Recent, w.ID)
	if terminal.NextAction != "terminal; mine receipt and transition if useful" || terminal.LandingStatus == "ready_to_close" {
		t.Fatalf("terminal delivery work must not be offered for close again: %+v", terminal)
	}
}

func TestCompletedCodeWorkRevalidatesContainedLandingProofAtClose(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "proof stays live", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(150*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(150*time.Second+time.Second))
	proof, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// A later uncommitted change to a path the envelope touched does not change
	// either authoritative ref. Close revalidates immutable containment and keeps
	// the later WIP visible without treating it as part of this candidate.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "conflicting local edit after proof\n")
	plan, err := store.ClosePlan(w.ID, now.Add(4*time.Minute), ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "outward_delta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == "not_closeable" || len(plan.CloseCommand) == 0 || strings.Contains(plan.NoAction, "dirty_target") {
		t.Fatalf("close plan must accept revalidated immutable containment: %+v", plan)
	}
	closed, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "contained; later WIP preserved", ProofRefs: []string{proof.EvidenceRef}}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("close with revalidated containment: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("close result = %+v", closed)
	}
}

// TestLandCheckReportsTruthfulContainmentUnderUnrelatedDirt is the core falsifier
// for the land-check containment lie: a commit provably on the target branch must
// report candidate_contained_in_target=true even when the target worktree carries
// unrelated preserved WIP -- and later owned-path WIP stays visible without
// overriding the immutable containment result.
func TestLandCheckReportsTruthfulContainmentUnderUnrelatedDirt(t *testing.T) {
	now := time.Date(2026, 7, 16, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "containment never lies under unrelated dirt", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))

	// Preserved unrelated WIP: the always-dirty repo / shared-checkout case.
	writeLandingFile(t, filepath.Join(repo, "unrelated-wip.txt"), "neighbor lane WIP\n")

	landed, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if landed.Status != "landed" {
		t.Fatalf("unrelated dirt must not block a landed commit: %+v", landed)
	}
	if !landed.CandidateContainedInTarget || !landed.CandidateContainedInPublication {
		t.Fatalf("containment fields lied under unrelated dirt: %+v", landed)
	}
	if landed.CleanTarget {
		t.Fatalf("CleanTarget must honestly report the whole-tree dirt: %+v", landed)
	}

	// Later owned-path WIP does not change the already-landed commit. It remains a
	// preservation diagnostic and CleanTarget remains honest.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "conflicting local edit\n")
	preserved, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != "landed" || preserved.CleanTarget {
		t.Fatalf("contained candidate must preserve later owned-path WIP: %+v", preserved)
	}
	if !preserved.CandidateContainedInTarget || !strings.Contains(preserved.NextAction, "feature.txt") {
		t.Fatalf("contained dirty diagnostic lost proof or path: %+v", preserved)
	}
}

func TestLandCheckPublishedContainmentOutranksOwnedPathDirt(t *testing.T) {
	now := time.Date(2026, 7, 18, 9, 30, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)
	landingGit(t, repo, "push", "origin", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "published containment is immutable close proof",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))

	// A later lane is editing a path this candidate also changed. The local and
	// publication refs already contain the exact candidate, so the worktree dirt
	// is future WIP, not part of the immutable landing proof.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "later lane WIP\n")
	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "landed" || !report.CandidateContainedInTarget || !report.CandidateContainedInPublication {
		t.Fatalf("published containment must outrank later owned-path dirt: %+v", report)
	}
	if report.CleanTarget {
		t.Fatalf("whole-tree dirt must remain visible: %+v", report)
	}
	if !strings.Contains(report.NextAction, "feature.txt") || !strings.Contains(report.NextAction, "preserve") {
		t.Fatalf("later WIP must remain visible as a preservation diagnostic: %+v", report)
	}
}

func TestCompletedRequiredPublicationWorkClosesOverLaterOwnedPathDirt(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 18, 9, 45, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)
	landingGit(t, repo, "push", "origin", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "exact target and publication containment stays closeable over later owned-path WIP",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(150*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(150*time.Second+time.Second))
	proof, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate, Record: true}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "landed" || proof.TargetSHA != candidate || proof.PublicationSHA != candidate ||
		!proof.CandidateContainedInTarget || !proof.CandidateContainedInPublication {
		t.Fatalf("fixture needs exact target and publication containment: %+v", proof)
	}

	const laterWIP = "later lane owns this uncommitted edit\n"
	featurePath := filepath.Join(repo, "feature.txt")
	writeLandingFile(t, featurePath, laterWIP)
	live, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != "landed" || live.CleanTarget || live.TargetSHA != candidate || live.PublicationSHA != candidate ||
		!strings.Contains(live.NextAction, "feature.txt") || !strings.Contains(live.NextAction, "preserve") {
		t.Fatalf("live revalidation must preserve exact containment and surface later dirt: %+v", live)
	}

	plan, err := store.ClosePlan(w.ID, now.Add(5*time.Minute), ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "outward_delta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == "not_closeable" || len(plan.CloseCommand) == 0 || strings.Contains(plan.NoAction, "dirty_target") {
		t.Fatalf("close plan must accept exact required-publication containment: %+v", plan)
	}
	if got, err := os.ReadFile(featurePath); err != nil || string(got) != laterWIP {
		t.Fatalf("close plan changed later WIP: got=%q err=%v", got, err)
	}

	closed, err := store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "published candidate contained; later WIP preserved",
		ProofRefs: []string{proof.EvidenceRef},
	}, now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("close over later owned-path WIP: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("close result = %+v", closed)
	}
	if got, err := os.ReadFile(featurePath); err != nil || string(got) != laterWIP {
		t.Fatalf("close changed later WIP: got=%q err=%v", got, err)
	}
}

func TestLandCheckPublishedContainmentReportsForeignDirt(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)
	landingGit(t, repo, "push", "origin", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "published containment preserves foreign WIP visibility",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))

	foreignPaths := []string{
		"foreign-lane-wip-1.txt",
		"foreign-lane-wip-2.txt",
		"foreign-lane-wip-3.txt",
		"foreign-lane-wip-4.txt",
		"foreign-lane-wip-5.txt",
		"foreign-lane-wip-6.txt",
	}
	for _, path := range foreignPaths {
		writeLandingFile(t, filepath.Join(repo, path), "neighbor lane WIP\n")
	}
	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "landed" || !report.CandidateContainedInTarget || !report.CandidateContainedInPublication {
		t.Fatalf("published containment must remain landed under foreign dirt: %+v", report)
	}
	if report.CleanTarget {
		t.Fatalf("whole-tree foreign dirt must remain visible: %+v", report)
	}
	for _, path := range foreignPaths {
		if !strings.Contains(report.NextAction, path) {
			t.Fatalf("preservation warning omitted dirty path %s: %+v", path, report)
		}
	}
	if !strings.Contains(report.NextAction, "preserve") {
		t.Fatalf("foreign dirt must produce a preservation warning: %+v", report)
	}
}

func TestLandCheckTargetContainmentStillAwaitsPublicationUnderForeignDirt(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 30, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "local containment cannot outrank stale publication",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))

	writeLandingFile(t, filepath.Join(repo, "foreign-lane-wip.txt"), "neighbor lane WIP\n")
	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "awaiting_publication" || !report.CandidateContainedInTarget || report.CandidateContainedInPublication {
		t.Fatalf("stale publication must block landed classification under dirt: %+v", report)
	}
	if report.CleanTarget {
		t.Fatalf("whole-tree foreign dirt must remain visible while awaiting publication: %+v", report)
	}
	if !strings.Contains(report.NextAction, "refs/remotes/origin/main") || strings.Contains(report.NextAction, "already contained in the authoritative target and publication refs") {
		t.Fatalf("next action must remain publication, never a landed preservation warning: %+v", report)
	}
}

func TestLandCheckMissingRequiredPublicationRefAwaitsPublication(t *testing.T) {
	now := time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)
	landingGit(t, repo, "push", "origin", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "a deleted required publication ref remains actionable",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))
	landingGit(t, repo, "update-ref", "-d", "refs/remotes/origin/main")

	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("missing valid publication ref must be an actionable classification, not an evaluator error: %v", err)
	}
	if report.Status != "awaiting_publication" || !report.CandidateContainedInTarget || report.CandidateContainedInPublication || report.PublicationSHA != "" {
		t.Fatalf("missing publication ref classification = %+v", report)
	}
	if !strings.Contains(report.NextAction, "publish") || !strings.Contains(report.NextAction, "refs/remotes/origin/main") {
		t.Fatalf("missing publication ref needs an exact publish action: %+v", report)
	}
}

func TestLandCheckRejectsGitInvalidPublicationRefThatLooksMissing(t *testing.T) {
	now := time.Date(2026, 7, 18, 11, 15, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)
	landingGit(t, repo, "push", "origin", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "malformed publication authority is never mistaken for missing publication",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))

	const invalidPublicationRef = "refs/remotes/.origin/main"
	w, err = store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.GitDelivery.PublicationRef = invalidPublicationRef
	if err := w.Validate(); err != nil {
		t.Fatalf("fixture ref must pass envelope lexical validation to exercise the Git seam: %v", err)
	}
	if err := store.writeJSON(store.workPath(w.ID), &w); err != nil {
		t.Fatal(err)
	}

	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "publication_ref") || !strings.Contains(err.Error(), "valid Git ref") {
		t.Fatalf("Git-invalid publication ref must fail authority validation, got report=%+v err=%v", report, err)
	}
}

func TestLandCheckLocalOnlyPreservationNamesOnlyTargetAuthority(t *testing.T) {
	now := time.Date(2026, 7, 18, 11, 30, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "local-only preservation names only its target authority",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(30*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(30*time.Second+time.Second))
	writeLandingFile(t, filepath.Join(repo, "local-only-wip.txt"), "later local WIP\n")

	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "landed" || report.CleanTarget {
		t.Fatalf("local-only contained dirty report = %+v", report)
	}
	if !strings.Contains(report.NextAction, "authoritative target ref") || strings.Contains(report.NextAction, "publication") {
		t.Fatalf("local-only warning must not claim publication authority: %+v", report)
	}
}

// TestCompletedCodeWorkClosesOverPreservedUnrelatedDirt is the falsifier for the
// shared-checkout hostage class: a published, landed envelope must close even when
// a neighbor lane's live unrelated WIP dirties the shared checkout after the proof
// was recorded, because close live-revalidates land-check.
func TestCompletedCodeWorkClosesOverPreservedUnrelatedDirt(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 16, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "published close is not hostage to a neighbor's WIP", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(150*time.Second))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(150*time.Second+time.Second))
	proof, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "landed" {
		t.Fatalf("fixture proof = %+v", proof)
	}

	// A neighbor lane's live, unrelated WIP dirties the shared checkout after the
	// proof was recorded. Close live-revalidates -- and must still close.
	writeLandingFile(t, filepath.Join(repo, "neighbor-wip.sh"), "#!/bin/sh\necho neighbor\n")

	plan, err := store.ClosePlan(w.ID, now.Add(4*time.Minute), ClosePlanOptions{Actor: "codex", Outcome: "completed", MoveType: "outward_delta"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == "not_closeable" || strings.Contains(plan.NoAction, "dirty_target") {
		t.Fatalf("unrelated neighbor WIP must not hold a published close hostage: %+v", plan)
	}
	closed, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "landed; neighbor WIP preserved", ProofRefs: []string{proof.EvidenceRef}}, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("close over unrelated dirt: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("close result = %+v", closed)
	}
}

func TestLandCheckRecordsMissingTipAndInvalidLineage(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "switch", "--orphan", "unrelated")
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeLandingFile(t, filepath.Join(repo, "unrelated.txt"), "unrelated\n")
	landingGit(t, repo, "add", "-A")
	landingGit(t, repo, "commit", "-m", "unrelated")
	unrelated := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "bad lineage stays actionable", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, strings.Repeat("f", 40), now.Add(30*time.Second))

	missing, err := store.LandCheck(w.ID, LandCheckOptions{
		RepoPath:        repo,
		CandidateTipSHA: strings.Repeat("f", 40),
		TargetRef:       "main",
		Record:          true,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != "invalid_lineage" || missing.NextAction == "" || missing.EvidenceRef == "" {
		t.Fatalf("missing candidate report = %+v", missing)
	}

	attachLandingCommit(t, store, w.ID, unrelated, now.Add(90*time.Second))
	invalid, err := store.LandCheck(w.ID, LandCheckOptions{
		RepoPath:        repo,
		CandidateTipSHA: unrelated,
		TargetRef:       "main",
		Record:          true,
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Status != "invalid_lineage" || invalid.ExpectedBaseAncestorOfCandidate || invalid.NextAction == "" {
		t.Fatalf("invalid lineage report = %+v", invalid)
	}

	if _, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: "abc123", TargetRef: "main"}, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("short candidate SHA err = %v", err)
	}
}

func TestLandCheckRequiresLatestAttachedCommitArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "only the attached final tip can land", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}

	missing, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != "missing_candidate_artifact" || missing.NextAction == "" {
		t.Fatalf("missing candidate artifact report = %+v", missing)
	}

	attachLandingCommit(t, store, w.ID, base, now.Add(2*time.Minute))
	mismatch, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.Status != "candidate_tip_mismatch" || !strings.Contains(mismatch.NextAction, "commit:"+candidate) {
		t.Fatalf("candidate mismatch report = %+v", mismatch)
	}

	attachLandingCommit(t, store, w.ID, candidate, now.Add(4*time.Minute))
	completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, candidate, now.Add(4*time.Minute+time.Second))
	ready, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != "ready_to_land" {
		t.Fatalf("latest attached candidate report = %+v", ready)
	}

	landingGit(t, repo, "switch", "feature")
	writeLandingFile(t, filepath.Join(repo, "second.txt"), "second\n")
	landingGit(t, repo, "add", "second.txt")
	landingGit(t, repo, "commit", "-m", "second")
	newTip := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	attachLandingCommit(t, store, w.ID, newTip, now.Add(6*time.Minute))
	old, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main"}, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "candidate_tip_mismatch" || !strings.Contains(old.NextAction, newTip) {
		t.Fatalf("old candidate report = %+v", old)
	}

	attachLandingArtifact(t, store, w.ID, "commit:abc123", now.Add(8*time.Minute))
	malformed, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: newTip, TargetRef: "main"}, now.Add(9*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if malformed.Status != "invalid_candidate_artifact" || malformed.NextAction == "" {
		t.Fatalf("malformed candidate artifact report = %+v", malformed)
	}
}

func TestRecordedLandCheckContentIsTamperEvident(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	// The cases are serial because they share one fully persisted baseline.
	// Each case still durably rewrites the proof through the production path,
	// and cleanup durably restores the exact proof before the next case.
	fixture := newLandingProofFixture(t, now)
	baseline := snapshotCloseAuthorityFiles(t, fixture.store.home)
	tests := []struct {
		name   string
		mutate func(*landingProofFixture, *LandCheckReport)
	}{
		{name: "repo path", mutate: func(f *landingProofFixture, p *LandCheckReport) {
			alias := filepath.Join(f.scratch, "repo-alias")
			if err := os.Symlink(f.repo, alias); err != nil {
				t.Fatal(err)
			}
			p.RepoPath = alias
		}},
		{name: "candidate tip", mutate: func(f *landingProofFixture, p *LandCheckReport) { p.CandidateTipSHA = f.base }},
		{name: "status", mutate: func(_ *landingProofFixture, p *LandCheckReport) { p.Status = "ready_to_land" }},
		{name: "expected base", mutate: func(_ *landingProofFixture, p *LandCheckReport) { p.ExpectedBaseSHA = strings.Repeat("a", 40) }},
		{name: "repository fingerprint", mutate: func(_ *landingProofFixture, p *LandCheckReport) { p.RepositoryFingerprint = strings.Repeat("a", 64) }},
		{name: "target ref", mutate: func(_ *landingProofFixture, p *LandCheckReport) { p.TargetRef = "refs/heads/main" }},
		{name: "target sha", mutate: func(f *landingProofFixture, p *LandCheckReport) { p.TargetSHA = f.base }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				if err := fixture.store.writeJSON(fixture.proof.ProofPath, &fixture.proof); err != nil {
					t.Errorf("restore exact recorded proof: %v", err)
					return
				}
				if after := snapshotCloseAuthorityFiles(t, fixture.store.home); !reflect.DeepEqual(after, baseline) {
					t.Error("tamper refusal changed persisted baseline authority")
				}
			})
			tampered := fixture.proof
			tc.mutate(&fixture, &tampered)
			if err := fixture.store.writeJSON(fixture.proof.ProofPath, &tampered); err != nil {
				t.Fatal(err)
			}
			_, err := fixture.store.Close(fixture.workID, CloseInput{
				Outcome:   "completed",
				MoveType:  "outward_delta",
				Actor:     "codex",
				Summary:   "tampered proof must not close",
				ProofRefs: []string{fixture.proof.EvidenceRef},
			}, now.Add(10*time.Minute))
			if err == nil || !strings.Contains(err.Error(), "content digest") {
				t.Fatalf("tampered %s close err = %v", tc.name, err)
			}
		})
	}
}

func TestLegacyLandCheckV03RequiresFreshCheckWithoutTamperAccusation(t *testing.T) {
	// Frozen output from the v0.3 sealer before repository_fingerprint entered
	// LandCheckReport. Its digest is valid for that historical payload shape.
	const legacyJSON = `{"schema_version":"compound.work_land_check.v0.3","id":"landcheck_305a66dfcdec","generated_at":"2026-07-13T22:00:00Z","work_id":"work_legacy_landcheck","status":"landed","repo_path":"/repo/product","candidate_tip_sha":"1111111111111111111111111111111111111111","candidate_artifact_ref":"commit:1111111111111111111111111111111111111111","attached_candidate_tip_sha":"1111111111111111111111111111111111111111","expected_base_sha":"0000000000000000000000000000000000000000","target_ref":"main","target_branch_ref":"refs/heads/main","target_sha":"1111111111111111111111111111111111111111","publication_mode":"local_only","candidate_exists":true,"expected_base_exists":true,"clean_target":true,"target_branch_worktree":true,"expected_base_ancestor_of_candidate":true,"target_ancestor_of_candidate":true,"candidate_contained_in_target":true,"candidate_contained_in_publication":true,"content_sha256":"305a66dfcdecdedab0e88c8b48143412b0ad58d8f95e17dc75c8f21c20658c11","recorded":true,"evidence_ref":"land-check:landcheck_305a66dfcdec","proof_path":"/compound/home/work/land-checks/landcheck_305a66dfcdec.json"}`
	var legacy LandCheckReport
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatal(err)
	}
	w := envelope.WorkEnvelope{
		Header: newHeader(envelope.SVWork, legacy.WorkID, time.Date(2026, 7, 13, 21, 0, 0, 0, time.UTC), "", "", "private", "tier1"),
		Owner:  "codex", State: "verifying", SuccessPredicate: "fresh repository identity proof is required",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: "/repo/product", RepositoryFingerprint: strings.Repeat("a", 64), TargetLocalRef: "refs/heads/main",
			ExpectedBaseSHA: strings.Repeat("0", 40), PublicationMode: "local_only",
		},
		ArtifactIDs: []string{"commit:" + strings.Repeat("1", 40)},
	}
	err := validateRecordedLandCheck(w, legacy.EvidenceRef, legacy.ID, legacy.ProofPath, legacy)
	if err == nil {
		t.Fatal("legacy v0.3 land-check unexpectedly satisfied current close proof")
	}
	message := err.Error()
	if !strings.Contains(message, "legacy") || !strings.Contains(message, "fresh") || !strings.Contains(message, "v0.5") {
		t.Fatalf("legacy land-check classification = %v", err)
	}
	if strings.Contains(message, "content digest") || strings.Contains(message, "modified after recording") || strings.Contains(message, "tamper") {
		t.Fatalf("valid historical v0.3 proof was falsely accused of tampering: %v", err)
	}
}

func TestPreviousLandCheckV04RequiresFreshSourceBoundCheck(t *testing.T) {
	report := LandCheckReport{
		SchemaVersion: PreviousLandCheckSchemaVersion,
		ID:            "landcheck_previous",
	}
	err := validateRecordedLandCheck(
		envelope.WorkEnvelope{},
		"land-check:landcheck_previous",
		report.ID,
		"/compound/home/work/land-checks/landcheck_previous.json",
		report,
	)
	if err == nil {
		t.Fatal("v0.4 land-check unexpectedly satisfied source-bound close proof")
	}
	message := err.Error()
	if !strings.Contains(message, "legacy/stale") ||
		!strings.Contains(message, PreviousLandCheckSchemaVersion) ||
		!strings.Contains(message, "sealed proof source identity") ||
		!strings.Contains(message, LandCheckSchemaVersion) {
		t.Fatalf("v0.4 land-check classification = %v", err)
	}
	if strings.Contains(message, "content digest") ||
		strings.Contains(message, "modified after recording") ||
		strings.Contains(message, "tamper") {
		t.Fatalf("historical v0.4 receipt was mislabeled as tampered: %v", err)
	}
}

func TestSourceBoundLandCheckUsesV05Schema(t *testing.T) {
	fixture := newLandingProofFixture(t, time.Date(2026, 7, 14, 6, 0, 0, 0, time.UTC))
	if fixture.proof.SchemaVersion != "compound.work_land_check.v0.5" {
		t.Fatalf("repository fingerprint changed sealed land-check content without a schema bump: %s", fixture.proof.SchemaVersion)
	}
	if fixture.proof.RepositoryFingerprint == "" {
		t.Fatal("v0.5 land-check did not seal repository fingerprint")
	}
}

func TestLandCheckV05FieldSetRequiresSchemaBump(t *testing.T) {
	if LandCheckSchemaVersion != "compound.work_land_check.v0.5" {
		t.Fatalf("update the sealed field-set fixture for new schema %s", LandCheckSchemaVersion)
	}
	typeOfReport := reflect.TypeOf(LandCheckReport{})
	fields := make([]string, 0, typeOfReport.NumField())
	for i := 0; i < typeOfReport.NumField(); i++ {
		name := strings.Split(typeOfReport.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	const v04Fields = "schema_version,id,generated_at,work_id,status,repo_path,candidate_tip_sha,candidate_artifact_ref,attached_candidate_tip_sha,expected_base_sha,repository_fingerprint,target_ref,target_branch_ref,expected_main_sha,target_sha,publication_mode,publication_ref,publication_sha,candidate_exists,expected_base_exists,clean_target,target_branch_worktree,expected_base_ancestor_of_candidate,target_ancestor_of_candidate,candidate_contained_in_target,candidate_contained_in_publication,next_action,content_sha256,recorded,evidence_ref,proof_path"
	const v05Fields = "schema_version,id,generated_at,work_id,status,repo_path,candidate_tip_sha,candidate_artifact_ref,attached_candidate_tip_sha,expected_base_sha,repository_fingerprint,proof_verification_ref,proof_head_sha,target_ref,target_branch_ref,expected_main_sha,target_sha,publication_mode,publication_ref,publication_sha,candidate_exists,expected_base_exists,clean_target,target_branch_worktree,expected_base_ancestor_of_candidate,target_ancestor_of_candidate,candidate_contained_in_target,candidate_contained_in_publication,next_action,content_sha256,recorded,evidence_ref,proof_path"
	if v04Fields == v05Fields {
		t.Fatal("v0.5 schema did not add source-bound proof identity")
	}
	if got := strings.Join(fields, ","); got != v05Fields {
		t.Fatalf("LandCheckReport field set changed under sealed v0.5; bump schema and add a previous-version fixture:\n got: %s\nwant: %s", got, v05Fields)
	}
}

func TestLandCheckRecordDoesNotAcceptTamperedExistingContent(t *testing.T) {
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "record collision stays tamper evident", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(time.Minute))
	checkAt := now.Add(2 * time.Minute)
	opts := LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}
	proof, err := store.LandCheck(w.ID, opts, checkAt)
	if err != nil {
		t.Fatal(err)
	}
	tampered := proof
	tampered.TargetSHA = candidate
	if err := store.writeJSON(proof.ProofPath, &tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LandCheck(w.ID, opts, checkAt); err == nil || !strings.Contains(err.Error(), "tampered existing") {
		t.Fatalf("re-record over tampered proof err = %v", err)
	}
}

func TestLandingCloseGateLeavesLegacyAndFailedWorkCompatible(t *testing.T) {
	now := time.Date(2026, 7, 13, 20, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	legacy, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "legacy work still closes"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, legacy.ID, now)
	if _, err := store.Close(legacy.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "codex", Summary: "legacy completed", ProofRefs: []string{"artifact:legacy"}}, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("legacy completion should not require landing proof: %v", err)
	}

	repo, base := sharedLandingBaseRepo(t)
	code, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "failed code work reports failure", GitDelivery: localGitDelivery(repo, base)}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, code.ID, now.Add(4*time.Minute))
	if _, err := store.Close(code.ID, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", Summary: "executor failed", ProofRefs: []string{"failure:executor"}}, now.Add(7*time.Minute)); err != nil {
		t.Fatalf("failed code work should not require landing proof: %v", err)
	}
}

func TestCompletedCommitArtifactWithoutGitDeliveryContractFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 13, 22, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "legacy code cannot disappear outside delivery authority"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, strings.Repeat("a", 40), now.Add(3*time.Minute))

	if _, err := store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "must fail without delivery authority",
		ProofRefs: []string{"check:focused-tests"},
	}, now.Add(4*time.Minute)); err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("commit artifact without git_delivery contract should fail closed, got %v", err)
	}
}

func TestCompletedCommitProofWithoutGitDeliveryContractFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 13, 22, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "close proof cannot hide source-controlled work"}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	if _, err := store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "must fail without delivery authority",
		ProofRefs: []string{"commit:" + strings.Repeat("a", 40)},
	}, now.Add(4*time.Minute)); err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("commit proof without git_delivery contract should fail closed, got %v", err)
	}
}

func TestLandCheckRejectsWrongRepoSecondCloneAndWrongBranch(t *testing.T) {
	now := time.Date(2026, 7, 13, 22, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)
	secondClone := filepath.Join(t.TempDir(), "second-clone")
	landingGit(t, filepath.Dir(secondClone), "clone", repo, secondClone)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "only the bound target worktree can certify delivery",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, candidate, now.Add(time.Minute))

	if _, err := store.LandCheck(w.ID, LandCheckOptions{
		RepoPath:        secondClone,
		CandidateTipSHA: candidate,
		TargetRef:       "refs/heads/main",
	}, now.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "target_worktree_root") {
		t.Fatalf("second clone must not certify bound work, got %v", err)
	}
	if _, err := store.LandCheck(w.ID, LandCheckOptions{
		RepoPath:        repo,
		CandidateTipSHA: candidate,
		TargetRef:       "refs/heads/feature",
	}, now.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "target_local_ref") {
		t.Fatalf("feature ref must not replace the bound target branch, got %v", err)
	}

	landingGit(t, repo, "switch", "feature")
	report, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "wrong_target_worktree" {
		t.Fatalf("feature-branch worktree must not certify the bound main target: %+v", report)
	}
}

func TestLandCheckCertifiesContainedTargetRefWithoutTargetCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "an atomic target-ref landing certifies without interrupting the active feature checkout",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(3*time.Minute))

	// Reproduce the documented checkout-independent landing escape hatch: the
	// canonical worktree stays on the active feature branch while a guarded
	// update-ref advances main. No worktree has main checked out.
	landingGit(t, repo, "switch", "feature")
	landingGit(t, repo, "update-ref", "refs/heads/main", candidate, base)
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(3*time.Minute+time.Second))

	report, err := store.LandCheck(w.ID, LandCheckOptions{
		CandidateTipSHA: candidate,
		Record:          true,
		Actor:           "codex",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "landed" || !report.CandidateContainedInTarget || report.TargetBranchWorktree {
		t.Fatalf("checkout-independent contained target must certify: %+v", report)
	}
}

func TestLandCheckRefusesCheckoutIndependentProofWhenTargetIsCheckedOutElsewhere(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "a live target checkout keeps worktree state authoritative",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(3*time.Minute))

	landingGit(t, repo, "switch", "feature")
	landingGit(t, repo, "update-ref", "refs/heads/main", candidate, base)
	targetCheckout := filepath.Join(t.TempDir(), "main-worktree")
	landingGit(t, repo, "worktree", "add", targetCheckout, "main")
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(3*time.Minute+time.Second))

	report, err := store.LandCheck(w.ID, LandCheckOptions{
		CandidateTipSHA: candidate,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "wrong_target_worktree" {
		t.Fatalf("a target checked out outside the bound worktree must refuse: %+v", report)
	}
}

func TestLandCheckRejectsSamePathRepositoryReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 2, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "remote", "add", "origin", "https://source.example.invalid/acme/product.git")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "merge", "--ff-only", candidate)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "repository identity remains bound at the target path",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(3*time.Minute))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(3*time.Minute+time.Second))
	proof, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate, Record: true}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "landed" {
		t.Fatalf("initial repository proof = %+v", proof)
	}

	replacementParent := t.TempDir()
	replacement := filepath.Join(replacementParent, "replacement")
	landingGit(t, replacementParent, "clone", "--no-local", repo, replacement)
	landingGit(t, replacement, "remote", "set-url", "origin", "https://other.example.invalid/acme/product.git")
	retired := repo + "-retired"
	if err := os.Rename(repo, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, repo); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate}, now.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Fatalf("same-path replacement with another origin must fail repository identity, got %v", err)
	}
}

func TestPublicationProofIsAttachedDiscoverableAndRequiredForDispatchClose(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 13, 22, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	landingGit(t, filepath.Dir(remote), "init", "--bare", remote)
	landingGit(t, repo, "remote", "add", "origin", remote)
	landingGit(t, repo, "push", "-u", "origin", "main")
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "candidate is on bound main and origin main before close",
		GitDelivery:      publishedGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	if _, err := store.Advance(w.ID, AdvanceInput{
		To:             "verifying",
		Actor:          "codex",
		ArtifactID:     "commit:" + candidate,
		VerificationID: "check:focused-tests",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	landingGit(t, repo, "merge", "--ff-only", candidate)
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(3*time.Minute+time.Second))

	localOnly, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate, Record: true, Actor: "codex"}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if localOnly.Status != "awaiting_publication" || !localOnly.CandidateContainedInTarget || localOnly.CandidateContainedInPublication {
		t.Fatalf("local-main-only state must remain actionable: %+v", localOnly)
	}
	beforePublish, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if contains(beforePublish.EvidenceRefs, localOnly.EvidenceRef) {
		t.Fatalf("non-successful land check must not become close proof: %+v", beforePublish.EvidenceRefs)
	}
	bench, err := store.Workbench(now.Add(5*time.Minute), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, bench.Open, w.ID)
	if item.LandingStatus != "awaiting_publication" {
		t.Fatalf("workbench must surface publication debt: %+v", item)
	}
	blocked, err := store.DispatchClosePlan(now.Add(6*time.Minute), DispatchClosePlanOptions{Actor: "codex", WorkID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Selected != nil || blocked.ClosePlan != nil || blocked.Status != "awaiting_proof" {
		t.Fatalf("dispatch must not offer close before discoverable current delivery proof: %+v", blocked)
	}

	landingGit(t, repo, "push", "origin", "main")
	proof, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate, Record: true, Actor: "codex"}, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "landed" || !proof.CandidateContainedInPublication {
		t.Fatalf("published proof = %+v", proof)
	}
	afterPublish, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(afterPublish.EvidenceRefs, proof.EvidenceRef) {
		t.Fatalf("successful proof must be indexed on its WorkEnvelope: %+v", afterPublish.EvidenceRefs)
	}
	bench, err = store.Workbench(now.Add(8*time.Minute), WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	item = findWorkbenchItem(t, bench.Open, w.ID)
	if item.LandingStatus != "ready_to_close" || !contains(item.EvidenceRefs, proof.EvidenceRef) {
		t.Fatalf("workbench must discover current delivery proof: %+v", item)
	}
	ready, err := store.DispatchClosePlan(now.Add(9*time.Minute), DispatchClosePlanOptions{Actor: "codex", WorkID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Selected == nil || ready.ClosePlan == nil || !contains(ready.ClosePlan.ProofRefs, proof.EvidenceRef) {
		t.Fatalf("dispatch close must consume the attached delivery proof: %+v", ready)
	}
}

func activateLandingWork(t *testing.T, store *Store, workID string, now time.Time) {
	t.Helper()
	if _, err := store.Claim(workID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(workID, AdvanceInput{To: "running", Actor: "codex"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

type landingProofFixture struct {
	store     *Store
	repo      string
	base      string
	candidate string
	workID    string
	proof     LandCheckReport
	scratch   string
}

func newLandingProofFixture(t *testing.T, now time.Time) landingProofFixture {
	t.Helper()
	repo, base, candidate := sharedLandingLandedRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "proof content remains exact", GitDelivery: localGitDelivery(repo, base)}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)
	attachLandingCommit(t, store, w.ID, candidate, now.Add(3*time.Minute))
	completeSourceBoundProofForWork(t, store, w.ID, "codex", repo, now.Add(3*time.Minute+time.Second))
	proof, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: candidate, TargetRef: "main", Record: true}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "landed" {
		t.Fatalf("fixture proof = %+v", proof)
	}
	return landingProofFixture{store: store, repo: repo, base: base, candidate: candidate, workID: w.ID, proof: proof, scratch: t.TempDir()}
}

func attachLandingCommit(t *testing.T, store *Store, workID, sha string, now time.Time) {
	t.Helper()
	attachLandingArtifact(t, store, workID, "commit:"+sha, now)
}

func attachLandingArtifact(t *testing.T, store *Store, workID, artifact string, now time.Time) {
	t.Helper()
	w, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(workID, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: artifact}, now); err != nil {
		t.Fatal(err)
	}
}

// landingGoldenFixture holds three prebuilt git repositories that this file's
// tests would otherwise reconstruct from scratch via repeated `git
// init`/`commit`/`switch`/`merge` subprocess chains (initLandingRepo alone is
// 6 subprocess spawns; the full init+feature+switch+merge chain is 12).
// dispatch_plan_test.go's TestMain builds this once per test binary process,
// before any test runs, and tears it down after m.Run() returns. Each test
// that needs a repo copies the relevant golden tree into its own
// t.TempDir() via one of the shared*Repo helpers below, so the copy is a
// fully independent repository: whatever git commands the test runs
// afterward mutate only its own copy, never the shared golden trees.
type landingGoldenFixture struct {
	baseDir string // init + README "base" commit on main
	baseSHA string

	readyDir          string // baseDir + "feature" branch/commit, HEAD back on main (unmerged)
	readyCandidateSHA string

	landedDir          string // readyDir fast-forward merged into main
	landedCandidateSHA string
}

var landingGolden *landingGoldenFixture

// setupLandingGoldenFixtures builds the shared landing-repo fixtures under a
// fresh temp root and returns that root (for later os.RemoveAll cleanup) plus
// any construction error. It must run once, before any test in this package
// executes; TestMain in dispatch_plan_test.go calls it ahead of m.Run().
func setupLandingGoldenFixtures() (string, error) {
	root, err := os.MkdirTemp("", "compound-work-landing-golden-*")
	if err != nil {
		return "", err
	}
	g, err := buildLandingGoldenFixture(root)
	if err != nil {
		os.RemoveAll(root)
		return "", err
	}
	landingGolden = g
	return root, nil
}

func buildLandingGoldenFixture(root string) (*landingGoldenFixture, error) {
	g := &landingGoldenFixture{
		baseDir:   filepath.Join(root, "base"),
		readyDir:  filepath.Join(root, "ready"),
		landedDir: filepath.Join(root, "landed"),
	}

	if err := os.MkdirAll(g.baseDir, 0o755); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Compound Test"},
	} {
		if err := landingGoldenGit(g.baseDir, args...); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(g.baseDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.baseDir, "add", "README.md"); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.baseDir, "commit", "-m", "base"); err != nil {
		return nil, err
	}
	baseSHA, err := landingGoldenGitOutput(g.baseDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	g.baseSHA = baseSHA

	// ready = baseDir copied forward, then a feature branch/commit, then back on main (unmerged).
	if err := copyLandingTree(g.baseDir, g.readyDir); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.readyDir, "switch", "-c", "feature"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(g.readyDir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.readyDir, "add", "feature.txt"); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.readyDir, "commit", "-m", "feature"); err != nil {
		return nil, err
	}
	candidateSHA, err := landingGoldenGitOutput(g.readyDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.readyDir, "switch", "main"); err != nil {
		return nil, err
	}
	g.readyCandidateSHA = candidateSHA

	// landed = readyDir copied forward, then fast-forward merged into main.
	if err := copyLandingTree(g.readyDir, g.landedDir); err != nil {
		return nil, err
	}
	if err := landingGoldenGit(g.landedDir, "merge", "--ff-only", candidateSHA); err != nil {
		return nil, err
	}
	g.landedCandidateSHA = candidateSHA

	return g, nil
}

// sharedLandingBaseRepo copies the prebuilt "init + base commit" fixture into
// a fresh t.TempDir(). It is behaviorally equivalent to calling
// initLandingRepo(t) directly: the returned repo is an independent git
// repository the test is free to mutate with further git commands.
func sharedLandingBaseRepo(t *testing.T) (string, string) {
	t.Helper()
	if landingGolden == nil {
		t.Fatal("landing golden fixtures not initialized (TestMain must build them before tests run)")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := copyLandingTree(landingGolden.baseDir, repo); err != nil {
		t.Fatalf("copy landing base fixture: %v", err)
	}
	return repo, landingGolden.baseSHA
}

// sharedLandingReadyRepo copies the prebuilt "init + feature commit + switch
// main" fixture (unmerged) into a fresh t.TempDir(). It is behaviorally
// equivalent to calling initLandingRepo(t), then landingFeatureCommit(t,
// repo), then landingGit(t, repo, "switch", "main").
func sharedLandingReadyRepo(t *testing.T) (string, string, string) {
	t.Helper()
	if landingGolden == nil {
		t.Fatal("landing golden fixtures not initialized (TestMain must build them before tests run)")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := copyLandingTree(landingGolden.readyDir, repo); err != nil {
		t.Fatalf("copy landing ready fixture: %v", err)
	}
	return repo, landingGolden.baseSHA, landingGolden.readyCandidateSHA
}

// sharedLandingLandedRepo copies the prebuilt "init + feature commit + switch
// main + merge --ff-only" fixture into a fresh t.TempDir(). It is
// behaviorally equivalent to calling initLandingRepo(t), then
// landingFeatureCommit(t, repo), then landingGit(t, repo, "switch", "main"),
// then landingGit(t, repo, "merge", "--ff-only", candidate).
func sharedLandingLandedRepo(t *testing.T) (string, string, string) {
	t.Helper()
	if landingGolden == nil {
		t.Fatal("landing golden fixtures not initialized (TestMain must build them before tests run)")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := copyLandingTree(landingGolden.landedDir, repo); err != nil {
		t.Fatalf("copy landing landed fixture: %v", err)
	}
	return repo, landingGolden.baseSHA, landingGolden.landedCandidateSHA
}

// copyLandingTree recursively copies a git repository directory (including
// .git) into a new location so the destination is a fully independent
// repository the caller may mutate freely without affecting src.
func copyLandingTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
}

// landingGoldenGit and landingGoldenGitOutput are error-returning
// counterparts of landingGit/landingGitOutput for use while building the
// golden fixtures in TestMain, before any *testing.T exists. (Production
// code already owns a similarly named runLandingGitOutput in land_check.go;
// these are test-fixture-construction only and intentionally distinct.)
func landingGoldenGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

func landingGoldenGitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func initLandingRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	landingGit(t, repo, "init", "-b", "main")
	landingGit(t, repo, "config", "user.email", "test@example.invalid")
	landingGit(t, repo, "config", "user.name", "Compound Test")
	writeLandingFile(t, filepath.Join(repo, "README.md"), "base\n")
	landingGit(t, repo, "add", "README.md")
	landingGit(t, repo, "commit", "-m", "base")
	return repo, landingGitOutput(t, repo, "rev-parse", "HEAD")
}

func landingFeatureCommit(t *testing.T, repo string) string {
	t.Helper()
	landingGit(t, repo, "switch", "-c", "feature")
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "feature\n")
	landingGit(t, repo, "add", "feature.txt")
	landingGit(t, repo, "commit", "-m", "feature")
	return landingGitOutput(t, repo, "rev-parse", "HEAD")
}

func landingGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func landingGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeLandingFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func localGitDelivery(repo, base string) *envelope.GitDeliveryContract {
	return &envelope.GitDeliveryContract{
		TargetWorktreeRoot: repo,
		TargetLocalRef:     "refs/heads/main",
		ExpectedBaseSHA:    base,
		PublicationMode:    "local_only",
	}
}

func publishedGitDelivery(repo, base string) *envelope.GitDeliveryContract {
	return &envelope.GitDeliveryContract{
		TargetWorktreeRoot: repo,
		TargetLocalRef:     "refs/heads/main",
		ExpectedBaseSHA:    base,
		PublicationMode:    "required",
		PublicationRef:     "refs/remotes/origin/main",
	}
}

// TestCloseRefusalNamesMissingPostLandProof reproduces the recorded p13/p15
// beat (keeper log, vacation-burst-20260813, pulses 13-15; first-attempt
// landcheck_87ffb48c4f08 for work_7f2587ddf89d): land-check runs at a landed
// tip, records "landed" against the pre-land proof battery, and only afterwards
// does the post-land battery's proof-check:pfrs_* store into the envelope's
// verification_ids. Close then refuses with the "proof tip X does not match
// current stored proof Y tip X" message quoting two identical tips -- a
// nonsense "proof mismatch" that cost a keeper rerun to diagnose both times.
// The refusal must instead detect the exact-shaped condition (recorded proof
// cites a verification ref no longer the latest attached proof) and name the
// missing post-land verification proof.
func TestCloseRefusalNamesMissingPostLandProof(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 8, 14, 6, 31, 0, 0, time.UTC)
	fixture := newLandingProofFixture(t, now)
	proof := fixture.proof
	if proof.ProofVerificationRef == "" || proof.ProofHeadSHA != fixture.candidate {
		t.Fatalf("fixture recorded proof must cite the landed tip: %+v", proof)
	}

	// The post-land battery completes at the identical landed tip, through the
	// real governed completion path. Its proof-check:pfrs_* verification stores
	// into the envelope only now -- after the recorded land-check -- exactly as
	// in the beat.
	completed := completeSourceBoundProofForWork(t, fixture.store, fixture.workID, "codex", fixture.repo, now.Add(5*time.Minute))
	verificationRef := completed.VerificationRef
	if verificationRef == proof.ProofVerificationRef {
		t.Fatalf("second battery must store a distinct verification ref: %s", verificationRef)
	}

	current, err := fixture.store.Get(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.store.recordedLandedProofForClose(current, []string{proof.EvidenceRef})
	if err == nil {
		t.Fatal("close must refuse a land-check recorded before its post-land proof stored")
	}
	if strings.Contains(err.Error(), "does not match current stored proof") {
		t.Fatalf("close refusal must not quote a same-tip proof mismatch (misleading, hit twice in production): %v", err)
	}
	if !strings.Contains(err.Error(), "missing post-land") ||
		!strings.Contains(err.Error(), "record a fresh land-check") ||
		!strings.Contains(err.Error(), verificationRef) {
		t.Fatalf("close refusal must name the missing post-land verification proof %s and its repair: %v", verificationRef, err)
	}

	// The documented repair from the beat -- rerun land-check after the
	// post-land proof stored -- must close cleanly. No assertion beyond the
	// refusal shape is relaxed to get here.
	fresh, err := fixture.store.LandCheck(fixture.workID, LandCheckOptions{
		RepoPath:        fixture.repo,
		CandidateTipSHA: fixture.candidate,
		TargetRef:       "main",
		Record:          true,
	}, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "landed" || fresh.ProofVerificationRef != verificationRef {
		t.Fatalf("fresh land-check must certify against the post-land proof: %+v", fresh)
	}
	current, err = fixture.store.Get(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.recordedLandedProofForClose(current, []string{fresh.EvidenceRef}); err != nil {
		t.Fatalf("close must accept the fresh post-land land-check: %v", err)
	}
}

func TestLandCheckReviewedMergePreservesExactProofAcrossContainedMain(t *testing.T) {
	repo, base := sharedLandingBaseRepo(t)
	parents := make([]string, 0, 2)
	for _, side := range []string{"left", "right"} {
		landingGit(t, repo, "switch", "-c", "integration-"+side, base)
		writeLandingFile(t, filepath.Join(repo, side+".txt"), side+"\n")
		landingGit(t, repo, "add", side+".txt")
		landingGit(t, repo, "commit", "-m", side+" candidate")
		parents = append(parents, landingGitOutput(t, repo, "rev-parse", "HEAD"))
	}
	landingGit(t, repo, "switch", "-c", "reviewed-integration", parents[0])
	landingGit(t, repo, "merge", "--no-ff", "-m", "reviewed sibling integration", parents[1])
	head := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w, err := store.Create(CreateInput{
		Owner: "codex", SuccessPredicate: "exact reviewed merge reaches main",
		GitDelivery: localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	attachLandingCommit(t, store, w.ID, head, now.Add(time.Second))
	completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, head, now.Add(2*time.Second))
	read := func() LandCheckReport {
		t.Helper()
		report, err := store.LandCheck(w.ID, LandCheckOptions{RepoPath: repo, CandidateTipSHA: head, TargetRef: "main"}, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	initial := read()
	if initial.Status != "ready_to_land" || initial.ProofVerificationRef == "" {
		t.Fatalf("initial merged candidate: %+v", initial)
	}
	for _, parent := range parents {
		// Reset only the synthetic fixture to exercise each possible next land.
		landingGit(t, repo, "reset", "--hard", base)
		landingGit(t, repo, "merge", "--ff-only", parent)
		report := read()
		if report.Status != "ready_to_land" || report.ExpectedBaseSHA != base ||
			report.ProofHeadSHA != head || report.ProofVerificationRef != initial.ProofVerificationRef {
			t.Fatalf("contained parent changed exact proof: %+v", report)
		}
	}
	writeLandingFile(t, filepath.Join(repo, "outside.txt"), "not in reviewed integration\n")
	landingGit(t, repo, "add", "outside.txt")
	landingGit(t, repo, "commit", "-m", "unreviewed main successor")
	if report := read(); report.Status != "rebase_required" {
		t.Fatalf("unrelated main must refuse old candidate: %+v", report)
	}
	landingGit(t, repo, "reset", "--hard", parents[1])
	landingGit(t, repo, "merge", "--ff-only", head)
	report := read()
	if report.Status != "landed" || report.ProofVerificationRef != initial.ProofVerificationRef || report.ProofHeadSHA != head {
		t.Fatalf("guarded fast-forward must use original merged-tip proof: %+v", report)
	}
}
