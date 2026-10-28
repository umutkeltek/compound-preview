package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// The Workbench delivery projection evaluates every Work's Git delivery
// independently. These tests pin the independence contract: two Works bound to
// the same repository and the same candidate tip must never inherit each
// other's proof, publication, base-lineage, or error outcomes, and every
// projection request must reread the mutable repository facts it consumed.

// landingDeliveryWork creates one WorkEnvelope bound to contract, walks it to
// verifying, and attaches the candidate commit artifact so the Workbench
// delivery projection evaluates it.
func landingDeliveryWork(t *testing.T, store *Store, contract *envelope.GitDeliveryContract, predicate, candidate string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: predicate,
		GitDelivery:      contract,
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
	updated, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// landingProcessingOrder returns the two Works in the order one Workbench call
// visits them: Store.list reads work/work/<id>.json in filename order.
func landingProcessingOrder(a, b envelope.WorkEnvelope) (envelope.WorkEnvelope, envelope.WorkEnvelope) {
	if a.ID < b.ID {
		return a, b
	}
	return b, a
}

func landingProjection(t *testing.T, store *Store, now time.Time) WorkbenchReport {
	t.Helper()
	report, err := store.Workbench(now, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// 1. Same root/tip but valid vs absent proof, both processing orders: verdicts
// and next actions remain correct for each Work.
func TestWorkbenchDeliveryVerdictsStayPerWorkForProofPresence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name       string
		proofFirst bool
	}{
		{name: "proof work processed first", proofFirst: true},
		{name: "proof work processed second", proofFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
			repo, base, candidate := sharedLandingReadyRepo(t)
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alpha := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared tip proof alpha", candidate, now)
			beta := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared tip proof beta", candidate, now)
			first, second := landingProcessingOrder(alpha, beta)
			proofWork, bareWork := first, second
			if !tc.proofFirst {
				proofWork, bareWork = second, first
			}
			completeSourceBoundProofForDetachedCandidate(t, store, proofWork.ID, "codex", repo, candidate, now.Add(3*time.Minute+time.Second))

			report := landingProjection(t, store, now.Add(5*time.Minute))
			proofItem := findWorkbenchItem(t, report.Open, proofWork.ID)
			bareItem := findWorkbenchItem(t, report.Open, bareWork.ID)

			if proofItem.LandingStatus != "awaiting_local_land" {
				t.Fatalf("work with a valid proof at shared tip: landing status %q, next %q", proofItem.LandingStatus, proofItem.NextAction)
			}
			if !strings.Contains(proofItem.NextAction, proofWork.ID) {
				t.Fatalf("proof work next action must name its own work: %q", proofItem.NextAction)
			}
			if strings.Contains(proofItem.NextAction, bareWork.ID) {
				t.Fatalf("proof work next action leaked the other work identity: %q", proofItem.NextAction)
			}
			if bareItem.LandingStatus != "awaiting_proof" {
				t.Fatalf("work without a proof at shared tip: landing status %q, next %q", bareItem.LandingStatus, bareItem.NextAction)
			}
			if !strings.Contains(bareItem.NextAction, "run fresh canonical certifying proof for attached candidate commit:"+candidate) {
				t.Fatalf("bare work next action must name its own missing proof: %q", bareItem.NextAction)
			}
			if strings.Contains(bareItem.NextAction, proofWork.ID) {
				t.Fatalf("bare work next action leaked the other work identity: %q", bareItem.NextAction)
			}
		})
	}
}

