// Red-first falsifiers for the delegated-session console slice: the reduction
// of work.delegate.{spawned,steered,refused,exited} into safe rows on the
// EXISTING ExecutorsProjection. Each test is green-with / red-without a single
// rule in executors_delegated.go (terminal-evidence-wins, refusal enum only,
// privacy whitelist, empty state). They drive the real orchestrator through
// the injected fakeSessionRunner — no real tmux, no real wrapper.
package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/runattempt"
)

func withPermissiveGuard(t *testing.T) {
	t.Helper()
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "", "open")
	t.Cleanup(func() { LaneCapacityGuard = orig })
}

// withNonHomeWorkspace populates opts.Workspace with a temp directory if it is
// empty. The delegate orchestrator refuses to fall back to COMPOUND_HOME as a
// workspace, so tests that only care about spawn/steer/projection behavior must
// supply an explicit workspace.
func withNonHomeWorkspace(t *testing.T, opts *DelegateOptions) {
	t.Helper()
	if strings.TrimSpace(opts.Workspace) == "" {
		opts.Workspace = t.TempDir()
	}
}

func delegatedRowsFor(t *testing.T, s *Store) []DelegatedSessionRow {
	t.Helper()
	proj, err := s.ExecutorsProjection(fixedNow)
	if err != nil {
		t.Fatalf("executors projection: %v", err)
	}
	return proj.DelegatedSessions
}

func findDelegatedRow(rows []DelegatedSessionRow, workID string) *DelegatedSessionRow {
	for i := range rows {
		if rows[i].WorkID == workID {
			return &rows[i]
		}
	}
	return nil
}

// Empty state: a store with no delegate events yields a non-nil, empty
// delegated-session board (clean empty state, never null).
func TestDelegatedSessionsEmptyState(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	proj, err := s.ExecutorsProjection(fixedNow)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if proj.DelegatedSessions == nil {
		t.Fatalf("delegated sessions must be a non-nil slice for a clean empty state")
	}
	if len(proj.DelegatedSessions) != 0 || proj.DelegatedCounts.Total != 0 {
		t.Fatalf("expected empty board, got rows=%d counts=%+v", len(proj.DelegatedSessions), proj.DelegatedCounts)
	}
}

// Lifecycle reduction: a spawned session with a live (running) RunAttempt folds
// to a running row carrying the exact ratt id/status and the last steer TIME.
func TestDelegatedSessionsSpawnedRunning(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()

	opts := DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex",
		Command: "claude", Args: []string{"--print", "TOP-SECRET-PROMPT"}, Now: fixedNow,
	}
	withNonHomeWorkspace(t, &opts)
	res, err := s.Delegate(opts, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "spawned" {
		t.Fatalf("want spawned, got %+v", res)
	}
	steer, err := s.Steer(SteerOptions{WorkID: wID, Actor: "codex", Text: "focus the diff", Enter: true, Now: fixedNow.Add(1)}, runner)
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	_ = steer

	row := findDelegatedRow(delegatedRowsFor(t, s), wID)
	if row == nil {
		t.Fatalf("no delegated row for %s", wID)
	}
	if row.Status != "running" {
		t.Fatalf("status = %q, want running (%+v)", row.Status, row)
	}
	if row.RunAttemptID != res.RunAttemptID || row.RunAttemptID == "" {
		t.Fatalf("run attempt id = %q, want %q", row.RunAttemptID, res.RunAttemptID)
	}
	if row.RunAttemptStatus != runattempt.StatusRunning {
		t.Fatalf("run attempt status = %q, want running", row.RunAttemptStatus)
	}
	if row.Lane != "claude" || row.Session != "compound-"+wID {
		t.Fatalf("lane/session wrong: %+v", row)
	}
	if row.LastSteerAt == "" {
		t.Fatalf("expected a last steer time, got empty (%+v)", row)
	}
}

