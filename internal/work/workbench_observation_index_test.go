package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Empty per-work results are still indexed results. Adding unrelated work must
// not multiply observation reads or repeat the same integrity warning.
func TestWorkbenchObservationIndexIncludesEmptyResults(t *testing.T) {
	const emptyWorks, foreignObservations = 64, 24
	for _, projection := range []string{"workbench", "coordination"} {
		t.Run(projection, func(t *testing.T) {
			s := newStore(t)
			observedID := "work_observation_present"
			for i := 0; i <= emptyWorks; i++ {
				id := fmt.Sprintf("work_observation_empty_%03d", i)
				if i == emptyWorks {
					id = observedID
				}
				w := BuildWork(CreateInput{WorkID: id, Owner: "fixture-actor", SuccessPredicate: "retain observation evidence"}, fixedNow)
				w.State = "running"
				if err := s.writeJSON(s.workPath(id), &w); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(s.home, "run-observations")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < foreignObservations+2; i++ {
				workID := fmt.Sprintf("work_foreign_%03d", i)
				if i >= foreignObservations {
					workID = observedID
				}
				observation := RunObservationResult{
					ID: fmt.Sprintf("obs_%03d", i), WorkID: workID,
					GeneratedAt: fixedNow.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
					Actor:       "fixture-actor", TraceID: fmt.Sprintf("trace_%03d", i),
					Note: "preserve observed evidence",
				}
				if err := s.writeJSON(filepath.Join(dir, observation.ID+".json"), &observation); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
			homeBefore := snapshotHome(t, s.home)
			readsBefore := s.runObservationFileReads.Load()
			var integrity []string
			if projection == "coordination" {
				report, err := s.Coordination(fixedNow.Add(time.Hour), CoordinationOptions{Actor: "fixture-actor"})
				if err != nil {
					t.Fatal(err)
				}
				integrity = report.Integrity
				if report.Me.Open != emptyWorks+1 || report.Me.CanContinue {
					t.Fatalf("coordination lost running-work summary: %+v", report.Me)
				}
			} else {
				report, err := s.Workbench(fixedNow.Add(time.Hour), WorkbenchOptions{RecentLimit: 8})
				if err != nil {
					t.Fatal(err)
				}
				integrity = report.Integrity
				if report.Counts.Open != emptyWorks+1 || report.Counts.RunObservations != 2 {
					t.Fatalf("workbench changed counts: %+v", report.Counts)
				}
				for _, item := range report.Open {
					if item.WorkID != observedID && len(item.RunObservations) != 0 {
						t.Fatalf("foreign observations leaked into %s: %+v", item.WorkID, item.RunObservations)
					}
				}
				item := findWorkbenchItem(t, report.Open, observedID)
				if len(item.RunObservations) != 2 || item.RunObservations[0].ID != "obs_025" ||
					item.RunObservations[1].ID != "obs_024" ||
					item.RunObservations[0].Note != "preserve observed evidence" {
					t.Fatalf("owned evidence or ordering changed: %+v", item.RunObservations)
				}
			}
			if got, want := s.runObservationFileReads.Load()-readsBefore, int64(foreignObservations+3); got != want {
				t.Errorf("observation file reads=%d want=%d for %d zero-observation works", got, want, emptyWorks)
			}
			warnings := 0
			for _, warning := range integrity {
				if strings.Contains(warning, "run observation corrupt.json unreadable") {
					warnings++
				}
			}
			if warnings != 1 {
				t.Errorf("corrupt observation warnings=%d want=1; index integrity must remain visible once", warnings)
			}
			if after := snapshotHome(t, s.home); after != homeBefore {
				t.Fatal("read-only projection changed home bytes")
			}
		})
	}
}

func TestWorkbenchStandaloneItemStillLoadsObservations(t *testing.T) {
	s := newStore(t)
	w := BuildWork(CreateInput{WorkID: "work_standalone_observation", Owner: "fixture-actor", SuccessPredicate: "standalone evidence remains visible"}, fixedNow)
	w.State = "running"
	dir := filepath.Join(s.home, "run-observations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	observation := RunObservationResult{ID: "obs_standalone", WorkID: w.ID, TraceID: "trace_standalone", GeneratedAt: fixedNow.Format(time.RFC3339)}
	if err := s.writeJSON(filepath.Join(dir, observation.ID+".json"), &observation); err != nil {
		t.Fatal(err)
	}
	report := WorkbenchReport{}
	before := s.runObservationFileReads.Load()
	item := s.workbenchItem(w, nil, &report, holdEvidenceIndex{}, fixedNow)
	if got := s.runObservationFileReads.Load() - before; got != 1 {
		t.Fatalf("standalone reads=%d want=1", got)
	}
	if len(item.RunObservations) != 1 || item.RunObservations[0].TraceID != observation.TraceID {
		t.Fatalf("standalone evidence missing: %+v", item.RunObservations)
	}
}
