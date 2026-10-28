package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work"
)

// TestHarnessExecutorsList: compound harness executors lists the three
// built-in adapters (claude, codex, os-harness-entrypoints). Pinned so
// the CLI surface always reflects the registry; if a built-in is
// accidentally removed, the test fails and the operator sees the gap.
func TestHarnessExecutorsList(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	out := captureStdout(t, func() {
		cmdHarness([]string{"executors"})
	})
	for _, want := range []string{"claude", "codex", "os-harness-entrypoints"} {
		if !strings.Contains(out, want) {
			t.Fatalf("executors list missing %q:\n%s", want, out)
		}
	}
}

// TestHarnessExecutorsListJSON: --json flag produces a JSON object
// with the executors array. Pinned so the operator can pipe the list
// into jq / other tooling.
func TestHarnessExecutorsListJSON(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var buf strings.Builder
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdHarness([]string{"executors", "--json"})
	w.Close()
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			break
		}
	}
	os.Stdout = old
	var got struct {
		Executors []string `json:"executors"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.String())
	}
	if len(got.Executors) < 3 {
		t.Fatalf("expected at least 3 executors, got %v", got.Executors)
	}
}

// TestHarnessRunOSEntrypoints: drives the os-harness-entrypoints
// adapter with a real echo command. The work envelope must exist (we
// create one in the test). The RunAttempt terminalizes succeeded.
//
// Falsifier: removing the adapter.Run call turns this RED with
// "expected succeeded status, got: ..." -- the test exercises the
// full Mint->SetWorkspace->SetCommand->spawn->capture->Complete chain
// end-to-end through the CLI surface.
func TestHarnessRunOSEntrypoints(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run smoke test",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdHarness([]string{"run", "--harness", "os-harness-entrypoints", "--work", w.ID, "--workspace", homePath, "--", "echo", "harness-run-smoke"})
	})
	if !strings.Contains(out, "status: succeeded") {
		t.Fatalf("expected succeeded status, got:\n%s", out)
	}
	if !strings.Contains(out, "harness: os-harness-entrypoints") {
		t.Fatalf("expected harness=os-harness-entrypoints, got:\n%s", out)
	}
	if !strings.Contains(out, "run_attempt_id: ratt_") {
		t.Fatalf("expected run_attempt_id, got:\n%s", out)
	}
	// RunAttempt JSON was persisted to <home>/run_attempts/<id>.json.
	runAttemptFiles, _ := filepath.Glob(filepath.Join(homePath, "run_attempts", "ratt_*.json"))
	if len(runAttemptFiles) != 1 {
		t.Fatalf("expected exactly 1 RunAttempt file, got %d", len(runAttemptFiles))
	}
	// Re-load the persisted RunAttempt to confirm the S2 fields landed
	// (workspace, command, status, transcript_ref).
	store := runattempt.Open(homePath)
	id := strings.TrimSuffix(filepath.Base(runAttemptFiles[0]), ".json")
	ra, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusSucceeded {
		t.Fatalf("persisted status = %q, want succeeded", ra.Status)
	}
	if ra.Workspace != homePath {
		t.Fatalf("persisted workspace = %q, want %q", ra.Workspace, homePath)
	}
	if ra.Command != "echo" {
		t.Fatalf("persisted command = %q, want %q", ra.Command, "echo")
	}
	if ra.TranscriptRef == "" {
		t.Fatalf("persisted transcript_ref must be set after a clean run")
	}
}

// TestHarnessRunFailedExit: a subprocess that exits non-zero (`false`)
// produces a failed RunAttempt and the CLI exits non-zero. Spawned as
// a subprocess so we can capture the exit code; in-process tests
// can't recover from os.Exit.
//
// Pinned: the shell sees a non-zero exit when the harness run failed,
// which is the natural operator contract (CI / make / orchestration
// can react to a non-zero status).
func TestHarnessRunFailedExit(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, _ := work.Open(homePath)
	w, _ := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run failed-exit path",
	}, cliTestNow)
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "harness", "run",
		"--harness", "os-harness-entrypoints", "--work", w.ID, "--", "false")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got 0. output:\n%s", out)
	}
	if !strings.Contains(string(out), "status: failed") {
		t.Fatalf("expected 'status: failed' in output, got:\n%s", out)
	}
}

// TestHarnessRunJSON: --json flag produces machine-readable output.
// Pinned so the operator can pipe into jq / other tooling.
func TestHarnessRunJSON(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, _ := work.Open(homePath)
	w, _ := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run json output",
	}, cliTestNow)
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "harness", "run",
		"--harness", "os-harness-entrypoints", "--work", w.ID, "--json",
		"--", "echo", "json-smoke")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("json run: %v\n%s", err, out)
	}
	var got struct {
		RunAttemptID string `json:"run_attempt_id"`
		Status       string `json:"status"`
		Harness      string `json:"harness"`
		ExitCode     int    `json:"exit_code"`
		TimedOut     bool   `json:"timed_out"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if got.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", got.Status)
	}
	if got.Harness != "os-harness-entrypoints" {
		t.Fatalf("harness = %q, want os-harness-entrypoints", got.Harness)
	}
	if !strings.HasPrefix(got.RunAttemptID, "ratt_") {
		t.Fatalf("run_attempt_id = %q, want ratt_ prefix", got.RunAttemptID)
	}
}