// TestDelegatedSessionsExposeRunProgress proves the existing executor
// projection carries the content-free cumulative progress stored on the exact
// delegated RunAttempt. The console must not inspect tmux output to learn it.
func TestDelegatedSessionsExposeRunProgress(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	opts := DelegateOptions{
		WorkID: wID, Harness: "claude-minimax", Actor: "codex", Command: "claude-minimax", Now: fixedNow,
	}
	withNonHomeWorkspace(t, &opts)
	res, err := s.Delegate(opts, runner)
	if err != nil || res.Status != "spawned" {
		t.Fatalf("delegate: %v %+v", err, res)
	}
	if _, err := s.DelegateProgress(wID, runattempt.RunProgress{Done: 3, Empty: 1, Failed: 0, Remaining: 6}, fixedNow.Add(1)); err != nil {
		t.Fatalf("progress: %v", err)
	}
	row := findDelegatedRow(delegatedRowsFor(t, s), wID)
	if row == nil {
		t.Fatalf("no delegated row for %s", wID)
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"progress"`, `"done":3`, `"empty":1`, `"failed":0`, `"remaining":6`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("delegated projection missing %s: %s", want, body)
		}
	}
}

// The delegated board consumes the same event snapshot Workbench already
// loaded. Reusing that Workbench must preserve the row without a second
// events/work.jsonl read.
func TestDelegatedSessionsReuseWorkbenchEventSnapshot(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	if _, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
		Workspace: t.TempDir(),
	}, runner); err != nil {
		t.Fatalf("delegate: %v", err)
	}

	bench, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		t.Fatalf("workbench: %v", err)
	}
	before := s.eventLogReadCount()
	projection, err := s.ExecutorsProjection(fixedNow, ExecutorsProjectionOptions{Workbench: &bench})
	if err != nil {
		t.Fatalf("executors projection: %v", err)
	}
	if delta := s.eventLogReadCount() - before; delta != 0 {
		t.Fatalf("reused projection read events/work.jsonl %d times, want 0", delta)
	}
	if row := findDelegatedRow(projection.DelegatedSessions, wID); row == nil || row.Status != "running" {
		t.Fatalf("delegated row missing from reused snapshot: %+v", row)
	}
}

// Distinct rented lanes used on the same work remain distinct board rows. A
// later MiniMax build must not erase the earlier Qwen review session.
func TestDelegatedSessionsPreserveLatestSessionPerLane(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()

	qwenOpts := DelegateOptions{
		WorkID: wID, Harness: "claude-qwen", Actor: "codex", Command: "claude",
		Session: "compound-qwen-review", Now: fixedNow,
	}
	withNonHomeWorkspace(t, &qwenOpts)
	qwen, err := s.Delegate(qwenOpts, runner)
	if err != nil || qwen.Status != "spawned" {
		t.Fatalf("qwen delegate: %v %+v", err, qwen)
	}
	qwenStatus := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{qwen.Binding.Session: qwenStatus}
	if _, err := s.DelegateSweep(qwen.Binding, qwenStatus, runner, "codex", fixedNow.Add(1)); err != nil {
		t.Fatalf("qwen sweep: %v", err)
	}

	minimaxOpts := DelegateOptions{
		WorkID: wID, Harness: "claude-minimax", Actor: "codex", Command: "claude",
		Session: "compound-minimax-build", Now: fixedNow.Add(2),
	}
	withNonHomeWorkspace(t, &minimaxOpts)
	minimax, err := s.Delegate(minimaxOpts, runner)
	if err != nil || minimax.Status != "spawned" {
		t.Fatalf("minimax delegate: %v %+v", err, minimax)
	}

	rows := delegatedRowsFor(t, s)
	if len(rows) != 2 {
		t.Fatalf("delegated rows = %+v, want one Qwen and one MiniMax row", rows)
	}
	byLane := make(map[string]DelegatedSessionRow, len(rows))
	for _, row := range rows {
		byLane[row.Lane] = row
	}
	if row := byLane["claude-qwen"]; row.Status != "exited" || row.Session != "compound-qwen-review" {
		t.Fatalf("qwen row = %+v, want exited review", row)
	}
	if row := byLane["claude-minimax"]; row.Status != "running" || row.Session != "compound-minimax-build" {
		t.Fatalf("minimax row = %+v, want running build", row)
	}
}

// A refusal newer than an already-exited binding is the lane's current truth;
// a historical session must not hide that the newest delegation could not
// start. (Refusals while a session is live remain binding-neutral.)
func TestDelegatedSessionsNewerRefusalSupersedesExitedBinding(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "", "open")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	spawnedOpts := DelegateOptions{
		WorkID: wID, Harness: "claude-qwen", Actor: "codex", Command: "claude",
		Session: "compound-qwen-review", Now: fixedNow,
	}
	withNonHomeWorkspace(t, &spawnedOpts)
	spawned, err := s.Delegate(spawnedOpts, runner)
	if err != nil || spawned.Status != "spawned" {
		t.Fatalf("delegate: %v %+v", err, spawned)
	}
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	runner.panes = map[string]SessionStatus{spawned.Binding.Session: dead}
	if _, err := s.DelegateSweep(spawned.Binding, dead, runner, "codex", fixedNow.Add(1)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	LaneCapacityGuard = fakeGuard(true, false, "walled: capacity exhausted", "walled")
	refusedOpts := DelegateOptions{
		WorkID: wID, Harness: "claude-qwen", Actor: "codex", Command: "claude",
		Session: "compound-qwen-retry", Now: fixedNow.Add(2),
	}
	withNonHomeWorkspace(t, &refusedOpts)
	refused, err := s.Delegate(refusedOpts, runner)
	if err != nil || refused.Status != "refused" {
		t.Fatalf("refusal: %v %+v", err, refused)
	}

	rows := delegatedRowsFor(t, s)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one current lane row", rows)
	}
	row := rows[0]
	if row.Status != "refused" || row.RefusedReason != "walled" || row.Session != "" || row.RunAttemptID != "" {
		t.Fatalf("row = %+v, want newer refusal without stale binding fields", row)
	}
}

// Terminal evidence wins: once the pane dies and the exit is swept, the row
// folds to exited with the exact RunAttempt terminal status — even though a
// spawned binding still exists in the eventlog.
func TestDelegatedSessionsExitedTerminalWins(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()

	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
		Workspace: t.TempDir(),
	}, runner)
	if err != nil || res.Status != "spawned" {
		t.Fatalf("delegate: %v %+v", err, res)
	}
	session := res.Binding.Session
	// Pane died cleanly (exit 0); sweep records exited + terminalizes the ratt.
	runner.panes = map[string]SessionStatus{session: {Exists: true, PaneDead: true, ExitCode: 0}}
	if _, err := s.DelegateSweep(res.Binding, runner.panes[session], runner, "codex", fixedNow.Add(2)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	row := findDelegatedRow(delegatedRowsFor(t, s), wID)
	if row == nil || row.Status != "exited" {
		t.Fatalf("status = %v, want exited (%+v)", statusOf(row), row)
	}
	if row.ObservedExit != "succeeded" {
		t.Fatalf("observed exit = %q, want succeeded", row.ObservedExit)
	}
	if row.RunAttemptStatus != runattempt.StatusSucceeded {
		t.Fatalf("run attempt status = %q, want succeeded", row.RunAttemptStatus)
	}
	if row.ExitedAt == "" {
		t.Fatalf("expected an exit time")
	}
}

// A never-spawned refusal surfaces WHY delegation could not happen — the enum
// reason only, with no session and no run attempt.
func TestDelegatedSessionsRefusedRow(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(true, false, "walled: lane at 0% for 4d", "walled")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	if _, err := s.Delegate(DelegateOptions{WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow, Workspace: t.TempDir()}, runner); err != nil {
		t.Fatalf("delegate: %v", err)
	}

	row := findDelegatedRow(delegatedRowsFor(t, s), wID)
	if row == nil || row.Status != "refused" {
		t.Fatalf("status = %v, want refused (%+v)", statusOf(row), row)
	}
	if row.RefusedReason != "walled" {
		t.Fatalf("refused reason = %q, want walled", row.RefusedReason)
	}
	if row.Session != "" || row.RunAttemptID != "" {
		t.Fatalf("refused row must not carry a session or run attempt: %+v", row)
	}
}

// Privacy: the reduced row (and the whole delegated board) must NEVER echo the
// prompt/argv the operator passed, nor any free-text guard message.
func TestDelegatedSessionsPrivacy(t *testing.T) {
	const secret = "TOP-SECRET-PROMPT-do-not-leak"
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	if _, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex",
		Command: "claude", Args: []string{"--system", secret}, Now: fixedNow,
		Workspace: t.TempDir(),
	}, runner); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if _, err := s.Steer(SteerOptions{WorkID: wID, Actor: "codex", Text: secret + "-steer", Enter: true, Now: fixedNow.Add(1)}, runner); err != nil {
		t.Fatalf("steer: %v", err)
	}

	proj, err := s.ExecutorsProjection(fixedNow)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	blob, err := json.Marshal(proj.DelegatedSessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "TOP-SECRET") {
		t.Fatalf("delegated board leaked prompt/argv content: %s", blob)
	}
	if strings.Contains(strings.ToLower(string(blob)), "steered len=") {
		t.Fatalf("delegated board leaked steer message: %s", blob)
	}
}

func statusOf(r *DelegatedSessionRow) string {
	if r == nil {
		return "<nil row>"
	}
	return r.Status
}

// Unknown state: a spawned binding that NAMES a ratt whose record is missing
// must fold to unknown — never collapsed with the legacy empty-ratt case and
// guessed running. Only an actually empty legacy RunAttemptID may lean on the
// event binding as running evidence.
func TestDelegatedSessionsUnknownWhenNamedRattMissing(t *testing.T) {
	s := newExperimentalDelegateStore(t)
	withPermissiveGuard(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow, Workspace: t.TempDir()}, runner)
	if err != nil || res.Status != "spawned" {
		t.Fatalf("delegate: %v %+v", err, res)
	}
	if res.RunAttemptID == "" {
		t.Fatalf("expected a named run attempt on the binding")
	}
	// Delete the named RunAttempt record out from under the binding.
	if err := os.Remove(filepath.Join(s.home, "run_attempts", res.RunAttemptID+".json")); err != nil {
		t.Fatalf("remove ratt record: %v", err)
	}

	row := findDelegatedRow(delegatedRowsFor(t, s), wID)
	if row == nil || row.Status != "unknown" {
		t.Fatalf("status = %v, want unknown for a named-but-missing ratt (%+v)", statusOf(row), row)
	}
}
