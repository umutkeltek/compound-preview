package contract

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/harness"
	"golang.org/x/sys/unix"
)

func worktreeKindGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitrepository.ReadEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func worktreeKindFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "primary")
	linked := filepath.Join(root, "lane")
	home := t.TempDir()
	if err := os.Mkdir(main, 0700); err != nil {
		t.Fatal(err)
	}
	worktreeKindGit(t, main, "init", "-q")
	worktreeKindGit(t, main, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "seed")
	worktreeKindGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
	worktreeKindRegistry(t, home, fmt.Sprintf(" - kind: control-plane\n   roots: [%q]\n", main))
	return main, linked, home
}

func worktreeKindRegistry(t *testing.T, home, rows string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "repo-kinds.yaml"), []byte("schema_version: "+repoKindRegistrySchema+"\nrepos:\n"+rows), 0600); err != nil {
		t.Fatal(err)
	}
}

func worktreeKindResolve(t *testing.T, home, cwd, want string) string {
	t.Helper()
	kind, via, err := ResolveRepoKindWithHome(home, "", cwd)
	if err != nil || kind != want {
		t.Fatalf("kind=%q via=%q err=%v; want %s", kind, via, err, want)
	}
	return via
}

func TestLinkedWorktreeKindPreservesContextAndIdentity(t *testing.T) {
	main, linked, home := worktreeKindFixture(t)
	nested := filepath.Join(linked, "a", "b")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{main, linked, nested} {
		via := worktreeKindResolve(t, home, cwd, "control-plane")
		if cwd != main && !strings.HasPrefix(via, "repo-kind-linked-worktree:") {
			t.Fatalf("missing declared provenance: %s", via)
		}
		input, err := ResolveOrientationIdentity(Input{Harness: "codex", Cwd: cwd}, harness.DefaultRegistry())
		if err != nil {
			t.Fatal(err)
		}
		physical, _ := filepath.EvalSymlinks(cwd)
		if input.Cwd != physical {
			t.Fatalf("workspace rewritten: %s != %s", input.Cwd, physical)
		}
		input.RepoKind = "control-plane"
		input.RepoKindVia = via
		pack := Pack(input, DefaultPolicy(), time.Now())
		for _, ref := range []string{"control-plane://orientation-map", "control-plane://workflow-contract", "control-plane://sop-packets"} {
			if !hasIncluded(pack, ref) {
				t.Fatalf("missing governance source %s at %s", ref, cwd)
			}
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(linked, alias); err != nil {
		t.Fatal(err)
	}
	worktreeKindResolve(t, home, alias, "control-plane")
}

func TestLinkedWorktreeKindDoesNotInferFromClone(t *testing.T) {
	main, _, home := worktreeKindFixture(t)
	clone := filepath.Join(t.TempDir(), "same-name")
	worktreeKindGit(t, main, "clone", "--quiet", main, clone)
	worktreeKindGit(t, clone, "remote", "set-url", "origin", "https://source.example/repo.git")
	worktreeKindGit(t, main, "remote", "add", "origin", "https://source.example/repo.git")
	worktreeKindResolve(t, home, clone, "product")
	worktreeKindResolve(t, home, t.TempDir(), "product")
}

func TestLinkedWorktreeKindDoesNotWidenSubtree(t *testing.T) {
	main, linked, home := worktreeKindFixture(t)
	docs := filepath.Join(main, "docs")
	if err := os.Mkdir(docs, 0700); err != nil {
		t.Fatal(err)
	}
	worktreeKindRegistry(t, home, fmt.Sprintf(" - kind: control-plane\n   roots: [%q]\n", docs))
	worktreeKindResolve(t, home, linked, "product")
	worktreeKindResolve(t, home, docs, "control-plane")
}

func TestLinkedWorktreeKindPrecedenceAndConflicts(t *testing.T) {
	main, linked, home := worktreeKindFixture(t)
	worktreeKindRegistry(t, home, fmt.Sprintf(" - kind: control-plane\n   roots: [%q]\n - kind: library\n   roots: [%q]\n", main, linked))
	other := filepath.Join(t.TempDir(), "other")
	worktreeKindGit(t, main, "worktree", "add", "--detach", other, "HEAD")
	if _, _, err := ResolveRepoKindWithHome(home, "", other); err == nil {
		t.Fatal("conflicting declarations accepted")
	}
	worktreeKindResolve(t, home, linked, "library")
	if err := os.MkdirAll(filepath.Join(other, ".compound"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".compound", "repo.yaml"), []byte("kind: product\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeKindResolve(t, home, other, "product")
	kind, via, err := ResolveRepoKindWithHome(home, "library", other)
	if err != nil || kind != "library" || via != "flag" {
		t.Fatalf("flag precedence: %s %s %v", kind, via, err)
	}
}

func TestLinkedWorktreeKindIgnoresAmbientGitSelectors(t *testing.T) {
	_, linked, home := worktreeKindFixture(t)
	for key, value := range map[string]string{
		"GIT_DIR": filepath.Join(t.TempDir(), "missing"), "GIT_WORK_TREE": t.TempDir(),
		"GIT_COMMON_DIR": t.TempDir(), "GIT_INDEX_FILE": filepath.Join(t.TempDir(), "index"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.worktree", "GIT_CONFIG_VALUE_0": t.TempDir(),
	} {
		t.Setenv(key, value)
	}
	worktreeKindResolve(t, home, linked, "control-plane")
}

func TestLinkedWorktreeKindRejectsForgedMetadata(t *testing.T) {
	for _, scenario := range []string{"copied-gitfile", "symlink-gitfile", "bad-gitfile", "fifo-gitfile", "missing-backpointer", "wrong-backpointer", "symlink-backpointer", "oversized-backpointer", "escaped-admin"} {
		t.Run(scenario, func(t *testing.T) {
			_, linked, home := worktreeKindFixture(t)
			id, ok, err := detectLinkedWorktree(linked)
			if err != nil || !ok {
				t.Fatalf("fixture: %v %v", ok, err)
			}
			cwd := linked
			gitfile := filepath.Join(linked, ".git")
			back := filepath.Join(id.GitDir, "gitdir")
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			remove := func(path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "copied-gitfile":
				cwd = t.TempDir()
				data, err := os.ReadFile(gitfile)
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(cwd, ".git"), string(data))
			case "symlink-gitfile":
				cwd = t.TempDir()
				if err := os.Symlink(gitfile, filepath.Join(cwd, ".git")); err != nil {
					t.Fatal(err)
				}
			case "bad-gitfile":
				write(gitfile, "not git metadata\n")
			case "fifo-gitfile":
				remove(gitfile)
				if err := unix.Mkfifo(gitfile, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-backpointer":
				remove(back)
			case "wrong-backpointer":
				write(back, filepath.Join(t.TempDir(), ".git")+"\n")
			case "symlink-backpointer":
				target := filepath.Join(t.TempDir(), "pointer")
				write(target, gitfile+"\n")
				remove(back)
				if err := os.Symlink(target, back); err != nil {
					t.Fatal(err)
				}
			case "oversized-backpointer":
				write(back, strings.Repeat("x", 16385))
			case "escaped-admin":
				// The real common directory no longer owns this worktree administration.
				write(filepath.Join(id.GitDir, "commondir"), t.TempDir()+"\n")
			}
			if kind, via, err := ResolveRepoKindWithHome(home, "", cwd); err == nil {
				t.Fatalf("accepted forged metadata as %s via %s", kind, via)
			}
		})
	}
}

func TestLinkedWorktreeKindRelativePaths(t *testing.T) {
	main, _, home := worktreeKindFixture(t)
	relative := filepath.Join(t.TempDir(), "relative")
	cmd := exec.Command("git", "-C", main, "worktree", "add", "--relative-paths", "--detach", relative, "HEAD")
	cmd.Env = gitrepository.ReadEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "unknown option") {
		t.Skip("Git lacks relative worktrees")
	}
	if err != nil {
		t.Fatalf("relative worktree: %v: %s", err, out)
	}
	nested := filepath.Join(relative, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	worktreeKindResolve(t, home, nested, "control-plane")
}

func TestLinkedWorktreeKindRejectsUnregisteredLinkedCloneAndNestedRepo(t *testing.T) {
	main, linked, home := worktreeKindFixture(t)
	clone := filepath.Join(t.TempDir(), "clone")
	worktreeKindGit(t, main, "clone", "--quiet", main, clone)
	foreign := filepath.Join(t.TempDir(), "foreign-lane")
	worktreeKindGit(t, clone, "worktree", "add", "--detach", foreign, "HEAD")
	worktreeKindResolve(t, home, foreign, "product")
	nested := filepath.Join(linked, "independent")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	worktreeKindGit(t, nested, "init", "-q")
	worktreeKindResolve(t, home, nested, "product")
}

func TestLinkedWorktreeKindBoundsGitQueries(t *testing.T) {
	main, linked, home := worktreeKindFixture(t)
	worktreeKindRegistry(t, home, fmt.Sprintf(" - kind: control-plane\n   roots: [%q, %q, %q]\n", main, main, main))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf x >> " + quote(calls) + "\nexec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	worktreeKindResolve(t, home, linked, "control-plane")
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 {
		t.Fatalf("wanted candidate plus one distinct root query, got %d", len(data))
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	worktreeKindResolve(t, t.TempDir(), linked, "product")
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("empty registry invoked Git: %v", err)
	}
}
