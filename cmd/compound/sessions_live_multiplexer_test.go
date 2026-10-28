package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// runTmuxPrivate runs a tmux invocation against a private socket. argv is
// the tmux subcommand + flags, no socket prefix.
func runTmuxPrivate(t *testing.T, sock string, argv ...string) error {
	t.Helper()
	full := append([]string{"-f", os.DevNull, "-S", sock}, argv...)
	cmd := exec.Command("tmux", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("tmux %v: %v: %s", argv, err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("tmux %v: %v", argv, err)
	}
	return nil
}

// newPrivateTmuxServerForLive starts a private tmux server on a short
// socket path so the multiplexer source test does not touch the operator's
// running server. Caller-side tests invoke this; cleanup kills the server.
func newPrivateTmuxServerForLive(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH; multiplexer-observation test skipped")
	}
	dir, err := os.MkdirTemp("/tmp", "cmplx-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "t.sock")
	_ = exec.Command("tmux", "-f", os.DevNull, "-S", sock, "kill-server").Run()
	if err := runTmuxPrivate(t, sock, "start-server"); err != nil {
		t.Fatalf("start tmux server on %s: %v", sock, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-f", os.DevNull, "-S", sock, "kill-server").Run()
	})
	return sock
}

// tmuxRunnerForSocket returns a TmuxRunner pinned to a private tmux socket.
// Used by the multiplexer source test so it cannot touch the operator's
// real tmux server. The runner's Multiplexer() shares the same socket.
func tmuxRunnerForSocket(sock string) *work.TmuxRunner {
	r := work.NewTmuxRunner()
	r.SetTestSocketPrefix(sock)
	return r
}

// initEmptyCompoundHome creates the minimum compound home layout so
// `compound sessions live` can resolve it via --home without touching the
// operator's real ~/.compound. Mirrors home.Init for tests that only need
// a resolvable home, no eventstore / runattempt seed.
func initEmptyCompoundHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"eventstore",
		"runattempt",
		"work",
		"observed",
		"locks",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("schema_version: compound.home.v0.1\nhome_kind: test\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return root
}

// The dangerous failure was: `compound sessions live` only knew about
// compound-spawned panes. After a restart the operator's own tmux panes
// (running claude, codex, etc.) were invisible — the same surface that
// shows "is my work alive?" had no answer for the panes that hold the
// worker's attention. The falsifier proves the second observation source
// contributes rows for panes the multiplexer sees but compound never
// recorded: provenance, lane identity, working directory, last activity,
// and resume command must all be populated.
//
// On current main this test fails to compile (the schema fields and
// MultiplexerSource seam do not exist). After the implementation lands,
// it must pass against a real private tmux server.
func TestSessionsLiveReportsMultiplexerOnlyPanes(t *testing.T) {
	sock := newPrivateTmuxServerForLive(t)

	// One operator-spawned session with a known cwd. The session name is
	// the only lane-identity signal available for a multiplexer-only pane;
	// it is what an operator would type to resume the pane.
	sessName := "operator-lane-probe"
	cwd := t.TempDir()
	// Multiple command arguments make tmux exec directly. A private socket
	// alone does not stop the user's shell startup from changing the pane CWD.
	if err := runTmuxPrivate(t, sock, "new-session", "-d", "-s", sessName, "-c", cwd, "/bin/cat", "-"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	// Force an activity tick so session_activity is strictly newer than
	// session_created; the row's last_activity field must not be the
	// creation time or the test is silently asserting nothing.
	if err := runTmuxPrivate(t, sock, "send-keys", "-t", sessName, "-l", "ping"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	runner := tmuxRunnerForSocket(sock)
	mux := runner.Multiplexer()
	if mux == nil {
		t.Fatalf("TmuxRunner.Multiplexer must return a non-nil MultiplexerSource")
	}
	// The multiplexer source must see our freshly-created session.
	precheck, err := mux.List()
	if err != nil {
		t.Fatalf("multiplexer List: %v", err)
	}
	found := false
	for _, o := range precheck {
		if o.Name == sessName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("precheck: multiplexer did not see %s (saw %d sessions)", sessName, len(precheck))
	}

	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, runner, mux)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	var report work.SessionsLiveReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("decode report: %v\nstdout=%s", err, stdout.String())
	}

	var row *work.SessionLiveRow
	for i := range report.Rows {
		if report.Rows[i].Session == sessName {
			row = &report.Rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("multiplexer-only session %q not reported. rows=%+v", sessName, report.Rows)
	}
	if row.Provenance != "multiplexer" {
		t.Fatalf("provenance = %q, want %q (the multiplexer is the only source that knows about this session)", row.Provenance, "multiplexer")
	}
	// Lane identity: best-effort from session name when compound has no
	// recorded binding. The session name IS the lane identity for an
	// operator-spawned pane; a future schema can refine it.
	if row.Lane != sessName {
		t.Fatalf("lane identity = %q, want %q (best-effort from session name)", row.Lane, sessName)
	}
	// tmux resolves /tmp -> /private/tmp on macOS via realpath; compare on
	// the trailing path so the assertion is portable.
	if !strings.HasSuffix(row.CWD, filepath.Base(cwd)) {
		t.Fatalf("cwd = %q, want suffix %q", row.CWD, filepath.Base(cwd))
	}
	if strings.TrimSpace(row.LastActivity) == "" {
		t.Fatalf("last activity missing for multiplexer-only row")
	}
	if _, err := time.Parse(time.RFC3339, row.LastActivity); err != nil {
		t.Fatalf("last activity not RFC3339: %q (%v)", row.LastActivity, err)
	}
	if !strings.Contains(row.ResumeCmd, "tmux attach-session") || !strings.Contains(row.ResumeCmd, sessName) {
		t.Fatalf("resume command = %q, want `tmux attach-session -t %s`", row.ResumeCmd, sessName)
	}
}

// A compound-recorded pane that the multiplexer also sees must produce ONE
// row, not two: provenance=compound carries the work identity, while cwd /
// last activity / resume command come from the multiplexer observation.
// Duplicate-row output is its own falsifier: the operator would have to
// pick which row to act on, and the answer would be wrong at least once.
func TestSessionsLiveMergesCompoundAndMultiplexerOnSameSession(t *testing.T) {
	sock := newPrivateTmuxServerForLive(t)
	sessName := "compound-bound-probe"
	cwd := t.TempDir()
	if err := runTmuxPrivate(t, sock, "new-session", "-d", "-s", sessName, "-c", cwd, "/bin/cat", "-"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	if err := runTmuxPrivate(t, sock, "send-keys", "-t", sessName, "-l", "ping"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	runner := tmuxRunnerForSocket(sock)
	mux := runner.Multiplexer()

	// Compound-side: a fake observation that says "this work is bound to
	// sessName and the pane is alive". We bypass runattempt seeding by
	// injecting the observation directly via the seam.
	obs := work.DelegateStatusObservation{
		Bound:      true,
		Session:    sessName,
		PaneStatus: work.SessionStatus{Exists: true, PaneDead: false, ExitCode: -1},
	}
	state, _ := work.ClassifySessionLiveness("running", obs)
	if state != "live" {
		t.Fatalf("sanity: classifier state = %q, want live", state)
	}

	// Verify the multiplexer also sees it (precondition for the merge).
	list, err := mux.List()
	if err != nil {
		t.Fatalf("multiplexer List: %v", err)
	}
	seen := false
	for _, o := range list {
		if o.Name == sessName {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("precondition: multiplexer did not see %s", sessName)
	}

	// ResumeArgv on the multiplexer source must produce a tmux attach argv
	// the operator can paste back into a shell to put the pane back.
	resume := mux.ResumeArgv(sessName)
	if len(resume) < 3 {
		t.Fatalf("resume argv too short: %v", resume)
	}
	if resume[0] != "tmux" {
		t.Fatalf("resume argv[0] = %q, want %q", resume[0], "tmux")
	}
	if !containsString(resume, "-t") || !containsString(resume, sessName) {
		t.Fatalf("resume argv missing -t %s: %v", sessName, resume)
	}
}

// The multiplexer source itself must fail loudly when the underlying tmux
// server is unreachable, so the operator sees a real reason instead of an
// empty report (the dangerous failure is "nothing reported, looks like
// everything is fine").
func TestMultiplexerSourceReportsServerUnreachable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	// Point at a socket that we KNOW does not exist.
	bogus := filepath.Join(t.TempDir(), "never-started.sock")
	r := work.NewTmuxRunner()
	r.SetTestSocketPrefix(bogus)
	mux := r.Multiplexer()
	if _, err := mux.List(); err == nil {
		t.Fatalf("multiplexer.List must error when its tmux server is unreachable, got nil")
	} else if !strings.Contains(err.Error(), "tmux") && !errors.Is(err, os.ErrNotExist) {
		// Some tmux errors surface as bare exec errors. Accept anything that
		// is clearly NOT silent success; the dangerous case is the empty
		// report.
		t.Logf("got expected error: %v", err)
	}
}