// readSingleRunAttemptJSON reads the one persisted RunAttempt JSON a fresh
// `harness run` leaves under <home>/run_attempts. Asserts exactly one file
// exists so a silent second mint (or a lost one) is surfaced. Returns the
// raw bytes so callers can assert on JSON keys byte-level (omitempty is a
// serialization concern, not a struct-field concern).
func readSingleRunAttemptJSON(t *testing.T, homePath string) []byte {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(homePath, "run_attempts", "ratt_*.json"))
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 RunAttempt file, got %d: %v", len(files), files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read RunAttempt JSON: %v", err)
	}
	return body
}

// TestHarnessRunByStampsLaneID: `harness run ... --by <lane>` flows the
// resolved actor into RunInput.LaneID, so the minted RunAttempt's persisted
// JSON carries lane_id = <lane>. This is the S6 lane-attribution falsifier,
// mirroring the rail-level TestRunnerStampsLaneIDPreTerminal but exercising
// the full CLI flag path: valueFlag(--by) -> RunInput.LaneID ->
// SetAttribution pre-terminalization -> persisted JSON.
//
// Falsifier (RED at base): cmdHarnessRun does not parse --by and never sets
// RunInput.LaneID, so lane_id is absent from the JSON even with --by passed.
// Asserting on the raw persisted bytes (not just the struct) pins that the
// lane identity actually round-trips to disk.
func TestHarnessRunByStampsLaneID(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run --by lane stamp",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		cmdHarness([]string{
			"run", "--harness", "os-harness-entrypoints",
			"--work", w.ID,
			"--workspace", homePath,
			"--by", "lane-a:worker",
			"--", "echo", "by-stamp-smoke",
		})
	})
	body := readSingleRunAttemptJSON(t, homePath)
	if !strings.Contains(string(body), `"lane_id": "lane-a:worker"`) {
		t.Fatalf("harness run --by must stamp lane_id=lane-a:worker; RunAttempt JSON:\n%s", body)
	}
}

// TestHarnessRunActorAliasIdenticalToBy: --actor is the documented alias of
// --by and must stamp lane_id identically. Falsifier: if the alias were wired
// to a different lane resolution (or unwired), lane_id would be absent or
// disagree with the --by path.
func TestHarnessRunActorAliasIdenticalToBy(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run --actor alias",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		cmdHarness([]string{
			"run", "--harness", "os-harness-entrypoints",
			"--work", w.ID,
			"--workspace", homePath,
			"--actor", "lane-a:worker",
			"--", "echo", "actor-alias-smoke",
		})
	})
	body := readSingleRunAttemptJSON(t, homePath)
	if !strings.Contains(string(body), `"lane_id": "lane-a:worker"`) {
		t.Fatalf("harness run --actor must stamp lane_id=lane-a:worker (identical to --by); RunAttempt JSON:\n%s", body)
	}
}

// TestHarnessRunNoActorOmitsLaneID: absent --by/--actor leaves RunInput.LaneID
// empty, which stamps NOTHING (the runner skips SetAttribution entirely), so
// the persisted RunAttempt JSON has NO lane_id key (omitempty at the rail
// level). Mirrors TestRunnerEmptyLaneIDOmitsStamp. Byte-level assert: an
// unattributed `harness run` is byte-identical to today's behavior.
func TestHarnessRunNoActorOmitsLaneID(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run no-actor omits lane_id",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		cmdHarness([]string{
			"run", "--harness", "os-harness-entrypoints",
			"--work", w.ID,
			"--workspace", homePath,
			"--", "echo", "no-actor-smoke",
		})
	})
	body := readSingleRunAttemptJSON(t, homePath)
	if strings.Contains(string(body), "lane_id") {
		t.Fatalf("unattributed harness run must omit lane_id (empty LaneID stamps nothing); RunAttempt JSON:\n%s", body)
	}
}

