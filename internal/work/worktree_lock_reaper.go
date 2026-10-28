// Delegate teardown reaps the artifacts a delegate created. A governed
// delegate leaves two things behind that outlive the process: the tmux
// session (reaped by DelegateSweep through the SessionRunner seam) and the
// `git worktree lock` over the workspace it ran in. The lock is the one with
// no owner today: a locked worktree refuses `git worktree remove` and
// `git worktree prune`, so every lock whose delegate has exited freezes a
// checkout until an operator unlocks it by hand.
//
// Ownership is the whole design constraint here, and it is the same principle
// spawn_ambiguous already encodes on the tmux side: NEVER touch an artifact
// that might be a stranger's. A lock is released only when compound can point
// at the evidence that it is free to release:
//
//   - the workspace comes from the terminalized delegate's own RunAttempt
//     (delegate.go stamps it before the spawn), not from a guess;
//   - the path is actually locked right now;
//   - no OTHER non-terminal RunAttempt names the same workspace.
//
// The limit is stated plainly rather than hidden: compound cannot prove a
// human did not lock a delegate workspace for an unrelated reason. What it
// proves is that no live compound run depends on that lock, which is the
// condition under which holding it protects nothing.
package work

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/authority/gitrepository"
	"compound/internal/runattempt"
)

// Reap outcomes. Small closed vocabulary so the sweep outcome and the doctor
// row can never disagree about what happened to a lock.
const (
	WorktreeLockReleased     = "released"
	WorktreeLockNotLocked    = "not_locked"
	WorktreeLockHeldByLive   = "held_by_live_attempt"
	WorktreeLockNoWorkspace  = "no_workspace_evidence"
	WorktreeLockReaperAbsent = "reaper_absent"
	WorktreeLockUnknown      = "unknown"
)

// WorktreeLockReaper is the seam between delegate teardown and git worktree
// locks, mirroring SessionRunner: only the production adapter shells out, so
// orchestrator tests never touch a real repository.
type WorktreeLockReaper interface {
	LockState(worktree string) (locked bool, reason string, err error)
	Unlock(worktree string) error
}

// DelegateWorktreeLockReaper is installed by the executable composition root
// (like DelegateHarnessReadiness and DelegateSessionStoreResolver). A nil
// reaper disables lock reaping and reports `reaper_absent` rather than
// failing a teardown: refusing to finish terminalizing a dead delegate
// because cleanup of an advisory lock is unavailable would trade a
// bookkeeping problem for a stuck work item.
var DelegateWorktreeLockReaper WorktreeLockReaper

// GitWorktreeLockReaper is the production adapter over real git.
type GitWorktreeLockReaper struct{}

// NewGitWorktreeLockReaper returns the real-git reaper for the composition root.
func NewGitWorktreeLockReaper() *GitWorktreeLockReaper { return &GitWorktreeLockReaper{} }

func (GitWorktreeLockReaper) LockState(worktree string) (bool, string, error) {
	state, err := gitrepository.ReadWorktreeLock(worktree)
	if err != nil {
		return false, "", err
	}
	return state.Locked, state.Reason, nil
}

func (GitWorktreeLockReaper) Unlock(worktree string) error {
	return gitrepository.UnlockWorktree(worktree)
}

// reapDelegateWorktreeLock releases the lock over the workspace this delegate
// ran in, when and only when the ownership evidence above holds. It returns
// one of the vocabulary constants plus any error encountered; callers treat
// the error as reportable, not fatal (see the package comment).
func (s *Store) reapDelegateWorktreeLock(b *DelegateBinding) (string, error) {
	reaper := DelegateWorktreeLockReaper
	if reaper == nil {
		return WorktreeLockReaperAbsent, nil
	}
	if b == nil || strings.TrimSpace(b.RunAttemptID) == "" {
		// A pre-slice binding carries no attempt, so it names no workspace.
		// No evidence, no reap.
		return WorktreeLockNoWorkspace, nil
	}
	raStore := runattempt.Open(s.home)
	ra, err := raStore.Get(b.RunAttemptID)
	if err != nil {
		return WorktreeLockUnknown, fmt.Errorf("delegate teardown: load run attempt %s: %w", b.RunAttemptID, err)
	}
	workspace := strings.TrimSpace(ra.Workspace)
	if workspace == "" {
		return WorktreeLockNoWorkspace, nil
	}
	locked, _, err := reaper.LockState(workspace)
	if err != nil {
		return WorktreeLockUnknown, fmt.Errorf("delegate teardown: read worktree lock: %w", err)
	}
	if !locked {
		return WorktreeLockNotLocked, nil
	}
	live, err := raStore.ListNonTerminal()
	if err != nil {
		return WorktreeLockUnknown, fmt.Errorf("delegate teardown: list live run attempts: %w", err)
	}
	for _, other := range live {
		if other.ID == b.RunAttemptID {
			continue
		}
		if sameWorkspacePath(other.Workspace, workspace) {
			return WorktreeLockHeldByLive, nil
		}
	}
	if err := reaper.Unlock(workspace); err != nil {
		return WorktreeLockUnknown, fmt.Errorf("delegate teardown: release worktree lock: %w", err)
	}
	return WorktreeLockReleased, nil
}

