package lanebeliefs

import (
	"fmt"
	"testing"
)

// F-l (identity, the reconciliation core): lane attribution is store membership, never a
// short-id guess. A receipt whose declared lane maps to a short-id but names NO imported
// agent-lane node must contribute ZERO evidence and be counted unresolved. The pre-B2 fold
// routed through AgentLaneNodeIDFromShortLaneID without store membership, so it *guessed* a node
// for "fixture/judge" even though nothing was ever imported — this test reds against that.
func TestFoldResolvesOnlyImportedLaneNodes(t *testing.T) {
	homePath := t.TempDir()
	// Deliberately DO NOT seed the capgraph store: "fixture/judge" has a deterministic short-id
	// mapping but no imported node, so a no-guessing resolver must refuse it.
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_membership_%02d", i),
			WorkID:    "work_membership",
			LaneID:    "fixture/judge",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}

	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if report.UnresolvedReceiptCount != MIN_LANE_RECEIPTS {
		t.Fatalf("unresolved receipts = %d, want %d (a short-id with no imported node must not resolve)", report.UnresolvedReceiptCount, MIN_LANE_RECEIPTS)
	}
	if report.CandidateCount != 0 || report.NewObservationCount != 0 {
		t.Fatalf("candidates=%d observations=%d, want zero evidence from an unimported lane: %+v", report.CandidateCount, report.NewObservationCount, report)
	}
}
