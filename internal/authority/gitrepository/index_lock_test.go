package gitrepository

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func indexLockFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	repo := initSnapshotRepo(t)
	// Preserve both staged and unstaged changes, not just a clean repository.
	writeSnapshotFile(t, repo, "seed.txt", "staged change\n")
	snapshotRunGit(t, repo, "add", "seed.txt")
	writeSnapshotFile(t, repo, "seed.txt", "unstaged change\n")
	now := time.Now().UTC()
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	return repo, now
}

func TestInspectIndexLockRefusesUnsafeStates(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, string, time.Time)
	}{
		{"fresh", func(t *testing.T, r string, n time.Time) {
			if err := os.Chtimes(filepath.Join(r, ".git/index.lock"), n, n); err != nil {
				t.Fatal(err)
			}
		}},
		{"nonempty", func(t *testing.T, r string, n time.Time) {
			writeSnapshotFile(t, r, ".git/index.lock", "unfinished index")
		}},
		{"symlink", func(t *testing.T, r string, n time.Time) {
			p := filepath.Join(r, ".git/index.lock")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("index", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, r string, n time.Time) {
			if err := os.Link(filepath.Join(r, ".git/index.lock"), filepath.Join(r, "linked-lock")); err != nil {
				t.Fatal(err)
			}
		}},
		{"merge", func(t *testing.T, r string, n time.Time) { writeSnapshotFile(t, r, ".git/MERGE_HEAD", "pending") }},
		{"rebase", func(t *testing.T, r string, n time.Time) {
			if err := os.Mkdir(filepath.Join(r, ".git/rebase-merge"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"other lock", func(t *testing.T, r string, n time.Time) { writeSnapshotFile(t, r, ".git/HEAD.lock", "") }},
		{"untracked", func(t *testing.T, r string, n time.Time) { writeSnapshotFile(t, r, "untracked.txt", "preserve me") }},
		{"hidden flag", func(t *testing.T, r string, n time.Time) {
			if err := os.Remove(filepath.Join(r, ".git/index.lock")); err != nil {
				t.Fatal(err)
			}
			snapshotRunGit(t, r, "update-index", "--assume-unchanged", "seed.txt")
			writeSnapshotFile(t, r, ".git/index.lock", "")
			old := n.Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(r, ".git/index.lock"), old, old); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, now := indexLockFixture(t)
			tc.change(t, r, now)
			if _, err := InspectIndexLock(context.Background(), r, now, idleIndexLock); err == nil {
				t.Fatal("unsafe repository accepted")
			}
			if _, err := os.Lstat(filepath.Join(r, ".git/index.lock")); err != nil {
				t.Fatal("inspection removed lock")
			}
		})
	}
}

func TestInspectIndexLockRejectsUnknownLiveness(t *testing.T) {
	r, now := indexLockFixture(t)
	for _, probe := range []IndexLockLiveness{nil, func(context.Context, ...string) error { return fmt.Errorf("probe unreadable") }} {
		if _, err := InspectIndexLock(context.Background(), r, now, probe); err == nil {
			t.Fatal("unknown liveness accepted")
		}
	}
}

func TestInspectIndexLockRejectsWorkingSymlink(t *testing.T) {
	r, now := indexLockFixture(t)
	if err := os.Remove(filepath.Join(r, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/config", filepath.Join(r, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectIndexLock(context.Background(), r, now, idleIndexLock); err == nil {
		t.Fatal("working symlink accepted")
	}
}

func TestInspectIndexLockIgnoresAmbientGitAndExternalDiff(t *testing.T) {
	r, now := indexLockFixture(t)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "wrong-index"))
	t.Setenv("GIT_EXTERNAL_DIFF", "false")
	snapshotRunGit(t, r, "config", "core.fsmonitor", "false")
	if _, err := InspectIndexLock(context.Background(), r, now, idleIndexLock); err != nil {
		t.Fatal(err)
	}
}

func TestIndexLockBackupBoundMatchesSerializedRepresentation(t *testing.T) {
	state := IndexLockState{Index: []byte("index"), StagedPatch: []byte("binary patch one"), UnstagedPatch: []byte("binary patch two")}
	body, err := marshalIndexLockBackup(state, indexLockMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marshalIndexLockBackup(state, len(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := marshalIndexLockBackup(state, len(body)-1); err == nil {
		t.Fatal("accepted a backup the reader would refuse")
	}
}

func TestInspectIndexLockRejectsExternalCleanFilters(t *testing.T) {
	r, now := indexLockFixture(t)
	snapshotRunGit(t, r, "config", "filter.synthetic.clean", "false")
	if _, err := InspectIndexLock(context.Background(), r, now, idleIndexLock); err == nil {
		t.Fatal("external clean filter accepted")
	}
}

func idleIndexLock(context.Context, ...string) error { return nil }

func TestInspectIndexLockPreservesIndexAndBothWIPPatches(t *testing.T) {
	repo, now := indexLockFixture(t)
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := InspectIndexLock(context.Background(), repo, now, idleIndexLock)
	if err != nil {
		t.Fatal(err)
	}
	if state.Snapshot.Lock.Size != 0 || state.Snapshot.Lock.Inode == 0 || state.Snapshot.Head == "" {
		t.Fatalf("missing exact lock/repository identity: %+v", state.Snapshot)
	}
	if !bytes.Equal(state.Index, before) || !bytes.Contains(state.StagedPatch, []byte("staged change")) || !bytes.Contains(state.UnstagedPatch, []byte("unstaged change")) {
		t.Fatal("inspection did not preserve index and both real WIP patches")
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("inspection changed index: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); err != nil {
		t.Fatal("inspection removed lock")
	}
}
