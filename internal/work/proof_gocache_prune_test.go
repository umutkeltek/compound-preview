package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every fixture lives under t.TempDir(); no test here reads or deletes the real
// user cache root.

var proofGoCachePruneNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

type proofGoCacheFixture struct {
	name     string
	owner    string    // written to owner-path when non-empty
	lastUsed time.Time // last-used mtime when non-zero
	legacy   time.Time // marker-free cache: mtime of the dir and its children
}

func seedProofGoCache(t *testing.T, root string, fixture proofGoCacheFixture) string {
	t.Helper()
	dir := filepath.Join(root, fixture.name)
	if err := os.MkdirAll(filepath.Join(dir, "00"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("fake go cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fixture.owner != "" {
		if err := os.WriteFile(filepath.Join(dir, "owner-path"), []byte(fixture.owner+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !fixture.lastUsed.IsZero() {
		path := filepath.Join(dir, "last-used")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, fixture.lastUsed, fixture.lastUsed); err != nil {
			t.Fatal(err)
		}
	}
	if !fixture.legacy.IsZero() {
		for _, child := range []string{"00", "README", ""} {
			if err := os.Chtimes(filepath.Join(dir, child), fixture.legacy, fixture.legacy); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func proofGoCacheHexName(i int) string {
	return fmt.Sprintf("feedface%08x", 0xa0000000+i)
}

func proofGoCacheRemovedReasons(report ProofGoCachePruneReport) map[string]string {
	reasons := make(map[string]string, len(report.Removed))
	for _, removed := range report.Removed {
		reasons[removed.Name] = removed.Reason
	}
	return reasons
}

func requireProofGoCacheExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	switch {
	case want && err != nil:
		t.Fatalf("%s should still exist: %v", path, err)
	case !want && !errors.Is(err, os.ErrNotExist):
		t.Fatalf("%s should be removed, lstat err = %v", path, err)
	}
}

func TestPruneProofGoCachesAppliesOrphanIdleAndGuardRules(t *testing.T) {
	root := t.TempDir()
	liveOwner := t.TempDir()
	goneOwner := filepath.Join(t.TempDir(), "retired-worktree")
	now := proofGoCachePruneNow

	orphan := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(1), owner: goneOwner, lastUsed: now.Add(-24 * time.Hour)})
	idle := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(2), owner: liveOwner, lastUsed: now.Add(-96 * time.Hour)})
	guarded := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(3), owner: goneOwner, lastUsed: now.Add(-time.Hour)})
	current := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(4), owner: goneOwner, lastUsed: now.Add(-240 * time.Hour)})
	busy := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(5), owner: liveOwner, lastUsed: now.Add(-48 * time.Hour)})
	legacyOld := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(6), legacy: now.Add(-120 * time.Hour)})
	legacyFresh := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(7), legacy: now.Add(-time.Hour)})

	report, err := PruneProofGoCaches(root, current, now, DefaultProofGoCachePrunePolicy())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	reasons := proofGoCacheRemovedReasons(report)
	want := map[string]string{
		filepath.Base(orphan):    "orphan",
		filepath.Base(idle):      "idle",
		filepath.Base(legacyOld): "idle",
	}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Fatalf("removed = %v, want %v (errors %v)", reasons, want, report.Errors)
	}
	for _, path := range []string{orphan, idle, legacyOld} {
		requireProofGoCacheExists(t, path, false)
	}
	for _, path := range []string{guarded, current, busy, legacyFresh} {
		requireProofGoCacheExists(t, path, true)
	}
	kept := append([]string(nil), report.Kept...)
	sort.Strings(kept)
	wantKept := []string{filepath.Base(guarded), filepath.Base(current), filepath.Base(busy), filepath.Base(legacyFresh)}
	sort.Strings(wantKept)
	if fmt.Sprint(kept) != fmt.Sprint(wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors = %v", report.Errors)
	}
	if got, want := report.Summary(), "proof-gocache: kept 4, removed 3 (orphan 1, idle 2, lru 0)"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestPruneProofGoCachesTrimsLeastRecentlyUsedToSixteen(t *testing.T) {
	root := t.TempDir()
	owner := t.TempDir()
	now := proofGoCachePruneNow
	paths := make([]string, 20)
	for i := range paths {
		// i=0 is the newest (7h old), i=19 the oldest (26h old): all past the
		// six-hour guard, none idle.
		paths[i] = seedProofGoCache(t, root, proofGoCacheFixture{
			name:     proofGoCacheHexName(i),
			owner:    owner,
			lastUsed: now.Add(-time.Duration(7+i) * time.Hour),
		})
	}

	report, err := PruneProofGoCaches(root, "", now, DefaultProofGoCachePrunePolicy())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	reasons := proofGoCacheRemovedReasons(report)
	if len(reasons) != 4 || len(report.Kept) != 16 {
		t.Fatalf("removed %v kept %d, want 4 lru removals and 16 kept", reasons, len(report.Kept))
	}
	for i, path := range paths {
		oldest := i >= 16
		requireProofGoCacheExists(t, path, !oldest)
		if oldest && reasons[filepath.Base(path)] != "lru" {
			t.Fatalf("%s removal reason = %q, want lru", path, reasons[filepath.Base(path)])
		}
	}
}

