package main

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopZipExcludesRuntimeTreesAndDocumentsTheSnapshotBoundary(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"bash", "git", "rsync", "zip"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is required for desktop zip integration test", command)
		}
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "desktop-compound-zip.sh")
	help, err := exec.Command("bash", scriptPath, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("script help: %v\n%s", err, help)
	}
	for _, want := range []string{
		"default 480",
		"harness/runtime directories remain excluded",
		".codex/",
		".agents/",
		"state/",
		".pytest_cache/",
		".ua/",
		".hermes/",
		"body/",
	} {
		if !strings.Contains(string(help), want) {
			t.Fatalf("help does not document %q:\n%s", want, help)
		}
	}

	source := t.TempDir()
	for _, path := range []string{
		"keep.txt",
		".claude/session.txt",
		".codex/worktrees/duplicate.txt",
		".agents/session.txt",
		"state/runtime.json",
		".pytest_cache/cache",
		".ua/graph.json",
		".hermes/worktrees/duplicate.txt",
		"body/index-v2.sqlite",
		"body/backup/operation.lock",
		"memory/compound/directory-only/.keep-dir",
	} {
		fullPath := filepath.Join(source, path)
		if strings.HasSuffix(path, ".keep-dir") {
			if err := os.MkdirAll(fullPath, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(path+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", source, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	desktop := t.TempDir()
	cmd := exec.Command(
		"bash", scriptPath,
		"--repo", source,
		"--desktop", desktop,
		"--name", "snapshot",
		"--zip", "snapshot.zip",
		"--max-zip-mb", "10",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package fixture: %v\n%s", err, out)
	}
	stage := filepath.Join(desktop, "snapshot")
	if _, err := os.Stat(filepath.Join(stage, "keep.txt")); err != nil {
		t.Fatalf("ordinary source file missing: %v", err)
	}
	for _, excluded := range []string{".claude", ".codex", ".agents", "state", ".pytest_cache", ".ua", ".hermes", "body"} {
		if _, err := os.Stat(filepath.Join(stage, excluded)); !os.IsNotExist(err) {
			t.Fatalf("runtime tree %q was packaged: %v", excluded, err)
		}
	}
	notes, err := os.ReadFile(filepath.Join(stage, "AUDIT-NOTES.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{".claude/", ".codex/", ".agents/", "state/", ".pytest_cache/", ".ua/", ".hermes/", "body/"} {
		if !strings.Contains(string(notes), excluded) {
			t.Fatalf("AUDIT-NOTES.md omits always-excluded %q:\n%s", excluded, notes)
		}
	}
}

func TestDesktopZipOversizeDiagnosticUsesPortableFind(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"bash", "git", "rsync", "zip"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is required for desktop zip integration test", command)
		}
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "desktop-compound-zip.sh")
	source := t.TempDir()
	payload := make([]byte, 2*1024*1024)
	if _, err := rand.New(rand.NewSource(1)).Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "large-fixture.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", source, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	desktop := t.TempDir()
	cmd := exec.Command(
		"bash", scriptPath,
		"--repo", source,
		"--desktop", desktop,
		"--name", "oversized",
		"--zip", "oversized.zip",
		"--max-zip-mb", "1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("oversized archive unexpectedly succeeded:\n%s", out)
	}
	for _, want := range []string{"Largest staged files:", "oversized zip deleted"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("oversize diagnostic missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "illegal trailing character") {
		t.Fatalf("oversize diagnostic used a non-portable find size suffix:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(desktop, "oversized.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("oversized archive was not deleted: %v", statErr)
	}
}
