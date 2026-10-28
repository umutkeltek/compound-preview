package executewire

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/work"
)

// R6P falsifier: the detached spawner runs the SAME governed door a human
// dispatches, as its own process group, wrapped in a bounded GNU timeout,
// with the exact argv recorded in the sidecar and the door's output in a log.
// A fake door script records what it was invoked with so the test can read
// the argv the real door would have seen.

func seedClaimedDetachedWork(t *testing.T, store *work.Store, id, targetRoot string) {
	t.Helper()
	sha := initGitRepoOrSkip(t, targetRoot)
	if _, err := store.Create(work.CreateInput{
		WorkID:           id,
		Owner:            "orchestrator:claude_minimax",
		Actor:            "claude_code:orchestrator",
		State:            "ready",
		Risk:             "tier1",
		Privacy:          "private",
		SuccessPredicate: "detached door fixture predicate",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: targetRoot,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    sha,
			PublicationMode:    "local_only",
		},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(id, "claude-minimax", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchDetachedViaDoorSpawnsTheGovernedDoorAsItsOwnGroup(t *testing.T) {
	timeoutBin, err := DetachedTimeoutBinary()
	if err != nil {
		t.Skip("no GNU timeout on PATH: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_door", targetRoot)

	recorded := filepath.Join(t.TempDir(), "door-argv.txt")
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	// Publish readiness only after the complete argv/home and required log output
	// exist. Seeing an earlier partial record raced the detached child's next write.
	recordedShell := "'" + strings.ReplaceAll(recorded, "'", "'\"'\"'") + "'"
	script := `#!/bin/sh
recorded=` + recordedShell + `
printf '%s\n' "$@" > "$recorded.pending"
printf 'home=%s cwd=%s\n' "$COMPOUND_HOME" "$(pwd)" >> "$recorded.pending"
# Keep setup observably incomplete before publishing the ready marker.
sleep 0.25
echo door-ran
mv "$recorded.pending" "$recorded"
sleep 2
`
	if err := os.WriteFile(fakeDoor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	t.Cleanup(func() { DetachedDoorBinary = prevDoor; DefaultDetachedLaunchBudget = prevBudget })
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(wasInstalled) })

	now := time.Date(2026, 9, 11, 19, 0, 0, 0, time.UTC)
	launch, err := LaunchDetachedViaDoor(store, "work_detached_door", "claude_minimax", "do the bounded thing", "claude-minimax", now)
	if err != nil {
		t.Fatalf("LaunchDetachedViaDoor: %v", err)
	}
	t.Cleanup(func() {
		// The fake door has a bounded two-second lifetime. Join it before its
		// fixture directories disappear; no unrelated process is signalled.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(launch.PID, &status, syscall.WNOHANG, nil)
			if pid == launch.PID || errors.Is(err, syscall.ECHILD) {
				return
			}
			if err != nil && !errors.Is(err, syscall.EINTR) {
				t.Errorf("join test-owned detached door: %v", err)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Errorf("test-owned detached door %d did not exit", launch.PID)
	})
	if launch.PID <= 0 || launch.PGID != launch.PID {
		t.Fatalf("door must be its own process-group leader: %+v", launch)
	}
	if launch.BudgetSeconds != 30 || launch.Outcome != work.DetachedOutcomeRunning {
		t.Fatalf("sidecar budget/outcome wrong: %+v", launch)
	}
	want := DetachedLaunchArgv(timeoutBin, fakeDoor, 30*time.Second, "work_detached_door", "claude_minimax", "do the bounded thing", "claude-minimax")
	if strings.Join(launch.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("recorded argv differs:\n got %q\nwant %q", launch.Command, want)
	}
	if !strings.Contains(strings.Join(want, " "), "--permission-mode bypass") || !strings.Contains(strings.Join(want, " "), "--to running --by claude-minimax --execute --harness claude_minimax") {
		t.Fatalf("argv is not the governed door shape: %q", want)
	}
	stored, err := work.ReadDetachedLaunch(homePath, "work_detached_door")
	if err != nil || stored.PID != launch.PID {
		t.Fatalf("sidecar not persisted: %+v err=%v", stored, err)
	}

	// The door is alive after return (the seam did not wait for it).
	if err := exec.Command("kill", "-0", itoa(launch.PID)).Run(); err != nil {
		t.Fatalf("door pid %d is not alive right after launch: %v", launch.PID, err)
	}
	// Wait for the fake door to record its argv, then read it: the door saw
	// exactly the governed argv (after the timeout wrapper) and COMPOUND_HOME.
	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		body, _ = os.ReadFile(recorded)
		if strings.Contains(string(body), "home=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	wantDoorArgs := want[5:] // after: timeout --signal --kill-after <s> <door>
	if len(lines) < len(wantDoorArgs)+1 {
		t.Fatalf("fake door recorded too little:\n%s", body)
	}
	for i, arg := range wantDoorArgs {
		if lines[i] != arg {
			t.Fatalf("door argv[%d] = %q, want %q (all: %q)", i, lines[i], arg, lines)
		}
	}
	last := lines[len(lines)-1]
	realRoot, _ := filepath.EvalSymlinks(targetRoot)
	if !strings.Contains(last, "home="+homePath) || !strings.Contains(last, "cwd="+realRoot) {
		t.Fatalf("door env/cwd wrong: %s", last)
	}
	logBody, _ := os.ReadFile(launch.LogPath)
	if !strings.Contains(string(logBody), "detached door start work=work_detached_door") || !strings.Contains(string(logBody), "door-ran") {
		t.Fatalf("door log missing start line or door output:\n%s", logBody)
	}
}

func TestLaunchDetachedViaDoorRefusesUnclaimedOrUnboundedLaunches(t *testing.T) {
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	sha := initGitRepoOrSkip(t, targetRoot)
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_detached_ready", Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", Risk: "tier1", Privacy: "private", SuccessPredicate: "still ready",
		GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: targetRoot, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: sha, PublicationMode: "local_only"},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(wasInstalled) })
	now := time.Now().UTC()
	if _, err := LaunchDetachedViaDoor(store, "work_detached_ready", "claude_minimax", "p", "claude-minimax", now); err == nil || !strings.Contains(err.Error(), "expected claimed") {
		t.Fatalf("a ready (unclaimed) envelope must be refused, got %v", err)
	}
	if _, err := store.Claim("work_detached_ready", "claude-minimax", now); err != nil {
		t.Fatal(err)
	}
	prev := DetachedTimeoutBinary
	DetachedTimeoutBinary = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { DetachedTimeoutBinary = prev })
	if _, err := LaunchDetachedViaDoor(store, "work_detached_ready", "claude_minimax", "p", "claude-minimax", now); err == nil {
		t.Fatal("a launch without a timeout wrapper must be refused, not run unbounded")
	}
	if _, statErr := os.Stat(filepath.Join(work.DetachedLaunchDir(homePath), "work_detached_ready.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused launch must leave no sidecar: %v", statErr)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// Start/record contract 2026-09-15: a sidecar write failure AFTER the door
// started is a typed recovery-required result carrying the started identity —
// never a plain failed-to-spawn error, and the reserved record stays behind
// with the pid line in the door log so a reconcile tick can adopt it.
func TestLaunchDetachedViaDoorPostStartRecordFailureIsRecoveryRequired(t *testing.T) {
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_recordfail", targetRoot)

	// The fake door sleeps long enough for the test to observe the started
	// child after the launcher returned an error.
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	script := "#!/bin/sh\necho door-ran\nsleep 3\n"
	if err := os.WriteFile(fakeDoor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	calls := 0
	prevWrite := writeDetachedLaunch
	writeDetachedLaunch = func(home string, launch work.DetachedLaunch) error {
		calls++
		if calls == 1 {
			return prevWrite(home, launch) // reserved record succeeds
		}
		return os.ErrPermission // every post-start write fails
	}
	t.Cleanup(func() {
		writeDetachedLaunch = prevWrite
		DetachedDoorBinary = prevDoor
		DefaultDetachedLaunchBudget = prevBudget
		SetLaunchContextInstalled(wasInstalled)
	})

	now := time.Now().UTC()
	launch, err := LaunchDetachedViaDoor(store, "work_detached_recordfail", "claude_minimax", "p", "claude-minimax", now)
	var recErr *work.DetachedLaunchRecordError
	if err == nil || !errorsAs(err, &recErr) {
		t.Fatalf("a post-start sidecar failure must return a typed recovery error, got %v (launch %+v)", err, launch)
	}
	if recErr.Launch.PID <= 0 || recErr.Launch.Outcome != work.DetachedOutcomeRunning {
		t.Fatalf("recovery error must carry the started identity: %+v", recErr.Launch)
	}
	// The started child must be observable (it really runs).
	if !processAlive(recErr.Launch.PID) {
		t.Fatalf("door pid %d must be alive after a record failure", recErr.Launch.PID)
	}
	// The pid line is in the door log — the adoption authority.
	logBody, _ := os.ReadFile(recErr.Launch.LogPath)
	if !strings.Contains(string(logBody), work.DetachedDoorStartedLogLine) {
		t.Fatalf("door log must carry the started pid line:\n%s", logBody)
	}
	// The sidecar on disk is still the reserved record (no pid).
	stored, readErr := work.ReadDetachedLaunch(homePath, "work_detached_recordfail")
	if readErr != nil || stored.Outcome != work.DetachedOutcomeReserved || stored.PID != 0 {
		t.Fatalf("expected reserved sidecar on disk, got %+v err=%v", stored, readErr)
	}
	// The caller contract: the envelope stays claimed — the door owns it.
	w, err := store.Get("work_detached_recordfail")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.State, "claimed") {
		t.Fatalf("a recovery_required launch must NOT fail-close the envelope, state=%s", w.State)
	}

	// A reconcile tick adopts the reserved record from the log.
	verdicts := store.ReconcileDetachedLaunches(time.Now().UTC())
	var adopted bool
	for _, v := range verdicts {
		if v.WorkID == "work_detached_recordfail" && v.PID == recErr.Launch.PID && v.Outcome == work.DetachedOutcomeRunning {
			adopted = true
		}
	}
	if !adopted {
		t.Fatalf("reconcile must adopt the started pid from the door log, got %+v", verdicts)
	}
	adoptedRec, err := work.ReadDetachedLaunch(homePath, "work_detached_recordfail")
	if err != nil || adoptedRec.PID != recErr.Launch.PID || adoptedRec.Outcome != work.DetachedOutcomeRunning {
		t.Fatalf("adopted record wrong: %+v err=%v", adoptedRec, err)
	}
}

// The start itself failing (the door's working directory vanished) must
// remove the reserved sidecar: nothing started, nothing owns the envelope.
func TestLaunchDetachedViaDoorStartFailureRemovesReservedSidecar(t *testing.T) {
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_startfail", targetRoot)
	prevDoor := DetachedDoorBinary
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	script := "#!/bin/sh\necho door-ran\nsleep 3\n"
	if err := os.WriteFile(fakeDoor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { DetachedDoorBinary = prevDoor; SetLaunchContextInstalled(wasInstalled) })
	// The door cannot even chdir into its workspace.
	if err := os.RemoveAll(targetRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := LaunchDetachedViaDoor(store, "work_detached_startfail", "claude_minimax", "p", "claude-minimax", time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "start door") {
		t.Fatalf("an unstartable door must be refused with the start error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(work.DetachedLaunchDir(homePath), "work_detached_startfail.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a failed start must leave no sidecar: %v", statErr)
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
