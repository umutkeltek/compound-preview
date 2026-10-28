package work

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TmuxRunner is the thin supervised-session adapter for governed delegation.
// It owns no meaning: work/session identity and lifecycle evidence remain in
// Store; tmux is only the observable, steerable process container.
type TmuxRunner struct {
	// socketPrefix is an unexported extra argv prefixed onto every tmux
	// invocation (typically "-S <sock>"). nil/empty means the operator's
	// server is used. Tests set this via setTestSocketPrefix to isolate
	// from any tmux the operator happens to be running.
	socketPrefix []string
	// tmuxCmd is an unexported *exec.Cmd builder used by every tmux call.
	// The default shells out via exec.Command("tmux", argv...). Tests
	// override via setTestCommandFactory to capture production argv
	// (real launch-order assertion) or to inject command-level failures
	// against a real tmux server (so production code is exercised).
	tmuxCmd func(argv []string) *exec.Cmd
	// logDir, when non-empty, makes Create mirror the pane's raw output
	// into <logDir>/<session>.log via tmux pipe-pane. Empty means no
	// capture (the historical behavior).
	logDir string
}

func NewTmuxRunner() *TmuxRunner { return &TmuxRunner{} }

// NewTmuxRunnerWithLogDir returns a runner that mirrors every pane's raw
// output into dir/<session>.log.
//
// Why this exists: without it a delegate's output lives ONLY in tmux
// scrollback, so a session that dies takes the entire record of what it
// did (and why it stopped) with it. That is not hypothetical -- on
// 2026-07-26 a 55-session burst vanished leaving zero artifacts and zero
// diagnosable cause, because there was nothing to read after the panes
// were gone.
//
// What it is NOT: a clean deliverable. The captured stream is the raw
// pane byte stream, so a TUI harness writes ANSI escapes and redraws
// into it. Treat the log as forensics -- "did it start, what did it
// print, how did it die" -- and keep real deliverables in files the
// task writes on purpose. The log is evidence-class and belongs under
// COMPOUND_HOME/observed/, never in the eventstore: event payloads
// never carry command stdout (privacy law of the packet).
func NewTmuxRunnerWithLogDir(dir string) *TmuxRunner {
	return &TmuxRunner{logDir: strings.TrimSpace(dir)}
}

// PaneLogPath reports where Create mirrors the named session's output,
// or "" when this runner captures nothing.
func (r *TmuxRunner) PaneLogPath(session string) string {
	session = strings.TrimSpace(session)
	if r.logDir == "" || session == "" {
		return ""
	}
	return filepath.Join(r.logDir, session+".log")
}

// tmuxExec builds the *exec.Cmd for the next tmux invocation, applying
// any test-only socketPrefix and tmuxCmd shims. Production callers see the
// operator's tmux server; tests see a private server. The injected
// command factory receives argv with the SUB-COMMAND only (no `-S sock`
// prefix), so test recorders and assertions can address the tmux
// subcommand at argv[0] without parsing the prefix.
func (r *TmuxRunner) tmuxExec(argv ...string) *exec.Cmd {
	full := append([]string{}, r.socketPrefix...)
	full = append(full, argv...)
	if r.tmuxCmd != nil {
		return r.tmuxCmd(argv)
	}
	return exec.Command("tmux", full...)
}

// setTestSocketPrefix points the runner at a private tmux server (e.g.
// `tmux -S /tmp/foo.sock`) so production code runs end-to-end against a
// server under the test's control. Caller-side tests invoke this; it is
// intentionally not part of the production seam.
func (r *TmuxRunner) setTestSocketPrefix(sock string) {
	sock = strings.TrimSpace(sock)
	if sock == "" {
		r.socketPrefix = nil
		return
	}
	r.socketPrefix = []string{"-S", sock}
}

// SetTestSocketPrefix is the exported variant of setTestSocketPrefix,
// used by tests that live outside the work package (e.g. cmd/compound's
// multiplexer-observation tests). The behavior is identical: a non-empty
// socket pins every tmux invocation at this runner — and at its
// Multiplexer() — to that private server. Empty string clears the pin.
func (r *TmuxRunner) SetTestSocketPrefix(sock string) {
	r.setTestSocketPrefix(sock)
}

// setTestCommandFactory injects a *exec.Cmd builder. The default builder
// calls exec.Command("tmux", argv...). Tests use it to capture production
// argv from real launches and to inject per-subcommand failures against
// an actual tmux server (no duplicate-mirror code path).
func (r *TmuxRunner) setTestCommandFactory(fn func(argv []string) *exec.Cmd) {
	r.tmuxCmd = fn
}

func (r *TmuxRunner) Available() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is unavailable: %w", err)
	}
	return nil
}

