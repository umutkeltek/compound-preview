package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/work"
)

func TestWorkShowEmitsWorkbenchPacketPathWithDefaultResolver(t *testing.T) {
	repoRoot := t.TempDir()
	packetDir := filepath.Join(repoRoot, "docs_private", "plan", "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatalf("create packet dir: %v", err)
	}
	canonicalPacketPath := filepath.Join(packetDir, "default-resolver.md")
	worktreePacketDir := filepath.Join(t.TempDir(), "executor-worktree", "docs_private", "plan", "packets")
	if err := os.MkdirAll(worktreePacketDir, 0o755); err != nil {
		t.Fatalf("create worktree packet dir: %v", err)
	}
	importedPacketPath := filepath.Join(worktreePacketDir, "default-resolver.md")
	packetBody := `# Default resolver packet
Date: 2026-07-14
Status: ready
Owner: codex
Proof-medium: cli
Success: work show uses the same default packet directory as workbench.

## Objective
Prove packet path resolution without COMPOUND_PACKET_DIR.
`
	if err := os.WriteFile(importedPacketPath, []byte(packetBody), 0o644); err != nil {
		t.Fatalf("write imported packet: %v", err)
	}

	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_PACKET_DIR", "")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	imported, err := store.ImportPacket(importedPacketPath, work.PacketImportOptions{Owner: "codex"}, cliTestNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if err := os.Remove(importedPacketPath); err != nil {
		t.Fatalf("remove stale worktree packet: %v", err)
	}
	if err := os.WriteFile(canonicalPacketPath, []byte(packetBody), 0o644); err != nil {
		t.Fatalf("write canonical packet: %v", err)
	}

	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("change cwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalCWD) })
	offered, err := work.OfferedPackets(defaultWorkbenchPacketDir())
	if err != nil || len(offered.Items) != 1 {
		t.Fatalf("workbench packet resolution: items=%+v err=%v", offered.Items, err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"show", imported.Work.ID, "--json"})
	})
	var shown struct {
		PacketPath string `json:"packet_path"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("decode work show: %v\n%s", err, out)
	}
	offeredPath, err := filepath.Abs(offered.Items[0].Path)
	if err != nil {
		t.Fatalf("resolve offered packet path: %v", err)
	}
	if shown.PacketPath != offeredPath {
		t.Fatalf("packet_path = %q, want workbench path %q", shown.PacketPath, offeredPath)
	}
	if shown.PacketPath == importedPacketPath {
		t.Fatalf("packet_path reused deleted import path %q", importedPacketPath)
	}
	shownInfo, err := os.Stat(shown.PacketPath)
	if err != nil {
		t.Fatalf("stat shown packet path: %v", err)
	}
	canonicalInfo, err := os.Stat(canonicalPacketPath)
	if err != nil {
		t.Fatalf("stat canonical packet path: %v", err)
	}
	if !os.SameFile(shownInfo, canonicalInfo) {
		t.Fatalf("packet_path = %q, want canonical current file %q", shown.PacketPath, canonicalPacketPath)
	}
	if _, err := os.Stat(shown.PacketPath); err != nil {
		t.Fatalf("packet_path does not exist: %v", err)
	}
}

func TestWorkShowOmitsPacketPathForNonPacketWork(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	created, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "non-packet work omits packet path"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"show", created.ID, "--json"})
	})
	var shown map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("decode work show: %v\n%s", err, out)
	}
	if _, ok := shown["packet_path"]; ok {
		t.Fatalf("non-packet work emitted packet_path:\n%s", out)
	}
}

// workShowDanglingPacketChildEnv, when set to a work id, tells this test binary to run as
// the `compound work show` child process instead of the driving test — the same
// self-exec pattern used by TestHomeDoctorJSONFailureExitsNonZero, needed because the
// current (pre-fix) code path calls os.Exit(1) on a dangling packet pointer and an
// in-process os.Exit would kill the whole test binary.
const workShowDanglingPacketChildEnv = "COMPOUND_TEST_WORK_SHOW_DANGLING_PACKET_CHILD"

// TestWorkShowSurvivesDanglingPacketPointer is the work_a55bb058f583 regression falsifier:
// `compound work show` on an envelope whose packet artifact/surface no longer matches any
// current workbench packet must still render the full envelope and exit 0, with an explicit
// packet_status note carrying the underlying reason — not hard-fail. RED on current code
// (non-zero exit, no envelope rendered); GREEN after the fix (exit 0, envelope + note).
func TestWorkShowSurvivesDanglingPacketPointer(t *testing.T) {
	if childID := os.Getenv(workShowDanglingPacketChildEnv); childID != "" {
		cmdWork([]string{"show", childID, "--json"})
		// Exit explicitly so the child process's stdout is exactly what a real
		// `compound work show` invocation would print — without this, the Go
		// testing framework appends its own "PASS\nok ..." trailer once the test
		// function returns normally, corrupting the JSON the parent decodes.
		os.Exit(0)
		return
	}

	homePath := initializedTestHome(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	created, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "work show survives a dangling packet pointer",
		CanonicalSurface: "packet:dangling-pointer-example",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	// A real, existing, but empty packet dir: the envelope's packet:<slug> surface has no
	// matching current workbench packet — a dangling pointer, not a missing directory.
	emptyPacketDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkShowSurvivesDanglingPacketPointer$")
	cmd.Env = append(os.Environ(),
		workShowDanglingPacketChildEnv+"="+created.ID,
		"COMPOUND_HOME="+homePath,
		"COMPOUND_PACKET_DIR="+emptyPacketDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("work show on a dangling packet pointer did not exit 0: %v\noutput:\n%s", err, out)
	}

	var shown struct {
		ID           string `json:"id"`
		State        string `json:"state"`
		PacketPath   string `json:"packet_path,omitempty"`
		PacketStatus string `json:"packet_status,omitempty"`
	}
	if err := json.Unmarshal(out, &shown); err != nil {
		t.Fatalf("decode work show: %v\noutput:\n%s", err, out)
	}
	if shown.ID != created.ID || shown.State == "" {
		t.Fatalf("work show did not render the full envelope for a dangling packet pointer:\n%s", out)
	}
	if shown.PacketPath != "" {
		t.Fatalf("work show emitted a packet_path for a dangling pointer:\n%s", out)
	}
	wantReason := "no current workbench packet matches packet:dangling-pointer-example"
	if !strings.Contains(shown.PacketStatus, "unavailable") || !strings.Contains(shown.PacketStatus, wantReason) {
		t.Fatalf("work show packet_status = %q, want an unavailable note containing %q\noutput:\n%s", shown.PacketStatus, wantReason, out)
	}
}
