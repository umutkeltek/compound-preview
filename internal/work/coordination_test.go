package work

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Falsifiers for the read-only coordination projection. These FAIL if the
// projection writes anything, hardcodes an actor, loses a wait edge, lets a
// refused lane keep can_continue=true, or invents data on an empty home.

func coordinationFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	s := newStore(t)
	a, err := s.Create(CreateInput{Owner: "lane-x", DesiredOutcome: "ship the parser", SuccessPredicate: "parser lands"}, fixedNow)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := s.Claim(a.ID, "lane-x", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if _, err := s.Advance(a.ID, AdvanceInput{To: "running", Actor: "lane-x", Note: "start"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("advance a: %v", err)
	}
	b, err := s.Create(CreateInput{Owner: "lane-y", DesiredOutcome: "write the docs", SuccessPredicate: "docs land"}, fixedNow)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if _, err := s.Claim(b.ID, "lane-y", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	return s, a.ID, b.ID
}

// F1 (the load-bearing falsifier): coordination is an observer. Over an
// unchanged home, running it twice changes NOTHING — byte-identical tree.
// The fixture includes a PROPOSED candidate so the measured window exercises
// the dry-run candidate prep (AdvancePlan without Record) — any write there
// turns this red.
func TestCoordinationIsReadOnlyByteIdenticalHome(t *testing.T) {
	s, _, _ := coordinationFixture(t)
	if _, err := s.CreatePlan(CreateInput{WorkID: "work_coord_proposed", Owner: "lane-x", Actor: "lane-x", State: "proposed", SuccessPredicate: "candidate prep stays read-only", DesiredOutcome: "prepare without writes", EvidenceRefs: []string{"test://coordination-proposed"}}, fixedNow, CreatePlanOptions{Record: true, Capability: "product.action_review", Action: "plan_work_from_reviewed_product_action", TargetKind: "product_action_review", TargetID: "coordination_proposed", MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local"}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	now := fixedNow.Add(10 * time.Minute)

	// Warm: any first-touch lock files a read door legitimately creates land
	// BEFORE the measured window (workbench itself is such a door).
	if _, err := s.Workbench(now, WorkbenchOptions{}); err != nil {
		t.Fatalf("warm workbench: %v", err)
	}
	before := snapshotHome(t, s.home)

	if _, err := s.Coordination(now, CoordinationOptions{}); err != nil {
		t.Fatalf("coordination 1: %v", err)
	}
	if _, err := s.Coordination(now, CoordinationOptions{Actor: "lane-x"}); err != nil {
		t.Fatalf("coordination 2: %v", err)
	}

	after := snapshotHome(t, s.home)
	if before != after {
		t.Fatalf("coordination mutated the home:\nBEFORE\n%s\nAFTER\n%s", before, after)
	}
}

// F2: actors are generic. Names like lane-x/lane-y (no codex/claude anywhere
// in the fixture) flow through unmodified; me/others split honors --actor.
func TestCoordinationProjectsGenericActorsAndWaits(t *testing.T) {
	s, a, b := coordinationFixture(t)
	now := fixedNow.Add(10 * time.Minute)

	report, err := s.Coordination(now, CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if report.SchemaVersion != CoordinationSchemaVersion {
		t.Fatalf("schema = %q", report.SchemaVersion)
	}
	if report.Actor != "lane-x" || !report.ActorExplicit {
		t.Fatalf("me selection = %q explicit=%v", report.Actor, report.ActorExplicit)
	}

	me := report.Me
	if me.Actor != "lane-x" {
		t.Fatalf("me = %+v", me)
	}
	if me.Doing == nil || me.Doing.WorkID != a || me.Doing.State != "running" {
		t.Fatalf("me.doing = %+v, want running %s", me.Doing, a)
	}
	if me.CanContinue {
		t.Fatalf("me.can_continue = true; a running work needs run evidence")
	}
	if me.WaitReason != "run_evidence" {
		t.Fatalf("me.wait_reason = %q, want run_evidence", me.WaitReason)
	}
	if !contains(me.BlockedOn, "work.run_evidence:"+a) {
		t.Fatalf("me.blocked_on = %v, want work.run_evidence:%s", me.BlockedOn, a)
	}
	if contains(me.BlockedOn, "work.claimed_by_other_actor:"+b) {
		t.Fatalf("unrelated claimed work became an actor constraint: %v", me.BlockedOn)
	}

	var other *ActorCoordination
	for i := range report.Others {
		if report.Others[i].Actor == "lane-y" {
			other = &report.Others[i]
		}
		if report.Others[i].Actor == "lane-x" {
			t.Fatalf("me leaked into others: %+v", report.Others)
		}
	}
	if other == nil {
		t.Fatalf("others missing lane-y: %+v", report.Others)
	}
	if other.Doing == nil || other.Doing.WorkID != b || other.Doing.State != "claimed" {
		t.Fatalf("lane-y.doing = %+v, want claimed %s", other.Doing, b)
	}
	if !other.CanContinue {
		t.Fatalf("lane-y.can_continue = false; its claimed work is dispatchable (next_action=%q wait=%q blocked=%v)", other.NextAction, other.WaitReason, other.BlockedOn)
	}

	// The wait graph: lane-x is waiting on run evidence for a. Work-scoped
	// edges name the work; lane-y (free to continue) waits on nothing.
	var found bool
	for _, edge := range report.Waits {
		if edge.Actor == "lane-x" && edge.Reason == "run_evidence" && contains(edge.BlockedOn, "work.run_evidence:"+a) {
			found = true
		}
		if edge.Actor == "lane-y" {
			t.Fatalf("wait edge leaked for lane-y, which can continue: %+v", edge)
		}
	}
	if !found {
		t.Fatalf("waits missing the lane-x run_evidence edge: %+v", report.Waits)
	}
	var workScoped bool
	for _, edge := range report.Waits {
		if edge.Work == a && edge.Reason == "run_evidence" {
			workScoped = true
		}
	}
	if !workScoped {
		t.Fatalf("waits missing the work-scoped run_evidence edge for %s: %+v", a, report.Waits)
	}
}

// A coordination snapshot is one point-in-time view shared by every actor.
// Rebuilding the full Workbench per actor makes the observer scale as
// actors × ledger size and can leave the human-facing projection unavailable
// on a mature home. Store.eventLogReads is atomic and increments at every
// event-log load, so this catches a Workbench reintroduction in any helper
// without installing mutable package-global test state.
func TestCoordinationBuildsWorkbenchOnceForAllActors(t *testing.T) {
	s, _, _ := coordinationFixture(t)

	before := s.eventLogReadCount()
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "lane-x"})
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

// F3: default me is the most-present actor, deterministically — never a
// hardcoded identity.
func TestCoordinationDefaultActorIsMostPresent(t *testing.T) {
	s, _, _ := coordinationFixture(t)
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if report.ActorExplicit {
		t.Fatalf("actor_explicit = true with no --actor")
	}
	if report.Actor != "lane-x" {
		t.Fatalf("default me = %q, want lane-x (doing running work)", report.Actor)
	}
	if report.Me.Doing == nil || report.Me.Doing.State != "running" {
		t.Fatalf("default me.doing = %+v", report.Me.Doing)
	}
}

// F4: a refused / confirmation-held lane is a WAIT — can_continue must flip
// off and the wait graph must carry the lane.* edge, even with dispatchable
// work. This is "lane full, must wait" expressed generically.
func TestCoordinationLaneGateRefusalIsWait(t *testing.T) {
	s := newStore(t)
	writeContextPack(t, s, "ctx_coordination_gate")
	w, err := s.Create(CreateInput{Owner: "lane-x", ContextPackID: "ctx_coordination_gate", DesiredOutcome: "ready work", SuccessPredicate: "dispatchable"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "lane-x", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	previousGuard := LaneCapacityGuard
	t.Cleanup(func() { LaneCapacityGuard = previousGuard })

	// Baseline: no gate → can continue.
	if LaneCapacityGuard != nil {
		t.Fatalf("test assumes no guard installed by default")
	}
	open, err := s.Coordination(fixedNow, CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination open: %v", err)
	}
	if !open.Me.CanContinue {
		t.Fatalf("open lane: can_continue = false (wait=%q blocked=%v)", open.Me.WaitReason, open.Me.BlockedOn)
	}
	if !contains(open.Integrity, "lane_capacity_guard_not_installed") {
		t.Fatalf("open lane integrity = %v, want lane_capacity_guard_not_installed note", open.Integrity)
	}

	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "walled", Remaining: 0, ResetsAt: "2026-07-22T00:00:00Z"},
			Refused:  true,
			Message:  "lane budget exhausted",
		}, nil
	}
	gated, err := s.Coordination(fixedNow.Add(time.Minute), CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination gated: %v", err)
	}
	if gated.Me.CanContinue {
		t.Fatalf("refused lane: can_continue = true")
	}
	if gated.Me.Lane == nil || !gated.Me.Lane.Refused || gated.Me.Lane.State != "walled" {
		t.Fatalf("refused lane face = %+v", gated.Me.Lane)
	}
	if gated.Me.WaitReason != "capacity_refused" || !contains(gated.Me.BlockedOn, "lane.capacity_refused:lane-x") {
		t.Fatalf("refused wait = %q %v", gated.Me.WaitReason, gated.Me.BlockedOn)
	}
	var laneEdge bool
	for _, edge := range gated.Waits {
		if edge.Actor == "lane-x" && edge.Reason == "capacity_refused" && edge.Lane != nil && edge.Lane.Refused {
			laneEdge = true
		}
	}
	if !laneEdge {
		t.Fatalf("waits missing the capacity_refused lane edge: %+v", gated.Waits)
	}

	// Confirmation-required is a hold, not a refusal — still a wait.
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		return LaneCapacityGuardResult{
			Capacity:             DispatchLaneCapacity{State: "open", Remaining: 1},
			ConfirmationRequired: true,
			Message:              "spend above floor",
		}, nil
	}
	held, err := s.Coordination(fixedNow.Add(2*time.Minute), CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination held: %v", err)
	}
	if held.Me.CanContinue || held.Me.WaitReason != "capacity_confirmation_required" {
		t.Fatalf("held lane: can_continue=%v wait=%q", held.Me.CanContinue, held.Me.WaitReason)
	}
	if !contains(held.Me.BlockedOn, "lane.capacity_confirmation:lane-x") {
		t.Fatalf("held blocked_on = %v", held.Me.BlockedOn)
	}
}

// F5: an empty home projects a valid empty report — never a crash, never
// invented actors or waits.
func TestCoordinationEmptyHomeIsValid(t *testing.T) {
	s := newStore(t)
	report, err := s.Coordination(fixedNow, CoordinationOptions{})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if report.SchemaVersion != CoordinationSchemaVersion || report.GeneratedAt == "" {
		t.Fatalf("empty report = %+v", report)
	}
	if len(report.Waits) != 0 {
		t.Fatalf("empty home waits = %+v", report.Waits)
	}
	if report.Me.Doing != nil || report.Me.CanContinue {
		t.Fatalf("empty home me invented activity: %+v", report.Me)
	}
	for _, other := range report.Others {
		if other.Doing != nil {
			t.Fatalf("empty home invented doing for %q: %+v", other.Actor, other)
		}
	}
}

// F6: the ReadOnly probe reaches the guard — coordination must ask the gate
// read-only, never via a recording dispatch-plan.
func TestCoordinationLaneProbeIsReadOnly(t *testing.T) {
	s := newStore(t)
	writeContextPack(t, s, "ctx_coordination_probe")
	if _, err := s.Create(CreateInput{Owner: "lane-x", ContextPackID: "ctx_coordination_probe", DesiredOutcome: "ready", SuccessPredicate: "dispatchable"}, fixedNow); err != nil {
		t.Fatalf("create: %v", err)
	}
	var sawReadOnly bool
	previousGuard := LaneCapacityGuard
	t.Cleanup(func() { LaneCapacityGuard = previousGuard })
	LaneCapacityGuard = func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		sawReadOnly = req.ReadOnly
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "open", Remaining: 5}}, nil
	}
	if _, err := s.Coordination(fixedNow, CoordinationOptions{Actor: "lane-x"}); err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if !sawReadOnly {
		t.Fatalf("lane probe reached the guard without ReadOnly=true")
	}
}

