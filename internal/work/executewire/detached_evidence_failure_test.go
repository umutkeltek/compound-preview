package executewire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/work"
)

// Unlike the external audit's Store shim, these tests exercise the real
// lifecycle and policy persistence after a real synthetic child starts.
func TestDetachedDualEvidenceFailureRetainsOwnership(t *testing.T) {
	homePath, store, fakeDoor, marker := detachedEvidenceFixture(t, "work_dual_evidence")
	oldDoor, oldWrite, oldLog := DetachedDoorBinary, writeDetachedLaunch, writeDetachedStartedLog
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	writes := 0
	writeDetachedLaunch = func(home string, launch work.DetachedLaunch) error {
		if launch.Outcome == work.DetachedOutcomeRunning {
			writes++
			return errors.New("injected running-sidecar failure")
		}
		return oldWrite(home, launch)
	}
	writeDetachedStartedLog = func(*os.File, string) error {
		return errors.New("injected started-log failure")
	}
	t.Cleanup(func() { DetachedDoorBinary, writeDetachedLaunch, writeDetachedStartedLog = oldDoor, oldWrite, oldLog })
	now := time.Now().UTC()
	launch, err := LaunchDetachedViaDoor(store, "work_dual_evidence", "claude_minimax", "synthetic fixture", "claude-minimax", now)
	cleanupDetachedEvidenceChild(t, launch.PID)
	var recordErr *work.DetachedLaunchRecordError
	if !errors.As(err, &recordErr) || writes != 2 {
		t.Fatalf("need a started child and both failed sidecar writes: launch=%+v err=%v writes=%d", launch, err, writes)
	}
	if !strings.Contains(err.Error(), "injected started-log failure") || !strings.Contains(err.Error(), "injected running-sidecar failure") {
		t.Fatalf("both failed evidence paths must be reported: %v", err)
	}
	waitDetachedEvidenceMarker(t, marker)
	assertDetachedEvidenceUncertain(t, homePath, "work_dual_evidence", launch.PID, now)
}

func TestDetachedRecordingErrorsRemainRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		logWrite    bool
		sidecarFail bool
	}{
		{name: "log write only", logWrite: true},
		{name: "log sync only"},
		{name: "log sync and sidecar", sidecarFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, store, fakeDoor, marker := detachedEvidenceFixture(t, "work_log_evidence")
			oldDoor, oldWrite, oldLog, oldSync := DetachedDoorBinary, writeDetachedLaunch, writeDetachedStartedLog, syncDetachedDoorLog
			DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
			if tc.logWrite {
				writeDetachedStartedLog = func(*os.File, string) error { return errors.New("injected log write failure") }
			} else {
				syncDetachedDoorLog = func(*os.File) error { return errors.New("injected log sync failure") }
			}
			if tc.sidecarFail {
				writeDetachedLaunch = func(home string, launch work.DetachedLaunch) error {
					if launch.Outcome == work.DetachedOutcomeRunning {
						return errors.New("injected sidecar failure")
					}
					return oldWrite(home, launch)
				}
			}
			t.Cleanup(func() {
				DetachedDoorBinary, writeDetachedLaunch, writeDetachedStartedLog, syncDetachedDoorLog = oldDoor, oldWrite, oldLog, oldSync
			})
			now := time.Now().UTC()
			launch, err := LaunchDetachedViaDoor(store, "work_log_evidence", "claude_minimax", "synthetic fixture", "claude-minimax", now)
			cleanupDetachedEvidenceChild(t, launch.PID)
			var recordErr *work.DetachedLaunchRecordError
			if !errors.As(err, &recordErr) || !strings.Contains(err.Error(), "injected log") {
				t.Fatalf("log write/sync failure must be reported after start: %v", err)
			}
			waitDetachedEvidenceMarker(t, marker)
			fresh, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			got := fresh.ReconcileDetachedLaunches(now.Add(work.DetachedReservationGrace + time.Minute))
			if len(got) != 1 || got[0].Outcome != work.DetachedOutcomeRunning || got[0].PID != launch.PID {
				t.Fatalf("surviving matching identity must recover the actual child: %+v", got)
			}
			if w, err := fresh.Get("work_log_evidence"); err != nil || w.State != "claimed" {
				t.Fatalf("recording failure changed actual lifecycle: state=%s err=%v", w.State, err)
			}
		})
	}
}

func TestDetachedCrashImmediatelyAfterStartRetainsOwnership(t *testing.T) {
	homePath, _, fakeDoor, marker := detachedEvidenceFixture(t, "work_crash_evidence")
	pidPath := filepath.Join(t.TempDir(), "synthetic-child-pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedEvidenceCrashHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_DETACHED_CRASH=1", "COMPOUND_TEST_DETACHED_HOME="+homePath,
		"COMPOUND_TEST_DETACHED_DOOR="+fakeDoor, "COMPOUND_TEST_DETACHED_PID="+pidPath)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
		t.Fatalf("launcher helper did not crash at the post-start boundary: %v\n%s", err, output)
	}
	pidBody, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBody))
	if err != nil {
		t.Fatal(err)
	}
	cleanupDetachedEvidenceChild(t, pid)
	waitDetachedEvidenceMarker(t, marker)
	assertDetachedEvidenceUncertain(t, homePath, "work_crash_evidence", pid, time.Now().UTC())
}

