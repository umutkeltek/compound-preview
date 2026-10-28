package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const consoleTestFormatValidCapability = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const consoleParentDeathControllerSubEnv = "COMPOUND_CONSOLE_PARENT_DEATH_CONTROLLER_SUB"

const consoleForwardTerminationFixtureScript = "#!/bin/sh\n" + testSubprocessShellParentGuard + `pid_tmp="${CONSOLE_PID_PATH}.tmp.$$"
printf '%s' "$$" > "$pid_tmp"
mv "$pid_tmp" "$CONSOLE_PID_PATH"
trap 'printf terminated > "$CONSOLE_TERM_MARKER"; exit 0' TERM INT
printf started > "$CONSOLE_STARTED_MARKER"
while test_fixture_parent_alive; do sleep 1; done
`

const consoleIgnoreTerminationFixtureScript = "#!/bin/sh\n" + testSubprocessShellParentGuard + `trap '' TERM INT HUP
tmp="${CONSOLE_PID_PATH}.tmp.$$"
printf '%s' "$$" > "$tmp"
mv "$tmp" "$CONSOLE_PID_PATH"
while test_fixture_parent_alive; do printf . >> "$CONSOLE_HEARTBEAT_PATH"; sleep 0.05; done
`

const consoleResidualChildFixtureScript = "#!/bin/sh\n" + testSubprocessShellParentGuard + `leader_tmp="${CONSOLE_LEADER_PID}.tmp.$$"
printf '%s' "$$" > "$leader_tmp"
mv "$leader_tmp" "$CONSOLE_LEADER_PID"
( trap '' TERM INT HUP; while test_fixture_parent_alive; do printf . >> "$CONSOLE_CHILD_HEARTBEAT"; sleep 0.05; done ) &
child=$!
child_tmp="${CONSOLE_CHILD_PID}.tmp.$$"
printf '%s' "$child" > "$child_tmp"
mv "$child_tmp" "$CONSOLE_CHILD_PID"
trap 'exit 0' TERM INT HUP
wait "$child"
`

func TestConsoleLifecycleDryRunsExposeDistinctDevBuildAndServePlans(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(root, ".compound")

	dev := runConsoleCommand(t, homePath, "dev", "--operator", "--console-dir", consoleDir, "--dry-run", "--json")
	build := runConsoleCommand(t, homePath, "build", "--operator", "--console-dir", consoleDir, "--dry-run", "--json")
	serve := runConsoleCommand(t, homePath, "serve", "--operator", "--console-dir", consoleDir, "--dry-run", "--json")

	if dev.Lifecycle != "dev" {
		t.Fatalf("dev lifecycle = %q, want dev", dev.Lifecycle)
	}
	if got, want := dev.Command, []string{"bun", "run", "next", "dev", "-H", "127.0.0.1", "-p", "3777"}; !sameStringSlice(got, want) {
		t.Fatalf("dev command = %v, want %v", got, want)
	}
	if got := dev.Env["COMPOUND_CONSOLE_BUILD_KIND"]; got != "development" {
		t.Fatalf("dev build kind = %q, want development", got)
	}

	if build.Lifecycle != "build" {
		t.Fatalf("build lifecycle = %q, want build", build.Lifecycle)
	}
	if got, want := build.Command, []string{"bun", "run", "next", "build"}; !sameStringSlice(got, want) {
		t.Fatalf("build command = %v, want %v", got, want)
	}
	if got := build.Env["COMPOUND_CONSOLE_BUILD_KIND"]; got != "production" {
		t.Fatalf("build kind = %q, want production", got)
	}

	if serve.Lifecycle != "serve" {
		t.Fatalf("serve lifecycle = %q, want serve", serve.Lifecycle)
	}
	if got, want := serve.Command, []string{"bun", "run", "next", "start", "-H", "127.0.0.1", "-p", "3777"}; !sameStringSlice(got, want) {
		t.Fatalf("serve command = %v, want %v", got, want)
	}
	if got := serve.Env["COMPOUND_CONSOLE_BUILD_KIND"]; got != "production" {
		t.Fatalf("serve build kind = %q, want production", got)
	}
}

func TestConsoleBuildReservesCanonicalPortBeforeBun(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:3777")
	if err == nil {
		defer listener.Close()
	}

	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "bun-ran")
	fakeBun := "#!/bin/sh\n: > \"$COMPOUND_CONSOLE_BUN_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "bun"), []byte(fakeBun), 0o755); err != nil {
		t.Fatal(err)
	}

	// Scope HOME/XDG_CACHE_HOME so the endpoint lifecycle lock is test-local:
	// without this the helper collides with a LIVE console's flock (os.UserCacheDir
	// is HOME-derived) and fails on the lock message before the port preflight
	// under test (observed intermittently red in full-battery runs 2026-07-16).
	_, stderr, runErr := invokeConsoleCommand(t, []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"COMPOUND_CONSOLE_BUN_MARKER=" + marker,
		"HOME=" + root,
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"COMPOUND_HOME=" + filepath.Join(root, ".compound"),
	}, "build", "--operator", "--console-dir", consoleDir)
	if runErr == nil {
		t.Fatal("console build succeeded with canonical port already owned")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("production build ran before port preflight; marker stat error = %v", err)
	}
	if !strings.Contains(stderr, "port 3777") || !strings.Contains(stderr, "status") {
		t.Fatalf("port preflight error must identify the canonical port and status action:\n%s", stderr)
	}
}

