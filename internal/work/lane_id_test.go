package work

import (
	"testing"
	"time"
)

// NOTE (B0 deviation): the former TestCloseStampsProducerLaneIDOnReceiptAndTransition asserted
// that close-minted receipts inherit the lane from in.Actor (a raw actor string). B0 removes
// that actor-derivation from the close path — a lane is stamped ONLY when it was DECLARED on the
// lifecycle (claim/advance --lane) and resolved to a canonical agent-lane node id. That behavior
// is now owned by TestF1DeclaredLaneStampsCanonicalNodeIDOnClose (declared → canonical) and
// TestF2NoLaneEmptyLaneIDDespiteActorOwnerHarness (no declaration → empty, no-guessing) in
// lane_declaration_test.go. The action-execution mint path below is a SEPARATE seam, out of B0's
// scope, and still stamps its actor-derived lane unchanged (see handoff deviations).

func TestActionExecutionReceiptDraftStampsProducerLaneID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, workID := runningActionContextWork(t, s)
	plan, err := s.ActionExecutionPlan(workID, fixedNow.Add(20*time.Minute), ActionExecutionPlanOptions{Actor: "fixture/lane", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReceiptDraft.Producer.LaneID != "fixture/lane" {
		t.Fatalf("receipt draft producer lane_id = %q, want fixture/lane", plan.ReceiptDraft.Producer.LaneID)
	}
}
