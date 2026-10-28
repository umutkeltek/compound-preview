package work

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Falsifiers for the coordination capacity seam. The projection must use ONE
// guard for the whole request, must never turn a hard source failure into an
// ungated actor, and must leave the package-level guard alone.

func coordinationCapacityFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "lane-x",
		DesiredOutcome:   "ready work",
		SuccessPredicate: "dispatchable",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "lane-x", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return s
}

// coordinationCapacityTwoActorFixture leaves BOTH actors at a dispatch point
// (each holds its own claimed work), so the capacity guard is reached once per
// actor inside one projection.
func coordinationCapacityTwoActorFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	for _, actor := range []string{"lane-x", "lane-y"} {
		w, err := s.Create(CreateInput{
			Owner:            actor,
			DesiredOutcome:   "ready work",
			SuccessPredicate: "dispatchable",
		}, fixedNow)
		if err != nil {
			t.Fatalf("create %s: %v", actor, err)
		}
		if _, err := s.Claim(w.ID, actor, fixedNow.Add(time.Minute)); err != nil {
			t.Fatalf("claim %s: %v", actor, err)
		}
	}
	return s
}

// The request's guard is chosen ONCE and used for every actor. A package-level
// guard must not be consulted when the caller injects its own — otherwise one
// projection could mix two guards.
func TestCoordinationUsesInjectedCapacityGuardForEveryActor(t *testing.T) {
	s := coordinationCapacityTwoActorFixture(t)
	previousGuard := LaneCapacityGuard
	t.Cleanup(func() { LaneCapacityGuard = previousGuard })

	globalCalls := 0
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		globalCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "global"}}, nil
	}
	var lanes []string
	var readOnly []bool
	injected := func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		lanes = append(lanes, req.Lane)
		readOnly = append(readOnly, req.ReadOnly)
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "open", Remaining: 5}}, nil
	}
	report, err := s.Coordination(fixedNow.Add(2*time.Minute), CoordinationOptions{Actor: "lane-x", CapacityGuard: injected})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if globalCalls != 0 {
		t.Fatalf("the package-level guard was consulted %d times even though the request injected its own", globalCalls)
	}
	if len(lanes) != 2 {
		t.Fatalf("injected guard calls = %v, want one call per eligible actor", lanes)
	}
	for _, lane := range lanes {
		if lane != "lane-x" && lane != "lane-y" {
			t.Fatalf("injected guard was asked about an unexpected actor %q", lane)
		}
	}
	for i, flag := range readOnly {
		if !flag {
			t.Fatalf("the coordination capacity probe for %q was not ReadOnly", lanes[i])
		}
	}
	if report.Me.Actor != "lane-x" {
		t.Fatalf("me = %+v", report.Me)
	}
}

// A hard source failure must NOT read as an ungated actor. The old
// coordinationLane dropped the guard error and returned a nil lane, which the
// projection then read as "no gate applied" — an unreadable capacity source
// looked exactly like a lane with unlimited capacity.
func TestCoordinationPropagatesCapacityGuardError(t *testing.T) {
	s := coordinationCapacityFixture(t)
	failing := func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		return LaneCapacityGuardResult{}, errors.New("read capacity usage history (codexbar/usage-history): permission denied")
	}
	report, err := s.Coordination(fixedNow.Add(2*time.Minute), CoordinationOptions{Actor: "lane-x", CapacityGuard: failing})
	if err == nil {
		t.Fatal("an unreadable capacity source must not look like an ungated actor")
	}
	if !strings.Contains(err.Error(), "capacity usage history") {
		t.Fatalf("the source failure was rewritten: %v", err)
	}
	if report.Actor != "" || len(report.Others) != 0 || len(report.Waits) != 0 {
		t.Fatalf("a failed projection returned a report: %+v", report)
	}
}

// The "no gate installed" integrity note describes the guard THIS request
// used. An injected guard is a gate: the note must not claim otherwise.
func TestCoordinationInjectedGuardReplacesMissingGuardNote(t *testing.T) {
	s := coordinationCapacityFixture(t)
	previousGuard := LaneCapacityGuard
	t.Cleanup(func() { LaneCapacityGuard = previousGuard })
	LaneCapacityGuard = nil

	without, err := s.Coordination(fixedNow.Add(2*time.Minute), CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination without a guard: %v", err)
	}
	if !contains(without.Integrity, "lane_capacity_guard_not_installed") {
		t.Fatalf("integrity = %v, want the missing-guard note", without.Integrity)
	}
	with, err := s.Coordination(fixedNow.Add(2*time.Minute), CoordinationOptions{
		Actor: "lane-x",
		CapacityGuard: func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
			return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "open", Remaining: 5}}, nil
		},
	})
	if err != nil {
		t.Fatalf("coordination with an injected guard: %v", err)
	}
	if contains(with.Integrity, "lane_capacity_guard_not_installed") {
		t.Fatalf("integrity = %v, an injected guard is a gate", with.Integrity)
	}
}

// The shared workbench snapshot stays shared: an injected capacity guard does
// not multiply the workbench reads.
func TestCoordinationReadsWorkbenchOnceWithInjectedCapacityGuard(t *testing.T) {
	s, _, _ := coordinationFixture(t)
	before := s.eventLogReadCount()
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{
		Actor: "lane-x",
		CapacityGuard: func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
			return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "open", Remaining: 5}}, nil
		},
	})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if delta := s.eventLogReadCount() - before; delta != 1 {
		t.Fatalf("coordination read events/work.jsonl %d times, want exactly 1 shared snapshot read", delta)
	}
	if report.Me.Actor != "lane-x" || len(report.Others) == 0 {
		t.Fatalf("shared snapshot lost actor faces: me=%+v others=%+v", report.Me, report.Others)
	}
}

// The delegate pre-mint capacity recheck sets ReadOnly=true ONLY to suppress
// a duplicate refusal observation. ReadOnly is NOT snapshot-safe: the guard
// must still re-read the current source, so a lane that walled after
// admission is still refused before the spawn. This is the falsifier that the
// coordination observation never leaked into the execution path.
func TestDelegatePrelaunchCapacityRecheckIsReadOnlyButStillRefuses(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "")
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{
			Ready: true,
			Evidence: []DelegateHarnessReadinessEvidence{{
				Kind: "permission_posture", Ref: "harness-posture://claude-zai", Revision: "sha256:stable",
			}},
		}, nil
	}
	var readOnly []bool
	calls := 0
	LaneCapacityGuard = func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		calls++
		readOnly = append(readOnly, req.ReadOnly)
		if calls == 1 {
			return LaneCapacityGuardResult{
				Capacity: DispatchLaneCapacity{State: "ok", Remaining: 0.8, DataRef: "test://capacity/open"},
			}, nil
		}
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "walled", Remaining: 0, DataRef: "test://capacity/walled"},
			Refused:  true,
			Message:  "capacity walled after admission",
		}, nil
	}

	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder, Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("a lane that walled before the pre-mint recheck must refuse: %+v", result)
	}
	if calls != 2 {
		t.Fatalf("capacity calls = %d, want the admission probe plus the pre-mint recheck", calls)
	}
	if readOnly[1] != true {
		t.Fatalf("the pre-mint recheck is not ReadOnly: %v", readOnly)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("a walled lane reached Create: %+v", runner.creates)
	}
}
