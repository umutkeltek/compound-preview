package executewire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// Review repair 2026-09-19 (detached): elapsed time never proved a delayed
// launcher stopped. The reservation grace window only bounds how long recovery
// WAITS; once it expires, recovery classifies the reservation lost and closes
// the envelope — while the original launcher is still sitting between its
// reserved sidecar write and cmd.Start(). That launcher used to go on to start
// a child against an envelope nobody owns any more.
//
// The start guard is the falsifier's subject: immediately before starting the
// child the launcher re-reads the sidecar, and starts only if that sidecar
// still holds ITS launch id in `reserved`.
func TestLaunchDetachedViaDoorRefusesToStartAfterRecoveryTookTheReservation(t *testing.T) {
	if _, err := DetachedTimeoutBinary(); err != nil {
		t.Skip("no GNU timeout on PATH: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_delayed", targetRoot)

	// A door that leaves a file behind, so "no child was started" is an
	// observation rather than an inference.
	ranMarker := filepath.Join(t.TempDir(), "door-ran.txt")
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	script := "#!/bin/sh\necho ran > " + ranMarker + "\nsleep 1\n"
	if err := os.WriteFile(fakeDoor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)

	// The launcher is delayed right after it reserved: a recovery tick runs
	// past the grace window, finds a reservation with no started pid, and
	// classifies the door lost.
	now := time.Now().UTC()
	prevWrite := writeDetachedLaunch
	writeDetachedLaunch = func(home string, launch work.DetachedLaunch) error {
		if err := prevWrite(home, launch); err != nil {
			return err
		}
		if launch.Outcome == work.DetachedOutcomeReserved {
			store.ReconcileDetachedLaunches(now.Add(work.DetachedReservationGrace + time.Minute))
		}
		return nil
	}
	t.Cleanup(func() {
		writeDetachedLaunch = prevWrite
		DetachedDoorBinary = prevDoor
		DefaultDetachedLaunchBudget = prevBudget
		SetLaunchContextInstalled(wasInstalled)
	})

	_, err = LaunchDetachedViaDoor(store, "work_detached_delayed", "claude_minimax", "p", "claude-minimax", now)
	if err == nil {
		t.Fatal("a launcher whose reservation was already classified lost must REFUSE to start a child")
	}
	if !strings.Contains(err.Error(), "reservation") {
		t.Fatalf("refusal must name the lost reservation, got %v", err)
	}
	if _, statErr := os.Stat(ranMarker); !os.IsNotExist(statErr) {
		t.Fatalf("a child was started after the reservation was lost (marker stat: %v)", statErr)
	}
	// Recovery's verdict is the sidecar's content now; the refused launcher
	// neither overwrites it with a running record nor deletes it.
	stored, err := work.ReadDetachedLaunch(homePath, "work_detached_delayed")
	if err != nil {
		t.Fatalf("the refused launcher deleted recovery's record: %v", err)
	}
	if stored.Outcome != work.DetachedOutcomeDoorLost {
		t.Fatalf("sidecar outcome = %q, want door_lost (recovery's verdict, untouched)", stored.Outcome)
	}
	if stored.PID != 0 {
		t.Fatalf("a refused start must record no pid, got %d", stored.PID)
	}
}

// The same guard must not fire on the ordinary immediate start: a launcher
// whose reservation is still its own starts the door and records it running.
func TestLaunchDetachedViaDoorStartsWhenItStillHoldsItsReservation(t *testing.T) {
	if _, err := DetachedTimeoutBinary(); err != nil {
		t.Skip("no GNU timeout on PATH: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_held", targetRoot)

	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	if err := os.WriteFile(fakeDoor, []byte("#!/bin/sh\necho door-ran\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)
	t.Cleanup(func() {
		DetachedDoorBinary = prevDoor
		DefaultDetachedLaunchBudget = prevBudget
		SetLaunchContextInstalled(wasInstalled)
	})

	launch, err := LaunchDetachedViaDoor(store, "work_detached_held", "claude_minimax", "p", "claude-minimax", time.Now().UTC())
	if err != nil {
		t.Fatalf("LaunchDetachedViaDoor: %v", err)
	}
	if launch.LaunchID == "" {
		t.Fatalf("every launch carries one minted identity: %+v", launch)
	}
	stored, err := work.ReadDetachedLaunch(homePath, "work_detached_held")
	if err != nil || stored.LaunchID != launch.LaunchID || stored.Outcome != work.DetachedOutcomeRunning {
		t.Fatalf("sidecar must carry the running record under the same launch id: %+v err=%v", stored, err)
	}
	// The log carries that identity on BOTH the marker and the started-pid
	// line — those two lines are what recovery binds to.
	logBody, err := os.ReadFile(launch.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	marker := work.DetachedDoorLaunchMarker + "work_detached_held " + work.DetachedDoorLaunchIDField + launch.LaunchID
	if !strings.Contains(string(logBody), marker) {
		t.Fatalf("door log is missing the identified launch marker %q:\n%s", marker, logBody)
	}
	started := work.DetachedDoorStartedLogLine + itoa(launch.PID)
	if !strings.Contains(string(logBody), started+" pgid="+itoa(launch.PID)+" "+work.DetachedDoorLaunchIDField+launch.LaunchID) {
		t.Fatalf("door log is missing the identified started-pid line:\n%s", logBody)
	}
}