// 2a. Different publication requirements remain independent for the same
// repository and candidate tip.
func TestWorkbenchDeliveryVerdictsStayPerWorkForPublicationRequirements(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
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
	required := landingDeliveryWork(t, store, publishedGitDelivery(repo, base), "shared tip required publication", candidate, now)
	localOnly := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared tip local only", candidate, now)
	landingGit(t, repo, "merge", "--ff-only", candidate)
	for _, w := range []envelope.WorkEnvelope{required, localOnly} {
		completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, candidate, now.Add(3*time.Minute+time.Second))
	}

	report := landingProjection(t, store, now.Add(6*time.Minute))
	requiredItem := findWorkbenchItem(t, report.Open, required.ID)
	localItem := findWorkbenchItem(t, report.Open, localOnly.ID)

	if requiredItem.LandingStatus != "awaiting_publication" {
		t.Fatalf("required publication at shared tip: landing status %q, next %q", requiredItem.LandingStatus, requiredItem.NextAction)
	}
	if !strings.Contains(requiredItem.NextAction, "publish refs/heads/main containing candidate "+candidate+" to refs/remotes/origin/main") {
		t.Fatalf("required publication next action must name its own publication debt: %q", requiredItem.NextAction)
	}
	if localItem.LandingStatus != "awaiting_proof_recording" {
		t.Fatalf("local-only at shared tip: landing status %q, next %q", localItem.LandingStatus, localItem.NextAction)
	}
	if !strings.Contains(localItem.NextAction, localOnly.ID) {
		t.Fatalf("local-only next action must name its own work: %q", localItem.NextAction)
	}
	if strings.Contains(localItem.NextAction, required.ID) {
		t.Fatalf("local-only next action leaked the other work identity: %q", localItem.NextAction)
	}
}

// 2b. Different expected-base requirements remain independent for the same
// repository and candidate tip.
func TestWorkbenchDeliveryVerdictsStayPerWorkForBaseRequirements(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)
	landingGit(t, repo, "switch", "-c", "side-lineage", base)
	writeLandingFile(t, filepath.Join(repo, "side.txt"), "side\n")
	landingGit(t, repo, "add", "side.txt")
	landingGit(t, repo, "commit", "-m", "side lineage")
	foreignBase := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ancestorBound := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared tip ancestor base", candidate, now)
	foreign := landingDeliveryWork(t, store, localGitDelivery(repo, foreignBase), "shared tip foreign base", candidate, now)
	completeSourceBoundProofForDetachedCandidate(t, store, ancestorBound.ID, "codex", repo, candidate, now.Add(3*time.Minute+time.Second))

	report := landingProjection(t, store, now.Add(5*time.Minute))
	ancestorItem := findWorkbenchItem(t, report.Open, ancestorBound.ID)
	foreignItem := findWorkbenchItem(t, report.Open, foreign.ID)

	if ancestorItem.LandingStatus != "awaiting_proof_recording" {
		t.Fatalf("ancestor base at shared tip: landing status %q, next %q", ancestorItem.LandingStatus, ancestorItem.NextAction)
	}
	if !strings.Contains(ancestorItem.NextAction, ancestorBound.ID) {
		t.Fatalf("ancestor base next action must name its own work: %q", ancestorItem.NextAction)
	}
	if foreignItem.LandingStatus != "awaiting_local_land" {
		t.Fatalf("foreign base at shared tip: landing status %q, next %q", foreignItem.LandingStatus, foreignItem.NextAction)
	}
	if !strings.Contains(foreignItem.NextAction, "rebuild candidate "+candidate+" from expected base "+foreignBase) {
		t.Fatalf("foreign base next action must name its own unbuildable base: %q", foreignItem.NextAction)
	}
	if strings.Contains(foreignItem.NextAction, ancestorBound.ID) {
		t.Fatalf("foreign base next action leaked the other work identity: %q", foreignItem.NextAction)
	}
}

// 3. Same failed shared Git input reports an integrity error for every
// affected Work.
func TestWorkbenchReportsSharedGitReadFailureForEveryWork(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alpha := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared read failure alpha", candidate, now)
	beta := landingDeliveryWork(t, store, localGitDelivery(repo, base), "shared read failure beta", candidate, now)

	// Sanity: with the repository present both Works evaluate their own
	// missing proof at the shared tip.
	before := landingProjection(t, store, now.Add(4*time.Minute))
	if item := findWorkbenchItem(t, before.Open, alpha.ID); item.LandingStatus != "awaiting_proof" {
		t.Fatalf("alpha before failure: landing status %q", item.LandingStatus)
	}
	if item := findWorkbenchItem(t, before.Open, beta.ID); item.LandingStatus != "awaiting_proof" {
		t.Fatalf("beta before failure: landing status %q", item.LandingStatus)
	}

	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	report := landingProjection(t, store, now.Add(5*time.Minute))
	for _, w := range []envelope.WorkEnvelope{alpha, beta} {
		item := findWorkbenchItem(t, report.Open, w.ID)
		if item.LandingStatus != "delivery_check_error" {
			t.Fatalf("work %s must report the shared Git failure, got landing status %q, next %q", w.ID, item.LandingStatus, item.NextAction)
		}
		if !strings.Contains(item.NextAction, "rerun compound work land-check "+w.ID+" --tip "+candidate) {
			t.Fatalf("work %s failure next action must name its own repair: %q", w.ID, item.NextAction)
		}
		if !integrityContains(report.Integrity, w.ID+": Git delivery check failed:") {
			t.Fatalf("work %s missing its integrity error for the shared Git failure: %v", w.ID, report.Integrity)
		}
	}
}

