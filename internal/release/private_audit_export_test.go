package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExportPrivateAuditRedactsAndExcludesHeavyRuntimePaths(t *testing.T) {
	requireGit(t)
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "# compound\n")
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial")

	homePath, _ := os.UserHomeDir()
	privateBody := "repo=" + repo + "\nhome=" + homePath + "\n"
	writeFile(t, filepath.Join(repo, "docs_private", "plan.md"), privateBody)
	writeFile(t, filepath.Join(repo, ".claude", "settings.json"), "{}\n")
	writeFile(t, filepath.Join(repo, "Plans", "packet.md"), "packet\n")
	writeFile(t, filepath.Join(repo, "memory", "claim.md"), "claim\n")
	writeFile(t, filepath.Join(repo, "home", "state.json"), "{}\n")
	writeFile(t, filepath.Join(repo, "docs", "decision.private.md"), "private decision\n")

	writeFile(t, filepath.Join(repo, "console", "node_modules", "pkg", "index.js"), "heavy\n")
	writeFile(t, filepath.Join(repo, "console", ".next", "cache.js"), "heavy\n")
	writeFile(t, filepath.Join(repo, ".DS_Store"), "finder\n")
	writeFile(t, filepath.Join(repo, "__MACOSX", "._README"), "finder\n")
	writeFile(t, filepath.Join(repo, "compound"), "binary\n")
	writeFile(t, filepath.Join(repo, "bin", "compound"), "binary\n")

	target := filepath.Join(t.TempDir(), "private-audit.tar.gz")
	report, err := ExportPrivateAudit(context.Background(), PrivateAuditExportOptions{
		RepoPath:   repo,
		TargetPath: target,
		Prefix:     "audit-snapshot",
	}, time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ExportPrivateAudit: %v", err)
	}
	if report.Status != "ok" || report.Repo != "${REPO}" || report.TargetPath != target {
		t.Fatalf("report = %+v, want ok redacted repo and target", report)
	}
	for _, proofRef := range []string{"private-audit-explicit-command", "private-audit-heavy-path-exclusions", "private-audit-local-path-redaction"} {
		if !containsString(report.ProofRefs, proofRef) {
			t.Fatalf("report proof refs missing %q: %+v", proofRef, report.ProofRefs)
		}
	}
	for _, surface := range []string{"docs_private", ".claude", "Plans", "memory", "home", "*.private.md"} {
		if !containsString(report.PrivateSurfaces, surface) {
			t.Fatalf("private surfaces missing %q: %+v", surface, report.PrivateSurfaces)
		}
	}

	entries := privateAuditArchiveEntries(t, target)
	privatePlan := string(entries["audit-snapshot/docs_private/plan.md"])
	if strings.Contains(privatePlan, repo) || (homePath != "" && strings.Contains(privatePlan, homePath)) {
		t.Fatalf("private plan was not redacted: %q", privatePlan)
	}
	if !strings.Contains(privatePlan, "${REPO}") || (homePath != "" && !strings.Contains(privatePlan, "${HOME}")) {
		t.Fatalf("private plan missing redaction labels: %q", privatePlan)
	}
	for _, excluded := range []string{
		"audit-snapshot/console/node_modules/pkg/index.js",
		"audit-snapshot/console/.next/cache.js",
		"audit-snapshot/.DS_Store",
		"audit-snapshot/__MACOSX/._README",
		"audit-snapshot/compound",
		"audit-snapshot/bin/compound",
	} {
		if _, ok := entries[excluded]; ok {
			t.Fatalf("archive contains excluded path %q", excluded)
		}
	}

	var manifest privateAuditManifest
	body, ok := entries["audit-snapshot/PRIVATE_AUDIT_MANIFEST.json"]
	if !ok {
		t.Fatal("archive missing PRIVATE_AUDIT_MANIFEST.json")
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, body)
	}
	if manifest.Repo != "${REPO}" || !containsString(manifest.ProofRefs, "private-audit-local-path-redaction") {
		t.Fatalf("manifest = %+v, want redacted repo and proof refs", manifest)
	}
	if !containsString(manifest.ExcludedPaths, "console/node_modules") || !containsString(manifest.ExcludedPaths, "console/.next") {
		t.Fatalf("manifest excluded paths missing heavy dirs: %+v", manifest.ExcludedPaths)
	}
}

func privateAuditArchiveEntries(t *testing.T, archivePath string) map[string][]byte {
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
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if !tarRegularFile(hdr) {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = buf.Bytes()
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
