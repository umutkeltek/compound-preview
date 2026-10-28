package policy

// Retention discovery — find what is ACTUALLY accumulating on disk and check it against the
// declared classes.
//
// Declaration alone is worthless. A registry nobody compares against reality is a document,
// and documents drift: the incident that produced this package featured owner records claiming
// a hook consumed a view when the hook had never been installed. So the durable half is here —
// walk the real filesystem, classify what is found, and report anything no class claims.
//
// The scan is deliberately shallow and allow-listed rather than a full tree walk. A recursive
// sweep of a home directory inside a health check would be slow enough that people stop
// running it, and a check that is not run protects nothing.

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// buildCacheDirNames are directory names that are pure, rebuildable build output wherever they
// appear inside a repository or worktree.
// Only names that are unambiguously regenerated build output. A directory here can be planned
// for automatic reclamation, so ambiguous state-bearing names must stay out.
var buildCacheDirNames = map[string]bool{
	"target":       true, // Rust
	".gocache":     true, // Go, per-worktree
	".cache":       true,
	"node_modules": true, // npm/pnpm — restored by install
	".build":       true, // Swift Package Manager
	".next":        true, // Next.js
	"dist":         true, // bundler output
	".venv":        true, // Python virtualenv — restored from requirements
}

// RetentionInstance is one accumulator found on disk.
type RetentionInstance struct {
	// ClassID is the declared class that claims it, or "" when nothing does.
	ClassID string `json:"class_id,omitempty"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	// Rebuildable mirrors the claiming class, so a reclaimer can prefer these first without a
	// second lookup.
	Rebuildable bool `json:"rebuildable"`
	// Truncated marks a size that hit the walk budget and is therefore a LOWER BOUND. Reported
	// rather than hidden: an unlabelled partial total is a wrong number, and this check exists
	// to stop wrong numbers going unnoticed.
	Truncated bool `json:"truncated,omitempty"`
	// RecentlyActive marks a cache touched inside the activity window — almost certainly a
	// build in flight. Still REPORTED (the bytes are real) but never planned for automatic
	// reclamation: deleting a target/ mid-compile breaks whatever lane is using it, and this
	// machine routinely runs several concurrently.
	RecentlyActive bool `json:"recently_active,omitempty"`
}

// RetentionScanReport is the result of one discovery pass.
type RetentionScanReport struct {
	AuthorityRef string `json:"authority_ref"`
	// Declared holds instances a class claims, largest first.
	Declared []RetentionInstance `json:"declared,omitempty"`
	// Undeclared holds accumulators nothing claims. NON-EMPTY IS A FAILURE: it means the
	// system grew a new way to fill the disk and no owner noticed.
	Undeclared []RetentionInstance `json:"undeclared,omitempty"`
	// FixedPathCachesIncluded says whether toolchain-owned caches outside Scanned
	// roots were requested. False is an explicit scope disclosure, not a green
	// claim about caches the caller did not inspect.
	FixedPathCachesIncluded bool `json:"fixed_path_caches_included"`
	// ReclaimableBytes totals the declared, rebuildable instances — the bytes a reclaimer
	// could recover today at zero risk.
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	// ClassesWithoutMechanism names declared classes that nothing can currently reclaim.
	ClassesWithoutMechanism []string `json:"classes_without_mechanism,omitempty"`
	// Scanned records the roots actually walked, so an empty report is distinguishable from
	// an unscanned one. A health surface that cannot tell "clean" from "did not look" is the
	// same silent-green failure this package exists to prevent.
	Scanned []string `json:"scanned"`
}

// RetentionScanOptions controls discovery outside the explicitly supplied roots.
//
// FixedPathCaches is opt-in because a scan must otherwise be reproducible from its roots. CLI
// callers that intentionally want host toolchain caches can request them; tests, doctor, and
// embedded callers do not silently inherit unrelated machine state.
type RetentionScanOptions struct {
	FixedPathCaches bool
}

// ScanRetention walks only the given repository roots.
func ScanRetention(roots []string) RetentionScanReport {
	return ScanRetentionWithOptions(roots, RetentionScanOptions{})
}

// ScanRetentionWithOptions walks the given repository roots and any explicitly enabled fixed
// locations.
//
// Roots are repository or worktree directories. For each, it finds build-cache directories at
// the top level and inside any nested worktrees, sizes them, and classifies them.
func ScanRetentionWithOptions(roots []string, options RetentionScanOptions) RetentionScanReport {
	report := RetentionScanReport{
		AuthorityRef:            RetentionAuthorityRef,
		FixedPathCachesIncluded: options.FixedPathCaches,
	}
	seen := map[string]bool{}

	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		report.Scanned = append(report.Scanned, root)

		for _, dir := range candidateDirs(root) {
			if seen[dir] {
				continue
			}
			seen[dir] = true

			classID, useIgnoreHeuristic := classifyDir(root, dir)

			// Only size a directory once it is worth a verdict. Sizing every source tree
			// would make the check slow without changing any answer.
			if classID == "" {
				if !useIgnoreHeuristic || !looksLikeAccumulator(root, dir) {
					continue
				}
			}
			size, truncated := dirBytes(dir)
			if size == 0 {
				continue
			}

			instance := RetentionInstance{
				Path:           dir,
				Bytes:          size,
				Truncated:      truncated,
				RecentlyActive: recentlyActive(dir, time.Now()),
			}
			if classID != "" {
				class, ok := RetentionClassByID(classID)
				if ok {
					instance.ClassID = class.ID
					instance.Rebuildable = class.Rebuildable
					report.Declared = append(report.Declared, instance)
					if class.Rebuildable {
						report.ReclaimableBytes += size
					}
					continue
				}
			}
			// Large, ignored, and claimed by nothing: a new way to fill the disk that no
			// owner is tracking. This is the finding the whole package exists to produce.
			if size >= undeclaredAccumulatorBytes {
				report.Undeclared = append(report.Undeclared, instance)
			}
		}
	}

	if options.FixedPathCaches {
		scanWellKnown(&report, seen)
	}

	sortInstances(report.Declared)
	sortInstances(report.Undeclared)
	for _, class := range RetentionClassesWithoutMechanism() {
		report.ClassesWithoutMechanism = append(report.ClassesWithoutMechanism, class.ID)
	}
	sort.Strings(report.Scanned)
	return report
}

// wellKnownAccumulator is a declared class whose instance lives at a FIXED path, outside any
// repository — so no amount of repo scanning will ever find it.
//
// The Go build cache is the case that proved this matters: it is the single largest reclaimable
// class on the machine, the storage guard's ONLY reclaimer targeted it, and a repo-rooted scan
// cannot see it at all. A reclaim plan blind to it would confidently report that automatic
// reclamation could not cover a shortfall while gigabytes sat one directory away.
type wellKnownAccumulator struct {
	classID string
	// resolve returns the live path, or "" when this machine has no such thing. Resolved by
	// ASKING the toolchain (`go env GOCACHE`) rather than hard-coding a platform path, which
	// would be wrong on any machine configured differently.
	resolve func() string
}

// wellKnownAccumulators returns fixed-location caches known to the public core.
func wellKnownAccumulators() []wellKnownAccumulator {
	return []wellKnownAccumulator{
		{
			classID: "go-build-cache",
			resolve: func() string {
				out, err := exec.Command("go", "env", "GOCACHE").Output()
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(out))
			},
		},
	}
}

// scanWellKnown adds fixed-location accumulators to a report.
func scanWellKnown(report *RetentionScanReport, seen map[string]bool) {
	for _, known := range wellKnownAccumulators() {
		path := strings.TrimSpace(known.resolve())
		if path == "" || seen[path] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[path] = true
		report.Scanned = append(report.Scanned, path)
		size, truncated := dirBytes(path)
		if size == 0 {
			continue
		}
		class, ok := RetentionClassByID(known.classID)
		if !ok {
			continue
		}
		report.Declared = append(report.Declared, RetentionInstance{
			ClassID:     class.ID,
			Path:        path,
			Bytes:       size,
			Rebuildable: class.Rebuildable,
			Truncated:   truncated,
		})
		if class.Rebuildable {
			report.ReclaimableBytes += size
		}
	}
}

// worktreeContainerPaths are the repo-relative directories that hold agent worktrees. They are
// structure, not accumulators: what grows inside them is worktrees, which the `agent-worktree`
// class already owns and which have their own governed retirement path.
//
// This list is the FALLBACK half of container discovery; git itself (gitLinkedWorktrees) is the
// authoritative half. Both are needed, and the gap between them is the point:
//
//   - A name list cannot see a container invented after it was written. That is not
//     hypothetical here — this list knew only .claude and .codex, of which .codex/worktrees did
//     not exist, while .agents/worktrees (1.9 GB) and .hermes/worktrees (443 MB) went unscanned
//     and `compound doctor` reported a clean retention picture over caches it never looked at.
//   - Git forgets a worktree the instant its admin dir is removed, but the directory keeps its
//     bytes. A leftover git no longer tracks is pure waste, and exactly what an operator is
//     owed by a retention scan.
var worktreeContainerPaths = []string{
	".claude/worktrees",
	".codex/worktrees",
	".agents/worktrees",
	".hermes/worktrees",
	".worktrees",
}

// worktreeContainerRoots is the depth-1 view of worktreeContainerPaths: the directory an
// operator sees at the top of the repo. DERIVED, never retyped — this file already carried the
// same fact in two hand-maintained lists that drifted apart, so the scan skipped a container
// while the classifier simultaneously reported it as an unclaimed accumulator.
var worktreeContainerRoots = func() map[string]bool {
	roots := map[string]bool{}
	for _, path := range worktreeContainerPaths {
		roots[strings.Split(path, "/")[0]] = true
	}
	return roots
}()

// classifyDir decides which class claims a directory, and whether the ignore heuristic may be
// applied to it when no class does.
//
// The heuristic is restricted to DIRECT children of the repo root on purpose. Worktree
// containers are themselves git-ignored, so every path beneath them inherits ignored-ness —
// applying the heuristic there flagged four ordinary `console/` source directories and both
// containers as "undeclared accumulators" on the first live run. Inside a worktree, only a
// known build-cache name counts; that keeps the finding list to things an operator can act on.
func classifyDir(root, dir string) (classID string, useIgnoreHeuristic bool) {
	base := filepath.Base(dir)
	if buildCacheDirNames[base] {
		return "worktree-build-cache", false
	}

	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false
	}
	depth := len(strings.Split(rel, string(filepath.Separator)))

	if depth == 1 {
		if worktreeContainerRoots[base] {
			// Report the container under the class that actually owns its contents, so its
			// size is visible without pretending nobody owns it.
			return "agent-worktree", false
		}
		return "", true
	}
	// Deeper than the root: only a recognized cache name earns a verdict.
	return "", false
}

// undeclaredAccumulatorBytes is the size at which an unclaimed, git-ignored directory becomes
// a finding. Small ignored directories (editor state, a stray log) are noise; the classes this
// registry exists to catch arrived as multi-gigabyte trees.
const undeclaredAccumulatorBytes = 512 * 1024 * 1024

// recentActivityWindow is how recently a cache must have been touched to count as in use.
// Generous on purpose: the cost of skipping a live cache is deferred reclamation, while the
// cost of deleting one is a broken build in another lane.
const recentActivityWindow = 45 * time.Minute

// recentlyActive reports whether a directory was modified inside the activity window.
func recentlyActive(dir string, now time.Time) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return true // unreadable: assume in use rather than delete it
	}
	return now.Sub(info.ModTime()) < recentActivityWindow
}

// looksLikeAccumulator reports whether an unrecognized directory is worth sizing.
//
// The discriminator is git-ignored status, not name or size. Source directories are tracked
// and bounded by the code; the things that grow without bound — build output, caches, captured
// state, downloaded corpora — are essentially always ignored. Using ignored-ness keeps the
// check quiet on ordinary repositories while still catching a shape nobody has seen before,
// which a name allow-list by construction cannot do.
func looksLikeAccumulator(root, dir string) bool {
	// `git check-ignore` is authoritative about this repo's own ignore rules, including
	// nested .gitignore files and the global excludes file — all of which a hand-rolled
	// matcher would get subtly wrong.
	cmd := exec.Command("git", "-C", root, "check-ignore", "-q", "--", dir)
	return cmd.Run() == nil
}

// candidateDirs returns the directories worth examining under one root: the root's own
// children, plus the children of every agent worktree it holds.
func candidateDirs(root string) []string {
	out := childDirs(root)
	// Agent worktrees hold their caches one level deeper.
	for _, worktree := range worktreeDirs(root) {
		out = append(out, childDirs(worktree)...)
	}
	return out
}

// worktreeDirs returns every agent worktree under root, from BOTH authorities that know about
// one: git's own registry and the on-disk container convention. See worktreeContainerPaths for
// why neither alone is sufficient.
func worktreeDirs(root string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, worktree := range gitLinkedWorktrees(root) {
		add(worktree)
	}
	for _, container := range worktreeContainerPaths {
		for _, worktree := range childDirs(filepath.Join(root, container)) {
			add(worktree)
		}
	}
	return out
}

// gitLinkedWorktrees asks git where root's linked worktrees actually are.
//
// Root's own checkout is excluded (it is the thing being scanned, not debt inside it), and so
// is any worktree outside root: a lane's scratch tree under /private/tmp belongs to whoever
// owns that path, and billing this root for those bytes would report space that reclaiming
// here cannot deliver.
func gitLinkedWorktrees(root string) []string {
	// Symlinks must be resolved on BOTH sides before the containment test. Git reports the
	// real path, so on macOS -- where /var, /tmp and every t.TempDir() are symlinks into
	// /private -- an unresolved comparison makes every worktree look like it lives outside
	// the root and silently scans nothing.
	rootAbs, err := resolvedDir(root)
	if err != nil {
		return nil
	}
	// Same instinct as looksLikeAccumulator: ask git rather than reimplement its bookkeeping.
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var worktrees []string
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !ok {
			continue
		}
		abs, err := resolvedDir(strings.TrimSpace(rest))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		worktrees = append(worktrees, abs)
	}
	return worktrees
}

// resolvedDir returns dir as an absolute, symlink-free path so two spellings of the same
// directory compare equal.
func resolvedDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// A path that cannot be resolved (it may not exist yet) is still usable as an absolute
	// path; only the symlink normalization is lost.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

func childDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, filepath.Join(dir, entry.Name()))
		}
	}
	return out
}

// walkEntryBudget caps how many entries one directory may cost.
//
// Sized from a real regression: the first version walked every file under the worktree
// containers (28 GB, hundreds of thousands of entries) and added 43 SECONDS to `compound
// doctor`. A health command that slow stops being run, which protects nothing — the exact
// failure this package was written to prevent, reintroduced by its own implementation.
const walkEntryBudget = 40_000

// dirBytes totals a directory's regular files, stopping at walkEntryBudget entries.
//
// Returns truncated=true when the budget was hit, so the caller reports a lower bound instead
// of presenting a partial sum as exact.
//
// Symlinks are never followed: a symlinked cache would otherwise be counted against whichever
// root happened to be scanned first, and could double-count or escape the root entirely.
func dirBytes(dir string) (int64, bool) {
	var total int64
	var seen int
	truncated := false
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a reason to abandon the whole count
		}
		seen++
		if seen > walkEntryBudget {
			truncated = true
			return filepath.SkipAll
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total, truncated
}

func sortInstances(items []RetentionInstance) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Bytes != items[j].Bytes {
			return items[i].Bytes > items[j].Bytes
		}
		return items[i].Path < items[j].Path
	})
}