// 4. A fresh projection rereads mutable repository facts: ref and publication
// movement, dirty state, origin/identity changes, and a repaired missing
// input. The governed mutation land-check keeps its own fresh reads and the
// projection picks up the proof it records.
func TestWorkbenchProjectionRereadsMutableGitStatePerRequest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
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
		SuccessPredicate: "fresh projection rereads mutable git facts",
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
	completeSourceBoundProofForDetachedCandidate(t, store, w.ID, "codex", repo, candidate, now.Add(3*time.Minute+time.Second))

	step := 5 * time.Minute
	read := func() WorkbenchItem {
		t.Helper()
		now = now.Add(step)
		report := landingProjection(t, store, now)
		return findWorkbenchItem(t, report.Open, w.ID)
	}
	readIntegrity := func() (WorkbenchItem, []string) {
		t.Helper()
		now = now.Add(step)
		report := landingProjection(t, store, now)
		return findWorkbenchItem(t, report.Open, w.ID), report.Integrity
	}

	// Unmerged candidate with a valid proof: local landing still owed.
	if item := read(); item.LandingStatus != "awaiting_local_land" || !strings.Contains(item.NextAction, "fast-forward main to candidate "+candidate) {
		t.Fatalf("unmerged candidate: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}

	// Owned-path dirt in the target worktree is a fresh per-request fact.
	writeLandingFile(t, filepath.Join(repo, "feature.txt"), "uncommitted\n")
	if item := read(); item.LandingStatus != "awaiting_local_land" || !strings.Contains(item.NextAction, "uncommitted changes to files this work owns") || !strings.Contains(item.NextAction, "feature.txt") {
		t.Fatalf("owned dirt: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}
	if err := os.Remove(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatal(err)
	}

	// Target ref movement: the candidate reaching main flips the verdict.
	landingGit(t, repo, "merge", "--ff-only", candidate)
	if item := read(); item.LandingStatus != "awaiting_publication" || !strings.Contains(item.NextAction, "publish refs/heads/main containing candidate "+candidate+" to refs/remotes/origin/main") {
		t.Fatalf("after merge: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}

	// Publication movement: publishing the candidate flips the verdict again.
	landingGit(t, repo, "push", "origin", "main")
	if item := read(); item.LandingStatus != "awaiting_proof_recording" || !strings.Contains(item.NextAction, "record the current delivery proof with compound work land-check "+w.ID) {
		t.Fatalf("after publish: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}

	// Canonical origin identity is a per-request fact: moving the origin
	// remote must fail the bound fingerprint on the next projection.
	landingGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "moved-origin.git"))
	item, integrity := readIntegrity()
	if item.LandingStatus != "delivery_check_error" || !integrityContains(integrity, w.ID+": Git delivery check failed:") {
		t.Fatalf("moved origin: landing status %q, integrity %v", item.LandingStatus, integrity)
	}
	landingGit(t, repo, "remote", "set-url", "origin", remote)
	if item := read(); item.LandingStatus != "awaiting_proof_recording" {
		t.Fatalf("restored origin: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}

	// A missing repository input fails every request until it is repaired.
	backup := filepath.Join(t.TempDir(), "repo-backup")
	if err := copyLandingTree(repo, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if item, integrity := readIntegrity(); item.LandingStatus != "delivery_check_error" || !integrityContains(integrity, w.ID+": Git delivery check failed:") {
		t.Fatalf("removed repository: landing status %q, integrity %v", item.LandingStatus, integrity)
	}
	if err := copyLandingTree(backup, repo); err != nil {
		t.Fatal(err)
	}
	if item := read(); item.LandingStatus != "awaiting_proof_recording" {
		t.Fatalf("repaired repository: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}

	// The governed mutation land-check keeps fresh reads and records the
	// delivery proof the projection then discovers.
	now = now.Add(step)
	recorded, err := store.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: candidate, Record: true, Actor: "codex"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Status != "landed" || !recorded.Recorded {
		t.Fatalf("mutation land-check must certify the current repository state: %+v", recorded)
	}
	if item := read(); item.LandingStatus != "ready_to_close" || !strings.Contains(item.NextAction, "close with the attached current Git delivery proof") {
		t.Fatalf("after recorded proof: landing status %q, next %q", item.LandingStatus, item.NextAction)
	}
}

// 5. Many distinct candidate tips share the common repository reads of one
// request. The counts are deterministic fact-read counts, never wall-clock
// assertions.
func TestLandingGitFactsShareRepositoryReadsAcrossDistinctTips(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	const works = 5
	now := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	tips := make([]string, 0, works)
	for i := 0; i < works; i++ {
		branch := fmt.Sprintf("feature-%d", i)
		landingGit(t, repo, "switch", "-c", branch, base)
		writeLandingFile(t, filepath.Join(repo, branch+".txt"), "feature\n")
		landingGit(t, repo, "add", branch+".txt")
		landingGit(t, repo, "commit", "-m", branch)
		tips = append(tips, landingGitOutput(t, repo, "rev-parse", "HEAD"))
	}
	landingGit(t, repo, "switch", "main")
	// Move main past the base so the target tip differs from every expected
	// base: the three ancestry questions per Work (base→candidate,
	// candidate→target, target→candidate) are then three distinct facts.
	writeLandingFile(t, filepath.Join(repo, "main-moved.txt"), "main moved\n")
	landingGit(t, repo, "add", "main-moved.txt")
	landingGit(t, repo, "commit", "-m", "main moved")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := make([]envelope.WorkEnvelope, 0, works)
	for i, tip := range tips {
		ws = append(ws, landingDeliveryWork(t, store, localGitDelivery(repo, base), fmt.Sprintf("distinct tip %d", i), tip, now))
	}

	// One request-local session: the repository-scoped reads are paid once no
	// matter how many distinct candidate tips the board carries. Only the
	// per-tip facts (object existence, ancestry) scale with the tips.
	shared := newLandingGitFacts()
	for i, w := range ws {
		report, err := store.evaluateLandCheckWithFacts(w, LandCheckOptions{CandidateTipSHA: tips[i]}, now.Add(10*time.Minute), shared)
		if err != nil {
			t.Fatalf("evaluate work %s: %v", w.ID, err)
		}
		// main moved past the shared base, so every unmerged candidate is a
		// per-Work rebase verdict reached through the full lineage walk.
		if report.Status != "rebase_required" {
			t.Fatalf("work %s status %q, want rebase_required", w.ID, report.Status)
		}
	}
	for kind, want := range map[string]int{
		landingReadCanonicalRepo: 1,
		landingReadTargetRef:     1,
		landingReadRefCommit:     1,
		landingReadCurrentBranch: 1,
		landingReadStatus:        1,
		landingReadIdentity:      1,
		landingReadCommitExists:  works + 1,
		landingReadAncestry:      3 * works,
	} {
		if got := shared.readCount(kind); got != want {
			t.Errorf("shared session read %s = %d, want %d", kind, got, want)
		}
	}

	// Without a session (one fresh read set per Work), the same board pays
	// every repository-scoped read once per Work. Pinning that contrast keeps
	// the amortization the session provides honest.
	repoScoped := []string{
		landingReadCanonicalRepo,
		landingReadTargetRef,
		landingReadRefCommit,
		landingReadCurrentBranch,
		landingReadStatus,
		landingReadIdentity,
	}
	freshTotals := map[string]int{}
	for i, w := range ws {
		fresh := newLandingGitFacts()
		if _, err := store.evaluateLandCheckWithFacts(w, LandCheckOptions{CandidateTipSHA: tips[i]}, now.Add(11*time.Minute), fresh); err != nil {
			t.Fatalf("evaluate work %s fresh: %v", w.ID, err)
		}
		for _, kind := range repoScoped {
			freshTotals[kind] += fresh.readCount(kind)
		}
	}
	for _, kind := range repoScoped {
		if got := freshTotals[kind]; got != works {
			t.Errorf("fresh per-work sessions read %s = %d, want %d (the cost the shared session removes)", kind, got, works)
		}
	}
}

// The worktree list is one repository-scoped read: Works over the same
// repository share it even when their delivery verdicts differ.
func TestLandingGitFactsShareWorktreeListReads(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	const works = 3
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	tips := make([]string, 0, works)
	for i := 0; i < works; i++ {
		branch := fmt.Sprintf("side-%d", i)
		landingGit(t, repo, "switch", "-c", branch, base)
		writeLandingFile(t, filepath.Join(repo, branch+".txt"), "side\n")
		landingGit(t, repo, "add", branch+".txt")
		landingGit(t, repo, "commit", "-m", branch)
		tips = append(tips, landingGitOutput(t, repo, "rev-parse", "HEAD"))
	}
	// Leave the target ref checked out nowhere so every evaluation consults
	// the worktree list before classifying the delivery.
	landingGit(t, repo, "switch", "-c", "foreign-checkout")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := make([]envelope.WorkEnvelope, 0, works)
	for i, tip := range tips {
		ws = append(ws, landingDeliveryWork(t, store, localGitDelivery(repo, base), fmt.Sprintf("worktree list tip %d", i), tip, now))
	}
	shared := newLandingGitFacts()
	for i, w := range ws {
		report, err := store.evaluateLandCheckWithFacts(w, LandCheckOptions{CandidateTipSHA: tips[i]}, now.Add(10*time.Minute), shared)
		if err != nil {
			t.Fatalf("evaluate work %s: %v", w.ID, err)
		}
		if report.Status != "wrong_target_worktree" {
			t.Fatalf("work %s status %q, want wrong_target_worktree", w.ID, report.Status)
		}
	}
	if got := shared.readCount(landingReadWorktreeList); got != 1 {
		t.Errorf("shared session read %s = %d, want 1", landingReadWorktreeList, got)
	}
	if got := shared.readCount(landingReadStatus); got != 1 {
		t.Errorf("shared session read %s = %d, want 1", landingReadStatus, got)
	}
}

// Repository identity is keyed by canonical root AND exact expected base, never
// the root alone: two Works in one repository over two different lineages must
// each keep their own identity fact (and their own verdict). A root-only key
// would hand the second lineage the first one's fingerprint and turn its own
// verdict into a spurious delivery_check_error.
func TestWorkbenchDeliveryVerdictsStayPerWorkForRepositoryIdentityBases(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 10, 24, 16, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	candidate := landingFeatureCommit(t, repo)
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "switch", "--orphan", "unrelated-lineage")
	writeLandingFile(t, filepath.Join(repo, "unrelated.txt"), "unrelated\n")
	landingGit(t, repo, "add", "unrelated.txt")
	landingGit(t, repo, "commit", "-m", "unrelated lineage")
	orphanBase := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Two Works per lineage: whichever lineage the projection visits first,
	// the other one must still read its own identity.
	lineageWorks := []envelope.WorkEnvelope{
		landingDeliveryWork(t, store, localGitDelivery(repo, base), "identity main alpha", candidate, now),
		landingDeliveryWork(t, store, localGitDelivery(repo, base), "identity main beta", candidate, now),
	}
	orphanWorks := []envelope.WorkEnvelope{
		landingDeliveryWork(t, store, localGitDelivery(repo, orphanBase), "identity orphan alpha", candidate, now),
		landingDeliveryWork(t, store, localGitDelivery(repo, orphanBase), "identity orphan beta", candidate, now),
	}

	report := landingProjection(t, store, now.Add(6*time.Minute))
	for _, w := range lineageWorks {
		item := findWorkbenchItem(t, report.Open, w.ID)
		if item.LandingStatus != "awaiting_proof" {
			t.Fatalf("main-lineage work %s: landing status %q, next %q", w.ID, item.LandingStatus, item.NextAction)
		}
		if !strings.Contains(item.NextAction, "run fresh canonical certifying proof for attached candidate commit:"+candidate) {
			t.Fatalf("main-lineage work %s next action: %q", w.ID, item.NextAction)
		}
	}
	for _, w := range orphanWorks {
		item := findWorkbenchItem(t, report.Open, w.ID)
		if item.LandingStatus != "awaiting_local_land" {
			t.Fatalf("orphan-lineage work %s: landing status %q, next %q", w.ID, item.LandingStatus, item.NextAction)
		}
		if !strings.Contains(item.NextAction, "rebuild candidate "+candidate+" from expected base "+orphanBase) {
			t.Fatalf("orphan-lineage work %s next action must name its own lineage base: %q", w.ID, item.NextAction)
		}
	}
}
