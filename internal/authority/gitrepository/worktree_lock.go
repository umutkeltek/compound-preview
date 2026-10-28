package gitrepository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// worktreeUnlockTimeout bounds the single `git worktree unlock` subprocess.
// Unlocking is a metadata-only operation (git removes one file), so a call
// that has not returned by then is a hung git, not slow work.
const worktreeUnlockTimeout = 30 * time.Second

// WorktreeLock is the read-only `git worktree lock` state of one checkout.
//
// A lock is an advisory claim: `git worktree remove` and `git worktree prune`
// refuse a locked tree, which is exactly why a supervised fleet takes one
// while a delegate is running -- and exactly why a lock that outlives its
// holder freezes cleanup until somebody unlocks it by hand.
//
// Linked distinguishes a linked worktree (lockable) from the main working
// tree (git refuses to lock it), so a caller never reports "unlocked" for a
// path that could not carry a lock in the first place.
type WorktreeLock struct {
	Worktree string `json:"worktree"`
	GitDir   string `json:"git_dir,omitempty"`
	Linked   bool   `json:"linked"`
	Locked   bool   `json:"locked"`
	Reason   string `json:"reason,omitempty"`
}

// ReadWorktreeLock reports whether worktree currently carries a lock.
//
// It reads git's own on-disk marker (`<worktree gitdir>/locked`, the file
// `git worktree lock` writes and `git worktree unlock` removes) instead of
// shelling out to `git worktree list --porcelain`. Two reasons, both
// load-bearing: a health surface may check dozens of workspaces in one run
// and a subprocess per workspace turns a check into a wait, and the porcelain
// listing must be run from a repository that still registers the worktree --
// this read answers for the path in front of it. The mutation still goes
// through git (see UnlockWorktree); only the observation is direct.
func ReadWorktreeLock(worktree string) (WorktreeLock, error) {
	path := strings.TrimSpace(worktree)
	if path == "" {
		return WorktreeLock{}, errors.New("worktree lock: worktree path is required")
	}
	out := WorktreeLock{Worktree: path}
	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return out, fmt.Errorf("worktree lock: resolve %s: %w", dotGit, err)
	}
	if info.IsDir() {
		// The main working tree keeps a real .git directory and cannot be
		// locked. Report it as unlocked-and-unlinked rather than guessing.
		out.GitDir = dotGit
		return out, nil
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return out, fmt.Errorf("worktree lock: read %s: %w", dotGit, err)
	}
	gitDir := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:"); ok {
			gitDir = strings.TrimSpace(v)
			break
		}
	}
	if gitDir == "" {
		return out, fmt.Errorf("worktree lock: %s does not name a gitdir", dotGit)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	out.GitDir = filepath.Clean(gitDir)
	out.Linked = true
	reason, err := os.ReadFile(filepath.Join(out.GitDir, "locked"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("worktree lock: read lock marker for %s: %w", path, err)
	}
	out.Locked = true
	out.Reason = strings.TrimSpace(string(reason))
	return out, nil
}

// UnlockWorktree releases the lock over worktree through git itself, so git's
// own validation (is this a registered worktree, is it actually locked) stays
// the authority on whether the release is legal. Unlocking an unlocked tree is
// an error in git and is surfaced as one here rather than smoothed over: a
// caller that believed it held a lock it did not hold has a stale premise.
func UnlockWorktree(worktree string) error {
	path := strings.TrimSpace(worktree)
	if path == "" {
		return errors.New("worktree unlock: worktree path is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktreeUnlockTimeout)
	defer cancel()
	if _, err := gitOutput(ctx, path, "worktree", "unlock", path); err != nil {
		return fmt.Errorf("worktree unlock %s: %w", path, err)
	}
	return nil
}
