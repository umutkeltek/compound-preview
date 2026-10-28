package authority

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Attribution must stay O(roots + records). Resolving symlinks per
// (root, record) PAIR is what took the live inventory over its 5s advisory
// budget at 390 worktrees, which degraded every row to `unknown` and pinned
// scratch_worktree_debt at 0 — the sweep could never fire, and the metric read
// "no debt" while the debt grew.
//
// The bound is asserted by COUNTING resolutions rather than by wall clock: a
// timing assertion on a machine that routinely runs several test lanes would
// flake, and flaky perf gates get widened until they assert nothing.
func TestLSOFAttributionResolvesPathsLinearlyNotPerRootRecordPair(t *testing.T) {
	rootParent := t.TempDir()
	const rootCount = 200
	const recordCount = 400
	roots := make([]string, rootCount)
	for i := range roots {
		roots[i] = filepath.Join(rootParent, "worktree-"+strconv.Itoa(i))
	}

	var stdout strings.Builder
	// One attributable record, so the matcher still does real work.
	stdout.WriteString("p1\nfcwd\nn" + filepath.Join(roots[rootCount-1], "active.go") + "\n")
	for i := 0; i < recordCount-1; i++ {
		stdout.WriteString("p2\nfcwd\nn/synthetic/outside/file-")
		stdout.WriteString(strconv.Itoa(i))
		stdout.WriteByte('\n')
	}

	var resolutions int64
	restore := worktreeInventoryEvalSymlinks
	worktreeInventoryEvalSymlinks = func(path string) (string, error) {
		atomic.AddInt64(&resolutions, 1)
		return restore(path)
	}
	defer func() { worktreeInventoryEvalSymlinks = restore }()

	activePaths, complete, records := attributeWorktreeInventoryLSOFPaths(
		context.Background(), stdout.String(), roots,
	)
	if !complete {
		t.Fatalf("attribution incomplete on an unbounded context: records=%d", records)
	}
	if got := len(activePaths); got != 1 {
		t.Fatalf("attributed roots = %d, want 1", got)
	}

	// Linear: one resolution per root at construction, one per record.
	// The pair-wise implementation performed up to 2*roots*records = 160,000.
	budget := int64(rootCount + recordCount + 8)
	if got := atomic.LoadInt64(&resolutions); got > budget {
		t.Fatalf("path resolutions = %d, want <= %d (roots=%d records=%d) — "+
			"attribution is resolving per root-record pair again, which takes the "+
			"live inventory over its advisory budget and degrades every row to unknown",
			got, budget, rootCount, recordCount)
	}
}

// A canonical match against the more specific child must still beat a string
// match against a shorter ancestor, or hoisting the resolution out of the loop
// would report a live child worktree as unused.
func TestLSOFAttributionPrefersCanonicalChildOverStringMatchedAncestor(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalChild, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	// lsof reports a process cwd, which is an existing directory: the canonical
	// fallback resolves the candidate, so a non-existent path would not match
	// under either the old or the new implementation.
	stdout := "p1\nfcwd\nn" + canonicalChild + "\n"

	activePaths, complete, _ := attributeWorktreeInventoryLSOFPaths(
		context.Background(), stdout, []string{parent, child},
	)
	if !complete {
		t.Fatal("attribution incomplete")
	}
	if _, ok := activePaths[child]; !ok {
		t.Fatalf("child root not attributed; activePaths=%+v", activePaths)
	}
	if _, ok := activePaths[parent]; ok {
		t.Fatalf("ancestor absorbed the child's handle; activePaths=%+v", activePaths)
	}
}
