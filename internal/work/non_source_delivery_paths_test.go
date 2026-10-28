package work

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateRuntimeDeliveryPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, *[]privateRuntimeDeliveryFile) string
		want   string
	}{
		{name: "exact private body"},
		{name: "body changed", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			writeDeliveryFile(t, repo, (*files)[0].Path, "newer body")
			return ""
		}, want: "hash"},
		{name: "tracked despite ignore", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			landingGit(t, repo, "add", "-f", "--", (*files)[0].Path)
			return ""
		}, want: "tracked"},
		{name: "tracked in HEAD but removed from index", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			landingGit(t, repo, "add", "-f", "--", (*files)[0].Path)
			landingGit(t, repo, "commit", "-qm", "tracked source control")
			landingGit(t, repo, "rm", "--cached", "--", (*files)[0].Path)
			return ""
		}, want: "tracked"},
		{name: "arbitrary ignored source", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			writeDeliveryFile(t, repo, "internal/local.go", "runtime body")
			(*files)[0].Path = "internal/local.go"
			return repo
		}, want: "private overlay"},
		{name: "not ignored", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			writeDeliveryFile(t, repo, ".gitignore", "internal/\n")
			return ""
		}, want: "ignored"},
		{name: "nested source repository", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			nested := filepath.Join(repo, "docs_private/plan")
			landingGit(t, nested, "init", "-q")
			landingGit(t, nested, "add", "body")
			return ""
		}, want: "nested"},
		{name: "hardlinked source", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			if err := os.Link(filepath.Join(repo, (*files)[0].Path), filepath.Join(repo, "source.go")); err != nil {
				t.Fatal(err)
			}
			return ""
		}, want: "single-link"},
		{name: "outside action target", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			writeDeliveryFile(t, repo, "docs_private/other/body", "runtime body")
			(*files)[0].Path = "docs_private/other/body"
			return ""
		}, want: "target"},
		{name: "duplicate", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			*files = append(*files, (*files)[0])
			return ""
		}, want: "duplicate"},
		{name: "traversal alias", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			(*files)[0].Path = "docs_private/plan/../plan/body"
			return ""
		}, want: "canonical"},
		{name: "absolute file", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			(*files)[0].Path = filepath.Join(repo, (*files)[0].Path)
			return ""
		}, want: "relative"},
		{name: "symlinked leaf", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			path := filepath.Join(repo, (*files)[0].Path)
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".original", path); err != nil {
				t.Fatal(err)
			}
			return ""
		}, want: "symlink"},
		{name: "symlinked parent", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			path := filepath.Join(repo, "docs_private/plan")
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".original", path); err != nil {
				t.Fatal(err)
			}
			return ""
		}, want: "symlink"},
		{name: "empty manifest", change: func(t *testing.T, repo string, files *[]privateRuntimeDeliveryFile) string {
			*files = nil
			return ""
		}, want: "file count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initLandingRepo(t)
			var err error
			repo, err = filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			writeDeliveryFile(t, repo, ".gitignore", "docs_private/\ninternal/\n")
			writeDeliveryFile(t, repo, "docs_private/plan/body", "runtime body")
			files := []privateRuntimeDeliveryFile{{Path: "docs_private/plan/body", AfterSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("runtime body")))}}
			target := filepath.Join(repo, "docs_private/plan")
			if tc.change != nil {
				if replacement := tc.change(t, repo, &files); replacement != "" {
					target = replacement
				}
			}
			err = validatePrivateRuntimeDeliveryPaths(repo, target, files)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func writeDeliveryFile(t *testing.T, repo, relative, body string) {
	t.Helper()
	path := filepath.Join(repo, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateRuntimeDeliveryRejectsReplacedRootPath(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	writeDeliveryFile(t, repo, "docs_private/plan/body", "runtime body")
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(repo, filepath.Join(parent, "retained-original")); err != nil {
		t.Fatal(err)
	}
	writeDeliveryFile(t, repo, "docs_private/plan/body", "runtime body")
	if _, err := privateDeliveryDigest(root, "docs_private/plan/body"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("replaced repository root accepted: %v", err)
	}
}

func TestPrivateRuntimeDeliveryIgnoresAmbientRepositorySelection(t *testing.T) {
	for _, variant := range []string{"foreign_git_dir", "foreign_index"} {
		t.Run(variant, func(t *testing.T) {
			repo, _ := initLandingRepo(t)
			other, _ := initLandingRepo(t)
			var err error
			repo, err = filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			writeDeliveryFile(t, repo, ".gitignore", "docs_private/\n")
			writeDeliveryFile(t, repo, "docs_private/plan/body", "runtime body")
			landingGit(t, repo, "add", "-f", "--", "docs_private/plan/body")
			if variant == "foreign_git_dir" {
				t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
				t.Setenv("GIT_WORK_TREE", repo)
			} else {
				t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git/index"))
			}
			files := []privateRuntimeDeliveryFile{{Path: "docs_private/plan/body", AfterSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("runtime body")))}}
			err = validatePrivateRuntimeDeliveryPaths(repo, filepath.Join(repo, "docs_private/plan"), files)
			if err == nil || !strings.Contains(err.Error(), "tracked") {
				t.Fatalf("ambient repository selection hid tracked source: %v", err)
			}
		})
	}
}
