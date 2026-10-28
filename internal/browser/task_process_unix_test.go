//go:build darwin || linux

package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestJevReaperFailsClosedAndUsesOwnedSignalTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		psFails    bool
		groupFails bool
		termFails  bool
		killFails  bool
		survives   bool
		nonLeader  bool
		wantError  bool
	}{
		{"ps_unavailable", true, false, false, false, false, false, true},
		{"group_inspection_error", false, true, false, false, false, false, true},
		{"term_error", false, false, true, false, false, false, true},
		{"force_kill_error", false, false, false, true, true, false, true},
		{"survivor", false, false, false, false, true, false, true},
		{"nonleader_exact_pid", false, false, false, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "owned.pid")
			if err := os.WriteFile(pidPath, []byte("4321"), 0o600); err != nil {
				t.Fatal(err)
			}
			alive := true
			var signalled []int
			ops := jevReaperOps{
				ps: func(_ int, field string) ([]byte, error) {
					if tc.psFails {
						return nil, errors.New("ps unavailable")
					}
					if field == "stat" {
						return []byte("S"), nil
					}
					return []byte("browser_harness.daemon --owned"), nil
				},
				groupInspect: func(int, string) (bool, bool, error) {
					if tc.groupFails {
						return false, false, errors.New("group inspection unavailable")
					}
					return alive, alive, nil
				},
				kill: func(target int, signal syscall.Signal) error {
					if signal == 0 {
						if alive {
							return nil
						}
						return syscall.ESRCH
					}
					signalled = append(signalled, target)
					if signal == syscall.SIGTERM && tc.termFails {
						return syscall.EPERM
					}
					if signal == syscall.SIGKILL && tc.killFails {
						return syscall.EPERM
					}
					if !tc.survives {
						alive = false
					}
					return nil
				},
				pgid: func(int) (int, error) {
					if tc.nonLeader {
						return 123, nil
					}
					return 4321, nil
				},
				sleep: func(time.Duration) {},
			}
			err := reapJevPIDWith(pidPath, "browser_harness.daemon", ops)
			if (err != nil) != tc.wantError {
				t.Fatalf("reaper error = %v, wantError=%t", err, tc.wantError)
			}
			if tc.nonLeader && (len(signalled) != 1 || signalled[0] != 4321) {
				t.Fatalf("nonleader signalled broad group: %v", signalled)
			}
			if tc.psFails && len(signalled) != 0 {
				t.Fatalf("unverified process was signalled: %v", signalled)
			}
		})
	}
}

func TestJevReaperRechecksZombieLeaderBeforeForceStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		matches    bool
		inspectErr bool
		stateErr   bool
		wantError  bool
	}{
		{"zombie with owned child", "Z", true, false, false, false},
		{"zombie with foreign child", "Z", false, false, false, true},
		{"zombie inspection unavailable", "Z", true, true, false, true},
		{"leader state unavailable", "Z", true, false, true, true},
		{"live foreign leader", "S", true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "owned.pid")
			if err := os.WriteFile(pidPath, []byte("4321"), 0o600); err != nil {
				t.Fatal(err)
			}
			terminated, alive, forceStops := false, true, 0
			ops := jevReaperOps{
				ps: func(_ int, field string) ([]byte, error) {
					if field == "stat" {
						if terminated && tc.stateErr {
							return nil, errors.New("state inspection unavailable")
						}
						if terminated {
							return []byte(tc.state), nil
						}
						return []byte("S"), nil
					}
					if terminated {
						return []byte("[process] <defunct>"), nil
					}
					return []byte("synthetic-owned-marker"), nil
				},
				groupInspect: func(_ int, marker string) (bool, bool, error) {
					if marker != "" && tc.inspectErr {
						return false, false, errors.New("group inspection unavailable")
					}
					return alive, alive && tc.matches, nil
				},
				kill: func(target int, signal syscall.Signal) error {
					if target != -4321 {
						t.Fatalf("signal target = %d, want owned group -4321", target)
					}
					if signal == syscall.SIGTERM {
						terminated = true
					} else if signal == syscall.SIGKILL {
						forceStops++
						alive = false
					}
					return nil
				},
				pgid:  func(int) (int, error) { return 4321, nil },
				sleep: func(time.Duration) {},
			}
			err := reapJevPIDWith(pidPath, "synthetic-owned-marker", ops)
			if (err != nil) != tc.wantError {
				t.Fatalf("reaper error = %v, wantError=%t", err, tc.wantError)
			}
			if tc.wantError && forceStops != 0 {
				t.Fatalf("unverified group was force stopped: %d", forceStops)
			}
			if !tc.wantError && (forceStops != 1 || alive) {
				t.Fatalf("owned child not stopped: forceStops=%d alive=%t", forceStops, alive)
			}
		})
	}
}