func TestPruneProofGoCachesLRUHonoursSixHourGuardAndCurrent(t *testing.T) {
	root := t.TempDir()
	owner := t.TempDir()
	now := proofGoCachePruneNow
	var fresh []string
	for i := 0; i < 18; i++ {
		fresh = append(fresh, seedProofGoCache(t, root, proofGoCacheFixture{
			name: proofGoCacheHexName(i), owner: owner, lastUsed: now.Add(-time.Duration(i) * time.Minute),
		}))
	}
	current := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(30), owner: owner, lastUsed: now.Add(-60 * time.Hour)})
	stale := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(31), owner: owner, lastUsed: now.Add(-50 * time.Hour)})

	report, err := PruneProofGoCaches(root, current, now, DefaultProofGoCachePrunePolicy())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := proofGoCacheRemovedReasons(report); len(got) != 1 || got[filepath.Base(stale)] != "lru" {
		t.Fatalf("removed = %v, want only %s as lru", got, filepath.Base(stale))
	}
	for _, path := range append(fresh, current) {
		requireProofGoCacheExists(t, path, true)
	}
	if len(report.Kept) != 19 {
		t.Fatalf("kept %d, want 19 (guarded caches may exceed the cap)", len(report.Kept))
	}
}

func TestPruneProofGoCachesLeavesNonCacheEntriesAndSymlinkTargetsAlone(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	now := proofGoCachePruneNow
	goneOwner := filepath.Join(t.TempDir(), "retired-worktree")
	old := now.Add(-240 * time.Hour)

	// Decoys: wrong names, a symlink and a plain file under cache-shaped names.
	notHex := seedProofGoCache(t, root, proofGoCacheFixture{name: "not-a-go-cache", owner: goneOwner, lastUsed: old})
	upper := seedProofGoCache(t, root, proofGoCacheFixture{name: "FEEDFACEA00000AB", owner: goneOwner, lastUsed: old})
	short := seedProofGoCache(t, root, proofGoCacheFixture{name: "feedfacea00000a", owner: goneOwner, lastUsed: old})
	target := seedProofGoCache(t, outside, proofGoCacheFixture{name: proofGoCacheHexName(1), owner: goneOwner, lastUsed: old})
	link := filepath.Join(root, proofGoCacheHexName(1))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, proofGoCacheHexName(2))
	if err := os.WriteFile(file, []byte("not a dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real orphan cache holding a symlink to data outside the root: the cache
	// goes, the data it points at stays.
	precious := filepath.Join(outside, "precious")
	if err := os.MkdirAll(precious, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(precious, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(3), owner: goneOwner, lastUsed: old})
	if err := os.Symlink(precious, filepath.Join(orphan, "escape")); err != nil {
		t.Fatal(err)
	}

	report, err := PruneProofGoCaches(root, "", now, DefaultProofGoCachePrunePolicy())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := proofGoCacheRemovedReasons(report); len(got) != 1 || got[filepath.Base(orphan)] != "orphan" {
		t.Fatalf("removed = %v, want only the real orphan cache", got)
	}
	requireProofGoCacheExists(t, orphan, false)
	for _, path := range []string{notHex, upper, short, link, target, file, filepath.Join(precious, "keep.txt")} {
		requireProofGoCacheExists(t, path, true)
	}
	if len(report.Kept) != 0 {
		t.Fatalf("kept = %v, decoys must not count as caches", report.Kept)
	}
}

func TestPruneProofGoCachesRefusesUnsafeRoots(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	homeLink := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(userHome, homeLink); err != nil {
		t.Fatal(err)
	}
	sentinel := seedProofGoCache(t, userHome, proofGoCacheFixture{name: proofGoCacheHexName(1), lastUsed: proofGoCachePruneNow.Add(-240 * time.Hour)})

	for _, root := range []string{"", "   ", string(filepath.Separator), userHome, homeLink} {
		report, err := PruneProofGoCaches(root, "", proofGoCachePruneNow, DefaultProofGoCachePrunePolicy())
		if err == nil {
			t.Fatalf("root %q accepted: %+v", root, report)
		}
		if len(report.Removed) != 0 {
			t.Fatalf("root %q removed %v", root, report.Removed)
		}
	}
	requireProofGoCacheExists(t, sentinel, true)
}

func TestPruneProofGoCachesRefusesUnsafePolicy(t *testing.T) {
	root := t.TempDir()
	cache := seedProofGoCache(t, root, proofGoCacheFixture{name: proofGoCacheHexName(1), lastUsed: proofGoCachePruneNow.Add(-240 * time.Hour)})
	for _, policy := range []ProofGoCachePrunePolicy{
		{},
		{RecentGuard: 6 * time.Hour, IdleAfter: time.Hour, MaxKept: 16},
		{RecentGuard: 6 * time.Hour, IdleAfter: 72 * time.Hour},
	} {
		if _, err := PruneProofGoCaches(root, "", proofGoCachePruneNow, policy); err == nil {
			t.Fatalf("policy %+v accepted", policy)
		}
	}
	requireProofGoCacheExists(t, cache, true)
}

func TestProofGoCacheWritesOwnerAndRefreshesLastUsed(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(userHome, "xdg-cache"))
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	cacheBase, err := os.UserCacheDir()
	if err != nil || !strings.HasPrefix(cacheBase, userHome+string(filepath.Separator)) {
		t.Fatalf("user cache dir %q (err %v) is not isolated under %q", cacheBase, err, userHome)
	}
	repo := t.TempDir()
	wantOwner, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}

	dir, ok := ProofGoCache(repo)
	if !ok || !strings.HasPrefix(dir, cacheBase+string(filepath.Separator)) {
		t.Fatalf("ProofGoCache = %q, %v; want a dir under %q", dir, ok, cacheBase)
	}
	owner, err := os.ReadFile(filepath.Join(dir, "owner-path"))
	if err != nil {
		t.Fatalf("owner-path: %v", err)
	}
	if strings.TrimSpace(string(owner)) != wantOwner {
		t.Fatalf("owner-path = %q, want %q", owner, wantOwner)
	}
	lastUsed := filepath.Join(dir, "last-used")
	info, err := os.Lstat(lastUsed)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("last-used: %v", err)
	}
	if age := time.Since(info.ModTime()); age < -time.Minute || age > time.Minute {
		t.Fatalf("new last-used mtime %v is not current", info.ModTime())
	}

	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(lastUsed, stale, stale); err != nil {
		t.Fatal(err)
	}
	if again, ok := ProofGoCache(repo); !ok || again != dir {
		t.Fatalf("second ProofGoCache = %q, %v; want %q", again, ok, dir)
	}
	info, err = os.Lstat(lastUsed)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(info.ModTime()); age > time.Minute {
		t.Fatalf("last-used was not refreshed: mtime %v", info.ModTime())
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "owner-path")); string(again) != string(owner) {
		t.Fatalf("owner-path rewritten: %q -> %q", owner, again)
	}
}
