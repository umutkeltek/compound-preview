package authority

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

var identityTestTime = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// A linked worktree is deleted when its branch lands, so its toplevel must never
// become a permanent identifier on the repository entity. Identifiers are never
// evicted and the row is re-appended in full on every change, so one identifier
// per worktree is how a 185KB row and a 614MB registry get built.
func TestGitSnapshotIdentityIgnoresLinkedWorktreePaths(t *testing.T) {
	repo := initGitRepo(t)
	// Without a remote the repo id falls back to the directory basename, which
	// would make a worktree look like a different repository for reasons that
	// have nothing to do with what this test is about.
	runGit(t, repo, "remote", "add", "origin", "git@example.invalid:acme/widget.git")
	worktree := filepath.Join(t.TempDir(), "wt-feature")
	runGit(t, repo, "worktree", "add", "-b", "feature", worktree)

	mainSnap, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	wtSnap, wtEntity, _, _, err := ImportGitSnapshot(worktree, nil)
	if err != nil {
		t.Fatal(err)
	}

	if wtSnap.GitRoot == mainSnap.GitRoot {
		t.Fatalf("precondition: worktree toplevel should differ from the main root, both are %q", wtSnap.GitRoot)
	}
	if wtSnap.RepoID != mainSnap.RepoID {
		t.Fatalf("a worktree must resolve to the same repo entity: %q vs %q", wtSnap.RepoID, mainSnap.RepoID)
	}
	if wtSnap.MainWorktreeRoot != mainSnap.GitRoot {
		t.Fatalf("main worktree root %q should be the main checkout %q", wtSnap.MainWorktreeRoot, mainSnap.GitRoot)
	}
	for _, id := range wtEntity.Identifiers {
		if strings.HasPrefix(id, worktree) {
			t.Fatalf("worktree path %q leaked into entity identifiers: %v", id, wtEntity.Identifiers)
		}
	}
}

// A file path inside the repo resolves through the root identifier by prefix, so
// recording it separately adds an identifier that buys nothing.
func TestGitSnapshotIdentityIgnoresPathsCoveredByTheRoot(t *testing.T) {
	// macOS resolves the temp root through /var -> /private/var, and the
	// snapshot canonicalizes the git root while the caller's path is left as
	// given. Canonicalize here so the test exercises the covered-path rule
	// rather than that symlink, which is its own (kept) case below.
	repo, err := filepath.EvalSymlinks(initGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "README.md")

	_, entity, _, _, err := ImportGitSnapshot(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range entity.Identifiers {
		if id == target {
			t.Fatalf("a path under the repo root became its own identifier: %v", entity.Identifiers)
		}
	}

	// The point of dropping it is that resolution is unchanged.
	store, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterEntity(entity, identityTestTime); err != nil {
		t.Fatal(err)
	}
	got, err := store.ResolveEntity(target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != entity.Slug {
		t.Fatalf("resolving %q gave %+v, want slug %q", target, got, entity.Slug)
	}
}

// Repeated imports of an unchanged repository must stop writing rows; this is
// the other half of what made the registry grow superlinearly.
func TestRepeatedGitImportsStopAppendingRows(t *testing.T) {
	repo := initGitRepo(t)
	store, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		existing, err := store.Entities()
		if err != nil {
			t.Fatal(err)
		}
		_, entity, _, _, err := ImportGitSnapshot(repo, existing)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.RegisterEntity(entity, identityTestTime); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.FoldEntities(identityTestTime, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.RowsBefore != 1 {
		t.Fatalf("20 unchanged imports wrote %d rows, want 1", report.RowsBefore)
	}
}

// The git root is canonicalized on read but identifier matching is literal, so
// dropping the requested path outright stopped a repo from resolving by the
// spelling the caller actually uses (on macOS every path under /var). The root's
// alias is kept for exactly this, and because matching is prefix-aware it also
// covers paths beneath it.
func TestGitSnapshotIdentityKeepsNonCanonicalRootSpelling(t *testing.T) {
	repo := initGitRepo(t)
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if repo == canonical {
		t.Skip("temp dir is already canonical; nothing to prove on this platform")
	}

	_, entity, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterEntity(entity, identityTestTime); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{repo, canonical, filepath.Join(repo, "README.md")} {
		got, err := store.ResolveEntity(token)
		if err != nil {
			t.Fatal(err)
		}
		if got.Slug != entity.Slug {
			t.Fatalf("resolving %q gave %+v, want %q", token, got, entity.Slug)
		}
	}
	// Bounded: the alias is one extra identifier, not one per inspected path.
	if len(entity.Identifiers) > 4 {
		t.Fatalf("identifier set is not bounded: %v", entity.Identifiers)
	}
}
