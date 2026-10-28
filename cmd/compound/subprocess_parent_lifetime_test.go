package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testSubprocessParentPIDEnv = "COMPOUND_TEST_CONTROLLER_PID"
const testSubprocessHardExpiryUnixEnv = "COMPOUND_TEST_HARD_EXPIRY_UNIX"
const testSubprocessShellParentGuard = `test_fixture_validate_parent() {
	[ -n "${COMPOUND_TEST_CONTROLLER_PID:-}" ] || exit 97
	[ "$PPID" -eq "$COMPOUND_TEST_CONTROLLER_PID" ] || exit 97
	case "${COMPOUND_TEST_HARD_EXPIRY_UNIX:-}" in
		''|*[!0-9]*) exit 97 ;;
	esac
	[ "$COMPOUND_TEST_HARD_EXPIRY_UNIX" -gt 0 ] || exit 97
}
test_fixture_parent_alive() {
	test_fixture_now_unix=$(date +%s) || return 1
	case "$test_fixture_now_unix" in
		''|*[!0-9]*) return 1 ;;
	esac
	[ "$test_fixture_now_unix" -lt "$COMPOUND_TEST_HARD_EXPIRY_UNIX" ] || return 1
	kill -0 "$COMPOUND_TEST_CONTROLLER_PID" 2>/dev/null
}
test_fixture_validate_parent
`

type testSubprocessParentLifetime struct {
	parentPID int
	deadline  time.Time
}

type testProcessState uint8

const (
	testProcessStateRunning testProcessState = iota + 1
	testProcessStateZombie
)

type testProcessSnapshot struct {
	pid     int
	pgid    int
	birthID string
	state   testProcessState
}

type testProcessIdentity struct {
	pid     int
	birthID string
}

type testProcessSnapshotReader func() ([]testProcessSnapshot, error)
type testProcessGroupSignaler func(int, syscall.Signal) error
type testProcessCommandReader func(int, int) (string, error)
type testProcessCommandOutput func(context.Context, string, ...string) ([]byte, error)

var errTestProcessGroupNotOwned = errors.New("test process group is not owned")

const testProcessCommandInspectionTimeout = time.Second

type testProcessGroup struct {
	pgid     int
	members  map[testProcessIdentity]struct{}
	read     testProcessSnapshotReader
	signal   testProcessGroupSignaler
	finished bool
	signaled bool
}

func captureLiveTestProcessGroup(pgid, ownerPID int) (*testProcessGroup, error) {
	return captureTestProcessGroup(pgid, ownerPID, readTestProcessSnapshots, signalTestProcessGroup)
}

func captureLiveTestProcessGroupWithCommandToken(pgid, ownerPID int, commandToken string) (*testProcessGroup, error) {
	return captureTestProcessGroupWithCommandToken(
		pgid,
		ownerPID,
		commandToken,
		readTestProcessSnapshots,
		signalTestProcessGroup,
		readTestProcessCommand,
	)
}

func captureTestProcessGroupWithCommandToken(
	pgid int,
	ownerPID int,
	commandToken string,
	read testProcessSnapshotReader,
	signal testProcessGroupSignaler,
	readCommand testProcessCommandReader,
) (*testProcessGroup, error) {
	if strings.TrimSpace(commandToken) == "" {
		return nil, fmt.Errorf("capture test process group %d: command token is required", pgid)
	}
	if readCommand == nil {
		return nil, fmt.Errorf("capture test process group %d: command reader is required", pgid)
	}
	group, err := captureTestProcessGroup(pgid, ownerPID, read, signal)
	if err != nil {
		return nil, err
	}
	command, err := readCommand(ownerPID, pgid)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(command, commandToken) {
		return nil, fmt.Errorf("%w: process %d command does not contain %q", errTestProcessGroupNotOwned, ownerPID, commandToken)
	}
	return group, nil
}

