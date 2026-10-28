package gitrepository

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise an older Git's explicit option refusal while using real Git for
// object reads and the compatible path. A clean range must not fail merely
// because --attr-source is unavailable, and mutable attributes must not hide
// errors or introduce errors absent from the reviewed tree.
func TestCommittedDiffCheckOlderGitKeepsReviewedAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, attributes, body string
		wantWhitespace         bool
	}{
		{"clean", "*.txt whitespace=blank-at-eol\n", "clean\n", false},
		{"reject-whitespace", "*.txt whitespace=blank-at-eol\n", "bad \n", true},
		{"reviewed-exemption", "*.txt -whitespace\n", "allowed \n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
			writeSnapshotFile(t, repo, ".gitattributes", "*.txt whitespace=blank-at-eol\n")
			if err := os.Mkdir(filepath.Join(repo, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			writeSnapshotFile(t, repo, "nested/.gitattributes", tc.attributes)
			writeSnapshotFile(t, repo, "nested/candidate.txt", tc.body)
			snapshotRunGit(t, repo, "add", "-A")
			snapshotRunGit(t, repo, "commit", "-m", "reviewed attributes")
			head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

			mutable := "*.txt -whitespace\n"
			if !tc.wantWhitespace {
				mutable = "*.txt whitespace=blank-at-eol\n"
			}
			writeSnapshotFile(t, repo, ".gitattributes", mutable)
			writeSnapshotFile(t, repo, "nested/.gitattributes", mutable)
			writeSnapshotFile(t, repo, "index-only.txt", "staged user work\n")
			snapshotRunGit(t, repo, "add", "-A")
			indexPath := snapshotGitOutput(t, repo, "rev-parse", "--git-path", "index")
			if !filepath.IsAbs(indexPath) {
				indexPath = filepath.Join(repo, indexPath)
			}
			indexBefore, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			ambient := filepath.Join(t.TempDir(), "ambient-index")
			sentinel := []byte("ambient selector must remain untouched")
			if err := os.WriteFile(ambient, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_INDEX_FILE", ambient)
			t.Setenv("GIT_WORK_TREE", t.TempDir())
			t.Setenv("GIT_ATTR_SOURCE", base)

			var commands []*exec.Cmd
			ctx := ContextWithCommandFactory(t.Context(), func(ctx context.Context, name string, args ...string) *exec.Cmd {
				cmd := olderGitAttributeCommand(ctx, name, args...)
				commands = append(commands, cmd)
				return cmd
			})
			err = CommittedDiffCheck(ctx, repo, base, head)
			if tc.wantWhitespace {
				if err == nil || !strings.Contains(err.Error(), "nested/candidate.txt:1:") {
					t.Fatalf("diff-check = %v, want the reviewed file's whitespace rejection", err)
				}
			} else if err != nil {
				t.Fatalf("older Git rejected a clean reviewed range: %v", err)
			}
			after, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(after, indexBefore) {
				t.Fatalf("caller index changed: %v", err)
			}
			after, err = os.ReadFile(ambient)
			if err != nil || !bytes.Equal(after, sentinel) {
				t.Fatalf("ambient index changed: %v", err)
			}
			for _, path := range []string{".gitattributes", "nested/.gitattributes"} {
				body, err := os.ReadFile(filepath.Join(repo, path))
				if err != nil || string(body) != mutable {
					t.Fatalf("caller attributes %s changed: %v", path, err)
				}
			}
			assertPrivateIndexesRemoved(t, commands)
		})
	}
}

func TestCommittedDiffCheckOlderGitCancellation(t *testing.T) {
	repo := initSnapshotRepo(t)
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var commands []*exec.Cmd
	ctx = ContextWithCommandFactory(ctx, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		for _, arg := range args {
			if arg == "read-tree" {
				cancel()
			}
		}
		cmd := olderGitAttributeCommand(ctx, name, args...)
		commands = append(commands, cmd)
		return cmd
	})
	err := CommittedDiffCheck(ctx, repo, head, head)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("fallback cancellation = %v, want context canceled", err)
	}
	assertPrivateIndexesRemoved(t, commands)
}

func TestCommittedDiffCheckDoesNotFallbackOnUnrelatedFailure(t *testing.T) {
	repo := initSnapshotRepo(t)
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	fallback := false
	ctx := ContextWithCommandFactory(t.Context(), func(ctx context.Context, name string, args ...string) *exec.Cmd {
		for _, arg := range args {
			if arg == "read-tree" {
				fallback = true
			}
			if strings.HasPrefix(arg, "--attr-source=") {
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'fatal: synthetic object read failure' >&2; exit 128")
			}
		}
		return exec.CommandContext(ctx, name, args...)
	})
	err := CommittedDiffCheck(ctx, repo, head, head)
	if err == nil || !strings.Contains(err.Error(), "synthetic object read failure") || fallback {
		t.Fatalf("unrelated failure = %v, fallback = %v", err, fallback)
	}
}

func olderGitAttributeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--attr-source=") {
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' \"$1\" >&2; exit 129", "git", "unknown option: "+arg)
		}
	}
	return exec.CommandContext(ctx, name, args...)
}

func assertPrivateIndexesRemoved(t *testing.T, commands []*exec.Cmd) {
	t.Helper()
	for _, cmd := range commands {
		for _, value := range cmd.Env {
			if path, ok := strings.CutPrefix(value, "GIT_INDEX_FILE="); ok {
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatalf("private index directory remains after the check: %v", err)
				}
			}
		}
	}
}

func TestCommittedDiffCheckOlderGitRepositoryLayouts(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "shared.txt", "existing whitespace \n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "shared ancestor")
	ancestor := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	snapshotRunGit(t, repo, "switch", "-c", "member-base")
	writeSnapshotFile(t, repo, "shared.txt", "cleaned by the other branch\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "diverged base")
	base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	snapshotRunGit(t, repo, "switch", "-c", "candidate", ancestor)
	writeSnapshotFile(t, repo, "candidate.txt", "clean addition\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "clean candidate")
	clean := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	writeSnapshotFile(t, repo, "candidate.txt", "introduced whitespace \n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "whitespace candidate")
	bad := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	linked := filepath.Join(t.TempDir(), "linked")
	snapshotRunGit(t, repo, "worktree", "add", "--detach", linked, clean)
	bare := filepath.Join(t.TempDir(), "bare.git")
	snapshotRunGit(t, repo, "clone", "--bare", repo, bare)
	for name, path := range map[string]string{"normal": repo, "linked": linked, "bare": bare} {
		t.Run(name, func(t *testing.T) {
			ctx := ContextWithCommandFactory(t.Context(), olderGitAttributeCommand)
			if err := CommittedDiffCheck(ctx, path, base, clean); err != nil {
				t.Fatalf("unchanged ancestor whitespace in a diverged range: %v", err)
			}
			err := CommittedDiffCheck(ctx, path, base, bad)
			if err == nil || !strings.Contains(err.Error(), "candidate.txt:1:") {
				t.Fatalf("introduced whitespace = %v, want reviewed-file refusal", err)
			}
		})
	}
}

func TestCommittedDiffCheckOlderGitDoesNotMutateRepository(t *testing.T) {
	for _, mode := range []string{"post-index-change", "split-index", "fsmonitor"} {
		t.Run(mode, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
			gitDir := snapshotGitOutput(t, repo, "rev-parse", "--absolute-git-dir")
			hooks := filepath.Join(t.TempDir(), "hooks")
			if err := os.Mkdir(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			snapshotRunGit(t, repo, "config", "core.hooksPath", hooks)
			snapshotRunGit(t, repo, "config", "core.splitIndex", "false")
			snapshotRunGit(t, repo, "config", "core.fsmonitor", "false")
			marker := filepath.Join(gitDir, "caller-side-effect")
			script := "#!/bin/sh\nprintf 'called\\n' >> '" + marker + "'\n"
			switch mode {
			case "post-index-change":
				if err := os.WriteFile(filepath.Join(hooks, "post-index-change"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			case "split-index":
				snapshotRunGit(t, repo, "config", "core.splitIndex", "true")
			case "fsmonitor":
				path := filepath.Join(hooks, "fsmonitor")
				if err := os.WriteFile(path, []byte(script+"printf 'token\\000'\n"), 0700); err != nil {
					t.Fatal(err)
				}
				snapshotRunGit(t, repo, "config", "core.fsmonitor", path)
			}
			before := snapshotGitDirectory(t, gitDir)
			ctx := ContextWithCommandFactory(t.Context(), olderGitAttributeCommand)
			if err := CommittedDiffCheck(ctx, repo, head, head); err != nil {
				t.Fatalf("clean range: %v", err)
			}
			after := snapshotGitDirectory(t, gitDir)
			changed := []string{}
			for name, data := range after {
				if old, ok := before[name]; !ok || old != data {
					changed = append(changed, name)
				}
			}
			for name := range before {
				if _, ok := after[name]; !ok {
					changed = append(changed, "removed:"+name)
				}
			}
			t.Logf("mode=%s caller Git directory changes=%v", mode, changed)
			if len(changed) > 0 {
				t.Fatalf("committed read modified caller Git directory: %v", changed)
			}
		})
	}
}

func snapshotGitDirectory(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
