package work

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloseRejectsTargetRefMoveBeforePreparedReferenceTransaction(t *testing.T) {
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	fixture := newLandingProofFixture(t, now)
	lateTip := landingEmptyDescendant(t, fixture.repo, "late-target")
	before := snapshotLifecycleAuthority(t, fixture.store)

	realStart := startGitReferenceTransaction
	startGitReferenceTransaction = func(repo string, refs []gitReferenceExpectation) (gitReferenceTransaction, error) {
		cmd := exec.Command("git", "-C", repo, "update-ref", "refs/heads/main", lateTip)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, &testGitCommandError{args: "update-ref refs/heads/main", err: err, output: string(out)}
		}
		return realStart(repo, refs)
	}
	t.Cleanup(func() { startGitReferenceTransaction = realStart })

	_, err := fixture.store.Close(fixture.workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "target movement must abort close",
		ProofRefs: []string{fixture.proof.EvidenceRef},
	}, now.Add(10*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "prepare Git reference transaction") {
		t.Fatalf("target move immediately before prepare must fail CAS, got %v", err)
	}
	after := snapshotLifecycleAuthority(t, fixture.store)
	assertLifecycleAuthorityUnchanged(t, before, after)
}

func TestCloseRejectsPublicationRefMoveBeforePreparedReferenceTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 3, 30, 0, 0, time.UTC)
	fixture := newPublishedLandingProofFixture(t, now)
	lateTip := landingEmptyDescendant(t, fixture.repo, "late-publication")
	before := snapshotLifecycleAuthority(t, fixture.store)

	realStart := startGitReferenceTransaction
	startGitReferenceTransaction = func(repo string, refs []gitReferenceExpectation) (gitReferenceTransaction, error) {
		cmd := exec.Command("git", "-C", repo, "update-ref", "refs/remotes/origin/main", lateTip)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, &testGitCommandError{args: "update-ref refs/remotes/origin/main", err: err, output: string(out)}
		}
		return realStart(repo, refs)
	}
	t.Cleanup(func() { startGitReferenceTransaction = realStart })

	_, err := fixture.store.Close(fixture.workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "publication movement must abort close",
		ProofRefs: []string{fixture.proof.EvidenceRef},
	}, now.Add(10*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "prepare Git reference transaction") {
		t.Fatalf("publication move immediately before prepare must fail CAS, got %v", err)
	}
	after := snapshotLifecycleAuthority(t, fixture.store)
	assertLifecycleAuthorityUnchanged(t, before, after)
}

func TestCloseRejectsRepositoryIdentityMoveBeforeLockedPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 3, 45, 0, 0, time.UTC)
	fixture := newPublishedLandingProofFixture(t, now)
	before := snapshotLifecycleAuthority(t, fixture.store)

	realStart := startGitReferenceTransaction
	startGitReferenceTransaction = func(repo string, refs []gitReferenceExpectation) (gitReferenceTransaction, error) {
		tx, err := realStart(repo, refs)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("git", "-C", repo, "config", "remote.origin.url", "https://replacement.example.invalid/repo.git")
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = tx.Abort()
			return nil, &testGitCommandError{args: "config remote.origin.url", err: err, output: string(out)}
		}
		return tx, nil
	}
	t.Cleanup(func() { startGitReferenceTransaction = realStart })

	_, err := fixture.store.Close(fixture.workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "identity must still match after exact ref locks are prepared",
		ProofRefs: []string{fixture.proof.EvidenceRef},
	}, now.Add(10*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "final repository identity revalidation") {
		t.Fatalf("repository identity move before locked persistence must fail, got %v", err)
	}
	after := snapshotLifecycleAuthority(t, fixture.store)
	assertLifecycleAuthorityUnchanged(t, before, after)
}

func TestRepositoryIdentityIsNotClaimedAtomicThroughTerminalPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 3, 50, 0, 0, time.UTC)
	fixture := newPublishedLandingProofFixture(t, now)

	realStart := startGitReferenceTransaction
	startGitReferenceTransaction = func(repo string, refs []gitReferenceExpectation) (gitReferenceTransaction, error) {
		tx, err := realStart(repo, refs)
		if err != nil {
			return nil, err
		}
		return &identityMutationAtCommitTransaction{inner: tx, repo: repo}, nil
	}
	t.Cleanup(func() { startGitReferenceTransaction = realStart })

	closed, err := fixture.store.Close(fixture.workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "ref atomicity does not overclaim repository config locking",
		ProofRefs: []string{fixture.proof.EvidenceRef},
	}, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("close result = %+v", closed)
	}
	if err := fixture.store.validateBoundGitDelivery(closed.Work); err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Fatalf("post-persistence origin mutation must sit outside the atomic ref guarantee, got %v", err)
	}
}

