package runattempt

import (
	"testing"
	"time"
)

func TestForWorkHydratesOnlyOwnedLaunchEvidenceOnEveryFreshLookup(t *testing.T) {
	s := Open(t.TempDir())
	owned := map[string]bool{}
	for i := range 130 {
		workID := "work_foreign_scope"
		if i < 2 {
			workID = "work_owned_scope"
		}
		ra, err := s.Mint(workID, "synthetic", "claude-code", byWorkNow.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			owned[ra.ID] = true
		}
	}
	original := runattemptReadLaunchEvidence
	t.Cleanup(func() { runattemptReadLaunchEvidence = original })
	counts := map[string]int{}
	runattemptReadLaunchEvidence = func(home, id, kind string) (launchEvidence, error) {
		counts[id]++
		return original(home, id, kind)
	}
	for range 2 {
		clear(counts)
		got, err := Open(s.home).ForWork("work_owned_scope")
		if err != nil || len(got) != len(owned) {
			t.Fatalf("fresh scoped lookup: attempts=%d err=%v", len(got), err)
		}
		for id, count := range counts {
			if !owned[id] {
				t.Fatalf("hydrated unrelated launch evidence %s (%d reads)", id, count)
			}
		}
		for id := range owned {
			if counts[id] == 0 {
				t.Fatalf("owned launch evidence %s was not freshly validated", id)
			}
		}
		t.Logf("records=130 owned=%d launch-sidecar probe counts=%v", len(owned), counts)
	}
}
