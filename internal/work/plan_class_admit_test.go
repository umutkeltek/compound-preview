package work

import (
	"strings"
	"testing"
	"time"
)

const designSketchFixtureRef = "design-sketch:finding:find_fixture"

// makePlanClassWork creates a ready work carrying planClass and claims it for actor, so the
// caller starts at the claimed->running edge the design-sketch gate defends.
func makePlanClassWork(t *testing.T, s *Store, planClass, id, actor string, now time.Time) string {
	t.Helper()
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-core",
		Actor:            actor,
		SuccessPredicate: "plan_class " + planClass + " reaches running only when aligned",
		PlanClass:        planClass,
	}, now)
	if err != nil {
		t.Fatalf("create %s work: %v", planClass, err)
	}
	if w.PlanClass != planClass {
		t.Fatalf("create must carry plan_class verbatim: want %q got %q", planClass, w.PlanClass)
	}
	if _, err := s.Claim(w.ID, actor, now); err != nil {
		t.Fatalf("claim %s work: %v", planClass, err)
	}
	return w.ID
}

// attachDesignSketch attaches ref to an already-claimed work through a same-state advance --
// the shape the refusal message teaches (`work advance <id> --to <state> --artifact ...`).
func attachDesignSketch(t *testing.T, s *Store, id, actor, ref string, now time.Time) {
	t.Helper()
	before, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Advance(id, AdvanceInput{To: before.State, Actor: actor, ArtifactID: ref}, now)
	if err != nil {
		t.Fatalf("attach %q: %v", ref, err)
	}
	found := false
	for _, a := range after.ArtifactIDs {
		if a == ref {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifact %q not attached, got %v", ref, after.ArtifactIDs)
	}
}

// TestPhasedWorkWithoutDesignSketchIsRefusedIntoRunning is the headline falsifier: phased work
// declares that alignment depth must scale with risk, so it may not START until the sketch it
// promised is attached to the envelope. The refusal must be fail-closed (state on disk
// unchanged), name the machine-readable reason, and leave durable deny evidence -- an error
// that vanishes with the process is not governance.
func TestPhasedWorkWithoutDesignSketchIsRefusedIntoRunning(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := makePlanClassWork(t, s, "phased", "work_plan_phased_gate", "codex", fixedNow)

	_, err := s.Advance(id, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("phased work with no design-sketch artifact must not reach running")
	}
	if !strings.Contains(err.Error(), "design_sketch_missing") {
		t.Fatalf("refusal must carry the machine-readable reason design_sketch_missing, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--artifact design-sketch:") {
		t.Fatalf("refusal must teach the fix (--artifact design-sketch:<ref>), got: %v", err)
	}

	// Fail-closed: the envelope on DISK is untouched, not merely the returned copy.
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" {
		t.Fatalf("refused advance must leave work claimed, got %q", got.State)
	}

	// Durable deny evidence: a lifecycle event that does not move state, carrying the
	// ActionIntent + deny PolicyDecision pair.
	events, err := s.readEventsStrictUnlocked(id)
	if err != nil {
		t.Fatal(err)
	}
	var deny *Event
	for i := range events {
		if strings.Contains(events[i].Note, "design-sketch deny") {
			deny = &events[i]
		}
	}
	if deny == nil {
		t.Fatalf("refusal must record a durable deny event, got events %+v", events)
	}
	if deny.From != "claimed" || deny.To != "claimed" {
		t.Fatalf("deny event must not move state, got %s->%s", deny.From, deny.To)
	}
	var sawIntent, sawDecision bool
	for _, att := range deny.Attached {
		if strings.HasPrefix(att, "act_") {
			sawIntent = true
		}
		if strings.HasPrefix(att, "pol_") {
			sawDecision = true
		}
	}
	if !sawIntent || !sawDecision {
		t.Fatalf("deny event must attach an ActionIntent and a PolicyDecision, got %v", deny.Attached)
	}
}

// TestPhasedWorkWithDesignSketchAdvances is the other half of the gate: the refusal is
// satisfiable by exactly the move the message teaches, and the gate does not re-fire once the
// work is already running (a same-state attach on running work must still work).
func TestPhasedWorkWithDesignSketchAdvances(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := makePlanClassWork(t, s, "phased", "work_plan_phased_ok", "codex", fixedNow)
	attachDesignSketch(t, s, id, "codex", designSketchFixtureRef, fixedNow.Add(time.Minute))

	advanced, err := s.Advance(id, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("phased work with an attached design-sketch must advance: %v", err)
	}
	if advanced.State != "running" {
		t.Fatalf("want running, got %q", advanced.State)
	}
	// The gate keys on transitions INTO running; work already running is not re-gated.
	if _, err := s.Advance(id, AdvanceInput{To: "running", Actor: "codex", ArtifactID: "artifact_progress"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatalf("already-running phased work must accept a same-state attach: %v", err)
	}
}

// TestUngatedPlanClassesReachRunningUnchanged pins the blast radius. Only `phased` is gated in
// v1; empty (every envelope that predates the field), oneshot, and plan_doc must behave
// EXACTLY as before -- the freeze-style "absent means nothing gated" rule. This is the test
// that fails if the guard is written as "anything but oneshot".
func TestUngatedPlanClassesReachRunningUnchanged(t *testing.T) {
	t.Parallel()
	for _, planClass := range []string{"", "oneshot", "plan_doc"} {
		name := planClass
		if name == "" {
			name = "legacy_empty"
		}
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			id := makePlanClassWork(t, s, planClass, "work_plan_"+name, "codex", fixedNow)
			advanced, err := s.Advance(id, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(time.Minute))
			if err != nil {
				t.Fatalf("plan_class=%q must not be gated: %v", planClass, err)
			}
			if advanced.State != "running" {
				t.Fatalf("plan_class=%q want running, got %q", planClass, advanced.State)
			}
		})
	}
}