func (r *TmuxRunner) Has(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("tmux session is required")
	}
	err := r.tmuxRunOpaque("has-session", "-t", name)
	if err == nil {
		return true, nil
	}
	if isTmuxMiss(err) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session failed: %w", err)
}

// SessionStatus probes the supervised pane through `tmux list-panes`. It is
// the orchestrator's source of truth for whether a previously-spawned
// session is still running: tmux keeps dead sessions around with
// remain-on-exit, so Has alone is false evidence once the command exits.
//
// The probes read tmux's documented formats:
//   - #{pane_dead}        -> "0" / "1"
//   - #{pane_dead_status} -> exit code ("0", "1", ...) or empty when alive / observed
//   - #{pane_dead_signal} -> signal name or empty
//
// ExitCode / Signal are mapped from those formats; ExitCode stays -1 when
// no observed exit value is available (live pane, server crash, or
// remain-on-exit set without metadata). The orchestrator MUST treat -1 as
// "unknown", never as 0.
func (r *TmuxRunner) SessionStatus(name string) (SessionStatus, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return SessionStatus{}, errors.New("tmux session is required")
	}
	cmd := r.tmuxExec("list-panes", "-t", name, "-F",
		"#{pane_dead}||#{pane_dead_status}||#{pane_dead_signal}")
	cmd.Stderr = &bytes.Buffer{}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := ""
			if b, ok := cmd.Stderr.(*bytes.Buffer); ok {
				stderr = strings.TrimSpace(b.String())
			}
			if tmuxTargetMissing(stderr) {
				// A named target miss is a normal vanished observation.
				return SessionStatus{Exists: false, ExitCode: -1}, nil
			}
			reason := fmt.Sprintf("tmux list-panes exit %d", exitErr.ExitCode())
			return SessionStatus{Exists: false, ExitCode: -1, UnknownReason: reason}, fmt.Errorf("tmux list-panes failed: %s", reason)
		}
		return SessionStatus{Exists: false, ExitCode: -1, UnknownReason: "tmux list-panes execution failed"}, fmt.Errorf("tmux list-panes failed: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	if !scanner.Scan() {
		return SessionStatus{Exists: false}, nil
	}
	parts := strings.Split(scanner.Text(), "||")
	if len(parts) < 3 {
		reason := "tmux list-panes returned malformed status"
		return SessionStatus{Exists: true, ExitCode: -1, UnknownReason: reason}, errors.New(reason)
	}
	deadFlag, status, signal := parts[0], parts[1], parts[2]
	st := SessionStatus{Exists: true, ExitCode: -1}
	switch deadFlag {
	case "1":
		st.PaneDead = true
	case "0":
	default:
		reason := "tmux list-panes returned invalid pane_dead"
		st.UnknownReason = reason
		return st, errors.New(reason)
	}
	if status != "" {
		code, perr := strconv.Atoi(status)
		if perr != nil {
			reason := "tmux list-panes returned invalid pane_dead_status"
			st.UnknownReason = reason
			return st, errors.New(reason)
		}
		st.ExitCode = code
	}
	if signal != "" {
		st.Signal = strings.TrimSpace(signal)
	}
	return st, nil
}

// Create brings up a new tmux session that supervises the requested
// command using the race-free launch sequence below. This is launch
// ordering, not an auto-respawn policy (no sentinel, no wrapper, no
// respawn-on-exit option):
//
//  1. new-session -d -s NAME -c DIR  (empty default shell; the real
//     command MUST NOT be passed here, otherwise it can exit before
//     remain-on-exit is installed and the dead-pane marker disappears),
//  2. set-option -t NAME remain-on-exit on (so the pane persists dead),
//  3. respawn-pane -k -c DIR -t NAME -- /bin/sh -c 'cd -- "$0" && exec "$@"' DIR COMMAND ARGS
//     (preserves argv verbatim through "$@"; -k kills the default
//     shell; -- terminates tmux's own flag parsing). The bare NAME
//     targets the (only) pane of the newly-created session regardless
//     of server base-index, which we can NOT control at the
//     new-session step without affecting the operator's other
//     workspaces.
//
// The sh cd-guard is load-bearing, not decoration: a wedged tmux
// server (observed 2026-07-24: server cwd stuck in a deleted worktree)
// silently ignores -c on BOTH new-session and respawn-pane and starts
// every pane with an invalid cwd, which kills bun-compiled binaries
// (claude and the claude-* lane wrappers) at startup with an opaque
// "ENOENT: Bun could not find a file" before the harness runs at all —
// the RunAttempt then rots in `running` with a dead pane. cd-ing
// inside the pane command makes workspace entry independent of tmux
// server state and turns a genuinely missing workspace into a loud
// in-pane `cd` failure (fail closed) instead of an inherited-garbage
// cwd.
//
// respawn-pane is the load-bearing supervision step: if it fails the
// session may exist but the real command never started. The runner
// returns an opaque, argv-free error AND performs ZERO kills: the
// orchestrator probes the server itself to determine whether the
// session exists, classifies it as spawn_ambiguous if so (a stranger's
// session must never be killed), and terminalizes the attempt as failed
// without emitting a spawned binding.
func (r *TmuxRunner) Create(name, dir, command string, args []string) error {
	return r.CreateWithEnvironment(name, dir, command, args, nil)
}