// TestHarnessRunTextWarnsUngoverned: `harness run` (human/text output) prints
// a single warning line after the result block making clear this verb mints
// a RunAttempt WITHOUT advancing or policy-binding the WorkEnvelope, and
// points the operator at `work advance --execute` for governed execution.
// This is the zip27/F12 surface-honesty falsifier (T1).
//
// Falsifier (RED at base): cmdHarnessRun's text branch has no such line, so
// the output does not contain the exact warning text.
func TestHarnessRunTextWarnsUngoverned(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run text warns ungoverned",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdHarness([]string{
			"run", "--harness", "os-harness-entrypoints",
			"--work", w.ID,
			"--workspace", homePath,
			"--", "echo", "warn-smoke",
		})
	})
	want := "warning: harness run records a RunAttempt but does not advance or policy-bind the WorkEnvelope; use work advance --execute for governed execution."
	if !strings.Contains(out, want) {
		t.Fatalf("expected warning line %q in text output, got:\n%s", want, out)
	}
}

// TestHarnessRunJSONGovernedFalse: `harness run --json` output carries an
// additive boolean field `governed: false`, marking that this verb does not
// advance or policy-bind the WorkEnvelope. This is the zip27/F12
// surface-honesty falsifier (T2).
//
// Falsifier (RED at base): the JSON object has no `governed` key, so
// unmarshalling into a struct with a required-by-test bool field leaves it
// at the zero value regardless of presence -- assert on the raw bytes so
// presence itself is pinned, not just the (indistinguishable) false value.
func TestHarnessRunJSONGovernedFalse(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, _ := work.Open(homePath)
	w, _ := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run json governed false",
	}, cliTestNow)
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "harness", "run",
		"--harness", "os-harness-entrypoints", "--work", w.ID, "--json",
		"--", "echo", "governed-smoke")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("json run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"governed":false`) {
		t.Fatalf("expected \"governed\":false in JSON output, got:\n%s", out)
	}
	var got struct {
		Governed *bool `json:"governed"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if got.Governed == nil {
		t.Fatalf("governed key missing from JSON output:\n%s", out)
	}
	if *got.Governed != false {
		t.Fatalf("governed = %v, want false", *got.Governed)
	}
}

// TestHarnessRunMissingWorkspaceRefusedBeforeMint: `harness run` with a
// workspace that does not exist must refuse at the workspace_existence gate
// BEFORE minting a RunAttempt -- the same fail-closed stat gate
// `work advance --execute` has, via the shared checkExecuteWorkspaceExists.
//
// Falsifier (RED at base): cmdHarnessRun had no existence gate, so a stale
// path (e.g. a retired git_delivery.target_worktree_root) minted an attempt
// that was guaranteed to terminalize failed at cmd.Start (chdir).
func TestHarnessRunMissingWorkspaceRefusedBeforeMint(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "harness run refuses a missing workspace before mint",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	// A path that is guaranteed absent: inside the per-test temp dir, never created.
	missing := filepath.Join(t.TempDir(), "retired-worktree-root")

	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "harness", "run",
		"--harness", "os-harness-entrypoints",
		"--work", w.ID,
		"--workspace", missing,
		"--", "echo", "hi")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected non-zero exit for a missing workspace dir, got 0. output:\n%s", out)
	}
	// The refusal must name the gate and the path it rejected, so the reader
	// is not left guessing which ladder rung produced it.
	for _, want := range []string{
		"gate workspace_existence",
		missing,
		"tried in order:",
		"prevents:",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("expected %q in the workspace-existence refusal, got:\n%s", want, out)
		}
	}
	// `harness run` does not claim, so a refusal mints NOTHING.
	runAttemptFiles, _ := filepath.Glob(filepath.Join(homePath, "run_attempts", "ratt_*.json"))
	if len(runAttemptFiles) != 0 {
		t.Fatalf("refusal must mint no RunAttempt; want 0 files, got %d", len(runAttemptFiles))
	}
}

var _ = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
