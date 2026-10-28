package work

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func causalFixtureWork(t *testing.T, s *Store, id, actor, surface, state string) string {
	t.Helper()
	w, err := s.Create(CreateInput{WorkID: id, Owner: actor, CanonicalSurface: surface, DesiredOutcome: "synthetic distinct project", SuccessPredicate: "bounded result"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(w.ID, actor, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if state != "claimed" {
		if _, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: actor}, fixedNow.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if state == "held" {
		if _, err = s.Advance(w.ID, AdvanceInput{To: "held", Actor: actor, Note: "synthetic operator review"}, fixedNow.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return w.ID
}

func causalActorDependencies(r CoordinationReport, actor string) []string {
	var refs []string
	for _, edge := range r.Waits {
		if edge.Actor == actor {
			for _, ref := range edge.BlockedOn {
				if strings.Contains(ref, ":work_") {
					refs = append(refs, ref)
				}
			}
		}
	}
	if r.Me.Actor == actor {
		for _, ref := range r.Me.BlockedOn {
			if strings.Contains(ref, ":work_") {
				refs = append(refs, ref)
			}
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func TestCoordinationCausalTwoProjectIndependence(t *testing.T) {
	for _, foreignState := range []string{"claimed", "running", "held"} {
		t.Run(foreignState, func(t *testing.T) {
			s := newStore(t)
			own := causalFixtureWork(t, s, "work_causal_alpha", "lane-a", "project://synthetic-alpha", "running")
			before, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "lane-a"})
			if err != nil {
				t.Fatal(err)
			}
			baseline := causalActorDependencies(before, "lane-a")
			if !slices.Contains(baseline, "work.run_evidence:"+own) {
				t.Fatal("own running evidence control missing", baseline)
			}
			for i := 0; i < 3; i++ {
				causalFixtureWork(t, s, fmt.Sprintf("work_causal_beta_%d", i), fmt.Sprintf("lane-b-%d", i), "project://synthetic-beta", foreignState)
			}
			after, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "lane-a"})
			if err != nil {
				t.Fatal(err)
			}
			got := causalActorDependencies(after, "lane-a")
			if !reflect.DeepEqual(got, baseline) {
				t.Fatalf("unrelated project changed actor dependency set: before=%v after=%v", baseline, got)
			}
		})
	}
}

func TestCoordinationCausalIdleActorHasNoForeignDependencies(t *testing.T) {
	s := newStore(t)
	causalFixtureWork(t, s, "work_causal_foreign", "lane-b", "project://synthetic-beta", "running")
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "lane-idle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := causalActorDependencies(report, "lane-idle"); len(got) != 0 {
		t.Fatalf("idle actor gained invented dependencies: %v", got)
	}
}

func TestCoordinationCausalOwnHeldReviewPreserved(t *testing.T) {
	s := newStore(t)
	own := causalFixtureWork(t, s, "work_causal_held", "lane-a", "project://synthetic-alpha", "held")
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "lane-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := causalActorDependencies(report, "lane-a"); !slices.Contains(got, "work.review:"+own) {
		t.Fatalf("own held review wait missing: %v", got)
	}
}

func TestCoordinationCausalExactForeignItemClassificationPreserved(t *testing.T) {
	item := WorkbenchItem{WorkID: "work_causal_exact_foreign", State: "claimed", ClaimedBy: "lane-b"}
	reason, token := coordinationItemWait(item, "lane-a")
	if reason != "claimed_by_other_actor" || token != "work.claimed_by_other_actor:"+item.WorkID {
		t.Fatalf("exact foreign claim wait lost: reason=%q token=%q", reason, token)
	}
}