// DelegateWorktreeLockRow is one workspace a delegate ran in, joined to its
// current lock state. Stale is the actionable column: locked, and every
// RunAttempt that ever ran there is terminal — i.e. a lock protecting nobody.
type DelegateWorktreeLockRow struct {
	Workspace    string   `json:"workspace"`
	Reason       string   `json:"reason,omitempty"`
	Attempts     int      `json:"attempts"`
	LiveAttempts int      `json:"live_attempts"`
	WorkIDs      []string `json:"work_ids,omitempty"`
	Stale        bool     `json:"stale"`
}

// DelegateWorktreeLockReport lists every LOCKED workspace that compound
// delegates have run in, and marks the ones whose owning delegates are all
// terminal. It is read-only: it names what a teardown failed to reap (or was
// never able to see, e.g. a lock taken by a fleet outside compound), and never
// releases anything itself — the release path is the governed
// `compound work delegate sweep`.
//
// Attribution runs from compound's own RunAttempt records outward, never from
// the repository inward: a locked worktree no compound run ever used is not
// compound's to report on, and is deliberately absent from this report.
func (s *Store) DelegateWorktreeLockReport(reaper WorktreeLockReaper) ([]DelegateWorktreeLockRow, error) {
	if reaper == nil {
		return nil, nil
	}
	attempts, err := runattempt.Open(s.home).List()
	if err != nil {
		return nil, err
	}
	type agg struct {
		attempts int
		live     int
		workIDs  map[string]bool
	}
	byWorkspace := map[string]*agg{}
	order := []string{}
	for _, ra := range attempts {
		workspace := strings.TrimSpace(ra.Workspace)
		if workspace == "" {
			continue
		}
		key := canonicalWorkspacePath(workspace)
		entry, ok := byWorkspace[key]
		if !ok {
			entry = &agg{workIDs: map[string]bool{}}
			byWorkspace[key] = entry
			order = append(order, key)
		}
		entry.attempts++
		if !runattempt.IsTerminal(ra.Status) {
			entry.live++
		}
		if id := strings.TrimSpace(ra.WorkID); id != "" {
			entry.workIDs[id] = true
		}
	}
	sort.Strings(order)
	rows := []DelegateWorktreeLockRow{}
	for _, workspace := range order {
		entry := byWorkspace[workspace]
		row := DelegateWorktreeLockRow{
			Workspace:    workspace,
			Attempts:     entry.attempts,
			LiveAttempts: entry.live,
		}
		locked, reason, lockErr := reaper.LockState(workspace)
		if lockErr != nil {
			// A workspace that no longer exists is the common case (the
			// checkout was removed after its delegate finished) and is not a
			// leak; it simply cannot be reported on.
			continue
		}
		if !locked {
			continue
		}
		row.Reason = reason
		row.Stale = entry.live == 0
		for id := range entry.workIDs {
			row.WorkIDs = append(row.WorkIDs, id)
		}
		sort.Strings(row.WorkIDs)
		rows = append(rows, row)
	}
	return rows, nil
}

// canonicalWorkspacePath normalizes a recorded workspace for comparison
// without resolving symlinks: a teardown must not depend on the checkout
// still existing on disk.
func canonicalWorkspacePath(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return ""
	}
	return filepath.Clean(trimmed)
}

func sameWorkspacePath(a, b string) bool {
	ca, cb := canonicalWorkspacePath(a), canonicalWorkspacePath(b)
	return ca != "" && ca == cb
}