func TestConsoleLifecycleLockRejectsConcurrentWorktreesForTheSameEndpoint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	consoleDirA := filepath.Join(root, "worktree-a", "console")
	consoleDirB := filepath.Join(root, "worktree-b", "console")
	first, err := acquireConsoleLifecycleLock(consoleDirA)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseConsoleLifecycleLock(first)

	if second, err := acquireConsoleLifecycleLock(consoleDirB); err == nil || !strings.Contains(err.Error(), "already active") {
		releaseConsoleLifecycleLock(second)
		t.Fatalf("second worktree lifecycle lock error = %v, want endpoint-global already active", err)
	}

	releaseConsoleLifecycleLock(first)
	first = nil
	third, err := acquireConsoleLifecycleLock(consoleDirB)
	if err != nil {
		t.Fatalf("lock was not released with its descriptor: %v", err)
	}
	releaseConsoleLifecycleLock(third)
}

func TestConsoleServeRefusesMissingProductionBuildBeforeBun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "bun-ran")
	fakeBun := "#!/bin/sh\n: > \"$COMPOUND_CONSOLE_BUN_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "bun"), []byte(fakeBun), 0o755); err != nil {
		t.Fatal(err)
	}

	// Same lock scoping as the build preflight test: keep the lifecycle lock
	// test-local so a live console cannot preempt the missing-BUILD_ID check.
	_, stderr, runErr := invokeConsoleCommand(t, []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"COMPOUND_CONSOLE_BUN_MARKER=" + marker,
		"HOME=" + root,
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"COMPOUND_HOME=" + filepath.Join(root, ".compound"),
	}, "serve", "--operator", "--console-dir", consoleDir)
	if runErr == nil {
		t.Fatal("console serve succeeded without a production BUILD_ID")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Bun ran before production build validation; marker stat error = %v", err)
	}
	if !strings.Contains(stderr, "console build") || !strings.Contains(stderr, "BUILD_ID") {
		t.Fatalf("missing-build error must name the build action and artifact:\n%s", stderr)
	}
}

func TestConsoleLifecycleCanRequireAnExplicitHome(t *testing.T) {
	t.Parallel()
	consoleDir := filepath.Join(t.TempDir(), "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := invokeConsoleCommand(
		t,
		[]string{"COMPOUND_HOME="},
		"dev", "--operator", "--require-explicit-home", "--console-dir", consoleDir, "--dry-run",
	)
	if err == nil {
		t.Fatal("package lifecycle silently pinned the implicit canonical home")
	}
	if !strings.Contains(stderr, "explicit COMPOUND_HOME") {
		t.Fatalf("implicit-home refusal must explain the pin requirement:\n%s", stderr)
	}
}

func TestConsoleStatusUsesShallowReadinessDuringStalledRoot(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	expectedHash := consoleHomeShortHash(homePath)
	var rootHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			rootHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html>streamed shell")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case "/api/readiness":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"schema_version":"compound.console_readiness.v0.1","home_hash":%q,"explicit":true,"build_kind":"production","build_id":"synthetic-build","static_assets":["/_next/static/chunks/app.js"],"data_freshness":"not_checked"}`, expectedHash)
		case "/_next/static/chunks/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "consoleReady()")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	status := inspectConsoleRuntimeAt(strings.TrimPrefix(server.URL, "http://"), homePath, &http.Client{Timeout: 200 * time.Millisecond})
	if status.State != "healthy" || status.AssetsChecked != 1 || status.BuildKind != "production" {
		t.Fatalf("status = %+v, want healthy production with one checked asset", status)
	}
	if got := rootHits.Load(); got != 0 {
		t.Fatalf("status fetched heavy root %d times, want zero", got)
	}
}

