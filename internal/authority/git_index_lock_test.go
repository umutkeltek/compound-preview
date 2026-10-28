package authority

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

func indexRecoveryIdle(context.Context, ...string) error { return nil }

func TestGitIndexLockArtifactInterruptedWriteNeverPoisonsFinalName(t *testing.T) {
	home := t.TempDir()
	err := writeIndexRecoveryArtifactUsing(home, "act_synthetic.outcome.json", map[string]string{"outcome": "completed"}, func(f *os.File, body []byte) error {
		if _, err := f.Write(body[:8]); err != nil {
			return err
		}
		return fmt.Errorf("interrupted artifact write")
	})
	if err == nil {
		t.Fatal("injected failure ignored")
	}
	var value map[string]string
	if err := readIndexRecoveryArtifact(home, "act_synthetic.outcome.json", &value); !os.IsNotExist(err) {
		t.Fatalf("partial final name became visible: %v", err)
	}
	if err := writeIndexRecoveryArtifact(home, "act_synthetic.outcome.json", map[string]string{"outcome": "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := writeIndexRecoveryArtifact(home, "act_synthetic.outcome.json", map[string]string{"outcome": "completed"}); err == nil {
		t.Fatal("overwrote existing final artifact")
	}
	if err := readIndexRecoveryArtifact(home, "act_synthetic.outcome.json", &value); err != nil || value["outcome"] != "failed" {
		t.Fatal("final artifact changed")
	}
}

func newIndexRecoveryFixture(t *testing.T) (string, GitIndexLockPlan, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	home := t.TempDir()
	if err := homepkg.Init(home, "test", now); err != nil {
		t.Fatal(err)
	}
	s, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(work.CreateInput{Owner: "operator:test", Actor: "operator:test", State: "ready", SuccessPredicate: "recover only exact reviewed stale index lock"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "operator:test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, now); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = gitrepository.ReadEnvironment()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git %v: %v %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Synthetic Test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("committed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("commit", "-m", "seed")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("foreign WIP\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, ".git/index.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildGitIndexLockPlan(context.Background(), repo, w.ID, "operator:test", indexRecoveryIdle, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordGitIndexLockPlan(home, plan, now); err != nil {
		t.Fatal(err)
	}
	return home, plan, now
}

func TestGitIndexLockRawGitPathStillDenied(t *testing.T) {
	_, plan, now := newIndexRecoveryFixture(t)
	action := indexRecoveryAction(plan, "manual-lock-move", "manual_move", filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock"), now)
	decision := policy.Evaluate(action, policy.DefaultConfig(), now)
	if policy.Allowed(decision) {
		t.Fatalf("generic .git protection weakened: %+v", decision)
	}
}

func TestGitIndexLockConcurrentApplyDoesNotStealOutcome(t *testing.T) {
	home, plan, now := newIndexRecoveryFixture(t)
	review, err := ReviewGitIndexLockPlan(home, plan.ID, GitIndexLockWindow{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(4 * time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	first := true
	probe := func(context.Context, ...string) error {
		if first {
			first = false
			close(entered)
			<-release
		}
		return nil
	}
	go func() {
		_, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, probe, time.Now)
		finished <- err
	}()
	<-entered
	_, secondErr := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now)
	close(release)
	if secondErr == nil {
		t.Error("concurrent apply admitted")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestGitIndexLockInterruptedMoveReconcilesWithoutSecondMove(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%v", replace), func(t *testing.T) {
			home, plan, now := newIndexRecoveryFixture(t)
			review, err := ReviewGitIndexLockPlan(home, plan.ID, GitIndexLockWindow{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(4 * time.Minute)}, now)
			if err != nil {
				t.Fatal(err)
			}
			action := indexRecoveryAction(plan, review.ID, "recover_reviewed_stale_index_lock", plan.State.Snapshot.Repo, now)
			action.EvidenceRefs = append(action.EvidenceRefs, "git-index-lock-review:"+review.ID, review.Window.EvidenceRef)
			cfg, _, err := policy.LoadConfig(home)
			if err != nil {
				t.Fatal(err)
			}
			decision := policy.Evaluate(action, cfg, now)
			in := work.ReceiptedActionInput{Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Summary: "awaiting exact index-lock recovery outcome", MoveType: "control_plane_repair", ProofRefs: []string{"git-index-lock-review:" + review.ID, review.Window.EvidenceRef}, BeforeRefs: []string{"sha256:" + plan.Digest}}
			store, err := work.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ReserveReceiptedAction(plan.WorkID, in, now, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			_, err = gitrepository.RecoverIndexLockJournaled(context.Background(), plan.State, indexRecoveryIdle, func() error { return nil }, func(locator gitrepository.IndexLockRecoveryResult) error {
				return writeIndexRecoveryArtifact(home, action.ID+".prepared.json", locator)
			})
			if err != nil {
				t.Fatal(err)
			}
			if replace {
				if err := os.WriteFile(filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock"), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now)
			if replace {
				if err == nil || result.Outcome != "failed" {
					t.Fatalf("replacement was not failed: %+v %v", result, err)
				}
				data, _ := os.ReadFile(filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock"))
				if string(data) != "replacement" {
					t.Fatal("replacement changed")
				}
			} else if err != nil || result.Outcome != "completed" {
				t.Fatalf("reconciliation: %+v %v", result, err)
			}
			if result.ReceiptID == "" || result.TransitionID == "" || !result.Replayed {
				t.Fatal("missing reconciled governed outcome")
			}
		})
	}
}

func TestGitIndexLockReviewRequiresBoundedExclusiveWindow(t *testing.T) {
	home, plan, now := newIndexRecoveryFixture(t)
	for _, window := range []GitIndexLockWindow{
		{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", ExpiresAt: now.Add(time.Minute)},
		{ReviewedBy: "codex:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(time.Minute)},
		{ReviewedBy: "operator:test", WritersPaused: true, ExpiresAt: now.Add(time.Minute)},
		{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(time.Hour)},
	} {
		if _, err := ReviewGitIndexLockPlan(home, plan.ID, window, now); err == nil {
			t.Fatal("unsafe review accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock")); err != nil {
		t.Fatal("review mutated lock")
	}
}

func TestGitIndexLockGovernedRecoveryAndReplay(t *testing.T) {
	home, plan, now := newIndexRecoveryFixture(t)
	review, err := ReviewGitIndexLockPlan(home, plan.ID, GitIndexLockWindow{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(4 * time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReceiptID == "" || result.TransitionID == "" || result.Outcome != "completed" {
		t.Fatalf("missing governed chain: %+v", result)
	}
	lock := filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock")
	if err := os.WriteFile(lock, []byte("new live lock"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.ReceiptID != result.ReceiptID {
		t.Fatal("did not replay exact historical outcome")
	}
	data, err := os.ReadFile(lock)
	if err != nil || string(data) != "new live lock" {
		t.Fatal("retry damaged new lock")
	}
}

func TestGitIndexLockOutcomeTamperingCannotBecomeReceipt(t *testing.T) {
	home, plan, now := newIndexRecoveryFixture(t)
	review, err := ReviewGitIndexLockPlan(home, plan.ID, GitIndexLockWindow{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(4 * time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now); err != nil {
		t.Fatal(err)
	}
	action := indexRecoveryAction(plan, review.ID, "recover_reviewed_stale_index_lock", plan.State.Snapshot.Repo, now)
	var saved gitIndexLockOutcome
	if err := readIndexRecoveryArtifact(home, action.ID+".outcome.json", &saved); err != nil {
		t.Fatal(err)
	}
	for _, recompute := range []bool{false, true} {
		tampered := saved
		tampered.Recovery.Phase = "prepared"
		tampered.Recovery.Changed = false
		if recompute {
			tampered.Digest = ""
			tampered.Digest = indexRecoveryDigest(tampered)
		}
		body, err := json.Marshal(tampered)
		if err != nil {
			t.Fatal(err)
		}
		path, err := indexRecoveryArtifactPath(home, action.ID+".outcome.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, time.Now); err == nil {
			t.Fatal("tampered outcome accepted")
		}
	}
}

func TestGitIndexLockApplyRejectsExpiredOrForeignReview(t *testing.T) {
	home, plan, now := newIndexRecoveryFixture(t)
	review, err := ReviewGitIndexLockPlan(home, plan.ID, GitIndexLockWindow{ReviewedBy: "operator:test", EvidenceRef: "evidence:actual-writer-agreement", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, "foreign:actor", indexRecoveryIdle, time.Now); err == nil {
		t.Fatal("foreign actor accepted")
	}
	if _, err := ApplyGitIndexLockRecovery(context.Background(), home, plan.ID, review.ID, plan.Actor, indexRecoveryIdle, func() time.Time { return now.Add(2 * time.Minute) }); err == nil {
		t.Fatal("expired window accepted")
	}
	if _, err := os.Stat(filepath.Join(plan.State.Snapshot.Repo, ".git/index.lock")); err != nil {
		t.Fatal("denied apply removed lock")
	}
}
