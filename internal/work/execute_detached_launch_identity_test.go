package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review repair 2026-09-19 (detached): ONE launch identity owns a detached
// launch from reservation through start to recovery.
//
// Before it, the door log's launch marker carried only `work=<id>`, so
// recovery could scope a pid no tighter than "the most recent marker in this
// log". That is the WRONG launch whenever a reconcile tick lands between a new
// reservation's sidecar write and that launcher's first log line: the only
// marker in the log is then the PREVIOUS launch's, and its pid was adopted
// into the new reservation. The launch id minted at reservation makes the
// binding exact; a reservation that carries one adopts nothing else.

// claimDetachedIdentityWork seeds a claimed envelope and returns the door-log
// path these falsifiers write by hand (the launcher is not involved: the point
// is what recovery does with bytes a previous launcher left behind).
func claimDetachedIdentityWork(t *testing.T, s *Store, workID string) string {
	t.Helper()
	seedReadyExecuteWork(t, s, workID)
	if _, err := s.Claim(workID, "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(t.TempDir(), workID+".door.log")
}

func writeDetachedDoorLog(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Falsifier 1 — old-log interleaving. The previous launch left a complete
// marker + started-pid pair in the O_APPEND log. The current reservation has
// been written but its launcher has not reached its first log line yet. A
// reconcile tick landing in that gap must adopt NOTHING: the reservation is
// still inside its grace window and its launcher still owns it.
func TestReconcileRefusesAPidFromAnotherLaunchIdentity(t *testing.T) {
	s := newStore(t)
	logPath := claimDetachedIdentityWork(t, s, "work_detached_old_log")
	writeDetachedDoorLog(t, logPath,
		"2026-06-28T11:00:00Z detached door start work=work_detached_old_log lane=claude-minimax actor=claude-minimax\n"+
			"2026-06-28T11:00:00Z "+DetachedDoorLaunchMarker+"work_detached_old_log "+DetachedDoorLaunchIDField+"dl_previous started_at=2026-06-28T11:00:00.000Z\n"+
			"2026-06-28T11:00:01Z "+DetachedDoorStartedLogLine+"99999 pgid=99999 "+DetachedDoorLaunchIDField+"dl_previous\n")
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_detached_old_log",
		LaunchID:  "dl_current",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   logPath,
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(pid int) bool { return pid == 99999 }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Minute))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeReserved || verdicts[0].PID != 0 {
		t.Fatalf("a reservation for launch dl_current must NOT adopt launch dl_previous's pid; want one reserved verdict with pid 0, got %+v", verdicts)
	}
	sidecar, err := ReadDetachedLaunch(s.home, "work_detached_old_log")
	if err != nil {
		t.Fatal(err)
	}
	if sidecar.PID != 0 || sidecar.Outcome != DetachedOutcomeReserved || sidecar.LaunchID != "dl_current" {
		t.Fatalf("sidecar took a foreign identity: %+v", sidecar)
	}
	w, _ := s.Get("work_detached_old_log")
	if w.State != "claimed" {
		t.Fatalf("envelope state = %q; an in-grace reservation must stay claimed for its launcher", w.State)
	}
}

// Falsifier 3 — happy path. The launcher started its child and logged the pid,
// then its sidecar update failed (the recovery_required shape). Recovery must
// adopt THAT pid: the started-pid line carries the reservation's own launch id,
// even though an older launch's pid sits earlier in the same log.
func TestReconcileAdoptsThePidOfItsOwnLaunchIdentity(t *testing.T) {
	s := newStore(t)
	logPath := claimDetachedIdentityWork(t, s, "work_detached_own_pid")
	writeDetachedDoorLog(t, logPath,
		"2026-06-28T11:00:00Z "+DetachedDoorLaunchMarker+"work_detached_own_pid "+DetachedDoorLaunchIDField+"dl_previous started_at=2026-06-28T11:00:00.000Z\n"+
			"2026-06-28T11:00:01Z "+DetachedDoorStartedLogLine+"99999 pgid=99999 "+DetachedDoorLaunchIDField+"dl_previous\n"+
			"2026-06-28T11:30:00Z "+DetachedDoorLaunchMarker+"work_detached_own_pid "+DetachedDoorLaunchIDField+"dl_current started_at=2026-06-28T11:30:00.000Z\n"+
			"2026-06-28T11:30:01Z "+DetachedDoorStartedLogLine+"888 pgid=888 "+DetachedDoorLaunchIDField+"dl_current\n")
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_detached_own_pid",
		LaunchID:  "dl_current",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   logPath,
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(pid int) bool { return pid == 888 }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Minute))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeRunning || verdicts[0].PID != 888 {
		t.Fatalf("recovery must adopt this reservation's own started pid (888); got %+v", verdicts)
	}
	sidecar, err := ReadDetachedLaunch(s.home, "work_detached_own_pid")
	if err != nil {
		t.Fatal(err)
	}
	if sidecar.PID != 888 || sidecar.Outcome != DetachedOutcomeRunning {
		t.Fatalf("adopted sidecar wrong: %+v", sidecar)
	}
	if sidecar.LaunchID != "dl_current" {
		t.Fatalf("adoption must preserve the launch identity, got %q", sidecar.LaunchID)
	}
}