func TestConsoleStatusRejectsInvalidShallowReadiness(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	hash := consoleHomeShortHash(homePath)
	valid := fmt.Sprintf(`{"schema_version":"compound.console_readiness.v0.1","home_hash":%q,"explicit":true,"build_kind":"production","build_id":"synthetic-build","static_assets":["/_next/static/chunks/app.js"],"data_freshness":"not_checked"}`, hash)
	for _, tc := range []struct {
		name       string
		body       string
		assetCode  int
		wantState  string
		wantDetail string
	}{
		{name: "foreign home", body: strings.Replace(valid, hash, "aaaaaaa", 1), wantState: "occupied_foreign"},
		{name: "unpinned", body: strings.Replace(valid, `"explicit":true`, `"explicit":false`, 1), wantState: "occupied_foreign"},
		{name: "missing build id", body: strings.Replace(valid, `"build_id":"synthetic-build"`, `"build_id":""`, 1), wantState: "unhealthy"},
		{name: "no assets", body: strings.Replace(valid, `"static_assets":["/_next/static/chunks/app.js"]`, `"static_assets":[]`, 1), wantState: "unhealthy"},
		{name: "foreign asset URL", body: strings.Replace(valid, `/_next/static/chunks/app.js`, `//example.invalid/app.js`, 1), wantState: "unhealthy"},
		{name: "broken asset", body: valid, assetCode: http.StatusNotFound, wantState: "unhealthy", wantDetail: "console static asset returned HTTP 404"},
		{name: "unsupported data claim", body: strings.Replace(valid, `"not_checked"`, `"live"`, 1), wantState: "unhealthy"},
		{name: "malformed", body: `{`, wantState: "unhealthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rootHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/readiness":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
				case "/_next/static/chunks/app.js":
					w.Header().Set("Content-Type", "application/javascript")
					if tc.assetCode != 0 {
						w.WriteHeader(tc.assetCode)
					}
					_, _ = io.WriteString(w, "consoleReady()")
				case "/":
					rootHits.Add(1)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			status := inspectConsoleRuntimeAt(strings.TrimPrefix(server.URL, "http://"), homePath, &http.Client{Timeout: time.Second})
			if status.State != tc.wantState || (tc.wantDetail != "" && status.Detail != tc.wantDetail) {
				t.Fatalf("status = %+v, want state %q detail %q", status, tc.wantState, tc.wantDetail)
			}
			if status.DataFreshness != "not_checked" || rootHits.Load() != 0 {
				t.Fatalf("status used a data/root shortcut: %+v root_hits=%d", status, rootHits.Load())
			}
		})
	}
}