func captureTestProcessGroup(
	pgid int,
	ownerPID int,
	read testProcessSnapshotReader,
	signal testProcessGroupSignaler,
) (*testProcessGroup, error) {
	if pgid <= 0 {
		return nil, fmt.Errorf("capture test process group: invalid pgid %d", pgid)
	}
	if ownerPID <= 0 {
		return nil, fmt.Errorf("capture test process group %d: invalid owner pid %d", pgid, ownerPID)
	}
	if read == nil || signal == nil {
		return nil, fmt.Errorf("capture test process group %d: process reader and signaler are required", pgid)
	}
	processes, err := read()
	if err != nil {
		return nil, fmt.Errorf("capture test process group %d: %w", pgid, err)
	}
	members := make(map[testProcessIdentity]struct{})
	live := false
	ownerLive := false
	for _, process := range processes {
		if process.pgid != pgid {
			continue
		}
		members[process.identity()] = struct{}{}
		if process.state != testProcessStateZombie {
			live = true
			if process.pid == ownerPID {
				ownerLive = true
			}
		}
	}
	if len(members) == 0 || !live || !ownerLive {
		return nil, fmt.Errorf("%w: group %d has no live owner pid %d", errTestProcessGroupNotOwned, pgid, ownerPID)
	}
	return &testProcessGroup{pgid: pgid, members: members, read: read, signal: signal}, nil
}

func readTestProcessCommand(pid, pgid int) (string, error) {
	return readTestProcessCommandWithOutput(
		pid,
		pgid,
		testProcessCommandInspectionTimeout,
		func(ctx context.Context, name string, args ...string) ([]byte, error) {
			command := exec.CommandContext(ctx, name, args...)
			command.Env = append(os.Environ(), "LC_ALL=C")
			return command.Output()
		},
	)
}