func (r *TmuxRunner) CreateWithEnvironment(name, dir, command string, args []string, environment map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("tmux session is required")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("tmux workspace is required")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New("tmux command is required")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is unavailable: %w", err)
	}
	// 1. Create the empty detached session (real command MUST NOT launch
	//    here: it would race remain-on-exit).
	if err := r.tmuxRunOpaque("new-session", "-d", "-s", name, "-c", dir); err != nil {
		return fmt.Errorf("tmux new-session failed: %s", opaqueOneLiner(err))
	}
	// 2. Install remain-on-exit BEFORE the real command runs.
	if err := r.tmuxRunOpaque("set-option", "-t", name, "remain-on-exit", "on"); err != nil {
		// NO KILL: orchestrator probes after any Create error and may
		// classify the partial session as ambiguous. Returning the
		// opaque error is the entire contract.
		return fmt.Errorf("tmux set-option remain-on-exit failed: %s", opaqueOneLiner(err))
	}
	// 2b. Install pane capture BEFORE the real command runs, so the very
	//     first byte is recorded. Verified empirically: pipe-pane set
	//     here survives the respawn-pane -k below (pane_pipe stays 1)
	//     and captures post-respawn output, so ordering it here loses
	//     nothing and gains the startup window -- which is exactly where
	//     the expensive failures live (a wedged cwd killing the binary
	//     before the harness runs, a trust prompt that never clears).
	//
	//     Fail closed, matching the remain-on-exit branch above: a spawn
	//     that cannot be observed is the failure mode this whole change
	//     exists to remove, so silently continuing without capture would
	//     reintroduce it under a green result.
	if err := r.startPaneCapture(name); err != nil {
		// NO KILL: same contract as the set-option branch above.
		return err
	}
	environmentKeys := make([]string, 0, len(environment))
	for key, value := range environment {
		if !validSupervisedEnvironmentKey(key) {
			return errors.New("tmux supervised environment has invalid key")
		}
		if strings.ContainsRune(value, '\x00') || len(value) > 4096 {
			return errors.New("tmux supervised environment has invalid value")
		}
		environmentKeys = append(environmentKeys, key)
	}
	sort.Strings(environmentKeys)
	for _, key := range environmentKeys {
		if err := r.tmuxRunOpaque("set-environment", "-t", name, key, environment[key]); err != nil {
			return fmt.Errorf("tmux set-environment failed: %s", opaqueExitOnly(err))
		}
	}
	// 3. respawn-pane -k with the user's exact argv behind the sh
	//    cd-guard (see the Create doc comment: -c alone is ignored by a
	//    wedged server). $0 is the workspace, "$@" is the verbatim
	//    command+args — no shell quoting of caller argv ever happens.
	//    -t uses the bare session name because the new session's window
	//    index is base-index-dependent and we can't pin base-index 0
	//    without writing to the operator's global tmux.conf.
	respawnArgv := append([]string{
		"respawn-pane", "-k", "-c", dir, "-t", name, "--",
		"/bin/sh", "-c", `cd -- "$0" && exec "$@"`, dir, command}, args...)
	if err := r.tmuxRunOpaque(respawnArgv...); err != nil {
		// NO KILL: same contract as the set-option branch above.
		return fmt.Errorf("tmux respawn-pane failed: %s", opaqueExitOnly(err))
	}
	return nil
}

func validSupervisedEnvironmentKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// startPaneCapture points tmux pipe-pane at this session's log file. It
// is a no-op for a runner built without a log directory, so the
// historical no-capture behavior stays reachable and tests that do not
// care are unaffected.
//
// The shell fragment handed to pipe-pane is built from the log path
// only. Session names are validated by tmux itself and the path is
// derived from an operator-configured directory plus that name, so no
// caller argv ever reaches this string.
func (r *TmuxRunner) startPaneCapture(session string) error {
	logPath := r.PaneLogPath(session)
	if logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("tmux pipe-pane log dir failed: %w", err)
	}
	// -o toggles the pipe on only when it is not already running; the
	// single-quoted path guards spaces. Appending (>>) keeps a respawn
	// of the same session name from truncating earlier evidence.
	shellCmd := "cat >> '" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	if err := r.tmuxRunOpaque("pipe-pane", "-o", "-t", session, shellCmd); err != nil {
		return fmt.Errorf("tmux pipe-pane failed: %s", opaqueOneLiner(err))
	}
	return nil
}

