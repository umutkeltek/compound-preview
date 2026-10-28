package gitrepository

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureCleanSnapshotResolvesSymbolicBaseOnce(t *testing.T) {
	repo := initSnapshotRepo(t)
	baseSHA := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	snapshotRunGit(t, repo, "switch", "-c", "candidate")
	writeSnapshotFile(t, repo, "candidate.txt", "candidate\n")
	snapshotRunGit(t, repo, "add", "candidate.txt")
	snapshotRunGit(t, repo, "commit", "-m", "candidate")

	got, err := CaptureCleanSnapshot(repo, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != baseSHA {
		t.Fatalf("base_sha = %q, want %q", got.BaseSHA, baseSHA)
	}
	if got.HeadSHA != snapshotGitOutput(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("head_sha = %q, want current committed HEAD", got.HeadSHA)
	}
	if got.TreeSHA != snapshotGitOutput(t, repo, "rev-parse", "HEAD^{tree}") {
		t.Fatalf("tree_sha = %q, want HEAD tree", got.TreeSHA)
	}
	if got.RepositoryFingerprint == "" || !got.Clean {
		t.Fatalf("snapshot = %+v, want fingerprinted clean source", got)
	}

	// The caller supplied a moving ref, but the captured snapshot sealed its
	// resolved commit. Moving the symbolic ref after capture cannot change the
	// range or invalidate the already-started battery.
	snapshotRunGit(t, repo, "branch", "-f", "main", got.HeadSHA)
	if err := RevalidateSnapshot(repo, got); err != nil {
		t.Fatalf("moving symbolic base changed sealed identity: %v", err)
	}
}

func TestCaptureAndRevalidateSnapshotSupportsSHA256Repository(t *testing.T) {
	repo := initSHA256SnapshotRepo(t)
	baseSHA := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	snapshotRunGit(t, repo, "switch", "-c", "candidate")
	writeSnapshotFile(t, repo, "candidate.txt", "candidate\n")
	snapshotRunGit(t, repo, "add", "candidate.txt")
	snapshotRunGit(t, repo, "commit", "-m", "candidate")

	got, err := CaptureCleanSnapshot(repo, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(baseSHA) != 64 ||
		len(got.BaseSHA) != 64 ||
		len(got.HeadSHA) != 64 ||
		len(got.TreeSHA) != 64 {
		t.Fatalf("sha256 snapshot did not retain full object ids: base=%q snapshot=%+v", baseSHA, got)
	}
	if got.BaseSHA != baseSHA ||
		got.HeadSHA != snapshotGitOutput(t, repo, "rev-parse", "HEAD") ||
		got.TreeSHA != snapshotGitOutput(t, repo, "rev-parse", "HEAD^{tree}") {
		t.Fatalf("sha256 snapshot identity mismatch: %+v", got)
	}
	if err := RevalidateSnapshot(repo, got); err != nil {
		t.Fatalf("revalidate sha256 snapshot: %v", err)
	}
}

func TestCaptureCleanSnapshotRejectsDirtyCandidate(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "uncommitted.txt", "not committed\n")

	_, err := CaptureCleanSnapshot(repo, "refs/heads/main")
	if err == nil || !strings.Contains(err.Error(), "clean committed") {
		t.Fatalf("dirty capture error = %v, want clean committed refusal", err)
	}
}

func TestCaptureCleanSnapshotRejectsHiddenTrackedMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    string
		wantErr string
	}{
		{name: "assume unchanged", flag: "--assume-unchanged", wantErr: "assume-unchanged"},
		{name: "skip worktree", flag: "--skip-worktree", wantErr: "skip-worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			snapshotRunGit(t, repo, "update-index", tc.flag, "seed.txt")
			writeSnapshotFile(t, repo, "seed.txt", "hidden mutation\n")

			_, err := CaptureCleanSnapshot(repo, "refs/heads/main")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("hidden tracked mutation error = %v, want %s refusal", err, tc.wantErr)
			}
		})
	}
}

func TestCaptureCleanSnapshotRejectsDirtySubmoduleDespiteIgnoreConfig(t *testing.T) {
	submodule := t.TempDir()
	snapshotRunGit(t, submodule, "init", "-b", "main")
	snapshotRunGit(t, submodule, "config", "user.email", "test@example.invalid")
	snapshotRunGit(t, submodule, "config", "user.name", "Compound Test")
	writeSnapshotFile(t, submodule, "tracked.txt", "committed\n")
	snapshotRunGit(t, submodule, "add", "tracked.txt")
	snapshotRunGit(t, submodule, "commit", "-m", "submodule seed")

	repo := initSnapshotRepo(t)
	snapshotRunGit(
		t,
		repo,
		"-c",
		"protocol.file.allow=always",
		"submodule",
		"add",
		submodule,
		"modules/child",
	)
	snapshotRunGit(t, repo, "commit", "-am", "add submodule")
	snapshotRunGit(t, repo, "config", "submodule.modules/child.ignore", "all")
	writeSnapshotFile(t, filepath.Join(repo, "modules/child"), "tracked.txt", "dirty\n")

	_, err := CaptureCleanSnapshot(repo, "refs/heads/main")
	if err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("ignored dirty submodule error = %v, want explicit submodule refusal", err)
	}
}

func TestRevalidateSnapshotRejectsHeadTreeAndDirtyMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, repo string)
	}{
		{
			name: "head and tree",
			mutate: func(t *testing.T, repo string) {
				writeSnapshotFile(t, repo, "later.txt", "later\n")
				snapshotRunGit(t, repo, "add", "later.txt")
				snapshotRunGit(t, repo, "commit", "-m", "later")
			},
		},
		{
			name: "dirty",
			mutate: func(t *testing.T, repo string) {
				writeSnapshotFile(t, repo, "dirty.txt", "dirty\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			sealed, err := CaptureCleanSnapshot(repo, "refs/heads/main")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, repo)
			if err := RevalidateSnapshot(repo, sealed); err == nil {
				t.Fatal("mutated repository revalidated against sealed source snapshot")
			}
		})
	}
}

func initSnapshotRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	snapshotRunGit(t, repo, "init", "-b", "main")
	snapshotRunGit(t, repo, "config", "user.email", "test@example.invalid")
	snapshotRunGit(t, repo, "config", "user.name", "Compound Test")
	snapshotRunGit(t, repo, "remote", "add", "origin", "https://example.invalid/compound/synthetic.git")
	writeSnapshotFile(t, repo, "seed.txt", "seed\n")
	snapshotRunGit(t, repo, "add", "seed.txt")
	snapshotRunGit(t, repo, "commit", "-m", "seed")
	return repo
}

func initSHA256SnapshotRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "--object-format=sha256", "-b", "main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "sha256") &&
			(strings.Contains(lower, "not supported") ||
				strings.Contains(lower, "unsupported") ||
				strings.Contains(lower, "unknown object format")) {
			t.Skipf("git explicitly reports SHA-256 object format unsupported: %s", out)
		}
		t.Fatalf("git init --object-format=sha256: %v\n%s", err, out)
	}
	snapshotRunGit(t, repo, "config", "user.email", "test@example.invalid")
	snapshotRunGit(t, repo, "config", "user.name", "Compound Test")
	snapshotRunGit(t, repo, "remote", "add", "origin", "https://example.invalid/compound/sha256-synthetic.git")
	writeSnapshotFile(t, repo, "seed.txt", "seed\n")
	snapshotRunGit(t, repo, "add", "seed.txt")
	snapshotRunGit(t, repo, "commit", "-m", "seed")
	return repo
}

func writeSnapshotFile(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotRunGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func snapshotGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