func TestConsoleSupervisorForwardsTerminationToChildGroup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	marker := filepath.Join(root, "terminated")
	started := filepath.Join(root, "started")
	script := filepath.Join(root, "child.sh")
	if err := os.WriteFile(script, []byte(consoleForwardTerminationFixtureScript), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := consoleLaunchPlan{
		ConsoleDir: root,
		Env: map[string]string{
			"CONSOLE_PID_PATH":       pidPath,
			"CONSOLE_TERM_MARKER":    marker,
			"CONSOLE_STARTED_MARKER": started,
		},
	}
	addTestSubprocessParentEnv(plan.Env, os.Getpid())
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	returned := false
	var group *testProcessGroup
	var err error
	go func() {
		done <- runConsoleSupervisedCommand(plan, []string{script}, signals)
	}()
	defer func() {
		cleanupConsoleSupervisorTest(t, signals, done, group, pidPath, script, returned)
	}()
	leaderPID := waitForTestPIDFile(t, pidPath, 3*time.Second)
	group, err = captureLiveTestProcessGroupWithCommandToken(leaderPID, leaderPID, script)
	if err != nil {
		t.Fatal(err)
	}

	waitForTestPath(t, started, 3*time.Second)
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		returned = true
		if err != nil {
			t.Fatalf("supervisor returned after forwarded TERM: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not reap the terminated child")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "terminated" {
		t.Fatalf("child did not receive forwarded termination: body=%q err=%v", body, err)
	}
	waitForTestProcessGroupQuiescence(t, group, time.Second)
}

func TestConsoleSupervisorKillsChildGroupAfterTerminationGrace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	heartbeatPath := filepath.Join(root, "heartbeat")
	script := filepath.Join(root, "ignores-term.sh")
	if err := os.WriteFile(script, []byte(consoleIgnoreTerminationFixtureScript), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := consoleLaunchPlan{
		ConsoleDir: root,
		Env: map[string]string{
			"CONSOLE_PID_PATH":       pidPath,
			"CONSOLE_HEARTBEAT_PATH": heartbeatPath,
		},
	}
	addTestSubprocessParentEnv(plan.Env, os.Getpid())
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	returned := false
	var group *testProcessGroup
	var err error
	go func() {
		done <- runConsoleSupervisedCommand(plan, []string{script}, signals)
	}()
	defer func() {
		cleanupConsoleSupervisorTest(t, signals, done, group, pidPath, script, returned)
	}()

	leaderPID := waitForTestPIDFile(t, pidPath, 3*time.Second)
	group, err = captureLiveTestProcessGroupWithCommandToken(leaderPID, leaderPID, script)
	if err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, heartbeatPath, 3*time.Second)

	shutdownTimeout := 2*consoleShutdownGrace + time.Second
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		returned = true
		if err != nil {
			t.Fatalf("supervisor returned after forced shutdown: %v", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("supervisor waited forever for a child that ignored termination")
	}
	waitForTestProcessGroupQuiescence(t, group, time.Second)
}

func TestConsoleSupervisorKillsResidualGroupWhenLeaderExitsOnTermination(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	leaderPIDPath := filepath.Join(root, "leader.pid")
	childPIDPath := filepath.Join(root, "child.pid")
	heartbeatPath := filepath.Join(root, "child-heartbeat")
	script := filepath.Join(root, "leader-exits.sh")
	if err := os.WriteFile(script, []byte(consoleResidualChildFixtureScript), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := consoleLaunchPlan{
		ConsoleDir: root,
		Env: map[string]string{
			"CONSOLE_LEADER_PID":      leaderPIDPath,
			"CONSOLE_CHILD_PID":       childPIDPath,
			"CONSOLE_CHILD_HEARTBEAT": heartbeatPath,
		},
	}
	addTestSubprocessParentEnv(plan.Env, os.Getpid())
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	returned := false
	var group *testProcessGroup
	var err error
	go func() { done <- runConsoleSupervisedCommand(plan, []string{script}, signals) }()
	defer func() {
		cleanupConsoleSupervisorTest(t, signals, done, group, leaderPIDPath, script, returned)
	}()

	leaderPID := waitForTestPIDFile(t, leaderPIDPath, 3*time.Second)
	_ = waitForTestPIDFile(t, childPIDPath, 3*time.Second)
	group, err = captureLiveTestProcessGroupWithCommandToken(leaderPID, leaderPID, script)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := group.status(); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, heartbeatPath, 3*time.Second)
	shutdownTimeout := 2*consoleShutdownGrace + time.Second
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		returned = true
		if err != nil {
			t.Fatalf("supervisor returned after leader termination: %v", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("supervisor did not return after the leader exited")
	}
	waitForTestProcessGroupQuiescence(t, group, time.Second)
}

func TestConsoleSupervisorFixtureChildrenExitWhenControllerDies(t *testing.T) {
	if os.Getenv(consoleParentDeathControllerSubEnv) == "1" {
		runConsoleParentDeathFixtureController(
			t,
			os.Getenv("COMPOUND_CONSOLE_PARENT_DEATH_ROLE"),
			os.Getenv("COMPOUND_CONSOLE_PARENT_DEATH_ROOT"),
		)
		return
	}

	for _, tc := range []struct {
		name string
		role string
	}{
		{name: "forwarded termination fixture", role: "forward"},
		{name: "forced shutdown fixture", role: "ignore"},
		{name: "residual child fixture", role: "residual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			pidPath := filepath.Join(root, "leader.pid")
			readyPath := filepath.Join(root, "ready")
			childCommandToken := filepath.Join(root, tc.role+".sh")
			controller := exec.Command(os.Args[0], "-test.run=^TestConsoleSupervisorFixtureChildrenExitWhenControllerDies$")
			controller.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			controller.Env = append(
				os.Environ(),
				consoleParentDeathControllerSubEnv+"=1",
				"COMPOUND_CONSOLE_PARENT_DEATH_ROLE="+tc.role,
				"COMPOUND_CONSOLE_PARENT_DEATH_ROOT="+root,
			)
			if err := controller.Start(); err != nil {
				t.Fatal(err)
			}
			controllerReapStarted := false
			var controllerGroup *testProcessGroup
			var childGroup *testProcessGroup
			controllerPGID := controller.Process.Pid
			var err error
			controllerGroup, err = captureLiveTestProcessGroup(controllerPGID, controller.Process.Pid)
			if err != nil {
				killPinnedTestProcessGroupAndReap(t, controller, controllerPGID, time.Second)
				childGroup = captureTestProcessGroupFromPIDFileForCleanup(t, pidPath, childCommandToken)
				cleanupTestProcessGroup(t, childGroup, time.Second)
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if childGroup == nil {
					childGroup = captureTestProcessGroupFromPIDFileForCleanup(t, pidPath, childCommandToken)
				}
				cleanupTestProcessGroup(t, childGroup, time.Second)
				cleanupTestProcessGroup(t, controllerGroup, time.Second)
				if !controllerReapStarted {
					killAndReapTestCommand(t, controller, time.Second)
					controllerReapStarted = true
				}
				if childGroup == nil {
					childGroup = captureTestProcessGroupFromPIDFileForCleanup(t, pidPath, childCommandToken)
					cleanupTestProcessGroup(t, childGroup, time.Second)
				}
			})

			waitForTestPath(t, readyPath, 5*time.Second)
			childPGID := waitForTestPIDFile(t, pidPath, time.Second)
			actualPGID, err := syscall.Getpgid(childPGID)
			if err != nil {
				t.Fatalf("get supervised child process group: %v", err)
			}
			if actualPGID != childPGID {
				t.Fatalf("supervised child process group = %d, want leader %d", actualPGID, childPGID)
			}
			childGroup, err = captureLiveTestProcessGroupWithCommandToken(childPGID, childPGID, childCommandToken)
			if err != nil {
				t.Fatal(err)
			}

			killAndReapTestCommand(t, controller, time.Second)
			controllerReapStarted = true
			waitForTestProcessGroupQuiescence(t, childGroup, 2*time.Second)
			waitForTestProcessGroupQuiescence(t, controllerGroup, 2*time.Second)
			if tc.role != "forward" {
				assertTestFileSizeStable(t, filepath.Join(root, "heartbeat"), 150*time.Millisecond)
			}
		})
	}
}