// tmuxRunOpaque runs a tmux invocation with stderr captured into an
// internal buffer. The returned error wraps tmux's exit code (when an
// *exec.ExitError is at the root) but exposes only tmux's stdout-free
// stderr text -- NEVER the user-side argv. Used for Create / Kill /
// Has / list-panes -- NOT SendKeys, which receives operator text and
// must keep any failure message argv-free / prompt-free.
func (r *TmuxRunner) tmuxRunOpaque(argv ...string) error {
	cmd := r.tmuxExec(argv...)
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err == nil {
		return nil
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := ""
			if b, ok := cmd.Stderr.(*bytes.Buffer); ok {
				stderr = strings.TrimSpace(b.String())
			}
			if stderr != "" {
				return &tmuxOpaqueErr{sub: argv[0], exitCode: exitErr.ExitCode(), stderr: stderr}
			}
			return &tmuxOpaqueErr{sub: argv[0], exitCode: exitErr.ExitCode()}
		}
		// Non-exit errors (PathError etc.) propagate directly -- these
		// can include the tmux binary path, which is NOT user argv.
		return err
	}
}

type tmuxOpaqueErr struct {
	sub      string
	exitCode int
	stderr   string
}

func (e *tmuxOpaqueErr) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("tmux %s exit %d: %s", e.sub, e.exitCode, e.stderr)
	}
	return fmt.Sprintf("tmux %s exit %d", e.sub, e.exitCode)
}

func (e *tmuxOpaqueErr) Unwrap() error { return nil }

// isTmuxMiss reports whether the opaque error corresponds to a session
// that tmux says does not exist (exit 1 with a "no session" message).
func isTmuxMiss(err error) bool {
	var oe *tmuxOpaqueErr
	if !errors.As(err, &oe) {
		return false
	}
	return oe.exitCode == 1 && tmuxTargetMissing(oe.stderr)
}

func tmuxTargetMissing(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no session") ||
		strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "can't find window")
}

func opaqueExitOnly(err error) string {
	var oe *tmuxOpaqueErr
	if errors.As(err, &oe) {
		return fmt.Sprintf("exit %d", oe.exitCode)
	}
	return "execution failed"
}

func (r *TmuxRunner) SendKeys(name, text string, enter bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("tmux session is required")
	}
	if text == "" {
		return errors.New("tmux steer text is required")
	}
	cmd := r.tmuxExec("send-keys", "-t", name, "-l", "--", text)
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		// Never include stderr text: a partial send-keys failure could
		// surface operator text. The orchestrator may log arg-free.
		return fmt.Errorf("tmux send-keys failed: %w", err)
	}
	if enter {
		cmd2 := r.tmuxExec("send-keys", "-t", name, "Enter")
		cmd2.Stderr = &bytes.Buffer{}
		if err := cmd2.Run(); err != nil {
			return fmt.Errorf("tmux send Enter failed: %w", err)
		}
	}
	return nil
}

func (r *TmuxRunner) Kill(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("tmux session is required")
	}
	if err := r.tmuxRunOpaque("kill-session", "-t", name); err != nil {
		return fmt.Errorf("tmux kill-session failed: %s", opaqueOneLiner(err))
	}
	return nil
}

// opaqueOneLiner pulls a single-line summary out of an opaque tmux
// error. Used by callers that already wrap with their own sub-command
// context (e.g. "tmux new-session failed: <stderr>"). It strips any
// embedded newlines and surrounding whitespace, but never touches argv.
func opaqueOneLiner(err error) string {
	if err == nil {
		return ""
	}
	var oe *tmuxOpaqueErr
	if errors.As(err, &oe) {
		if oe.stderr != "" {
			// Use only the first line of stderr to keep messages
			// single-line and argv-leak-resistant.
			first := strings.SplitN(oe.stderr, "\n", 2)[0]
			return fmt.Sprintf("exit %d: %s", oe.exitCode, first)
		}
		return fmt.Sprintf("exit %d", oe.exitCode)
	}
	return err.Error()
}

func (r *TmuxRunner) AttachArgv(name string) []string {
	return []string{"tmux", "attach-session", "-t", strings.TrimSpace(name)}
}

var _ SessionRunner = (*TmuxRunner)(nil)