func snapshotHome(t *testing.T, homePath string) string {
	t.Helper()
	type entry struct {
		path string
		hash string
	}
	var entries []entry
	err := filepath.WalkDir(homePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		rel, relErr := filepath.Rel(homePath, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, entry{path: filepath.ToSlash(rel), hash: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatalf("walk home: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.hash)
		sb.WriteString("  ")
		sb.WriteString(e.path)
		sb.WriteString("\n")
	}
	return sb.String()
}

// F7: candidate prep is exact, not approximate. A proposed candidate policy
// admits keeps can_continue=true ("prepare candidate to ready") — the same
// step `work continue` would take; an operator-required candidate flips to a
// work.ready_state wait instead of a false "go ahead."
func TestCoordinationCandidatePrepIsExact(t *testing.T) {
	prepOptions := CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "coordination_prep",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	}

	s := newStore(t)
	if _, err := s.CreatePlan(CreateInput{WorkID: "work_coord_prep_ok", Owner: "lane-x", Actor: "lane-x", State: "proposed", SuccessPredicate: "prepare allowed"}, fixedNow, prepOptions); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	report, err := s.Coordination(fixedNow.Add(time.Minute), CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	// The proposed create-plan surfaces as an admission preview; `continue`'s
	// first step there is admit — coordination must agree.
	if !report.Me.CanContinue || report.Me.NextAction != "add suggested task to work" {
		t.Fatalf("eligible preview: can_continue=%v next_action=%q (want true / add suggested task to work)", report.Me.CanContinue, report.Me.NextAction)
	}

	s2 := newStore(t)
	operatorOptions := prepOptions
	operatorOptions.TargetID = "coordination_prep_operator"
	operatorOptions.ActionContext = &WorkActionContext{
		Kind:             "workbench_review_card",
		Title:            "private config review",
		ActionSummary:    "operator supplies private config",
		RequiresOperator: true,
	}
	if _, err := s2.CreatePlan(CreateInput{WorkID: "work_coord_prep_operator", Owner: "lane-x", Actor: "lane-x", State: "proposed", SuccessPredicate: "operator input required"}, fixedNow, operatorOptions); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	blocked, err := s2.Coordination(fixedNow.Add(time.Minute), CoordinationOptions{Actor: "lane-x"})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if blocked.Me.CanContinue {
		t.Fatalf("operator-required candidate: can_continue = true (wait=%q blocked=%v)", blocked.Me.WaitReason, blocked.Me.BlockedOn)
	}
	// Same verdict `work continue` reaches for operator-required previews
	// (TestContinueLeavesOperatorAdmissionPreviewWaiting): needs_review, blocked
	// on the admission preview.
	if blocked.Me.WaitReason != "needs_review" || !contains(blocked.Me.BlockedOn, "work.admission_preview:work_coord_prep_operator") {
		t.Fatalf("operator-required wait = %q %v", blocked.Me.WaitReason, blocked.Me.BlockedOn)
	}
}

// F8: an explicit actor nothing enumerates (no fleet row, lease, claim, or
// preview) still gets its REAL workbench face projected — never flattened to
// a zero face. Shared queue availability is preserved, while constraints
// require owned/held/selected work or an actual lane gate.
func TestCoordinationExplicitActorWithoutPresenceIsProjected(t *testing.T) {
	s, _, _ := coordinationFixture(t)
	report, err := s.Coordination(fixedNow.Add(10*time.Minute), CoordinationOptions{Actor: "ghost"})
	if err != nil {
		t.Fatalf("coordination: %v", err)
	}
	if report.Actor != "ghost" || !report.ActorExplicit || report.Me.Actor != "ghost" {
		t.Fatalf("me selection = %q explicit=%v me.actor=%q", report.Actor, report.ActorExplicit, report.Me.Actor)
	}
	if report.Me.CanContinue {
		t.Fatalf("ghost.can_continue = true (wait=%q blocked=%v)", report.Me.WaitReason, report.Me.BlockedOn)
	}
	// An actor with no owned/held/selected work sees shared queue availability,
	// never fabricated constraints on other actors' tasks.
	if len(report.Me.BlockedOn) != 0 {
		t.Fatalf("idle actor has unrelated blocked chips: %v", report.Me.BlockedOn)
	}
	for _, edge := range report.Waits {
		if edge.Actor == "ghost" {
			t.Fatalf("idle actor gained unrelated work constraint: %+v", edge)
		}
	}
	if report.Me.WaitReason != "no_dispatchable_work" || report.Me.Open != 2 {
		t.Fatalf("shared queue summary missing: %+v", report.Me)
	}
	var laneX bool
	for _, other := range report.Others {
		if other.Actor == "lane-x" {
			laneX = true
		}
	}
	if !laneX {
		t.Fatalf("others missing lane-x now that ghost is me: %+v", report.Others)
	}
}
