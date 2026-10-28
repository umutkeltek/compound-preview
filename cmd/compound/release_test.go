package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseExportPublicCLIJSON(t *testing.T) {
	releaseRequireGit(t)
	repo := releaseInitGitRepo(t)
	releaseWriteFile(t, filepath.Join(repo, ".gitignore"), "docs_private/\nPlans/\n.DS_Store\n")
	releaseWriteFile(t, filepath.Join(repo, "README.md"), "# public\n")
	releaseGit(t, repo, "add", ".gitignore", "README.md")
	releaseGit(t, repo, "commit", "-m", "initial")
	releaseWriteFile(t, filepath.Join(repo, "docs_private", "example.md"), "example\n")

	target := filepath.Join(t.TempDir(), "compound-public.tar.gz")
	out := captureStdout(t, func() {
		cmdRelease([]string{"export-public", "--repo", repo, "--target", target, "--json"})
	})
	var report struct {
		Status     string   `json:"status"`
		TargetPath string   `json:"target_path"`
		Files      int      `json:"files"`
		ProofRefs  []string `json:"proof_refs"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.TargetPath != target || report.Files != 2 {
		t.Fatalf("report = %+v, want ok target and two tracked files", report)
	}
	if !releaseContains(report.ProofRefs, "git-archive-head") || !releaseContains(report.ProofRefs, "public-export-private-path-gate") {
		t.Fatalf("proof refs missing: %+v", report.ProofRefs)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target archive missing: %v", err)
	}
}

func TestReleaseExportPrivateAuditCLIJSON(t *testing.T) {
	releaseRequireGit(t)
	repo := releaseInitGitRepo(t)
	releaseWriteFile(t, filepath.Join(repo, "README.md"), "# public\n")
	releaseGit(t, repo, "add", "README.md")
	releaseGit(t, repo, "commit", "-m", "initial")
	releaseWriteFile(t, filepath.Join(repo, "docs_private", "example.md"), "private\n")
	releaseWriteFile(t, filepath.Join(repo, "console", "node_modules", "pkg", "index.js"), "heavy\n")

	target := filepath.Join(t.TempDir(), "compound-private-audit.tar.gz")
	out := captureStdout(t, func() {
		cmdRelease([]string{"export-private-audit", "--repo", repo, "--target", target, "--prefix", "audit", "--json"})
	})
	var report struct {
		Status          string   `json:"status"`
		Repo            string   `json:"repo"`
		TargetPath      string   `json:"target_path"`
		Files           int      `json:"files"`
		PrivateSurfaces []string `json:"private_surfaces"`
		ExcludedPaths   []string `json:"excluded_paths"`
		ProofRefs       []string `json:"proof_refs"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Repo != "${REPO}" || report.TargetPath != target || report.Files < 2 {
		t.Fatalf("report = %+v, want ok redacted repo target and files", report)
	}
	if !releaseContains(report.PrivateSurfaces, "docs_private") {
		t.Fatalf("private surfaces missing docs_private: %+v", report.PrivateSurfaces)
	}
	if !releaseContains(report.ExcludedPaths, "console/node_modules") {
		t.Fatalf("excluded paths missing node_modules: %+v", report.ExcludedPaths)
	}
	if !releaseContains(report.ProofRefs, "private-audit-explicit-command") || !releaseContains(report.ProofRefs, "private-audit-heavy-path-exclusions") {
		t.Fatalf("proof refs missing: %+v", report.ProofRefs)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target archive missing: %v", err)
	}
}

func releaseRequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func releaseInitGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	releaseGit(t, repo, "init")
	releaseGit(t, repo, "config", "user.email", "compound-test")
	releaseGit(t, repo, "config", "user.name", "Compound Test")
	return repo
}

func releaseGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func releaseWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func releaseContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