func TestJevReaperProcessHelper(t *testing.T) {
	mode := os.Getenv("COMPOUND_TEST_REAPER_MODE")
	if mode == "" {
		return
	}
	if mode == "child" {
		signal.Ignore(syscall.SIGTERM)
		marker := os.Getenv("COMPOUND_TEST_REAPER_MARKER")
		// Existence is the readiness signal; publish only a complete PID.
		if err := os.WriteFile(marker+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		if err := os.Rename(marker+".tmp", marker); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestJevReaperProcessHelper$", "--", os.Getenv("COMPOUND_TEST_REAPER_GROUP_MARKER"))
	child.Env = append(os.Environ(), "COMPOUND_TEST_REAPER_MODE=child")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM)
	<-stopped
	os.Exit(0)
}

func TestJevReaperStopsOwnedGroupAfterLeaderExits(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJevReaperProcessHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_REAPER_MODE=leader", "COMPOUND_TEST_REAPER_MARKER="+marker)
	cmd.Env = append(cmd.Env, "COMPOUND_TEST_REAPER_GROUP_MARKER=TestJevReaperProcessHelper")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			childPID, err = strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidPath := filepath.Join(t.TempDir(), "owned.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reapJevPID(pidPath, "TestJevReaperProcessHelper"); err != nil {
		t.Fatal(err)
	}
	if alive, err := jevLiveGroupMembers(cmd.Process.Pid); err != nil || alive {
		t.Fatalf("owned process group survived: alive=%t err=%v", alive, err)
	}
	if err := syscall.Kill(childPID, 0); err == nil {
		state, stateErr := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "stat=").Output()
		if stateErr != nil || !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			t.Fatalf("child remained live after cleanup: state=%q err=%v", state, stateErr)
		}
	}
}

func TestJevReaperStopsBoundGroupWhenLeaderAlreadyExited(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	groupMarker := "--user-data-dir=" + filepath.Join(t.TempDir(), "chrome")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJevReaperProcessHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_REAPER_MODE=leader", "COMPOUND_TEST_REAPER_MARKER="+marker, "COMPOUND_TEST_REAPER_GROUP_MARKER="+groupMarker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(t.TempDir(), "chrome.pid")
	binding, err := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "pgid": cmd.Process.Pid, "marker": groupMarker})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, binding, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reapJevPID(pidPath, groupMarker); err != nil {
		t.Fatal(err)
	}
	if alive, err := jevLiveGroupMembers(cmd.Process.Pid); err != nil || alive {
		t.Fatalf("pre-exited leader's group survived: alive=%t err=%v", alive, err)
	}
}