func TestPreparedReferenceTransactionLocksTargetAndPublicationThroughTerminalPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 14, 4, 0, 0, 0, time.UTC)
	fixture := newPublishedLandingProofFixture(t, now)
	lateTarget := landingEmptyDescendant(t, fixture.repo, "concurrent-target")
	latePublication := landingEmptyDescendant(t, fixture.repo, "concurrent-publication")
	commitEntered := make(chan struct{})
	release := make(chan struct{})

	realStart := startGitReferenceTransaction
	startGitReferenceTransaction = func(repo string, refs []gitReferenceExpectation) (gitReferenceTransaction, error) {
		tx, err := realStart(repo, refs)
		if err != nil {
			return nil, err
		}
		return &commitGateReferenceTransaction{inner: tx, entered: commitEntered, release: release}, nil
	}
	t.Cleanup(func() { startGitReferenceTransaction = realStart })

	type closeResult struct {
		result CloseResult
		err    error
	}
	done := make(chan closeResult, 1)
	go func() {
		result, err := fixture.store.Close(fixture.workID, CloseInput{
			Outcome:   "completed",
			MoveType:  "outward_delta",
			Actor:     "codex",
			Summary:   "refs remain fixed through close persistence",
			ProofRefs: []string{fixture.proof.EvidenceRef},
		}, now.Add(10*time.Minute))
		done <- closeResult{result: result, err: err}
	}()

	select {
	case <-commitEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not reach Git commit after terminal persistence")
	}
	assertTerminalClosePersistedBeforeGitCommit(t, fixture.store, fixture.workID)
	assertReferenceLocked(t, fixture.repo, "refs/heads/main", lateTarget)
	assertReferenceLocked(t, fixture.repo, "refs/remotes/origin/main", latePublication)
	close(release)

	select {
	case closed := <-done:
		if closed.err != nil {
			t.Fatal(closed.err)
		}
		if closed.result.Work.State != "completed" {
			t.Fatalf("close result = %+v", closed.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish after releasing prepared transaction block")
	}
	if got := landingGitOutput(t, fixture.repo, "rev-parse", "refs/heads/main"); got != fixture.candidate {
		t.Fatalf("close committed against moved target %s, want %s", got, fixture.candidate)
	}
	if got := landingGitOutput(t, fixture.repo, "rev-parse", "refs/remotes/origin/main"); got != fixture.candidate {
		t.Fatalf("close committed against moved publication %s, want %s", got, fixture.candidate)
	}
}

type commitGateReferenceTransaction struct {
	inner   gitReferenceTransaction
	entered chan struct{}
	release chan struct{}
}

func (tx *commitGateReferenceTransaction) Commit() error {
	close(tx.entered)
	<-tx.release
	return tx.inner.Commit()
}

func (tx *commitGateReferenceTransaction) Abort() error { return tx.inner.Abort() }

type identityMutationAtCommitTransaction struct {
	inner gitReferenceTransaction
	repo  string
}

func (tx *identityMutationAtCommitTransaction) Commit() error {
	cmd := exec.Command("git", "-C", tx.repo, "config", "remote.origin.url", "https://post-persistence.example.invalid/repo.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = tx.inner.Abort()
		return &testGitCommandError{args: "config remote.origin.url", err: err, output: string(out)}
	}
	return tx.inner.Commit()
}

func (tx *identityMutationAtCommitTransaction) Abort() error { return tx.inner.Abort() }

func assertTerminalClosePersistedBeforeGitCommit(t *testing.T, store *Store, workID string) {
	t.Helper()
	w, events, err := store.Show(workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "completed" || w.IDs.TransitionID == "" {
		t.Fatalf("terminal WorkEnvelope not persisted before Git commit: %+v", w)
	}
	if len(events) == 0 {
		t.Fatal("terminal lifecycle event not persisted before Git commit")
	}
	closed := events[len(events)-1]
	if closed.To != "completed" || len(closed.Attached) != 4 || closed.WorkSHA256 == "" || closed.ReceiptSHA256 == "" {
		t.Fatalf("terminal lifecycle event incomplete before Git commit: %+v", closed)
	}
	for _, id := range closed.Attached {
		if exists, err := store.eventExists(id); err != nil || !exists {
			t.Fatalf("terminal envelope %s not persisted before Git commit: %v", id, err)
		}
	}
}

func assertReferenceLocked(t *testing.T, repo, ref, next string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "update-ref", ref, next)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "cannot lock ref") {
		t.Fatalf("concurrent mutation of %s must fail after terminal persistence and before Git commit: err=%v output=%s", ref, err, out)
	}
}

func newPublishedLandingProofFixture(t *testing.T, now time.Time) landingProofFixture {
	t.Helper()
	repo, base := initLandingRepo(t)
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
		SuccessPredicate: "published refs remain fixed through terminal close",
		GitDelivery:      publishedGitDelivery(repo, base),
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
		t.Fatalf("published fixture proof = %+v", proof)
	}
	return landingProofFixture{store: store, repo: repo, base: base, candidate: candidate, workID: w.ID, proof: proof, scratch: t.TempDir()}
}

func landingEmptyDescendant(t *testing.T, repo, branch string) string {
	t.Helper()
	landingGit(t, repo, "switch", "-c", branch)
	landingGit(t, repo, "commit", "--allow-empty", "-m", branch)
	tip := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	return tip
}

type testGitCommandError struct {
	args   string
	err    error
	output string
}

func (e *testGitCommandError) Error() string {
	return e.args + ": " + e.err.Error() + ": " + strings.TrimSpace(e.output)
}
