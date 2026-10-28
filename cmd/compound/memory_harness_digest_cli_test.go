package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

const memoryHarnessDigestCLIBoundarySubEnv = "COMPOUND_MEMORY_HARNESS_DIGEST_CLI_BOUNDARY_SUB"

// promoteHarnessDigestFixture captures + directly promotes (BasisRuntimeProof
// needs no operator confirmation dance, mirroring
// contract_volatile_test.go's fixture pattern) one topic-tagged claim.
func promoteHarnessDigestFixture(t *testing.T, store *mem.Store, entity, predicate, object, topicRef string, now time.Time) {
	t.Helper()
	got, err := store.Capture([]mem.Candidate{{
		Entity: entity, Predicate: predicate, Object: object,
		Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
		Qualifiers:      map[string]string{"domain": "fictional-digest-fixture"},
		TopicRefs:       []string{topicRef},
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "trace_fictional",
		SnippetHash:     "sha256:fictional",
		Visibility:      "operational",
		ObservationDate: now.Format(time.RFC3339),
	}}, now)
	if err != nil {
		t.Fatalf("capture %s.%s: %v", entity, predicate, err)
	}
	if len(got) != 1 {
		t.Fatalf("capture %s.%s produced %d candidates, want 1", entity, predicate, len(got))
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "", now); err != nil {
		t.Fatalf("promote %s.%s: %v", entity, predicate, err)
	}
}

// TestCmdMemoryRenderHarnessDigestThenDigestDoctorEndToEnd exercises the full
// wiring: `compound memory render-harness-digest` writes the file and reports
// it, then `compound memory digest-doctor` reads that exact file back clean
// (no drift). The hand-edit-detects-drift half of this flow runs as a
// subprocess in TestCmdMemoryDigestDoctorExitsNonZeroOnDrift below —
// digest-doctor intentionally os.Exit(1)s on drift even under --json, so
// driving that path in-process would kill this entire test binary.
func TestCmdMemoryRenderHarnessDigestThenDigestDoctorEndToEnd(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	promoteHarnessDigestFixture(t, store, "tool:fictional-redact", "floor", "clean redact scan gates every public push", "standing-law", now)
	promoteHarnessDigestFixture(t, store, "claude-code:fictional-permission-mode", "default", "plan-mode default, bypass is explicit opt-in", "harness:claude-code", now)

	outPath := filepath.Join(t.TempDir(), "operator-local", "claude-code-digest.md")

	out := captureStdout(t, func() {
		cmdMemory([]string{"render-harness-digest", "--harness", "claude-code", "--out", outPath, "--json"})
	})
	var report mem.HarnessDigestReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("render-harness-digest --json did not parse: %v\n%s", err, out)
	}
	if report.Harness != "claude-code" {
		t.Fatalf("report.Harness = %q, want claude-code", report.Harness)
	}
	if report.OutPath != outPath {
		t.Fatalf("report.OutPath = %q, want %q", report.OutPath, outPath)
	}
	if report.PositionCount != 2 {
		t.Fatalf("report.PositionCount = %d, want 2: %+v", report.PositionCount, report)
	}

	onDisk, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("digest file was not written: %v", err)
	}
	if !strings.Contains(string(onDisk), "tool:fictional-redact") || !strings.Contains(string(onDisk), "claude-code:fictional-permission-mode") {
		t.Fatalf("digest file missing expected fixture content:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), "content_sha256: "+report.ContentSHA256) {
		t.Fatalf("digest file front matter missing report.ContentSHA256=%s:\n%s", report.ContentSHA256, onDisk)
	}

	doctorOut := captureStdout(t, func() {
		cmdMemory([]string{"digest-doctor", "--path", outPath, "--json"})
	})
	var drift mem.HarnessDigestDriftReport
	if err := json.Unmarshal([]byte(doctorOut), &drift); err != nil {
		t.Fatalf("digest-doctor --json did not parse: %v\n%s", err, doctorOut)
	}
	if drift.Drifted {
		t.Fatalf("fresh digest must not report drift: %+v", drift)
	}
	if drift.DeclaredContentSHA256 != report.ContentSHA256 || drift.RecomputedContentSHA256 != report.ContentSHA256 {
		t.Fatalf("drift report hashes = %+v, want both to equal report.ContentSHA256=%s", drift, report.ContentSHA256)
	}
}

// TestCmdMemoryDigestDoctorBypassesMutableStoreOpen mirrors
// TestCmdMemoryConsumptionWithoutRecordBypassesMutableStoreOpen: digest-doctor
// reads an arbitrary operator-local file and must never create ledger/inbox
// state under COMPOUND_HOME as a side effect of running.
func TestCmdMemoryDigestDoctorBypassesMutableStoreOpen(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	for _, rel := range []string{"events", candidates.InboxDir(candidates.Memory), "ledger"} {
		_ = os.RemoveAll(filepath.Join(homePath, rel))
	}
	t.Setenv("COMPOUND_HOME", homePath)

	digestPath := filepath.Join(t.TempDir(), "digest.md")
	{
		s2home := t.TempDir()
		if err := home.Init(s2home, "test", now); err != nil {
			t.Fatal(err)
		}
		store, err := mem.Open(s2home)
		if err != nil {
			t.Fatal(err)
		}
		promoteHarnessDigestFixture(t, store, "tool:fictional-redact", "floor", "clean redact scan gates every public push", "standing-law", now)
		report, body, err := store.RenderHarnessDigest(mem.HarnessDigestOptions{Harness: "claude-code"}, mem.HarnessDigestMeta{}, now)
		if err != nil {
			t.Fatal(err)
		}
		_ = report
		if err := mem.WriteHarnessDigestAtomic(digestPath, body); err != nil {
			t.Fatal(err)
		}
	}

	before := snapshotDirFiles(t, homePath)
	_ = captureStdout(t, func() { cmdMemory([]string{"digest-doctor", "--path", digestPath, "--json"}) })
	after := snapshotDirFiles(t, homePath)
	if len(after) != len(before) {
		t.Fatalf("digest-doctor must never mutate COMPOUND_HOME; before=%d after=%d", len(before), len(after))
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger")); err == nil {
		t.Fatal("digest-doctor must not create ledger/ under COMPOUND_HOME")
	}
}