func TestJevReaperRefusesAbsentUnboundOrUninspectableGroup(t *testing.T) {
	for _, tc := range []struct {
		name, binding string
		inspectErr    bool
	}{
		{"plain_pid", "4321", false},
		{"wrong_marker", `{"pid":4321,"pgid":4321,"marker":"foreign"}`, false},
		{"inspection_error", `{"pid":4321,"pgid":4321,"marker":"owned"}`, true},
		{"unmatched_member", `{"pid":4321,"pgid":4321,"marker":"owned"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "chrome.pid")
			if err := os.WriteFile(pidPath, []byte(tc.binding), 0o600); err != nil {
				t.Fatal(err)
			}
			signals := 0
			ops := jevReaperOps{
				ps: func(int, string) ([]byte, error) { return nil, syscall.ESRCH },
				groupInspect: func(int, string) (bool, bool, error) {
					if tc.inspectErr {
						return false, false, errors.New("inspection failed")
					}
					return true, false, nil
				},
				kill: func(int, syscall.Signal) error { signals++; return nil },
			}
			if err := reapJevPIDWith(pidPath, "owned", ops); err == nil || signals != 0 {
				t.Fatalf("unverified group signalled: err=%v signals=%d", err, signals)
			}
		})
	}
}

// The handoff test below substitutes chrome_binary. This pins the other half of
// that seam: without a binary the bridge refuses before any spawn, and the real
// lookup answers an existing file or None on this host, never a guessed path.
func TestJevIsolatedChromeRefusesWithoutBinaryBeforeSpawn(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	source, err := filepath.Abs("jev_bridge.py")
	if err != nil {
		t.Fatal(err)
	}
	const script = `import pathlib, runpy, sys
source = sys.argv[1]
isolated_chrome = runpy.run_path(source)["isolated_chrome"]
scope = isolated_chrome.__wrapped__.__globals__
scope["chrome_binary"] = lambda: None
spawned = []
def refuse_spawn(*args, **_kwargs):
    spawned.append(args)
    raise AssertionError("spawned without a Chrome binary")
scope["subprocess"].Popen = refuse_spawn
try:
    with isolated_chrome("https://example.com/"):
        raise AssertionError("entered without a Chrome binary")
except RuntimeError as exc:
    assert str(exc) == "isolated Chrome unavailable", exc
else:
    raise AssertionError("missing Chrome was ignored")
assert not spawned, spawned
found = runpy.run_path(source)["chrome_binary"]()
assert found is None or pathlib.Path(found).is_file(), found
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, python, "-c", script, source).CombinedOutput(); err != nil {
		t.Fatalf("missing-Chrome refusal failed: %v: %s", err, output)
	}
}

// jevSpawnHandoffScript drives isolated_chrome through its Chrome ownership
// handoff with a synthetic detached child in place of Chrome. "signal" delivers
// SIGTERM inside Popen, before its return value is assigned; "write_failure"
// fails the chrome.pid sidecar write after the spawn. Either way the bridge must
// reap the child it spawned. The script exits non-zero only when the bridge
// raised something other than the expected interruption.
const jevSpawnHandoffScript = `import os, pathlib, runpy, signal, subprocess, sys, time
source, mode, session_dir = sys.argv[1:]
session = pathlib.Path(session_dir)
os.environ["COMPOUND_JEV_SESSION_DIR"] = session_dir
isolated_chrome = runpy.run_path(source)["isolated_chrome"]
scope = isolated_chrome.__wrapped__.__globals__
# Substitute the one host lookup, not shutil.which: on darwin the bridge uses a
# fixed app path, so a PATH patch never reached the handoff on a Mac without
# Chrome and the test failed before it could prove anything about reaping.
scope["chrome_binary"] = lambda: sys.executable
real_popen = subprocess.Popen
def spawn_then_interrupt(*_args, **_kwargs):
    child = real_popen([sys.executable, "-c", "import time; time.sleep(60)"],
                       start_new_session=True, stdin=subprocess.DEVNULL,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    (session / "synthetic-child.pid").write_text(str(child.pid))
    if mode == "signal":
        os.kill(os.getpid(), signal.SIGTERM)
    return child
scope["subprocess"].Popen = spawn_then_interrupt
if mode == "write_failure":
    original_write = pathlib.Path.write_text
    def fail_sidecar(path, *args, **kwargs):
        if path.name == "chrome.pid":
            raise OSError("synthetic sidecar write failure")
        return original_write(path, *args, **kwargs)
    pathlib.Path.write_text = fail_sidecar
signal.signal(signal.SIGTERM, lambda _signal, _frame: (_ for _ in ()).throw(SystemExit(143)))
try:
    with isolated_chrome("https://example.com/"):
        raise AssertionError("synthetic Chrome cannot become ready")
except BaseException as exc:
    if mode == "signal" and not isinstance(exc, SystemExit):
        raise
    if mode == "write_failure" and not isinstance(exc, OSError):
        raise
else:
    raise AssertionError("handoff interruption was ignored")
`

// runJevSpawnHandoff runs jevSpawnHandoffScript against the given bridge source
// and reports whether the synthetic child's process group outlived the handoff.
// Any survivor is SIGKILLed at cleanup.
func runJevSpawnHandoff(t *testing.T, python, source, mode string) bool {
	t.Helper()
	sessionDir, err := newJevSession()
	if err != nil {
		t.Fatal(err)
	}
	var childPID int
	t.Cleanup(func() {
		if childPID > 1 {
			_ = syscall.Kill(-childPID, syscall.SIGKILL)
		}
		_ = os.RemoveAll(sessionDir)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", jevSpawnHandoffScript, source, mode, sessionDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic Chrome handoff failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(sessionDir, "synthetic-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err = strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	alive, err := jevLiveGroupMembers(childPID)
	if err != nil {
		t.Fatalf("inspect synthetic child group after %s handoff: %v", mode, err)
	}
	return alive
}

func TestJevChromeSpawnHandoffAlwaysReapsDetachedChild(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	source, err := filepath.Abs("jev_bridge.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"signal", "write_failure"} {
		t.Run(mode, func(t *testing.T) {
			if runJevSpawnHandoff(t, python, source, mode) {
				t.Fatalf("detached child survived %s handoff", mode)
			}
		})
	}
}

// Falsifier for the harness above: run it against bridge copies with the reap
// or the SIGTERM deferral cut out, and require it to report the survivor. A
// harness that cannot fail would turn the reap test into a green no-op, which
// is the failure mode a missing Chrome used to hide.
func TestJevChromeSpawnHandoffHarnessDetectsUnreapedChild(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	original, err := os.ReadFile("jev_bridge.py")
	if err != nil {
		t.Fatal(err)
	}
	const reap = "if chrome is not None and chrome.poll() is None:"
	const deferral = "signal.signal(signal.SIGTERM, defer_term)"
	for _, tc := range []struct{ name, mode, anchor, replacement string }{
		{"reap_removed/signal", "signal", reap, "if False:"},
		{"reap_removed/write_failure", "write_failure", reap, "if False:"},
		{"deferral_removed/signal", "signal", deferral, "pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n := strings.Count(string(original), tc.anchor); n != 1 {
				t.Fatalf("mutation anchor %q appears %d times in jev_bridge.py, want 1; move this falsifier with the handoff code", tc.anchor, n)
			}
			mutated := filepath.Join(t.TempDir(), "jev_bridge.py")
			if err := os.WriteFile(mutated, []byte(strings.Replace(string(original), tc.anchor, tc.replacement, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			if !runJevSpawnHandoff(t, python, mutated, tc.mode) {
				t.Fatalf("harness passed a bridge with %s: it no longer sees an unreaped child", tc.name)
			}
		})
	}
}

// Opt-in because this uses real Chrome and resolves the pinned upstream uv
// dependency. It proves a long-running browser/daemon is reaped on timeout,
// rather than relying only on the instant synthetic stdio helper.
func TestJevRealBrowserCancellationCleansChrome(t *testing.T) {
	if os.Getenv("COMPOUND_JEV_REAL_PROCESS_TEST") != "1" {
		t.Skip("set COMPOUND_JEV_REAL_PROCESS_TEST=1 for real browser timeout proof")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv unavailable")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessionDir, err := newJevSession()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupJevSession(sessionDir)
	script := `import os, runpy, signal, time
signal.signal(signal.SIGTERM, lambda _sig, _frame: (_ for _ in ()).throw(SystemExit(143)))
ns = runpy.run_path("jev_bridge.py")
with ns["isolated_chrome"]("https://example.com/") as chrome_pid:
    from jev_ultrafast.browser import Browser
    browser = Browser("https://example.com/")
    browser.observe(screenshot=False)
    print("READY:" + str(chrome_pid) + ":" + os.environ["BH_HOME"], flush=True)
    time.sleep(60)
`
	cmd := exec.CommandContext(ctx, "uv", "run", "--no-project", "--with", "git+https://github.com/browser-use/jev-ultrafast.git@"+jevRevision, "python", "-c", script)
	configureJevProcess(cmd)
	cmd.Env = append(os.Environ(), "COMPOUND_JEV_SESSION_DIR="+sessionDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil // no page content or provider credentials are supplied
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
	}()
	var line string
	select {
	case line = <-ready:
	case <-time.After(45 * time.Second):
		cancel()
		_ = cmd.Wait()
		t.Fatal("real browser did not reach ready state")
	}
	parts := strings.SplitN(line, ":", 3)
	if len(parts) != 3 || parts[0] != "READY" {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("real browser start failed: %q", line)
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled process returned success")
	}
	var daemonPID int
	if entries, err := os.ReadDir(filepath.Join(sessionDir, "runtime")); err == nil {
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".pid") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sessionDir, "runtime", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				PID int `json:"pid"`
			}
			if json.Unmarshal(data, &record) == nil {
				daemonPID = record.PID
			} else {
				daemonPID, _ = strconv.Atoi(strings.Fields(string(data))[0])
			}
		}
	}
	if err := cleanupJevSession(sessionDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parts[2]); !os.IsNotExist(err) {
		t.Fatalf("browser temp state survived cancellation: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned Chrome pid %d survived cancellation: %v", pid, err)
	}
	if daemonPID > 0 {
		if err := syscall.Kill(daemonPID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("owned Browser Harness daemon %d survived cancellation: %v", daemonPID, err)
		}
	}
}