// Falsifier 4 — legacy. A sidecar written before the launch id existed has no
// id to match, so it keeps exactly today's behaviour: scope to the most recent
// marker and adopt the pid after it. That holds even when the log itself was
// written by a NEW launcher (a binary upgrade between the two), which is the
// case an over-strict "ids must match on both sides" rule would break.
func TestReconcileKeepsLegacyIdlessRecoveryBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logBody string
	}{
		{
			name: "id-less log",
			logBody: "2026-06-28T11:00:00Z " + DetachedDoorStartedLogLine + "99999 pgid=99999\n" +
				"2026-06-28T11:30:00Z " + DetachedDoorLaunchMarker + "work_detached_legacy started_at=2026-06-28T11:30:00.000Z\n" +
				"2026-06-28T11:30:01Z " + DetachedDoorStartedLogLine + "888 pgid=888\n",
		},
		{
			name: "log written by a newer launcher",
			logBody: "2026-06-28T11:00:00Z " + DetachedDoorStartedLogLine + "99999 pgid=99999\n" +
				"2026-06-28T11:30:00Z " + DetachedDoorLaunchMarker + "work_detached_legacy " + DetachedDoorLaunchIDField + "dl_current started_at=2026-06-28T11:30:00.000Z\n" +
				"2026-06-28T11:30:01Z " + DetachedDoorStartedLogLine + "888 pgid=888 " + DetachedDoorLaunchIDField + "dl_current\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			logPath := claimDetachedIdentityWork(t, s, "work_detached_legacy")
			writeDetachedDoorLog(t, logPath, tc.logBody)
			if err := WriteDetachedLaunch(s.home, DetachedLaunch{
				WorkID:    "work_detached_legacy",
				Lane:      "claude-minimax",
				Actor:     "claude-minimax",
				LogPath:   logPath,
				StartedAt: fixedNow.UTC().Format(time.RFC3339),
				Outcome:   DetachedOutcomeReserved,
			}); err != nil {
				t.Fatal(err)
			}
			prevAlive := detachedProcessAlive
			detachedProcessAlive = func(pid int) bool { return pid == 888 }
			t.Cleanup(func() { detachedProcessAlive = prevAlive })

			verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Minute))
			if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeRunning || verdicts[0].PID != 888 {
				t.Fatalf("a legacy id-less reservation must keep marker-scoped recovery (pid 888); got %+v", verdicts)
			}
			sidecar, _ := ReadDetachedLaunch(s.home, "work_detached_legacy")
			if sidecar.PID != 888 {
				t.Fatalf("legacy sidecar PID = %d, want 888", sidecar.PID)
			}
		})
	}
}

// The launch id is written into a whitespace-separated log field, so a minted
// id that carried a space would silently truncate the field a reconcile reads.
func TestNewDetachedLaunchIDIsUniqueAndLoggable(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		id := NewDetachedLaunchID(fixedNow)
		if id == "" || len(strings.Fields(id)) != 1 {
			t.Fatalf("launch id %q is not a single log field", id)
		}
		if seen[id] {
			t.Fatalf("launch id %q was minted twice; one launch, one identity", id)
		}
		seen[id] = true
	}
}