// TestDesignSketchGuardsGenericAdvanceSeam proves the gate lives at the ONE choke point every
// path into running funnels through, not on the plain-advance branch alone. It drives the real
// dispatch-claim auto-advance (Store.DispatchAdvance -> Store.Advance), which is the path that
// starts work without a human typing `work advance` -- exactly the bypass a CLI-level check
// would leave open.
func TestDesignSketchGuardsGenericAdvanceSeam(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := makePlanClassWork(t, s, "phased", "work_plan_phased_dispatch", "codex", fixedNow)

	_, err := s.DispatchAdvance(fixedNow.Add(time.Minute), DispatchPlanOptions{Actor: "codex"})
	if err == nil || !strings.Contains(err.Error(), "design_sketch_missing") {
		t.Fatalf("dispatch auto-advance must not bypass the design-sketch gate, got %v", err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" {
		t.Fatalf("dispatch-refused work must stay claimed, got %q", got.State)
	}

	// Same store, same dispatch entry point: once the sketch is attached the dispatch runs.
	attachDesignSketch(t, s, id, "codex", designSketchFixtureRef, fixedNow.Add(2*time.Minute))
	result, err := s.DispatchAdvance(fixedNow.Add(3*time.Minute), DispatchPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("dispatch advance after attaching the sketch: %v", err)
	}
	if result.Status != "running" || result.AdvancedWorkID != id {
		t.Fatalf("dispatch advance result = %+v, want running for %s", result, id)
	}
}

// TestDesignSketchArtifactRefScansOnlyItsOwnPrefix pins the scan-time convention. Artifact refs
// are free strings; the gate is only as good as the prefix match, so a neighbouring kind
// (`commit:`) and a ref that merely CONTAINS the token must not satisfy it.
func TestDesignSketchArtifactRefScansOnlyItsOwnPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		artifacts []string
		wantRef   string
		wantOK    bool
	}{
		{"empty", nil, "", false},
		{"other kinds only", []string{"commit:abc", "artifact_x"}, "", false},
		{"prefix not at start", []string{"see-design-sketch:x"}, "", false},
		{"bare kind with no ref", []string{"design-sketch:"}, "", false},
		{"finding ref", []string{"commit:abc", designSketchFixtureRef}, designSketchFixtureRef, true},
		{"path ref", []string{"design-sketch:docs/architecture/runtime.md"}, "design-sketch:docs/architecture/runtime.md", true},
		{"latest wins", []string{"design-sketch:a", "design-sketch:b"}, "design-sketch:b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := designSketchArtifactRef(tc.artifacts)
			if ok != tc.wantOK || ref != tc.wantRef {
				t.Fatalf("designSketchArtifactRef(%v) = (%q, %v), want (%q, %v)", tc.artifacts, ref, ok, tc.wantRef, tc.wantOK)
			}
		})
	}
}

// TestCreateRejectsUnknownPlanClass proves the vocabulary fails closed BEFORE persistence: a
// bogus plan_class never reaches disk, so no envelope can carry an unrecognized (and therefore
// ungated) class that a later reader would have to interpret.
func TestCreateRejectsUnknownPlanClass(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create refuses an unknown plan class",
		PlanClass:        "bogus",
	}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "plan_class") {
		t.Fatalf("expected a plan_class validation error, got %v", err)
	}
	works, listErr := s.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("refused create must not persist work: %+v", works)
	}
}
