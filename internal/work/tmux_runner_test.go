// Package-level falsifiers for the governed execution adapter
// `internal/work/tmux_runner.go`. Tests target a real tmux server rooted
// at a private socket so the operator's running tmux stays untouched.
// Every assertion exercises production Create / SessionStatus / Kill
// against real tmux (the only seam injected is a private socket prefix
// and a *exec.Cmd factory that records argv from production code).
//
// What the tests cover:
//   - TmuxRunner.Create uses the approved new-session -> set-option
//     remain-on-exit on -> respawn-pane -k -c DIR -- sh cd-guard launch
//     sequence, preserving argv verbatim behind the guard and NOT
//     launching the real command in new-session. Verified by capturing
//     the actual production argv via the tmuxCmd factory.
//   - A respawn-pane failure leaves the half-created session intact
//     (TmuxRunner never kills); the opaque error mentions respawn-pane.
//   - A set-option remain-on-exit failure leaves the half-created
//     session intact; the opaque error mentions set-option.
//   - SessionStatus queries `#{pane_dead}`, `#{pane_dead_status}` and
//     `#{pane_dead_signal}` -- NOT #{EXIT_CODE}/#{EXIT_SIGNAL}, and
//     returns ExitCode=-1 when no observed exit value is available.
//   - Real /bin/true and /bin/false delegated sessions: SessionStatus
//     reflects observed exit 0 vs 1 (NOT 0/1 ambiguity, NOT 0/-1).
package work

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func runTmux(sock string, args ...string) error {
	full := append([]string{"-S", sock}, args...)
	cmd := exec.Command("tmux", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("tmux %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("tmux %v: %v", args, err)
	}
	return nil
}

func runTmuxCapture(sock string, args ...string) (string, error) {
	full := append([]string{"-S", sock}, args...)
	cmd := exec.Command("tmux", full...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("tmux %v: %v", args, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func newPrivateTmuxServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH; integration test skipped")
	}
	// Use os.MkdirTemp("/tmp", ...) rather than t.TempDir() -- macOS's
	// tmux socket sun_path limit (~104 chars) rejects the deeply-nested
	// paths Go's TempDir produces on CI / developer machines.
	dir, err := os.MkdirTemp("/tmp", "tsx-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "t.sock")
	_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
	if err := runTmux(sock, "start-server"); err != nil {
		t.Fatalf("start tmux server on %s: %v", sock, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
	return sock
}

// hostCanRespawnPanes probes whether this HOST can respawn a tmux pane at all right
// now, driving tmux directly rather than the code under test.
//
// It exists to separate two failures that produce the identical error string.
// TmuxRunner.Create reports `tmux respawn-pane failed: exit 1` with tmux's stderr
// deliberately withheld (opaqueExitOnly, for privacy), so "the host cannot give us a
// pane" and "our respawn argv is wrong" are indistinguishable from the error alone.
// Skipping on the message would therefore hide a real Create regression, and failing
// on it reds the suite for unrelated host load.
//
// Observed 2026-07-25 (find_4a206b688909): a full-suite run failed here with
// `create /usr/bin/true: tmux respawn-pane failed: exit 1` on a host running 163
// tmux sessions from other lanes. The same test passed alone and the package passed
// on re-run, with no code change — the result was a property of the host, not the
// code. A test that reds for reasons unrelated to the code teaches the reader that
// red runs are noise, and then the next real red gets waved off too.
//
// Probe succeeds + our Create fails => our bug, fail loudly.
// Probe fails                       => the host cannot host the test, skip with why.
func hostCanRespawnPanes(sock string) bool {
	const probe = "probe-host-can-respawn"
	_ = runTmux(sock, "kill-session", "-t", probe)
	if err := runTmux(sock, "new-session", "-d", "-s", probe, "/bin/sh", "-c", "sleep 30"); err != nil {
		return false
	}
	defer func() { _ = runTmux(sock, "kill-session", "-t", probe) }()
	if err := runTmux(sock, "set-option", "-t", probe, "remain-on-exit", "on"); err != nil {
		return false
	}
	return runTmux(sock, "respawn-pane", "-k", "-t", probe, "--", "/bin/sh", "-c", "exit 0") == nil
}

// createForStatusTest creates a session for an exit-observation test, absorbing a
// transient host failure and skipping only when the host provably cannot host panes.
func createForStatusTest(t *testing.T, r *TmuxRunner, sock, name, dir, command string) {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := r.Create(name, dir, command, nil); err == nil {
			return
		} else {
			lastErr = err
		}
		// Create is documented NOT to kill a half-created session, so the retry has to
		// clear it or the next new-session collides with the leftover.
		_ = r.Kill(name)
		time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
	}
	if !hostCanRespawnPanes(sock) {
		t.Skipf("host cannot respawn a tmux pane right now (tmux-side, not this code): create %s: %v", command, lastErr)
	}
	t.Fatalf("create %s: %v (host CAN respawn panes, so this is a Create regression)", command, lastErr)
}

func sessionExistsOnSock(t *testing.T, sock, name string) bool {
	t.Helper()
	return runTmux(sock, "has-session", "-t", name) == nil
}

// capturingRunner routes TmuxRunner through a private tmux server and
// captures every production-side argv call. It exposes a per-subcommand
// failure shim backed by /bin/false (exec returns *exec.ExitError) so
// failure tests run production code paths -- not a duplicate mirror.
type capturingRunner struct {
	*TmuxRunner
	sock     string
	mu       sync.Mutex
	calls    [][]string
	failStep map[string]bool
}

func newCapturingRunner(t *testing.T, sock string) *capturingRunner {
	t.Helper()
	r := &TmuxRunner{}
	r.setTestSocketPrefix(sock)
	// Resolve a real "false" binary once. macOS lacks /bin/false; PATH
	// lookups find /usr/bin/false (or a Homebrew coreutils variant).
	falsePath, ferr := exec.LookPath("false")
	if ferr != nil {
		t.Skip("`false` not in PATH")
	}
	cr := &capturingRunner{
		TmuxRunner: r,
		sock:       sock,
		calls:      [][]string{},
		failStep:   map[string]bool{},
	}
	r.setTestCommandFactory(func(argv []string) *exec.Cmd {
		// argv is the SUB-command-only argv: TmuxRunner has already
		// applied `-S <sock>` to the *exec.Cmd path but not to the
		// factory's argv so asserts on production ordering stay simple.
		cr.mu.Lock()
		cr.calls = append(cr.calls, append([]string(nil), argv...))
		sub := ""
		if len(argv) > 0 {
			sub = argv[0]
		}
		fail := cr.failStep[sub]
		cr.mu.Unlock()
		if fail {
			// falsePath exits 1; Run() returns *exec.ExitError. The
			// runner reads Run() and surfaces an opaque, argv-free error.
			return exec.Command(falsePath)
		}
		return exec.Command("tmux", append([]string{"-S", sock}, argv...)...)
	})
	return cr
}

func (cr *capturingRunner) forceFail(sub string) {
	cr.mu.Lock()
	cr.failStep[sub] = true
	cr.mu.Unlock()
}

func (cr *capturingRunner) recorded() [][]string {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	out := make([][]string, len(cr.calls))
	for i, c := range cr.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func TestTmuxRunnerCreateLaunchOrderAgainstProduction(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	dir := t.TempDir()
	if err := cr.Create("sess-launch-order", dir, "/bin/sleep", []string{"30"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cr.Kill("sess-launch-order") })

	calls := cr.recorded()
	if len(calls) < 3 {
		t.Fatalf("captured %d tmux calls, want >=3: %+v", len(calls), calls)
	}
	first := calls[0]
	if first[0] != "new-session" || first[1] != "-d" || first[2] != "-s" || first[3] != "sess-launch-order" || first[4] != "-c" || first[5] != dir {
		t.Fatalf("new-session argv wrong (production): %v", first)
	}
	for _, tok := range first {
		if tok == "/bin/sleep" {
			t.Fatalf("new-session launched the real command (forbidden): %v", first)
		}
	}
	second := calls[1]
	if second[0] != "set-option" || second[1] != "-t" || second[2] != "sess-launch-order" || second[3] != "remain-on-exit" || second[4] != "on" {
		t.Fatalf("set-option argv wrong (production): %v", second)
	}
	third := calls[2]
	// The sh cd-guard is a regression tripwire, not style: without it a
	// wedged tmux server (deleted server cwd, observed 2026-07-24)
	// ignores -c, panes start with an invalid cwd, and bun-compiled
	// harness binaries die at spawn with an opaque ENOENT while the
	// RunAttempt rots in `running`. $0 must be the workspace and the
	// caller argv must follow verbatim after the script.
	want := []string{
		"respawn-pane", "-k", "-c", dir, "-t", "sess-launch-order", "--",
		"/bin/sh", "-c", `cd -- "$0" && exec "$@"`, dir, "/bin/sleep", "30"}
	if len(third) != len(want) {
		t.Fatalf("respawn-pane argv wrong (cd-guard or verbatim args missing): got %v want %v", third, want)
	}
	for i := range want {
		if third[i] != want[i] {
			t.Fatalf("respawn-pane argv[%d] = %q, want %q (full: %v)", i, third[i], want[i], third)
		}
	}
}

func TestTmuxRunnerCreateWithEnvironmentInstallsOpaqueValuesBeforeRespawn(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	dir := t.TempDir()
	opaque := `opaque value $(never-execute); "quoted"`
	if err := cr.CreateWithEnvironment(
		"sess-environment",
		dir,
		"/bin/sleep",
		[]string{"30"},
		map[string]string{
			"SB_LAUNCH_PROFILE_ID": "claude-minimax",
			"COMPOUND_WORK_ID":     opaque,
		},
	); err != nil {
		t.Fatalf("create with environment: %v", err)
	}
	t.Cleanup(func() { _ = cr.Kill("sess-environment") })

	calls := cr.recorded()
	if len(calls) < 5 {
		t.Fatalf("captured %d tmux calls, want >=5: %+v", len(calls), calls)
	}
	if calls[2][0] != "set-environment" ||
		calls[2][3] != "COMPOUND_WORK_ID" ||
		calls[2][4] != opaque {
		t.Fatalf("first sorted environment value was not passed as one opaque argv: %v", calls[2])
	}
	if calls[3][0] != "set-environment" ||
		calls[3][3] != "SB_LAUNCH_PROFILE_ID" ||
		calls[3][4] != "claude-minimax" {
		t.Fatalf("second sorted environment value was not passed exactly: %v", calls[3])
	}
	if calls[4][0] != "respawn-pane" {
		t.Fatalf("environment was not installed before supervised respawn: %+v", calls)
	}
	got, err := runTmuxCapture(sock, "show-environment", "-t", "sess-environment", "COMPOUND_WORK_ID")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "COMPOUND_WORK_ID="+opaque {
		t.Fatalf("tmux session environment = %q, want exact opaque value", got)
	}
}

// TestTmuxRunnerCreatePaneCwdIsWorkspace proves the pane's REAL working
// directory is the workspace by observing behavior, not argv: the spawned
// command writes $(pwd) to a file. This is the test that fails if the
// cd-guard regresses to -c-only on a server that honors -c but would not
// on a wedged one — the guard must hold regardless.
func TestTmuxRunnerCreatePaneCwdIsWorkspace(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	dir := t.TempDir()
	out := dir + "/pwd.txt"
	// Publish by rename: `pwd > out` creates the file before writing it, so a
	// poll between the two read "" and failed under load.
	if err := cr.Create("sess-cwd-proof", dir, "/bin/sh", []string{"-c", "pwd > " + out + ".tmp && mv " + out + ".tmp " + out}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cr.Kill("sess-cwd-proof") })
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(out)
		if err == nil {
			got := strings.TrimSpace(string(b))
			resolved, _ := filepath.EvalSymlinks(dir)
			if got != dir && got != resolved {
				t.Fatalf("pane cwd = %q, want workspace %q", got, dir)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never wrote pwd proof file: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTmuxRunnerCreateSetOptionFailureDoesNotKill(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	cr.forceFail("set-option")
	dir := t.TempDir()
	err := cr.Create("sess-set-fail", dir, "/bin/sleep", []string{"30"})
	if err == nil {
		t.Fatalf("create must fail when set-option fails")
	}
	if !strings.Contains(err.Error(), "set-option") {
		t.Fatalf("error must mention set-option: %v", err)
	}
	if strings.Contains(err.Error(), "/bin/sleep") || strings.Contains(err.Error(), "/bin/false") {
		t.Fatalf("error leaks argv: %v", err)
	}
	// CRITICAL: no kill-session issued for the partial create.
	for _, c := range cr.recorded() {
		if c[0] == "kill-session" {
			t.Fatalf("TmuxRunner issued kill-session on partial create (forbidden): %v", c)
		}
	}
	// respawn-pane MUST NOT be reached because set-option failed first.
	for _, c := range cr.recorded() {
		if c[0] == "respawn-pane" {
			t.Fatalf("respawn-pane reached when set-option failed: %v", c)
		}
	}
	// The orphan session really exists on the private server -- the
	// orchestrator (not the runner) is what classifies it as ambiguous.
	if !sessionExistsOnSock(t, sock, "sess-set-fail") {
		t.Fatalf("partial session should exist on private server (orchestrator will probe)")
	}
}

func TestTmuxRunnerCreateRespawnFailureDoesNotKill(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	cr := newCapturingRunner(t, sock)
	cr.forceFail("respawn-pane")
	dir := t.TempDir()
	err := cr.Create("sess-rsp-fail", dir, "/bin/sleep", []string{"30"})
	if err == nil {
		t.Fatalf("create must fail when respawn-pane fails")
	}
	if !strings.Contains(err.Error(), "respawn-pane") {
		t.Fatalf("error must mention respawn-pane: %v", err)
	}
	if strings.Contains(err.Error(), "/bin/sleep") || strings.Contains(err.Error(), "/bin/false") {
		t.Fatalf("error leaks argv: %v", err)
	}
	for _, c := range cr.recorded() {
		if c[0] == "kill-session" {
			t.Fatalf("TmuxRunner issued kill-session on partial create (forbidden): %v", c)
		}
	}
	if !sessionExistsOnSock(t, sock, "sess-rsp-fail") {
		t.Fatalf("partial session should exist on private server (orchestrator will probe)")
	}
}

func TestTmuxRunnerRespawnFailureNeverEchoesCommandArguments(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	r := NewTmuxRunner()
	const privateArg = "SYNTHETIC-PRIVATE-UTTERANCE"
	r.setTestCommandFactory(func(argv []string) *exec.Cmd {
		if len(argv) > 0 && argv[0] == "respawn-pane" {
			return exec.Command("sh", "-c", "echo "+privateArg+" >&2; exit 1")
		}
		return exec.Command("tmux", append([]string{"-S", sock}, argv...)...)
	})
	err := r.Create("sess-private-error", t.TempDir(), "/usr/bin/false", []string{privateArg})
	if err == nil {
		t.Fatal("respawn failure must surface")
	}
	if strings.Contains(err.Error(), privateArg) || strings.Contains(err.Error(), "/usr/bin/false") {
		t.Fatalf("respawn error leaked command arguments: %v", err)
	}
}

func TestTmuxRunnerSessionStatusMalformedOutputIsUncertain(t *testing.T) {
	r := NewTmuxRunner()
	r.setTestCommandFactory(func(argv []string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf malformed")
	})
	status, err := r.SessionStatus("sess-malformed")
	if err == nil || status.UnknownReason == "" || status.ExitCode != -1 {
		t.Fatalf("malformed status = %+v err=%v", status, err)
	}
}

func TestTmuxRunnerSessionStatusTrueAndFalseExit(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	r := NewTmuxRunner()
	r.setTestSocketPrefix(sock)

	// Resolve real `true` and `false` absolute paths via LookPath -- the
	// tmux process must execve a real binary or pane_dead_status will
	// reflect a 127/command-not-found rather than 0 / 1. macOS lacks
	// /bin/{true,false}; LookPath finds them under /usr/bin.
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("`true` not in PATH")
	}
	falsePath, err := exec.LookPath("false")
	if err != nil {
		t.Skip("`false` not in PATH")
	}

	dir1 := t.TempDir()
	createForStatusTest(t, r, sock, "sess-true", dir1, truePath)
	t.Cleanup(func() { _ = r.Kill("sess-true") })
	waitPaneDead(t, sock, "sess-true")
	stTrue, err := r.SessionStatus("sess-true")
	if err != nil {
		t.Fatalf("status %s: %v", truePath, err)
	}
	if !stTrue.Exists || !stTrue.PaneDead {
		t.Fatalf("expected exists+dead for true, got %+v", stTrue)
	}
	if stTrue.ExitCode != 0 {
		t.Fatalf("true observed exit = %d, want 0 (got %+v)", stTrue.ExitCode, stTrue)
	}

	dir2 := t.TempDir()
	createForStatusTest(t, r, sock, "sess-false", dir2, falsePath)
	t.Cleanup(func() { _ = r.Kill("sess-false") })
	waitPaneDead(t, sock, "sess-false")
	stFalse, err := r.SessionStatus("sess-false")
	if err != nil {
		t.Fatalf("status %s: %v", falsePath, err)
	}
	if !stFalse.PaneDead {
		t.Fatalf("expected pane_dead for false, got %+v", stFalse)
	}
	if stFalse.ExitCode != 1 {
		t.Fatalf("false observed exit = %d, want 1 (got %+v)", stFalse.ExitCode, stFalse)
	}
}

func TestTmuxRunnerSessionStatusLivePaneReturnsMinusOne(t *testing.T) {
	sock := newPrivateTmuxServer(t)
	r := NewTmuxRunner()
	r.setTestSocketPrefix(sock)
	dir := t.TempDir()
	if err := r.Create("sess-live", dir, "/bin/sleep", []string{"30"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill("sess-live") })
	st, err := r.SessionStatus("sess-live")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Exists || st.PaneDead {
		t.Fatalf("live pane wrong: %+v", st)
	}
	if st.ExitCode != -1 {
		t.Fatalf("live pane ExitCode = %d, want -1 (unknown)", st.ExitCode)
	}
}

func waitPaneDead(t *testing.T, sock, name string) {
	t.Helper()
	deadline := 50
	for i := 0; i < deadline; i++ {
		out, err := runTmuxCapture(sock, "list-panes", "-t", name, "-F", "#{pane_dead}")
		if err == nil && strings.TrimSpace(out) == "1" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pane %q did not die within 5s", name)
}
