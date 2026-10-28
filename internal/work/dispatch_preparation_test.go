package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/laneselect"
)

func newReadyStore(t *testing.T) (*Store, string) {
	t.Helper()
	s := newStore(t)
	ready, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "ready preparation work",
		DesiredOutcome:   "prove preparation lane routing",
		EvidenceRefs:     []string{"test://prep"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return s, ready.ID
}

// f3: work outside the preparation class keeps today's behavior byte-identical. An empty class
// and a non-preparation class must both produce the exact same plan JSON as before — actor
// "codex", no lane_selection — and must never call the selector.
func TestDispatchPlanNonPreparationByteIdentical(t *testing.T) {
	s, _ := newReadyStore(t)
	called := false
	prev := PreparationLaneSelector
	PreparationLaneSelector = func(homePath, class string, now time.Time) (*laneselect.PreparationLaneRanking, error) {
		called = true
		return &laneselect.PreparationLaneRanking{Winner: &laneselect.PreparationLaneScore{Lane: "should-not-be-used"}}, nil
	}
	t.Cleanup(func() { PreparationLaneSelector = prev })

	empty, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{})
	if err != nil {
		t.Fatalf("dispatch plan (empty class): %v", err)
	}
	nonPrep, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{CapabilityClass: "build-implement"})
	if err != nil {
		t.Fatalf("dispatch plan (non-preparation class): %v", err)
	}
	if called {
		t.Fatalf("selector must not be called for empty/non-preparation classes")
	}
	if empty.Actor != "codex" || nonPrep.Actor != "codex" {
		t.Fatalf("non-preparation dispatch must default actor to codex; empty=%s nonPrep=%s", empty.Actor, nonPrep.Actor)
	}
	if empty.LaneSelection != nil || nonPrep.LaneSelection != nil {
		t.Fatalf("non-preparation dispatch must not attach a lane_selection")
	}
	emptyJSON, _ := json.Marshal(empty)
	nonPrepJSON, _ := json.Marshal(nonPrep)
	if string(emptyJSON) != string(nonPrepJSON) {
		t.Fatalf("non-preparation plan must be byte-identical to the empty-class plan:\n empty=%s\n nonPrep=%s", emptyJSON, nonPrepJSON)
	}
	if strings.Contains(string(emptyJSON), "lane_selection") {
		t.Fatalf("plan JSON must omit lane_selection when no selection ran")
	}
}

// The wired path: a preparation class with no explicit actor runs the selector, adopts the
// winning lane as the dispatch actor, and records the ranking in the plan (Law 9 receipt).
func TestDispatchPlanPreparationAdoptsWinningLane(t *testing.T) {
	s, readyID := newReadyStore(t)
	var gotClass string
	prev := PreparationLaneSelector
	PreparationLaneSelector = func(homePath, class string, now time.Time) (*laneselect.PreparationLaneRanking, error) {
		gotClass = class
		return &laneselect.PreparationLaneRanking{
			SchemaVersion:   laneselect.PreparationLaneRankingSchemaVersion,
			CapabilityClass: class,
			Status:          laneselect.PreparationLaneStatusRanked,
			Candidates: []laneselect.PreparationLaneScore{
				{Lane: "scout/chat", Eligible: true, MeetsFloor: true},
			},
			Winner: &laneselect.PreparationLaneScore{Lane: "scout/chat", Eligible: true, MeetsFloor: true},
		}, nil
	}
	t.Cleanup(func() { PreparationLaneSelector = prev })

	plan, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{CapabilityClass: "classify-extract"})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if gotClass != "classify-extract" {
		t.Fatalf("selector must be called with the preparation class, got %q", gotClass)
	}
	if plan.Actor != "scout/chat" {
		t.Fatalf("dispatch must adopt the winning lane as actor; got %q", plan.Actor)
	}
	if plan.LaneSelection == nil || plan.LaneSelection.Winner == nil || plan.LaneSelection.Winner.Lane != "scout/chat" {
		t.Fatalf("plan must record the lane selection receipt: %+v", plan.LaneSelection)
	}
	// The selected candidate's claim command must run as the chosen lane, not codex.
	if plan.Selected == nil {
		t.Fatalf("expected a selected candidate")
	}
	cmd := strings.Join(plan.Selected.Command, " ")
	if !strings.Contains(cmd, "--by scout/chat") || !strings.Contains(cmd, readyID) {
		t.Fatalf("claim command must run as the selected lane: %q", cmd)
	}
}

// Selector wired but no eligible lane: dispatch falls back to codex, records a warning, and
// still attaches the (blocked) ranking so the refusal is auditable.
func TestDispatchPlanPreparationNoEligibleLaneFallsBack(t *testing.T) {
	s, _ := newReadyStore(t)
	prev := PreparationLaneSelector
	PreparationLaneSelector = func(homePath, class string, now time.Time) (*laneselect.PreparationLaneRanking, error) {
		return &laneselect.PreparationLaneRanking{
			SchemaVersion:   laneselect.PreparationLaneRankingSchemaVersion,
			CapabilityClass: class,
			Status:          laneselect.PreparationLaneStatusBlocked,
			Candidates:      []laneselect.PreparationLaneScore{{Lane: "down-lane", Eligible: false, OmitReason: "liveness probe reports lane down"}},
		}, nil
	}
	t.Cleanup(func() { PreparationLaneSelector = prev })

	plan, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{CapabilityClass: "classify-extract"})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if plan.Actor != "codex" {
		t.Fatalf("no eligible lane must fall back to codex; got %q", plan.Actor)
	}
	if plan.LaneSelection == nil || plan.LaneSelection.Status != laneselect.PreparationLaneStatusBlocked {
		t.Fatalf("blocked ranking must still be recorded for audit: %+v", plan.LaneSelection)
	}
	joined := strings.Join(plan.Warnings, " | ")
	if !strings.Contains(joined, "no eligible lane") {
		t.Fatalf("fallback must surface a warning: %v", plan.Warnings)
	}
}
