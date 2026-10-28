package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exist because of a real, expensive failure: on 2026-07-26 a
// 55-session delegate burst vanished leaving zero artifacts and zero
// diagnosable cause, because pane output was never captured anywhere. Each
// test below fails if that hole reopens.

// TestTmuxRunnerCreateCapturesPaneOutputToLog is the end-to-end proof: a
// command run by Create must leave its stdout in the log file.
//
// It is also the ORDERING proof. Capture is installed before
// respawn-pane -k, and respawn kills and restarts the pane; if pipe-pane
// did not survive that (or someone reorders it after the respawn and the
// command is fast enough to finish first), the marker never lands and
// this fails.
func TestTmuxRunnerCreateCapturesPaneOutputToLog(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	logDir := t.TempDir()
	cr.logDir = logDir

	const session = "sess-capture-proof"
	const marker = "MARKER_FROM_INSIDE_THE_PANE"
	if err := cr.Create(session, t.TempDir(), "/bin/sh", []string{"-c", "echo " + marker}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cr.Kill(session) })

	logPath := filepath.Join(logDir, session+".log")
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(b), marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane output never reached %s (err=%v, contents=%q) -- delegate output is unobservable again", logPath, err, string(b))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// pipe-pane must be installed before respawn-pane, or the startup
	// window (where wedged-cwd and trust-prompt failures live) is blind.
	var pipeIdx, respawnIdx = -1, -1
	for i, call := range cr.recorded() {
		if len(call) == 0 {
			continue
		}
		switch call[0] {
		case "pipe-pane":
			if pipeIdx == -1 {
				pipeIdx = i
			}
		case "respawn-pane":
			if respawnIdx == -1 {
				respawnIdx = i
			}
		}
	}
	if pipeIdx == -1 {
		t.Fatal("Create never called pipe-pane: capture is not installed at all")
	}
	if respawnIdx == -1 {
		t.Fatal("Create never called respawn-pane")
	}
	if pipeIdx > respawnIdx {
		t.Fatalf("pipe-pane ran at %d, after respawn-pane at %d: startup output is unobservable", pipeIdx, respawnIdx)
	}
}

// TestTmuxRunnerCreatePipePaneFailureFailsClosed pins the fail-closed
// contract. A spawn that cannot be observed is precisely the failure this
// change removes, so a capture that cannot be installed must fail the
// spawn rather than quietly continue and look green.
func TestTmuxRunnerCreatePipePaneFailureFailsClosed(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	cr.logDir = t.TempDir()
	cr.forceFail("pipe-pane")

	const session = "sess-capture-failclosed"
	err := cr.Create(session, t.TempDir(), "/bin/sh", []string{"-c", "true"})
	t.Cleanup(func() { _ = cr.Kill(session) })
	if err == nil {
		t.Fatal("Create succeeded with a failed pipe-pane: an unobservable spawn was reported as success")
	}
	if !strings.Contains(err.Error(), "pipe-pane") {
		t.Fatalf("error does not name the failing step: %v", err)
	}
	// The runner must never kill a session it may not own, and must never
	// echo caller argv in the error.
	if strings.Contains(err.Error(), "/bin/sh") {
		t.Fatalf("error leaked caller argv: %v", err)
	}
}

// TestTmuxRunnerWithoutLogDirInstallsNoPipe keeps the historical
// no-capture path reachable, so an operator or test that does not want a
// log does not silently start writing one.
func TestTmuxRunnerWithoutLogDirInstallsNoPipe(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock) // logDir intentionally unset

	const session = "sess-capture-optout"
	if err := cr.Create(session, t.TempDir(), "/bin/sh", []string{"-c", "true"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cr.Kill(session) })

	for _, call := range cr.recorded() {
		if len(call) > 0 && call[0] == "pipe-pane" {
			t.Fatal("pipe-pane was installed for a runner with no log directory")
		}
	}
	if got := cr.PaneLogPath(session); got != "" {
		t.Fatalf("PaneLogPath = %q, want empty for a non-capturing runner", got)
	}
}

// TestPaneLogPathIsSessionScoped guards the naming contract that the
// sweep, doctor, and any future attach surface depend on to find the log.
func TestPaneLogPathIsSessionScoped(t *testing.T) {
	r := NewTmuxRunnerWithLogDir("/var/tmp/logs")
	if got, want := r.PaneLogPath("alpha"), filepath.Join("/var/tmp/logs", "alpha.log"); got != want {
		t.Fatalf("PaneLogPath = %q, want %q", got, want)
	}
	if got := r.PaneLogPath("  "); got != "" {
		t.Fatalf("blank session should have no log path, got %q", got)
	}
	if got := NewTmuxRunner().PaneLogPath("alpha"); got != "" {
		t.Fatalf("default runner should not capture, got %q", got)
	}
}