func readTestProcessCommandWithOutput(
	pid int,
	pgid int,
	timeout time.Duration,
	outputCommand testProcessCommandOutput,
) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("inspect test process %d command: timeout must be positive", pid)
	}
	if outputCommand == nil {
		return "", fmt.Errorf("inspect test process %d command: output command is required", pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := outputCommand(
		ctx,
		"/bin/ps",
		"-ww",
		"-p",
		strconv.Itoa(pid),
		"-o",
		"pid=",
		"-o",
		"pgid=",
		"-o",
		"command=",
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("inspect test process %d command: %w", pid, ctxErr)
		}
		if probeErr := syscall.Kill(pid, 0); probeErr == syscall.ESRCH {
			return "", fmt.Errorf("%w: process %d exited before command verification", errTestProcessGroupNotOwned, pid)
		}
		return "", fmt.Errorf("inspect test process %d command: %w", pid, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 3 {
		return "", fmt.Errorf("inspect test process %d command: malformed ps row %q", pid, output)
	}
	gotPID, pidErr := strconv.Atoi(fields[0])
	gotPGID, pgidErr := strconv.Atoi(fields[1])
	if pidErr != nil || pgidErr != nil || gotPID != pid || gotPGID != pgid {
		return "", fmt.Errorf("%w: ps identity pid=%q pgid=%q, want %d/%d", errTestProcessGroupNotOwned, fields[0], fields[1], pid, pgid)
	}
	return strings.Join(fields[2:], " "), nil
}

func (process testProcessSnapshot) identity() testProcessIdentity {
	return testProcessIdentity{pid: process.pid, birthID: process.birthID}
}

func (group *testProcessGroup) status() (owned bool, live bool, err error) {
	if group == nil {
		return false, false, fmt.Errorf("test process group is required")
	}
	if group.finished {
		return false, false, nil
	}
	processes, err := group.read()
	if err != nil {
		return false, false, fmt.Errorf("inspect test process group %d: %w", group.pgid, err)
	}
	groupPresent := false
	for _, process := range processes {
		if process.pgid != group.pgid {
			continue
		}
		groupPresent = true
		if process.state != testProcessStateZombie {
			live = true
		}
		if _, ok := group.members[process.identity()]; ok {
			owned = true
		}
	}
	if !groupPresent || !live {
		group.finished = true
		return owned, false, nil
	}
	if owned {
		for _, process := range processes {
			if process.pgid == group.pgid {
				group.members[process.identity()] = struct{}{}
			}
		}
	}
	// A live group with no surviving known identity is deliberately not
	// adopted or signaled. It may be an original short-lived descendant we
	// did not observe, or it may be a numerically reused PGID; waiting is safe
	// in both cases, while signaling is not.
	return owned, true, nil
}

func (group *testProcessGroup) signalIfOwned(sig syscall.Signal) (bool, error) {
	if group == nil {
		return false, fmt.Errorf("test process group is required")
	}
	if group.signaled || group.finished {
		return false, nil
	}
	owned, live, err := group.status()
	if err != nil {
		return false, err
	}
	if !owned || !live {
		if !live {
			group.finished = true
		}
		return false, nil
	}
	group.signaled = true
	if err := group.signal(group.pgid, sig); err != nil {
		return false, fmt.Errorf("signal owned test process group %d: %w", group.pgid, err)
	}
	return true, nil
}

func (group *testProcessGroup) waitForQuiescence(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("wait for test process group %d: timeout must be positive", group.pgid)
	}
	deadline := time.Now().Add(timeout)
	for {
		_, live, err := group.status()
		if err != nil {
			return err
		}
		if !live {
			group.finished = true
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("test process group %d still has live members after %s", group.pgid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func signalTestProcessGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("signal test process group: invalid pgid %d", pgid)
	}
	err := syscall.Kill(-pgid, sig)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func cleanupTestProcessGroup(t *testing.T, group *testProcessGroup, timeout time.Duration) {
	t.Helper()
	if group == nil {
		return
	}
	if _, err := group.signalIfOwned(syscall.SIGKILL); err != nil {
		t.Errorf("clean test process group %d: %v", group.pgid, err)
		return
	}
	if err := group.waitForQuiescence(timeout); err != nil {
		t.Errorf("wait for test process group %d cleanup: %v", group.pgid, err)
	}
}

func waitForTestCommandReap(done <-chan error, timeout time.Duration) error {
	if done == nil {
		return fmt.Errorf("wait for test command reap: result channel is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("wait for test command reap: timeout must be positive")
	}
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("test command did not reap within %s", timeout)
	}
}

func killThenWaitTestCommand(
	kill func() error,
	wait func() error,
	timeout time.Duration,
) (killErr error, reapErr error) {
	if kill == nil {
		return fmt.Errorf("kill test command: operation is required"), nil
	}
	if wait == nil {
		return nil, fmt.Errorf("wait for test command: operation is required")
	}
	killErr = kill()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	return killErr, waitForTestCommandReap(done, timeout)
}

func killPinnedGroupThenWaitTestCommand(
	signalGroup func() error,
	kill func() error,
	wait func() error,
	timeout time.Duration,
) (signalErr error, killErr error, reapErr error) {
	if signalGroup == nil {
		return fmt.Errorf("signal pinned test process group: operation is required"), nil, nil
	}
	signalErr = signalGroup()
	killErr, reapErr = killThenWaitTestCommand(kill, wait, timeout)
	return signalErr, killErr, reapErr
}

func reportTestCommandTerminationErrors(t *testing.T, pid int, killErr, reapErr error) {
	t.Helper()
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		t.Errorf("kill test command %d: %v", pid, killErr)
	}
	if reapErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(reapErr, &exitErr) {
			t.Errorf("reap test command %d: %v", pid, reapErr)
		}
	}
}

func killAndReapTestCommand(t *testing.T, command *exec.Cmd, timeout time.Duration) {
	t.Helper()
	if command == nil || command.Process == nil {
		t.Error("kill and reap test command: started command is required")
		return
	}
	killErr, reapErr := killThenWaitTestCommand(command.Process.Kill, command.Wait, timeout)
	reportTestCommandTerminationErrors(t, command.Process.Pid, killErr, reapErr)
}

func killPinnedTestProcessGroupAndReap(t *testing.T, command *exec.Cmd, pgid int, timeout time.Duration) {
	t.Helper()
	if command == nil || command.Process == nil {
		t.Error("kill pinned test process group and reap: started command is required")
		return
	}
	signalErr, killErr, reapErr := killPinnedGroupThenWaitTestCommand(
		func() error { return signalTestProcessGroup(pgid, syscall.SIGKILL) },
		command.Process.Kill,
		command.Wait,
		timeout,
	)
	if signalErr != nil {
		t.Errorf("kill pinned test process group %d: %v", pgid, signalErr)
	}
	reportTestCommandTerminationErrors(t, command.Process.Pid, killErr, reapErr)
}

func waitForTestProcessGroupQuiescence(t *testing.T, group *testProcessGroup, timeout time.Duration) {
	t.Helper()
	if group == nil {
		t.Fatal("test process group is required")
	}
	if err := group.waitForQuiescence(timeout); err != nil {
		t.Fatal(err)
	}
}

func TestTestProcessGroupDoesNotSignalReusedIdentity(t *testing.T) {
	old := testProcessSnapshot{pid: 41, pgid: 41, birthID: "old", state: testProcessStateRunning}
	current := []testProcessSnapshot{old}
	signalCalls := 0
	group, err := captureTestProcessGroup(
		41,
		41,
		func() ([]testProcessSnapshot, error) { return current, nil },
		func(int, syscall.Signal) error {
			signalCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	current = []testProcessSnapshot{{pid: 41, pgid: 41, birthID: "reused", state: testProcessStateRunning}}
	signaled, err := group.signalIfOwned(syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}
	if signaled || signalCalls != 0 {
		t.Fatalf("reused numeric process group was signaled: signaled=%v calls=%d", signaled, signalCalls)
	}
}

func TestTestProcessGroupCommandTokenRejectsStaleInitialIdentity(t *testing.T) {
	signalCalls := 0
	group, err := captureTestProcessGroupWithCommandToken(
		44,
		44,
		"/tmp/owned-fixture.invalid",
		func() ([]testProcessSnapshot, error) {
			return []testProcessSnapshot{{pid: 44, pgid: 44, birthID: "reused", state: testProcessStateRunning}}, nil
		},
		func(int, syscall.Signal) error {
			signalCalls++
			return nil
		},
		func(int, int) (string, error) { return "/bin/sh /tmp/foreign-fixture.invalid", nil },
	)
	if !errors.Is(err, errTestProcessGroupNotOwned) || group != nil {
		t.Fatalf("stale initial identity = group %+v, err %v; want not-owned refusal", group, err)
	}
	if signalCalls != 0 {
		t.Fatalf("stale initial identity signaled group %d time(s)", signalCalls)
	}
}

func TestReadTestProcessCommandBoundsFullWidthInspection(t *testing.T) {
	var gotName string
	var gotArgs []string
	_, err := readTestProcessCommandWithOutput(
		44,
		44,
		20*time.Millisecond,
		func(ctx context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked process inspection error = %v, want deadline exceeded", err)
	}
	if gotName != "/bin/ps" {
		t.Fatalf("process inspection executable = %q, want /bin/ps", gotName)
	}
	fullWidth := false
	for _, arg := range gotArgs {
		if arg == "-ww" {
			fullWidth = true
			break
		}
	}
	if !fullWidth {
		t.Fatalf("process inspection args = %q, want full-width -ww", gotArgs)
	}
}

func TestWaitForTestCommandReapBoundsFailureCleanup(t *testing.T) {
	started := time.Now()
	err := waitForTestCommandReap(make(chan error), 20*time.Millisecond)
	if err == nil {
		t.Fatal("blocked process reap returned nil")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked process reap took %s, want bounded cleanup", elapsed)
	}
}

func TestKillThenWaitTestCommandPreservesUnreapedOwnershipUntilKill(t *testing.T) {
	var order []string
	killErr, reapErr := killThenWaitTestCommand(
		func() error {
			order = append(order, "kill")
			return nil
		},
		func() error {
			order = append(order, "wait")
			return nil
		},
		time.Second,
	)
	if killErr != nil || reapErr != nil {
		t.Fatalf("kill-then-wait errors = (%v, %v)", killErr, reapErr)
	}
	if got := strings.Join(order, ","); got != "kill,wait" {
		t.Fatalf("test command termination order = %q, want kill,wait", got)
	}
}

func TestKillPinnedGroupThenWaitTestCommandTerminatesCohortBeforeReap(t *testing.T) {
	var order []string
	signalErr, killErr, reapErr := killPinnedGroupThenWaitTestCommand(
		func() error {
			order = append(order, "group")
			return nil
		},
		func() error {
			order = append(order, "leader")
			return nil
		},
		func() error {
			order = append(order, "wait")
			return nil
		},
		time.Second,
	)
	if signalErr != nil || killErr != nil || reapErr != nil {
		t.Fatalf("group-kill-leader-wait errors = (%v, %v, %v)", signalErr, killErr, reapErr)
	}
	if got := strings.Join(order, ","); got != "group,leader,wait" {
		t.Fatalf("pinned cohort termination order = %q, want group,leader,wait", got)
	}
}

func TestTestProcessGroupSignalsMatchingIdentityAtMostOnce(t *testing.T) {
	current := []testProcessSnapshot{{pid: 42, pgid: 42, birthID: "owned", state: testProcessStateRunning}}
	signalCalls := 0
	group, err := captureTestProcessGroup(
		42,
		42,
		func() ([]testProcessSnapshot, error) { return current, nil },
		func(pgid int, sig syscall.Signal) error {
			signalCalls++
			if pgid != 42 || sig != syscall.SIGKILL {
				t.Fatalf("signal = (%d, %v), want (42, SIGKILL)", pgid, sig)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := group.signalIfOwned(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if signalCalls != 1 {
		t.Fatalf("signal calls = %d, want 1", signalCalls)
	}
}

func TestTestProcessGroupInspectionFailureFailsClosed(t *testing.T) {
	reads := 0
	signalCalls := 0
	group, err := captureTestProcessGroup(
		43,
		43,
		func() ([]testProcessSnapshot, error) {
			reads++
			if reads == 1 {
				return []testProcessSnapshot{{pid: 43, pgid: 43, birthID: "owned", state: testProcessStateRunning}}, nil
			}
			return nil, fmt.Errorf("injected inspection failure")
		},
		func(int, syscall.Signal) error {
			signalCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if signaled, err := group.signalIfOwned(syscall.SIGKILL); err == nil || signaled {
		t.Fatalf("inspection failure = signaled %v, err %v; want false and error", signaled, err)
	}
	if signalCalls != 0 {
		t.Fatalf("inspection failure signaled group %d time(s)", signalCalls)
	}
}

func TestTestProcessGroupTreatsZombieOnlyMembersAsQuiescent(t *testing.T) {
	process := testProcessSnapshot{pid: 52, pgid: 52, birthID: "same", state: testProcessStateRunning}
	current := []testProcessSnapshot{process}
	group, err := captureTestProcessGroup(
		52,
		52,
		func() ([]testProcessSnapshot, error) { return current, nil },
		func(int, syscall.Signal) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	current = []testProcessSnapshot{{pid: 52, pgid: 52, birthID: "same", state: testProcessStateZombie}}
	owned, live, err := group.status()
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("zombie identity no longer pinned its original process group")
	}
	if live {
		t.Fatal("zombie-only process group reported a live descendant")
	}
	if err := group.waitForQuiescence(time.Second); err != nil {
		t.Fatal(err)
	}
	if signaled, err := group.signalIfOwned(syscall.SIGKILL); err != nil || signaled {
		t.Fatalf("quiescent group cleanup = signaled %v, err %v; want disarmed", signaled, err)
	}
}

func TestTestProcessGroupTracksDescendantsWhileIdentityIsPinned(t *testing.T) {
	leader := testProcessSnapshot{pid: 63, pgid: 63, birthID: "leader", state: testProcessStateRunning}
	child := testProcessSnapshot{pid: 64, pgid: 63, birthID: "child", state: testProcessStateRunning}
	current := []testProcessSnapshot{leader}
	group, err := captureTestProcessGroup(
		63,
		63,
		func() ([]testProcessSnapshot, error) { return current, nil },
		func(int, syscall.Signal) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	current = []testProcessSnapshot{leader, child}
	if _, live, err := group.status(); err != nil || !live {
		t.Fatalf("adopt live descendant: live=%v err=%v", live, err)
	}
	current = []testProcessSnapshot{child}
	owned, live, err := group.status()
	if err != nil {
		t.Fatal(err)
	}
	if !owned || !live {
		t.Fatalf("adopted descendant did not preserve group identity: owned=%v live=%v", owned, live)
	}
}

func TestLiveTestProcessSnapshotIncludesCurrentProcess(t *testing.T) {
	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	group, err := captureLiveTestProcessGroup(pgid, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	owned, live, err := group.status()
	if err != nil {
		t.Fatal(err)
	}
	if !owned || !live {
		t.Fatalf("current process group snapshot: owned=%v live=%v", owned, live)
	}
}

func TestTestSubprocessParentLifetimeRequiresConfiguredParent(t *testing.T) {
	t.Setenv(testSubprocessParentPIDEnv, "")
	lifetime, err := newTestSubprocessParentLifetime(2 * minimumProofCheckReservationTTL)
	if err == nil {
		t.Fatalf("missing %s returned lifetime %+v without error", testSubprocessParentPIDEnv, lifetime)
	}
	if err := (*testSubprocessParentLifetime)(nil).check(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("nil parent lifetime check = %v, want required error", err)
	}
}

func TestTestSubprocessParentLifetimeExpiresWhileParentLives(t *testing.T) {
	parentPID := os.Getppid()
	t.Setenv(testSubprocessParentPIDEnv, strconv.Itoa(parentPID))
	lifetime, err := newTestSubprocessParentLifetime(10 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := lifetime.check(); err == nil || !strings.Contains(err.Error(), "hard expiry") {
		t.Fatalf("expired lifetime check = %v, want hard-expiry error", err)
	}
	if err := syscall.Kill(parentPID, 0); err != nil {
		t.Fatalf("parent %d was not alive at expiry: %v", parentPID, err)
	}
}

func TestTestSubprocessShellGuardExpiresWhileParentLives(t *testing.T) {
	parentPID := os.Getpid()
	deadline := time.Now().Add(1500 * time.Millisecond).Unix()
	command := exec.Command("/bin/sh", "-c", testSubprocessShellParentGuard+`while test_fixture_parent_alive; do sleep 0.05; done`)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = append(
		os.Environ(),
		testSubprocessParentPIDEnv+"="+strconv.Itoa(parentPID),
		testSubprocessHardExpiryUnixEnv+"="+strconv.FormatInt(deadline, 10),
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := command.Process.Pid
	group, err := captureLiveTestProcessGroup(pgid, pgid)
	if err != nil {
		killPinnedTestProcessGroupAndReap(t, command, pgid, time.Second)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	waited := make(chan struct{})
	go func() {
		done <- command.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		cleanupTestProcessGroup(t, group, time.Second)
		select {
		case <-waited:
		case <-time.After(time.Second):
			t.Errorf("shell guard process did not reap during cleanup")
		}
	})

	select {
	case err := <-done:
		t.Fatalf("shell guard exited before its wall-clock deadline: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell guard expiry exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell guard did not exit after its wall-clock deadline")
	}
	waitForTestProcessGroupQuiescence(t, group, time.Second)
	if err := syscall.Kill(parentPID, 0); err != nil {
		t.Fatalf("parent %d was not alive at shell expiry: %v", parentPID, err)
	}
}

func newTestSubprocessParentLifetime(maxDuration time.Duration) (*testSubprocessParentLifetime, error) {
	rawParentPID := os.Getenv(testSubprocessParentPIDEnv)
	if rawParentPID == "" {
		return nil, fmt.Errorf("missing required %s", testSubprocessParentPIDEnv)
	}
	parentPID, err := strconv.Atoi(rawParentPID)
	if err != nil || parentPID <= 0 {
		return nil, fmt.Errorf("invalid %s %q", testSubprocessParentPIDEnv, rawParentPID)
	}
	if actual := os.Getppid(); actual != parentPID {
		return nil, fmt.Errorf("test subprocess parent = %d, want controller %d", actual, parentPID)
	}
	if maxDuration <= 0 {
		return nil, fmt.Errorf("test subprocess hard expiry must be positive: %s", maxDuration)
	}
	return &testSubprocessParentLifetime{
		parentPID: parentPID,
		deadline:  time.Now().Add(maxDuration),
	}, nil
}

func (lifetime *testSubprocessParentLifetime) check() error {
	if lifetime == nil {
		return fmt.Errorf("test subprocess parent lifetime is required")
	}
	if actual := os.Getppid(); actual != lifetime.parentPID {
		return fmt.Errorf("test subprocess controller %d exited (current parent %d)", lifetime.parentPID, actual)
	}
	if !time.Now().Before(lifetime.deadline) {
		return fmt.Errorf("test subprocess exceeded hard expiry while controller %d remained alive", lifetime.parentPID)
	}
	if err := syscall.Kill(lifetime.parentPID, 0); err != nil {
		return fmt.Errorf("probe test subprocess controller %d: %w", lifetime.parentPID, err)
	}
	return nil
}

func addTestSubprocessParentEnv(env map[string]string, parentPID int) {
	env[testSubprocessParentPIDEnv] = strconv.Itoa(parentPID)
	env[testSubprocessHardExpiryUnixEnv] = strconv.FormatInt(time.Now().Add(2*minimumProofCheckReservationTTL).Unix(), 10)
}

func publishTestPIDFile(path string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("publish test pid: invalid pid %d", pid)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	defer os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
