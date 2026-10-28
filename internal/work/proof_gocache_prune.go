package work

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Per-worktree proof Go caches are bounded by the code that creates them:
// ProofGoCache stamps owner and last use, PruneProofGoCaches enforces the
// policy below. These constants are the only place the policy is set.
const (
	proofGoCacheOwnerFile    = "owner-path"
	proofGoCacheLastUsedFile = "last-used"
	proofGoCacheTouchEvery   = time.Minute
	proofGoCacheOwnerMax     = 4096

	proofGoCacheRecentGuard = 6 * time.Hour
	proofGoCacheIdleAfter   = 72 * time.Hour
	proofGoCacheMaxKept     = 16
)

// ProofGoCachePrunePolicy bounds the per-worktree proof Go caches. Caches used
// within RecentGuard are never removed; otherwise orphans and caches idle for
// IdleAfter go first, then the least recently used until MaxKept remain.
type ProofGoCachePrunePolicy struct {
	RecentGuard time.Duration
	IdleAfter   time.Duration
	MaxKept     int
}

// DefaultProofGoCachePrunePolicy is the only production policy.
func DefaultProofGoCachePrunePolicy() ProofGoCachePrunePolicy {
	return ProofGoCachePrunePolicy{
		RecentGuard: proofGoCacheRecentGuard,
		IdleAfter:   proofGoCacheIdleAfter,
		MaxKept:     proofGoCacheMaxKept,
	}
}

// ProofGoCacheRemoval names one removed cache and the rule that removed it:
// "orphan", "idle" or "lru".
type ProofGoCacheRemoval struct {
	Name   string
	Reason string
}

// ProofGoCachePruneReport lists caches kept and removed. Errors never fail a
// proof; a cache that could not be judged or removed is kept.
type ProofGoCachePruneReport struct {
	Kept    []string
	Removed []ProofGoCacheRemoval
	Errors  []string
}

// Summary is the single stderr line a proof run prints after pruning.
func (r ProofGoCachePruneReport) Summary() string {
	counts := map[string]int{}
	for _, removed := range r.Removed {
		counts[removed.Reason]++
	}
	return fmt.Sprintf(
		"proof-gocache: kept %d, removed %d (orphan %d, idle %d, lru %d)",
		len(r.Kept), len(r.Removed), counts["orphan"], counts["idle"], counts["lru"],
	)
}

type proofGoCacheEntry struct {
	name     string
	path     string
	lastUsed time.Time
}

// PruneProofGoCaches removes stale per-worktree proof Go caches under root.
// Every compound proof-check run calls it once. Retiring a worktree needs no
// extra step: its cache becomes an orphan and the next proof run in any
// worktree removes it once the six-hour guard has passed.
//
// It only considers direct children named by ProofGoCache (16 lowercase hex
// characters) that Lstat reports as real directories, never touches current,
// and never follows a symlink: os.RemoveAll unlinks symlinks inside a cache.
func PruneProofGoCaches(root string, current string, now time.Time, policy ProofGoCachePrunePolicy) (ProofGoCachePruneReport, error) {
	var report ProofGoCachePruneReport
	if policy.RecentGuard <= 0 || policy.IdleAfter <= policy.RecentGuard || policy.MaxKept <= 0 {
		return report, fmt.Errorf("proof go cache prune policy is unsafe: %+v", policy)
	}
	resolved, err := proofGoCachePruneRoot(root)
	if err != nil {
		return report, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return report, fmt.Errorf("read proof go cache root: %w", err)
	}
	currentName := ""
	if strings.TrimSpace(current) != "" {
		currentName = filepath.Base(filepath.Clean(current))
	}

	var survivors, candidates []proofGoCacheEntry
	for _, dirEntry := range entries {
		name := dirEntry.Name()
		if !proofGoCacheName(name) {
			continue
		}
		path := filepath.Join(resolved, name)
		info, err := os.Lstat(path)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !info.IsDir() {
			continue
		}
		entry := proofGoCacheEntry{name: name, path: path}
		if name == currentName {
			survivors = append(survivors, entry)
			continue
		}
		entry.lastUsed, err = proofGoCacheLastUsed(path)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: last use: %v", name, err))
			survivors = append(survivors, entry)
			continue
		}
		age := now.Sub(entry.lastUsed)
		if age < policy.RecentGuard {
			survivors = append(survivors, entry)
			continue
		}
		orphan, err := proofGoCacheOrphan(path)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: owner: %v", name, err))
		}
		switch {
		case orphan:
			if !report.remove(entry, "orphan") {
				survivors = append(survivors, entry)
			}
		case age >= policy.IdleAfter:
			if !report.remove(entry, "idle") {
				survivors = append(survivors, entry)
			}
		default:
			candidates = append(candidates, entry)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].lastUsed.Equal(candidates[j].lastUsed) {
			return candidates[i].lastUsed.Before(candidates[j].lastUsed)
		}
		return candidates[i].name < candidates[j].name
	})
	excess := len(survivors) + len(candidates) - policy.MaxKept
	for _, entry := range candidates {
		if excess > 0 && report.remove(entry, "lru") {
			excess--
			continue
		}
		survivors = append(survivors, entry)
	}
	for _, entry := range survivors {
		report.Kept = append(report.Kept, entry.name)
	}
	sort.Strings(report.Kept)
	return report, nil
}

