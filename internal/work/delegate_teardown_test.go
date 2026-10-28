// Falsifiers for delegate teardown reaping its own artifacts: the git
// worktree lock over the delegate's workspace, and a tmux pane left behind by
// a teardown whose Kill failed. Same idiom as delegate_test.go -- the
// orchestrator stays pure (fake SessionRunner, fake WorktreeLockReaper) and
// each test is proved red by a single-line mutation of the orchestrator, then
// reverted.
package work

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"compound/internal/runattempt"
)

// fakeWorktreeLockReaper is the test double for the WorktreeLockReaper seam.
// It records every unlock so "released nothing" and "released a stranger's
// lock" are both observable failures, not silent ones.
type fakeWorktreeLockReaper struct {
	mu sync.Mutex

	locked    map[string]string // worktree -> reason
	unlocked  []string
	stateErr  error
	unlockErr error
}

func newFakeLockReaper() *fakeWorktreeLockReaper {
	return &fakeWorktreeLockReaper{locked: map[string]string{}}
}

func (f *fakeWorktreeLockReaper) LockState(worktree string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return false, "", f.stateErr
	}
	reason, ok := f.locked[worktree]
	return ok, reason, nil
}

func (f *fakeWorktreeLockReaper) Unlock(worktree string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlocked = append(f.unlocked, worktree)
	if f.unlockErr != nil {
		return f.unlockErr
	}
	delete(f.locked, worktree)
	return nil
}

func (f *fakeWorktreeLockReaper) unlockCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unlocked...)
}

// installFakeLockReaper wires the fake for one test and restores whatever was
// installed before (nil in package tests; the git reaper in the executable).
func installFakeLockReaper(t *testing.T, reaper WorktreeLockReaper) {
	t.Helper()
	prev := DelegateWorktreeLockReaper
	DelegateWorktreeLockReaper = reaper
	t.Cleanup(func() { DelegateWorktreeLockReaper = prev })
}

// spawnDelegateForTeardown spawns one governed delegate against a locked
// workspace and returns its work id, session name and binding.
func spawnDelegateForTeardown(t *testing.T, s *Store, workspace string) (string, *DelegateBinding, *fakeSessionRunner) {
	t.Helper()
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "alive", "alive")
	t.Cleanup(func() { LaneCapacityGuard = orig })

	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID:    wID,
		Harness:   "claude",
		Actor:     "codex",
		Command:   "claude",
		Workspace: workspace,
		Now:       fixedNow,
	}, runner)
	if err != nil || res.Status != "spawned" {
		t.Fatalf("delegate: err=%v result=%+v", err, res)
	}
	return wID, res.Binding, runner
}

// A delegate that has exited must not leave the lock over its workspace
// behind: today's failure mode is 60+ merged, clean, delegate-free worktrees
// that `git worktree remove` refuses until somebody unlocks each one by hand.
func TestDelegateSweepReleasesWorktreeLockOnTerminalExit(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	workspace := filepath.Join(t.TempDir(), "lane-worktree")
	reaper := newFakeLockReaper()
	reaper.locked[workspace] = "active governed delegate"
	installFakeLockReaper(t, reaper)

	_, binding, runner := spawnDelegateForTeardown(t, s, workspace)
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{binding.Session: dead}

	out, err := s.DelegateSweep(binding, dead, runner, "operator", fixedNow)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if out.TerminalStatus != runattempt.StatusSucceeded {
		t.Fatalf("sweep terminal status = %q, want succeeded", out.TerminalStatus)
	}
	if out.WorktreeLock != WorktreeLockReleased {
		t.Fatalf("worktree lock outcome = %q (err %q), want %q", out.WorktreeLock, out.WorktreeLockError, WorktreeLockReleased)
	}
	if got := reaper.unlockCalls(); len(got) != 1 || got[0] != workspace {
		t.Fatalf("unlock calls = %v, want exactly [%s]", got, workspace)
	}
	// Both artifacts, not one: the pane is reaped in the same teardown.
	if len(runner.kills) != 1 || runner.kills[0] != binding.Session {
		t.Fatalf("kills = %v, want exactly [%s]", runner.kills, binding.Session)
	}
}

