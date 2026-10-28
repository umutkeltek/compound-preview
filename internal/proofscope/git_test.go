package proofscope

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "fixture@example.invalid")
	runGit(t, repo, "config", "user.name", "fixture")
	return repo
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFiles(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if body == "" {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "fixture")
	return runGit(t, repo, "rev-parse", "HEAD")
}

func TestDeriveGitReadsCommitsNotWorktree(t *testing.T) {
	repo := gitRepo(t)
	base := commitFiles(t, repo, fixtureFiles())
	head := commitFiles(t, repo, map[string]string{"internal/a/a.go": "package a\n\nvar A = 1\n"})
	// An uncommitted worktree edit must not widen the scope.
	if err := os.WriteFile(filepath.Join(repo, "internal", "d", "d.go"), []byte("package d\n\nvar D = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DeriveGit(t.Context(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := Scope{Mode: Affected, Reason: "diff", Base: base, Head: head,
		Packages: pkgs("a", "b", "c", "e", "lit", "rr"), Changed: pkgs("a"), RootReaders: pkgs("rr"),
		Reached: pkgs("a", "b", "c", "e", "lit")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// The diff is the two-tree diff: a later revert is still a real change.
	reverted := commitFiles(t, repo, map[string]string{"internal/a/a.go": "package a\n", "internal/d/d.go": "package d\n"})
	if got, err := DeriveGit(t.Context(), repo, head, reverted); err != nil || got.Reason != "diff" || !reflect.DeepEqual(got.Changed, pkgs("a")) {
		t.Fatalf("revert scope=%+v err=%v", got, err)
	}
	if got, err := DeriveGit(t.Context(), repo, base, reverted); err != nil || got.Mode != Full || got.Reason != "empty-diff" {
		t.Fatalf("identical trees scope=%+v err=%v; want full empty-diff", got, err)
	}
}

func TestDeriveGitRefusesInexactOrMissingCommits(t *testing.T) {
	repo := gitRepo(t)
	head := commitFiles(t, repo, fixtureFiles())
	tree := runGit(t, repo, "rev-parse", "HEAD^{tree}")
	for name, base := range map[string]string{
		"short sha":    head[:12],
		"symbolic ref": "HEAD",
		"tree object":  tree,
		"missing":      strings.Repeat("0", 40),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := DeriveGit(t.Context(), repo, base, head); err == nil {
				t.Fatalf("scope=%+v; want refusal", got)
			}
		})
	}
}
