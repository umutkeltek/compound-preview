package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidatePublicArchiveRejectsPrivateRuntimePaths(t *testing.T) {
	tests := []string{
		"compound-public/docs_private/plan.md",
		"compound-public/.claude/settings.json",
		"compound-public/.compound-private/config.yaml",
		"compound-public/Plans/draft.md",
		"compound-public/memory/external/.git/config",
		"compound-public/bin/compound",
		"compound-public/compound",
		"compound-public/docs/decision.private.md",
		"compound-public/.DS_Store",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ValidatePublicArchive(tarWithEntries(t, name), "compound-public/")
			if err == nil {
				t.Fatalf("ValidatePublicArchive(%q) succeeded, want rejection", name)
			}
		})
	}
}

func TestValidatePublicArchiveAllowsPublicTrackedShapes(t *testing.T) {
	_, err := ValidatePublicArchive(tarWithEntries(t,
		"compound-public/.gitignore",
		"compound-public/README.md",
		"compound-public/cmd/compound/main.go",
		"compound-public/docs/memory-architecture.md",
	), "compound-public/")
	if err != nil {
		t.Fatalf("ValidatePublicArchive rejected public paths: %v", err)
	}
}

func TestCopyCheckedTarGzipDrainsTrailingArchivePadding(t *testing.T) {
	archive, err := io.ReadAll(tarWithEntries(t, "compound-public/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	archive = append(archive, bytes.Repeat([]byte{0}, 1<<20)...)
	src := bytes.NewReader(archive)

	var dst bytes.Buffer
	if _, err := copyCheckedTarGzip(src, &dst, "compound-public/"); err != nil {
		t.Fatalf("copyCheckedTarGzip: %v", err)
	}
	if remaining := src.Len(); remaining != 0 {
		t.Fatalf("copyCheckedTarGzip left %d trailing archive bytes unread", remaining)
	}
}

func TestExportPublicUsesGitArchiveAndIgnoresWorkingTreePrivateFiles(t *testing.T) {
	requireGit(t)
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "docs_private/\nPlans/\nmemory/\nbin/\n.DS_Store\n/compound\n")
	writeFile(t, filepath.Join(repo, "README.md"), "# public\n")
	git(t, repo, "add", ".gitignore", "README.md")
	git(t, repo, "commit", "-m", "initial")

	writeFile(t, filepath.Join(repo, "docs_private", "example.md"), "example\n")
	writeFile(t, filepath.Join(repo, "Plans", "draft.md"), "private\n")
	writeFile(t, filepath.Join(repo, "memory", "external", ".git", "config"), "private\n")
	writeFile(t, filepath.Join(repo, "bin", "compound"), "binary\n")
	writeFile(t, filepath.Join(repo, ".DS_Store"), "finder\n")
	writeFile(t, filepath.Join(repo, "compound"), "binary\n")

	target := filepath.Join(t.TempDir(), "compound-public.tar.gz")
	report, err := ExportPublic(context.Background(), PublicExportOptions{RepoPath: repo, TargetPath: target}, time.Date(2026, 7, 3, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ExportPublic: %v", err)
	}
	if report.Status != "ok" || report.Files != 2 || report.Commit == "" {
		t.Fatalf("report = %+v, want ok with tracked files only", report)
	}
	names := exportedGzipTarNames(t, target)
	if !contains(names, "compound-public/README.md") || !contains(names, "compound-public/.gitignore") {
		t.Fatalf("export missing tracked public files: %v", names)
	}
	for _, forbidden := range []string{"docs_private", "Plans", "memory", "bin", ".DS_Store", "compound-public/compound"} {
		for _, name := range names {
			if strings.Contains(name, forbidden) {
				t.Fatalf("export contains forbidden %q in %v", forbidden, names)
			}
		}
	}
}

func TestExportPublicRejectsTrackedPrivatePath(t *testing.T) {
	requireGit(t)
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "# public\n")
	writeFile(t, filepath.Join(repo, "docs_private", "example.md"), "example\n")
	git(t, repo, "add", "README.md", "docs_private/example.md")
	git(t, repo, "commit", "-m", "tracked private")

	target := filepath.Join(t.TempDir(), "compound-public.tar.gz")
	_, err := ExportPublic(context.Background(), PublicExportOptions{RepoPath: repo, TargetPath: target}, time.Now())
	if err == nil {
		t.Fatal("ExportPublic succeeded with tracked docs_private file, want rejection")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("failed export should not leave target archive, stat err=%v", statErr)
	}
}

func tarWithEntries(t *testing.T, names ...string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := []byte("x")
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func exportedGzipTarNames(t *testing.T, archivePath string) []string {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.email", "compound-test")
	git(t, repo, "config", "user.name", "Compound Test")
	return repo
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