func runConsoleParentDeathFixtureController(t *testing.T, role, root string) {
	t.Helper()
	pidPath := filepath.Join(root, "leader.pid")
	readyPath := filepath.Join(root, "ready")
	scriptPath := filepath.Join(root, role+".sh")
	plan := consoleLaunchPlan{ConsoleDir: root, Env: map[string]string{}}
	var body string
	var startedPath string
	switch role {
	case "forward":
		body = consoleForwardTerminationFixtureScript
		startedPath = filepath.Join(root, "started")
		plan.Env["CONSOLE_PID_PATH"] = pidPath
		plan.Env["CONSOLE_TERM_MARKER"] = filepath.Join(root, "terminated")
		plan.Env["CONSOLE_STARTED_MARKER"] = startedPath
	case "ignore":
		body = consoleIgnoreTerminationFixtureScript
		startedPath = filepath.Join(root, "heartbeat")
		plan.Env["CONSOLE_PID_PATH"] = pidPath
		plan.Env["CONSOLE_HEARTBEAT_PATH"] = startedPath
	case "residual":
		body = consoleResidualChildFixtureScript
		startedPath = filepath.Join(root, "heartbeat")
		plan.Env["CONSOLE_LEADER_PID"] = pidPath
		plan.Env["CONSOLE_CHILD_PID"] = filepath.Join(root, "child.pid")
		plan.Env["CONSOLE_CHILD_HEARTBEAT"] = startedPath
	default:
		t.Fatalf("unknown console parent-death fixture role %q", role)
	}
	addTestSubprocessParentEnv(plan.Env, os.Getpid())
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runConsoleSupervisedCommand(plan, []string{scriptPath}, make(chan os.Signal, 1))
	}()
	waitForTestPIDFile(t, pidPath, 3*time.Second)
	waitForTestPath(t, startedPath, 3*time.Second)
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("console parent-death fixture returned: %v", err)
	}
}

func cleanupConsoleSupervisorTest(
	t *testing.T,
	signals chan os.Signal,
	done <-chan error,
	group *testProcessGroup,
	pidPath string,
	commandToken string,
	returned bool,
) {
	t.Helper()
	if group == nil {
		group = captureTestProcessGroupFromPIDFileForCleanup(t, pidPath, commandToken)
	}
	if !returned {
		select {
		case signals <- syscall.SIGKILL:
		default:
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	if group == nil {
		group = captureTestProcessGroupFromPIDFileForCleanup(t, pidPath, commandToken)
	}
	cleanupTestProcessGroup(t, group, time.Second)
}

func captureTestProcessGroupFromPIDFileForCleanup(t *testing.T, pidPath, commandToken string) *testProcessGroup {
	t.Helper()
	pid, err := readTestPIDFile(pidPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Errorf("read test process group owner %s during cleanup: %v", pidPath, err)
		return nil
	}
	group, err := captureLiveTestProcessGroupWithCommandToken(pid, pid, commandToken)
	if errors.Is(err, errTestProcessGroupNotOwned) {
		return nil
	}
	if err != nil {
		t.Errorf("capture test process group %d during cleanup: %v", pid, err)
		return nil
	}
	return group
}

func waitForTestPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		pid, err := readTestPIDFile(path)
		if err == nil {
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read atomic pid file %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for atomic pid file %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readTestPIDFile(path string) (int, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}
	return pid, nil
}

func waitForTestPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertTestFileSizeStable(t *testing.T, path string, duration time.Duration) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(duration)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("test fixture heartbeat %s advanced after process-group quiescence: %d -> %d bytes", path, before.Size(), after.Size())
	}
}

func TestConsoleStatusReportsCanonicalHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	stdout, stderr, err := invokeConsoleCommand(t, nil, "status", "--json")
	if err != nil {
		t.Fatalf("compound console status --json failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	var status struct {
		SchemaVersion string `json:"schema_version"`
		State         string `json:"state"`
		Address       string `json:"address"`
		Port          int    `json:"port"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("parse console status: %v\n%s", err, stdout)
	}
	if status.SchemaVersion != consoleStatusSchemaVersion {
		t.Fatalf("status schema = %q", status.SchemaVersion)
	}
	if status.Address != "127.0.0.1:3777" || status.Port != 3777 {
		t.Fatalf("status endpoint = %s (port %d), want canonical 127.0.0.1:3777", status.Address, status.Port)
	}
	if status.State != "healthy" && status.State != "unhealthy" && status.State != "occupied_foreign" && status.State != "stopped" {
		t.Fatalf("status state = %q", status.State)
	}
}

func TestConsoleNextBuildDirectoriesAreLifecycleIsolated(t *testing.T) {
	t.Parallel()
	consoleDir, err := filepath.Abs(filepath.Join("..", "..", "console"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		want string
	}{
		{kind: "development", want: ".next/development"},
		{kind: "production", want: ".next/production"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if _, err := exec.LookPath("bun"); err != nil {
				t.Skip("bun is required to load the console next config (ci-console and host-bound runs cover bun-present paths)")
			}
			cmd := exec.Command("bun", "-e", `import config from "./next.config.ts"; console.log(config.distDir ?? "")`)
			cmd.Dir = consoleDir
			cmd.Env = append(os.Environ(), "COMPOUND_CONSOLE_BUILD_KIND="+tc.kind)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("load next config: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("distDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConsolePackageScriptsExposeOnlyOwnedLifecycles(t *testing.T) {
	t.Parallel()
	packagePath, err := filepath.Abs(filepath.Join("..", "..", "console", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		script string
		owner  string
	}{
		{script: "dev", owner: "console dev"},
		{script: "build", owner: "console build"},
		{script: "serve", owner: "console serve"},
	} {
		if got := pkg.Scripts[tc.script]; !strings.Contains(got, tc.owner) {
			t.Fatalf("%s script = %q, want CLI owner %q", tc.script, got, tc.owner)
		}
		if got := pkg.Scripts[tc.script]; !strings.Contains(got, "--require-explicit-home") {
			t.Fatalf("%s script = %q, want explicit-home guard", tc.script, got)
		}
		if got := pkg.Scripts[tc.script]; strings.Contains(got, "next ") {
			t.Fatalf("%s script bypasses the CLI lifecycle owner: %q", tc.script, got)
		}
	}
	for name := range pkg.Scripts {
		if strings.HasPrefix(name, "_next:") {
			t.Fatalf("package exposes unmanaged Next lifecycle script %q", name)
		}
	}
}

type consoleLifecyclePlanView struct {
	Lifecycle string            `json:"lifecycle"`
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env"`
}

func runConsoleCommand(t *testing.T, homePath string, args ...string) consoleLifecyclePlanView {
	t.Helper()
	stdout, stderr, err := invokeConsoleCommand(t, []string{"COMPOUND_HOME=" + homePath}, args...)
	if err != nil {
		t.Fatalf("compound console %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	var plan consoleLifecyclePlanView
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("parse console launch plan: %v\n%s", err, stdout)
	}
	return plan
}

func invokeConsoleCommand(t *testing.T, extraEnv []string, args ...string) (string, string, error) {
	t.Helper()
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleCommandHelper$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_CONSOLE_TEST_HELPER=1",
		"COMPOUND_CONSOLE_TEST_ARGS="+string(encodedArgs),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestConsoleCommandHelper(t *testing.T) {
	t.Parallel()
	if os.Getenv("COMPOUND_CONSOLE_TEST_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("COMPOUND_CONSOLE_TEST_ARGS")), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cmdConsole(args)
	os.Exit(0)
}

func TestConsoleCompoundBinPrefersStablePathForTemporaryExecutable(t *testing.T) {
	t.Parallel()
	stable := filepath.Join(t.TempDir(), "bin", "compound")
	got := resolveConsoleCompoundBin(filepath.Join(string(filepath.Separator), "tmp", "compound-dashboard"), func(name string) (string, error) {
		if name != "compound" {
			t.Fatalf("lookup name = %q", name)
		}
		return stable, nil
	})
	if got != stable {
		t.Fatalf("compound bin = %q, want %q", got, stable)
	}
}

func TestConsoleCompoundBinKeepsStableExecutable(t *testing.T) {
	t.Parallel()
	current := filepath.Join(string(filepath.Separator), "usr", "local", "bin", "compound")
	got := resolveConsoleCompoundBin(current, func(name string) (string, error) {
		t.Fatalf("stable executable should not need PATH lookup")
		return "", os.ErrNotExist
	})
	if got != current {
		t.Fatalf("compound bin = %q, want %q", got, current)
	}
}

func TestConsoleOperatorDryRunIsProjectionOnlyByDefault(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"scripts":{"dev":"next dev -p 3777"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(root, ".compound")
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdConsole([]string{"--operator", "--console-dir", consoleDir, "--dry-run", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("console dry-run missing JSON:\n%s", out)
	}
	var plan consoleLaunchPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse console launch plan: %v\n%s", err, out)
	}
	if plan.Mode != "operator" || !plan.DryRun || plan.MutationsEnabled || plan.PrivateFallbacks {
		t.Fatalf("operator launch flags = %+v", plan)
	}
	if got, want := plan.Command, []string{"bun", "run", "next", "dev", "-H", "127.0.0.1", "-p", "3777"}; !sameStringSlice(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	if plan.ConsoleDir != consoleDir {
		t.Fatalf("console_dir = %q, want %q", plan.ConsoleDir, consoleDir)
	}
	if plan.Env["COMPOUND_HOME"] != homePath ||
		plan.Env["COMPOUND_CONSOLE_MUTATIONS"] != "0" ||
		strings.TrimSpace(plan.Env["COMPOUND_BIN"]) == "" ||
		plan.Env["COMPOUND_CONSOLE_PRIVATE_FALLBACKS"] != "0" {
		t.Fatalf("env = %+v", plan.Env)
	}
	t.Setenv("COMPOUND_CONSOLE_MUTATIONS", "1")
	t.Setenv("COMPOUND_CONSOLE_PRIVATE_FALLBACKS", "1")
	t.Setenv("COMPOUND_CONSOLE_SESSION_TOKEN", "legacy-session-sentinel")
	t.Setenv("COMPOUND_CONSOLE_CAPABILITY", consoleTestFormatValidCapability)
	launched := consoleLaunchEnv(plan)
	for _, want := range []string{
		"COMPOUND_CONSOLE_MUTATIONS=0",
		"COMPOUND_CONSOLE_PRIVATE_FALLBACKS=0",
	} {
		if !containsString(launched, want) {
			key, _, _ := strings.Cut(want, "=")
			t.Fatalf("launch environment missing safe override for %s", key)
		}
	}
	for _, forbiddenKey := range []string{"COMPOUND_CONSOLE_SESSION_TOKEN", "COMPOUND_CONSOLE_CAPABILITY"} {
		for _, entry := range launched {
			if strings.HasPrefix(entry, forbiddenKey+"=") {
				t.Fatalf("projection-only launch inherited forbidden key %s", forbiddenKey)
			}
		}
	}
}

func TestConsoleBuildRejectsMutationExtensionAndStripsAmbientSecrets(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", filepath.Join(root, ".compound"))
	t.Setenv("COMPOUND_CONSOLE_MUTATIONS", "1")
	t.Setenv("COMPOUND_CONSOLE_PRIVATE_FALLBACKS", "1")
	t.Setenv("COMPOUND_CONSOLE_SESSION_TOKEN", "legacy-session-sentinel")
	t.Setenv("COMPOUND_CONSOLE_CAPABILITY", consoleTestFormatValidCapability)

	plan, err := buildConsoleLifecycleLaunchPlan(consoleLifecycleBuild, consoleDir, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range consoleLaunchEnv(plan) {
		for _, forbiddenKey := range []string{"COMPOUND_CONSOLE_SESSION_TOKEN", "COMPOUND_CONSOLE_CAPABILITY"} {
			if strings.HasPrefix(entry, forbiddenKey+"=") {
				t.Fatalf("build inherited forbidden key %s", forbiddenKey)
			}
		}
	}

	if mutationPlan, err := buildConsoleLifecycleLaunchPlan(consoleLifecycleBuild, consoleDir, true, false, true); err == nil {
		t.Fatalf("build accepted a runtime mutation extension: %+v", mutationPlan)
	}
}

func TestConsoleOperatorDryRunRequiresExplicitMutationOptIn(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"scripts":{"dev":"next dev -p 3777"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(root, ".compound")
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_CONSOLE_CAPABILITY", consoleTestFormatValidCapability)

	out := captureStdout(t, func() {
		cmdConsole([]string{"--operator", "--mutations", "--console-dir", consoleDir, "--dry-run", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("console dry-run missing JSON:\n%s", out)
	}
	var plan consoleLaunchPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse console launch plan: %v\n%s", err, out)
	}
	if !plan.MutationsEnabled || plan.Env["COMPOUND_CONSOLE_MUTATIONS"] != "1" {
		t.Fatalf("explicit mutation opt-in was not carried by launch plan: %+v", plan)
	}
	if !plan.CapabilityRequired {
		t.Fatal("mutation launch did not disclose the out-of-band capability requirement")
	}
	if _, exposed := plan.Env["COMPOUND_CONSOLE_CAPABILITY"]; exposed {
		t.Fatal("launch plan rendered the forbidden capability key")
	}
}

func TestConsoleMutationModeRequiresExplicitHomeAndFormatValidCapabilityBeforePlan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("implicit home", func(t *testing.T) {
		stdout, stderr, err := invokeConsoleCommand(
			t,
			[]string{
				"COMPOUND_HOME=",
				"COMPOUND_CONSOLE_CAPABILITY=" + consoleTestFormatValidCapability,
			},
			"dev", "--operator", "--mutations", "--console-dir", consoleDir, "--dry-run", "--json",
		)
		if err == nil {
			t.Fatalf("mutation launch accepted an implicit canonical home:\n%s", stdout)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("refused mutation launch emitted a plan:\n%s", stdout)
		}
		if !strings.Contains(stderr, "explicit COMPOUND_HOME") {
			t.Fatalf("implicit-home refusal did not explain the requirement:\n%s", stderr)
		}
	})

	for _, tc := range []struct {
		name       string
		capability string
	}{
		{name: "missing", capability: ""},
		{name: "short", capability: "abcd"},
		{name: "non hexadecimal", capability: strings.Repeat("z", 64)},
		{name: "too long", capability: consoleTestFormatValidCapability + "00"},
	} {
		t.Run(tc.name+" capability", func(t *testing.T) {
			stdout, stderr, err := invokeConsoleCommand(
				t,
				[]string{
					"COMPOUND_HOME=" + filepath.Join(root, ".compound"),
					"COMPOUND_CONSOLE_CAPABILITY=" + tc.capability,
				},
				"dev", "--operator", "--mutations", "--console-dir", consoleDir, "--dry-run", "--json",
			)
			if err == nil {
				t.Fatalf("mutation launch accepted malformed capability %q:\n%s", tc.name, stdout)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Fatalf("refused mutation launch emitted a plan:\n%s", stdout)
			}
			if !strings.Contains(stderr, "exactly 64 hexadecimal characters") {
				t.Fatalf("capability refusal did not explain the accepted encoding:\n%s", stderr)
			}
		})
	}
}

func TestConsoleExtensionPlanDisclosesCapabilityWithoutRenderingIt(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", filepath.Join(root, ".compound"))
	capability := consoleTestFormatValidCapability
	t.Setenv("COMPOUND_CONSOLE_CAPABILITY", capability)

	plan, err := buildConsoleLifecycleLaunchPlan(consoleLifecycleDev, consoleDir, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CapabilityRequired {
		t.Fatal("extension plan must disclose capability requirement")
	}
	if _, exposed := plan.Env["COMPOUND_CONSOLE_CAPABILITY"]; exposed {
		t.Fatal("launch JSON env contains the forbidden capability key")
	}
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(capability)) {
		t.Fatal("launch JSON exposed out-of-band capability material")
	}
	if !containsString(consoleLaunchEnv(plan), "COMPOUND_CONSOLE_CAPABILITY="+capability) {
		t.Fatal("console child process did not inherit the out-of-band capability")
	}
}

func TestConsoleOperatorDryRunSurfacesPinnedHomeIdentity(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"scripts":{"dev":"next dev -p 3777"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(root, ".compound")
	t.Setenv("COMPOUND_HOME", homePath)

	// Dry-run WITHOUT --json prints the human launch line: it must name the exact
	// home this instance is pinned to so an operator driving several ports can
	// tell which instance a click lands on. A regression that drops the home from
	// the line reintroduces the "which instance am I on?" blind spot the
	// 2026-07-11 breach exploited.
	out := captureStdout(t, func() {
		cmdConsole([]string{"--operator", "--console-dir", consoleDir, "--dry-run"})
	})
	wantHash := consoleHomeShortHash(homePath)
	for _, want := range []string{"home ", ".compound", "#" + wantHash, homePath} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run launch line missing %q:\n%s", want, out)
		}
	}
	if len(wantHash) != 7 {
		t.Fatalf("home short hash = %q, want 7 hex chars", wantHash)
	}
}

func TestConsoleLaunchPlanPinsResolvedHome(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"scripts":{"dev":"next dev -p 3777"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	scratchA := filepath.Join(root, "scratch-a", ".compound")
	t.Setenv("COMPOUND_HOME", scratchA)

	plan, err := buildConsoleLaunchPlan(consoleDir, false, true)
	if err != nil {
		t.Fatal(err)
	}
	// The child process is pinned to the started home by construction: the launch
	// env carries the resolved home, and consoleLaunchEnv injects exactly that —
	// even overriding a DIFFERENT COMPOUND_HOME that happened to be in the parent
	// environment. This is what stops a clone from inheriting/writing another home.
	if plan.Env["COMPOUND_HOME"] != scratchA {
		t.Fatalf("plan home = %q, want pinned %q", plan.Env["COMPOUND_HOME"], scratchA)
	}
	if !filepath.IsAbs(plan.Env["COMPOUND_HOME"]) {
		t.Fatalf("pinned home must be absolute, got %q", plan.Env["COMPOUND_HOME"])
	}
	t.Setenv("COMPOUND_HOME", filepath.Join(root, "scratch-b", ".compound"))
	env := consoleLaunchEnv(plan)
	var got string
	for _, entry := range env {
		if strings.HasPrefix(entry, "COMPOUND_HOME=") {
			got = strings.TrimPrefix(entry, "COMPOUND_HOME=")
		}
	}
	if got != scratchA {
		t.Fatalf("launch env COMPOUND_HOME = %q, want %q (parent env must not leak a different home)", got, scratchA)
	}
}

func TestConsoleOperatorDryRunRefusesPrivateFallbacks(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "package.json"), []byte(`{"scripts":{"dev":"next dev -p 3777"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", filepath.Join(root, ".compound"))

	plan, err := buildConsoleLaunchPlan(consoleDir, true, true)
	if err == nil || !strings.Contains(err.Error(), "private fallbacks are unavailable") {
		t.Fatalf("private fallback plan = %+v, error = %v; want enforced deferral", plan, err)
	}
}
