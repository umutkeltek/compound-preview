package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUncertainDetachedStartBlocksContinuationAndLaterAdoptsEvidence(t *testing.T) {
	s := newStore(t)
	reserveDetachedLaunchForOwnership(t, s, "work_uncertain_continue", "dl_uncertain_continue")
	writeDetachedDecisionRecord(t, s.home, "work_uncertain_continue", "dl_uncertain_continue", DetachedDecisionStart, fixedNow)
	installDetachedGreenGates(t, s)
	launches := 0
	restore := SwapExecuteStageDetachedLaunch(func(*Store, string, string, string, string, time.Time) (DetachedLaunch, error) {
		launches++
		return DetachedLaunch{}, errors.New("uncertain work must not execute again")
	})
	t.Cleanup(restore)
	before, _ := s.Get("work_uncertain_continue")
	beforeBody, _ := json.Marshal(before)
	res, err := s.Continue(fixedNow.Add(DetachedReservationGrace+time.Minute), ContinueOptions{Actor: "claude-minimax", MaxExecuteSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if launches != 0 || res.CanContinue {
		t.Fatalf("uncertain door became executable: launches=%d result=%+v", launches, res)
	}
	if len(res.DetachedReconciled) != 1 || res.DetachedReconciled[0].Outcome != DetachedOutcomeRecoveryRequired {
		t.Fatalf("real continuation must surface recovery_required: %+v", res.DetachedReconciled)
	}
	after, _ := s.Get("work_uncertain_continue")
	afterBody, _ := json.Marshal(after)
	if !bytes.Equal(beforeBody, afterBody) {
		t.Fatalf("continuation changed uncertain envelope: before=%s after=%s", before.State, after.State)
	}
	// Later observation of the exact launch can resolve uncertainty without a
	// new launch or lifecycle transition. The test process is merely observed.
	launch, err := ReadDetachedLaunch(s.home, "work_uncertain_continue")
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%s%d pgid=%d %s%s\n", DetachedDoorStartedLogLine, os.Getpid(), os.Getpid(), DetachedDoorLaunchIDField, launch.LaunchID)
	if err := os.WriteFile(launch.LogPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.ReconcileDetachedLaunches(fixedNow.Add(2 * DetachedReservationGrace))
	if len(got) != 1 || got[0].Outcome != DetachedOutcomeRunning || got[0].PID != os.Getpid() {
		t.Fatalf("later matching observation was not adopted: %+v", got)
	}
}

func TestDetachedSidecarReportsFileAndDirectorySyncFailures(t *testing.T) {
	for _, boundary := range []string{"file", "directory"} {
		t.Run(boundary, func(t *testing.T) {
			homePath := t.TempDir()
			launch := DetachedLaunch{WorkID: "work_sync_evidence", LaunchID: "dl_sync_evidence", Outcome: DetachedOutcomeReserved}
			if err := WriteDetachedLaunch(homePath, launch); err != nil {
				t.Fatal(err)
			}
			prior, err := os.ReadFile(detachedLaunchPath(homePath, launch.WorkID))
			if err != nil {
				t.Fatal(err)
			}
			oldSync := syncDetachedLaunchRecord
			sentinel := errors.New("injected " + boundary + " sync failure")
			syncDetachedLaunchRecord = func(file *os.File) error {
				isFile := strings.HasSuffix(file.Name(), ".tmp")
				if (boundary == "file") == isFile {
					return sentinel
				}
				return oldSync(file)
			}
			t.Cleanup(func() { syncDetachedLaunchRecord = oldSync })
			launch.PID, launch.PGID, launch.Outcome = 42, 42, DetachedOutcomeRunning
			if err := WriteDetachedLaunch(homePath, launch); !errors.Is(err, sentinel) {
				t.Fatalf("durability failure was hidden: %v", err)
			}
			if _, err := os.Stat(detachedLaunchPath(homePath, launch.WorkID) + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("failed publication left temporary record: %v", err)
			}
			if boundary == "file" {
				after, err := os.ReadFile(filepath.Join(DetachedLaunchDir(homePath), launch.WorkID+".json"))
				if err != nil || !bytes.Equal(prior, after) {
					t.Fatalf("failed file sync replaced prior reservation: %v", err)
				}
			}
		})
	}
}