// PruneProofGoCachesForRun prunes once for a proof run of repo. ok is false
// when the run does not use per-worktree caches (COMPOUND_PROOF_SHARED_GOCACHE)
// or repo lives under os.TempDir(): a throwaway checkout, such as every test
// fixture, never garbage-collects the operator's worktree caches.
func PruneProofGoCachesForRun(repo string, now time.Time) (report ProofGoCachePruneReport, ok bool, err error) {
	current, owner, ok := proofGoCacheDir(repo)
	if !ok || proofGoCacheOwnerIsTemporary(owner) {
		return report, false, nil
	}
	root := filepath.Dir(current)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return report, true, nil
	}
	report, err = PruneProofGoCaches(root, current, now, DefaultProofGoCachePrunePolicy())
	return report, true, err
}

func proofGoCacheOwnerIsTemporary(owner string) bool {
	candidates := []string{filepath.Clean(owner)}
	if resolved, err := filepath.EvalSymlinks(owner); err == nil {
		candidates = append(candidates, resolved)
	}
	roots := []string{filepath.Clean(os.TempDir())}
	if resolved, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		roots = append(roots, resolved)
	}
	for _, candidate := range candidates {
		for _, root := range roots {
			if candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// remove deletes one cache unless it changed since it was judged; another
// proof run may have started using it while this one was scanning.
func (r *ProofGoCachePruneReport) remove(entry proofGoCacheEntry, reason string) bool {
	info, err := os.Lstat(entry.path)
	if errors.Is(err, fs.ErrNotExist) {
		return true // a concurrent proof run already removed it
	}
	if err != nil || !info.IsDir() {
		r.Errors = append(r.Errors, fmt.Sprintf("%s: no longer a real directory: %v", entry.name, err))
		return false
	}
	lastUsed, err := proofGoCacheLastUsed(entry.path)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("%s: last use: %v", entry.name, err))
		return false
	}
	if lastUsed.After(entry.lastUsed) {
		return false
	}
	if err := os.RemoveAll(entry.path); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("%s: remove: %v", entry.name, err))
		return false
	}
	r.Removed = append(r.Removed, ProofGoCacheRemoval{Name: entry.name, Reason: reason})
	return true
}

// proofGoCachePruneRoot refuses roots whose deletion scope would be wider
// than a cache directory: empty, the filesystem root, or the user's home.
func proofGoCachePruneRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("proof go cache root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve proof go cache root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve proof go cache root: %w", err)
	}
	refused := []string{string(filepath.Separator)}
	if userHome, err := os.UserHomeDir(); err == nil && strings.TrimSpace(userHome) != "" {
		refused = append(refused, filepath.Clean(userHome))
		if resolvedHome, err := filepath.EvalSymlinks(userHome); err == nil {
			refused = append(refused, resolvedHome)
		}
	}
	for _, candidate := range []string{filepath.Clean(abs), resolved} {
		for _, unsafe := range refused {
			if candidate == unsafe {
				return "", fmt.Errorf("refusing to prune proof go caches in %s", candidate)
			}
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat proof go cache root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("proof go cache root is not a directory: %s", resolved)
	}
	return resolved, nil
}

func proofGoCacheName(name string) bool {
	if len(name) != 16 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// proofGoCacheLastUsed reads the last-used marker. A legacy cache written
// before markers existed falls back to the newest mtime of the cache dir and
// its immediate children.
func proofGoCacheLastUsed(dir string) (time.Time, error) {
	info, err := os.Lstat(filepath.Join(dir, proofGoCacheLastUsedFile))
	if err == nil && info.Mode().IsRegular() {
		return info.ModTime(), nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, err
	}
	newest := dirInfo.ModTime()
	children, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, err
	}
	for _, child := range children {
		childInfo, err := child.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if childInfo.ModTime().After(newest) {
			newest = childInfo.ModTime()
		}
	}
	return newest, nil
}

// proofGoCacheOrphan reports whether the cache's recorded owner worktree is
// gone. A cache without a readable absolute owner is never an orphan.
func proofGoCacheOrphan(dir string) (bool, error) {
	path := filepath.Join(dir, proofGoCacheOwnerFile)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > proofGoCacheOwnerMax {
		return false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	owner := strings.TrimSpace(string(body))
	if owner == "" || !filepath.IsAbs(owner) {
		return false, nil
	}
	ownerInfo, err := os.Stat(owner)
	switch {
	case err == nil:
		return !ownerInfo.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	default:
		return false, err
	}
}

// markProofGoCacheUse writes owner-path once and refreshes last-used at most
// once per proofGoCacheTouchEvery. Failures leave the cache usable; pruning
// then judges it by its own mtimes.
func markProofGoCacheUse(dir, owner string, now time.Time) {
	ownerPath := filepath.Join(dir, proofGoCacheOwnerFile)
	if _, err := os.Lstat(ownerPath); errors.Is(err, fs.ErrNotExist) {
		writeProofGoCacheOwner(dir, ownerPath, owner)
	}
	lastUsed := filepath.Join(dir, proofGoCacheLastUsedFile)
	info, err := os.Lstat(lastUsed)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if file, err := os.OpenFile(lastUsed, os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			_ = file.Close()
			_ = os.Chtimes(lastUsed, now, now)
		}
	case err != nil || !info.Mode().IsRegular():
		return
	case info.ModTime().After(now) || now.Sub(info.ModTime()) >= proofGoCacheTouchEvery:
		_ = os.Chtimes(lastUsed, now, now)
	}
}

// writeProofGoCacheOwner publishes owner-path by rename so a concurrent
// pruner never reads a partial path and mistakes the cache for an orphan.
func writeProofGoCacheOwner(dir, ownerPath, owner string) {
	temp, err := os.CreateTemp(dir, ".owner-path-*")
	if err != nil {
		return
	}
	_, writeErr := temp.WriteString(owner + "\n")
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(temp.Name(), ownerPath) != nil {
		_ = os.Remove(temp.Name())
	}
}