func TestDetachedExitBeforeStartRetainsUncertainOwnership(t *testing.T) {
	homePath, _, fakeDoor, marker := detachedEvidenceFixture(t, "work_prestart_exit")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedBeforeStartExitHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_DETACHED_PRESTART_EXIT=1",
		"COMPOUND_TEST_DETACHED_HOME="+homePath, "COMPOUND_TEST_DETACHED_DOOR="+fakeDoor)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 74 {
		t.Fatalf("launcher did not exit at the pre-start boundary: %v\n%s", err, output)
	}
	launch, err := work.ReadDetachedLaunch(homePath, "work_prestart_exit")
	if err != nil || launch.PID != 0 || launch.LaunchID == "" || launch.Outcome != work.DetachedOutcomeReserved {
		t.Fatalf("pre-start exit must leave identified reservation without PID: %+v err=%v", launch, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("launcher exited before Start but its door ran: %v", err)
	}
	for i := 0; i < 2; i++ {
		fresh, err := work.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		before, err := fresh.Get(launch.WorkID)
		if err != nil {
			t.Fatal(err)
		}
		beforeBody, _ := json.Marshal(before)
		got := fresh.ReconcileDetachedLaunches(time.Now().Add(24 * time.Hour))
		if len(got) != 1 || got[0].Outcome != work.DetachedOutcomeRecoveryRequired || strings.Contains(got[0].Detail, "holds ownership lock") {
			t.Fatalf("released kernel lock cannot prove whether Start occurred: %+v", got)
		}
		after, err := fresh.Get(launch.WorkID)
		afterBody, _ := json.Marshal(after)
		if err != nil || !bytes.Equal(beforeBody, afterBody) || after.State != "claimed" {
			t.Fatalf("recovery changed unresolved launch lifecycle: %+v err=%v", after, err)
		}
		if _, owned := work.DetachedDoorOwns(homePath, launch.WorkID); !owned {
			t.Fatal("pre-start exit lost reservation and could invite duplicate execution")
		}
	}
}

func TestDetachedBeforeStartExitHelper(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_DETACHED_PRESTART_EXIT") != "1" {
		return
	}
	store, err := work.Open(os.Getenv("COMPOUND_TEST_DETACHED_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	DetachedDoorBinary = func() (string, error) { return os.Getenv("COMPOUND_TEST_DETACHED_DOOR"), nil }
	SetLaunchContextInstalled(true)
	// The real start decision is durable and its flock is held. Exiting here
	// releases the kernel lock, without ever starting the detached child.
	detachedAfterStartDecision = func() { os.Exit(74) }
	_, err = LaunchDetachedViaDoor(store, "work_prestart_exit", "claude_minimax", "synthetic pre-start exit", "claude-minimax", time.Now().UTC())
	t.Fatalf("pre-start exit hook was not reached: %v", err)
}

func TestDetachedEvidenceCrashHelper(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_DETACHED_CRASH") != "1" {
		return
	}
	store, err := work.Open(os.Getenv("COMPOUND_TEST_DETACHED_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	DetachedDoorBinary = func() (string, error) { return os.Getenv("COMPOUND_TEST_DETACHED_DOOR"), nil }
	SetLaunchContextInstalled(true)
	detachedAfterStart = func(cmd *exec.Cmd) {
		// Test-only observation; production recovery cannot read this path.
		if err := os.WriteFile(os.Getenv("COMPOUND_TEST_DETACHED_PID"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			t.Fatal(err)
		}
		os.Exit(73) // no deferred release or parent PID/log/sidecar recording
	}
	_, err = LaunchDetachedViaDoor(store, "work_crash_evidence", "claude_minimax", "synthetic fixture", "claude-minimax", time.Now().UTC())
	t.Fatalf("launcher unexpectedly returned: %v", err)
}

func detachedEvidenceFixture(t *testing.T, workID string) (string, *work.Store, string, string) {
	t.Helper()
	if _, err := DetachedTimeoutBinary(); err != nil {
		t.Fatal("synthetic detached integration requires GNU timeout: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	seedClaimedDetachedWork(t, store, workID, workspace)
	marker := filepath.Join(workspace, "synthetic-door-started")
	t.Setenv("COMPOUND_TEST_DETACHED_MARKER", marker)
	fakeDoor := filepath.Join(workspace, "synthetic-door")
	if err := os.WriteFile(fakeDoor, []byte("#!/bin/sh\nprintf started > \"$COMPOUND_TEST_DETACHED_MARKER\"\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(wasInstalled) })
	return homePath, store, fakeDoor, marker
}

func cleanupDetachedEvidenceChild(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		t.Fatalf("fixture has no child pid: %d", pid)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, 0, nil) // ECHILD is expected for the crashed launcher's child.
	})
}

func waitDetachedEvidenceMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && string(body) == "started" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("synthetic door did not start")
}

func assertDetachedEvidenceUncertain(t *testing.T, homePath, workID string, pid int, now time.Time) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("child must still be alive: %v", err)
	}
	store, err := work.Open(homePath) // fresh instance, no in-memory launch identity
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeBody, _ := json.Marshal(before)
	for i := 1; i <= 2; i++ {
		got := store.ReconcileDetachedLaunches(now.Add(time.Duration(i) * (work.DetachedReservationGrace + time.Minute)))
		if len(got) != 1 || got[0].Outcome != "recovery_required" {
			t.Errorf("no PID evidence cannot prove no start: reconciliation %d = %+v", i, got)
		}
		after, err := store.Get(workID)
		if err != nil {
			t.Fatal(err)
		}
		afterBody, _ := json.Marshal(after)
		if !bytes.Equal(beforeBody, afterBody) {
			t.Errorf("recovery changed the actual lifecycle: before %s, after %s", before.State, after.State)
		}
		if _, owned := work.DetachedDoorOwns(homePath, workID); !owned {
			t.Error("uncertain started child must retain ownership against duplicate dispatch")
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal(fmt.Errorf("reconciliation must not touch the synthetic process: %w", err))
	}
}