func TestValidateDigestOutPathRequiresAbsolute(t *testing.T) {
	if err := validateDigestOutPath("relative/digest.md", filepath.Join(t.TempDir(), "relative/digest.md")); err == nil {
		t.Fatal("expected a relative --out to be refused")
	}
}

// TestValidateDigestOutPathRefusesInsideRepoAllowsOutside pins spec point 1:
// --out is an operator-local target, NEVER inside the repo.
func TestValidateDigestOutPathRefusesInsideRepoAllowsOutside(t *testing.T) {
	repo := initCompoundCommandGitRepo(t)
	t.Chdir(repo)

	insideOut := filepath.Join(repo, "nested", "digest.md")
	if err := validateDigestOutPath(insideOut, insideOut); err == nil {
		t.Fatalf("expected --out inside the repo (%s) to be refused", repo)
	}

	outsideOut := filepath.Join(t.TempDir(), "digest.md")
	if err := validateDigestOutPath(outsideOut, outsideOut); err != nil {
		t.Fatalf("expected --out outside the repo to be allowed, got: %v", err)
	}
}

// TestCmdMemoryRenderHarnessDigestExitsUsageOnMissingFlags proves --harness
// and --out are both required (usage error, exit 2), and that refusing an
// --out path inside the repo also exits 2 rather than writing the file.
// Runs in a subprocess because the CLI path calls os.Exit.
func TestCmdMemoryRenderHarnessDigestExitsUsageOnMissingFlags(t *testing.T) {
	switch os.Getenv(memoryHarnessDigestCLIBoundarySubEnv) {
	case "missing-flags":
		cmdMemory([]string{"render-harness-digest"})
		return
	case "inside-repo":
		repo := initCompoundCommandGitRepo(t)
		if err := os.Chdir(repo); err != nil {
			t.Fatal(err)
		}
		cmdMemory([]string{"render-harness-digest", "--harness", "claude-code", "--out", filepath.Join(repo, "digest.md")})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	for _, mode := range []string{"missing-flags", "inside-repo"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCmdMemoryRenderHarnessDigestExitsUsageOnMissingFlags$")
		cmd.Env = append(os.Environ(), memoryHarnessDigestCLIBoundarySubEnv+"="+mode, "COMPOUND_HOME="+homePath)
		output, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("[%s] must exit non-zero, got err=%v output=%s", mode, err, output)
		}
		if exitErr.ExitCode() != 2 {
			t.Fatalf("[%s] must exit 2 (usage), got %d output=%s", mode, exitErr.ExitCode(), output)
		}
	}
}

// TestCmdMemoryDigestDoctorExitsNonZeroOnDrift proves the doctor's exit code
// reflects drift (a CI/hook gate would key off this): 0 clean, 1 drifted.
// Runs in a subprocess because the CLI path calls os.Exit(1) on drift.
func TestCmdMemoryDigestDoctorExitsNonZeroOnDrift(t *testing.T) {
	if mode := os.Getenv(memoryHarnessDigestCLIBoundarySubEnv); mode != "" {
		cmdMemory([]string{"digest-doctor", "--path", mode, "--json"})
		return
	}

	homePath := t.TempDir()
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	promoteHarnessDigestFixture(t, store, "tool:fictional-redact", "floor", "clean redact scan gates every public push", "standing-law", now)
	_, body, err := store.RenderHarnessDigest(mem.HarnessDigestOptions{Harness: "claude-code"}, mem.HarnessDigestMeta{}, now)
	if err != nil {
		t.Fatal(err)
	}
	digestPath := filepath.Join(t.TempDir(), "digest.md")
	if err := mem.WriteHarnessDigestAtomic(digestPath, body); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCmdMemoryDigestDoctorExitsNonZeroOnDrift$")
	cmd.Env = append(os.Environ(), memoryHarnessDigestCLIBoundarySubEnv+"="+digestPath, "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clean digest must exit 0, got err=%v output=%s", err, output)
	}

	edited := strings.Replace(string(body), "tool:fictional-redact", "tool:fictional-redact-EDITED", 1)
	if err := os.WriteFile(digestPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestCmdMemoryDigestDoctorExitsNonZeroOnDrift$")
	cmd.Env = append(os.Environ(), memoryHarnessDigestCLIBoundarySubEnv+"="+digestPath, "COMPOUND_HOME="+homePath)
	output, err = cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("drifted digest must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("drifted digest must exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
}