// Ownership, not proximity: a lock another LIVE delegate still depends on is
// never released, even though this delegate's workspace matches.
func TestDelegateSweepLeavesWorktreeLockHeldByAnotherLiveAttempt(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	workspace := filepath.Join(t.TempDir(), "shared-worktree")
	reaper := newFakeLockReaper()
	reaper.locked[workspace] = "active governed delegate"
	installFakeLockReaper(t, reaper)

	_, binding, runner := spawnDelegateForTeardown(t, s, workspace)

	// A second, still-running attempt in the SAME workspace.
	raStore := runattempt.Open(s.home)
	other, err := raStore.Mint("work_other0000", "codex", "claude", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raStore.SetWorkspace(other.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := raStore.MarkRunning(other.ID, fixedNow); err != nil {
		t.Fatal(err)
	}

	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{binding.Session: dead}
	out, err := s.DelegateSweep(binding, dead, runner, "operator", fixedNow)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if out.WorktreeLock != WorktreeLockHeldByLive {
		t.Fatalf("worktree lock outcome = %q, want %q", out.WorktreeLock, WorktreeLockHeldByLive)
	}
	if got := reaper.unlockCalls(); len(got) != 0 {
		t.Fatalf("released a lock a live attempt depends on: %v", got)
	}
}

// A lock failure must not swallow the teardown: the attempt still terminalizes
// and the reason is reported rather than silently dropped.
func TestDelegateSweepReportsWorktreeLockFailureWithoutBlockingTeardown(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	workspace := filepath.Join(t.TempDir(), "lane-worktree")
	reaper := newFakeLockReaper()
	reaper.locked[workspace] = "active governed delegate"
	reaper.unlockErr = errors.New("git refused")
	installFakeLockReaper(t, reaper)

	_, binding, runner := spawnDelegateForTeardown(t, s, workspace)
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{binding.Session: dead}

	out, err := s.DelegateSweep(binding, dead, runner, "operator", fixedNow)
	if err != nil {
		t.Fatalf("sweep must not fail on an advisory-lock error: %v", err)
	}
	if out.TerminalStatus != runattempt.StatusSucceeded {
		t.Fatalf("terminal status = %q, want succeeded", out.TerminalStatus)
	}
	if out.WorktreeLock != WorktreeLockUnknown || out.WorktreeLockError == "" {
		t.Fatalf("lock failure not reported: outcome=%q err=%q", out.WorktreeLock, out.WorktreeLockError)
	}
}

// The spawn_ambiguous loop: a teardown records the exit BEFORE it kills, so a
// failed Kill leaves a dead pane under a session name whose binding is no
// longer alive. Every later delegate on that work then hits Create ->
// spawn_ambiguous with no way back except a manual tmux kill-session. Teardown
// must retry the reap on its own recorded exit.
func TestDelegateRespawnReapsOrphanPaneLeftByRecordedExit(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	workspace := filepath.Join(t.TempDir(), "lane-worktree")
	installFakeLockReaper(t, newFakeLockReaper())

	wID, binding, runner := spawnDelegateForTeardown(t, s, workspace)
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{binding.Session: dead}
	// Real tmux refuses a duplicate session name -- without that, an orphan
	// pane could never produce the spawn_ambiguous loop this test falsifies.
	runner.duplicateNameErr = errors.New("duplicate session: " + binding.Session)

	// First teardown records the exit, then fails to remove the pane.
	runner.killErr = errors.New("tmux server busy")
	if _, err := s.DelegateSweep(binding, dead, runner, "operator", fixedNow); err == nil {
		t.Fatal("sweep with a failing Kill must surface the failure")
	}
	runner.killErr = nil
	if !runner.live[binding.Session] {
		t.Fatalf("precondition: the orphan pane should still exist, live=%v", runner.live)
	}
	if bound, alive, err := s.RecoverBoundSession(wID); err != nil || alive || bound != nil {
		t.Fatalf("precondition: binding should be non-alive after the recorded exit (alive=%v err=%v)", alive, err)
	}

	// Respawn on the same deterministic session name.
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "alive", "alive")
	t.Cleanup(func() { LaneCapacityGuard = orig })
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Workspace: workspace, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if res.Status != "spawned" {
		t.Fatalf("respawn status = %q refusal=%+v, want spawned (orphan pane was not reaped)", res.Status, res.Refusal)
	}
	killedOrphan := false
	for _, k := range runner.kills {
		if k == binding.Session {
			killedOrphan = true
		}
	}
	if !killedOrphan {
		t.Fatalf("orphan pane %s was never reaped: kills=%v", binding.Session, runner.kills)
	}
}

// Never a stranger's: a LIVE pane under a session name whose exit was already
// recorded is exactly the ownership uncertainty spawn_ambiguous encodes. It is
// left alone, and the spawn refuses on it.
func TestDelegateRespawnDoesNotKillLivePaneUnderRecordedExit(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	workspace := filepath.Join(t.TempDir(), "lane-worktree")
	installFakeLockReaper(t, newFakeLockReaper())

	wID, binding, runner := spawnDelegateForTeardown(t, s, workspace)
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{binding.Session: dead}
	// Real tmux refuses a duplicate session name -- without that, an orphan
	// pane could never produce the spawn_ambiguous loop this test falsifies.
	runner.duplicateNameErr = errors.New("duplicate session: " + binding.Session)
	runner.killErr = errors.New("tmux server busy")
	if _, err := s.DelegateSweep(binding, dead, runner, "operator", fixedNow); err == nil {
		t.Fatal("sweep with a failing Kill must surface the failure")
	}
	runner.killErr = nil
	// Somebody else now owns a LIVE session under that name.
	runner.panes[binding.Session] = SessionStatus{Exists: true, ExitCode: -1}

	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "alive", "alive")
	t.Cleanup(func() { LaneCapacityGuard = orig })
	killsBefore := len(runner.kills)
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Workspace: workspace, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if len(runner.kills) != killsBefore {
		t.Fatalf("killed a potentially foreign live pane: kills=%v", runner.kills)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "spawn_ambiguous" {
		t.Fatalf("respawn over a live foreign pane = %+v, want refused{spawn_ambiguous}", res)
	}
}

// The doctor join: a locked workspace whose delegates are ALL terminal is
// stale; one that still has a live attempt is not.
func TestDelegateWorktreeLockReportMarksTerminalOwnersStale(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	stale := filepath.Join(t.TempDir(), "stale-worktree")
	held := filepath.Join(t.TempDir(), "held-worktree")
	unlocked := filepath.Join(t.TempDir(), "unlocked-worktree")
	reaper := newFakeLockReaper()
	reaper.locked[stale] = "active governed delegate"
	reaper.locked[held] = "active governed delegate"
	installFakeLockReaper(t, reaper)

	raStore := runattempt.Open(s.home)
	seed := func(workID, workspace, status string) {
		t.Helper()
		ra, err := raStore.Mint(workID, "codex", "claude", fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raStore.SetWorkspace(ra.ID, workspace); err != nil {
			t.Fatal(err)
		}
		if status == runattempt.StatusRunning {
			if _, err := raStore.MarkRunning(ra.ID, fixedNow); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := raStore.Complete(ra.ID, status, fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	seed("work_stale000000", stale, runattempt.StatusAbandoned)
	seed("work_held0000000", held, runattempt.StatusRunning)
	seed("work_unlocked000", unlocked, runattempt.StatusSucceeded)

	rows, err := s.DelegateWorktreeLockReport(reaper)
	if err != nil {
		t.Fatal(err)
	}
	byWorkspace := map[string]DelegateWorktreeLockRow{}
	for _, row := range rows {
		byWorkspace[row.Workspace] = row
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want only the two LOCKED workspaces", rows)
	}
	if row, ok := byWorkspace[stale]; !ok || !row.Stale || row.LiveAttempts != 0 {
		t.Fatalf("stale row = %+v (ok=%v), want stale with zero live attempts", row, ok)
	}
	if row, ok := byWorkspace[held]; !ok || row.Stale {
		t.Fatalf("held row = %+v (ok=%v), want present and NOT stale", row, ok)
	}
	if _, ok := byWorkspace[unlocked]; ok {
		t.Fatal("an unlocked workspace must not appear in a lock report")
	}
}
