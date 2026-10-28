package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
)

// seedLane imports one agent-lane node into the work store's home so a short lane id resolves
// through the store-backed no-guessing resolver. Returns the short id and the canonical node id
// (computed via the exported identity function — never a literal, hard rule 1).
func seedLane(t *testing.T, s *Store, shortID string) (string, string) {
	t.Helper()
	cg, err := capgraph.Open(s.home)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	tuple, ok := capgraph.AgentLaneTupleFromShortLaneID(shortID)
	if !ok {
		t.Fatalf("short id %q has no tuple", shortID)
	}
	nodeID := capgraph.AgentLaneNodeID(tuple)
	if err := cg.Add(capgraph.Node{ID: nodeID, Kind: "agent-lane", Name: nodeID, Weight: 0.25, EvidenceOnly: true}); err != nil {
		t.Fatalf("seed lane %q: %v", nodeID, err)
	}
	return shortID, nodeID
}

// F1: a declared lane (claim --lane <valid-short-id>) is stamped on BOTH the close-minted
// Receipt and Transition as the canonical agent-lane node id — byte-identical to the exported
// AgentLaneNodeID(tuple), proving hard rule 1 (same function, never a re-slug or a literal).
func TestF1DeclaredLaneStampsCanonicalNodeIDOnClose(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	shortID, wantNode := seedLane(t, s, "fixture/judge")

	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "fixture proof exists"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimWithLane(w.ID, "some-actor", shortID, fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim --lane %q: %v", shortID, err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "some-actor"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "some-actor",
		Summary:   "fixture proof landed",
		ProofRefs: []string{"proof:fixture"},
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Receipt.Producer.LaneID != wantNode {
		t.Fatalf("receipt lane_id = %q, want canonical %q", res.Receipt.Producer.LaneID, wantNode)
	}
	if res.Transition.Producer.LaneID != wantNode {
		t.Fatalf("transition lane_id = %q, want canonical %q", res.Transition.Producer.LaneID, wantNode)
	}
}

// F2: with NO --lane anywhere, close-minted receipts carry an EMPTY LaneID even though the
// actor string, the owner, and a harness-ish token are all present. Nothing may be derived
// from actor/owner/harness (no-guessing, falsifier l).
func TestF2NoLaneEmptyLaneIDDespiteActorOwnerHarness(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "operator-owner", SuccessPredicate: "fixture proof exists"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// actor is a harness:session actor string — exactly the shape that must NOT resolve.
	const actor = "agent-harness:0f0f0f0f"
	if _, err := s.Claim(w.ID, actor, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: actor}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     actor,
		Summary:   "landed",
		ProofRefs: []string{"proof:fixture"},
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Receipt.Producer.LaneID != "" {
		t.Fatalf("receipt lane_id = %q, want empty (no-guessing: actor/owner/harness must not resolve to a lane)", res.Receipt.Producer.LaneID)
	}
	if res.Transition.Producer.LaneID != "" {
		t.Fatalf("transition lane_id = %q, want empty (no-guessing)", res.Transition.Producer.LaneID)
	}
}

// F3: a bogus ref and a bare harness are REFUSED at the claim gate (fail-closed) with an error
// naming valid ref forms, and the envelope is left exactly as it was — never claimed, no lease.
func TestF3BogusAndBareHarnessLaneRefusedAtClaim(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedLane(t, s, "fixture/judge") // a real lane exists, so refusal is not "empty store"

	for _, bad := range []string{"bogus-ref", "codex", "fixture"} {
		// Distinct SuccessPredicate per iteration so each Create derives a unique work id.
		w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "fixture proof exists for " + bad}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.ClaimWithLane(w.ID, "some-actor", bad, fixedNow.Add(time.Minute))
		if err == nil {
			t.Fatalf("claim --lane %q: want refusal, got nil", bad)
		}
		if !strings.Contains(err.Error(), "does not resolve") || !strings.Contains(err.Error(), "short lane id") {
			t.Fatalf("claim --lane %q error %q does not name valid ref forms", bad, err.Error())
		}
		got, err := s.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "ready" {
			t.Fatalf("refused claim --lane %q mutated envelope state to %q, want ready (unchanged)", bad, got.State)
		}
		leases, err := s.ListLeases()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range leases {
			if l.WorkID == w.ID {
				t.Fatalf("refused claim --lane %q minted a lease %+v", bad, l)
			}
		}
	}
}

// F5: advance --lane declares (when claim had none) or corrects (when claim had one) the lane
// mid-lifecycle, under the SAME holder-scoped lease rules as any other advance mutation.
func TestF5AdvanceLaneDeclaresAndCorrectsHolderScoped(t *testing.T) {
	t.Parallel()
	laneA := "fixture/judge"
	laneB := "fixture/extract"

	t.Run("declare at advance when claim had no lane", func(t *testing.T) {
		s := newStore(t)
		_, wantB := seedLane(t, s, laneB)
		w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "fixture proof exists"}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(w.ID, "holder", fixedNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "holder", LaneRef: laneB}, fixedNow.Add(2*time.Minute)); err != nil {
			t.Fatalf("advance --lane %q: %v", laneB, err)
		}
		res, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "holder", Summary: "x", ProofRefs: []string{"proof:fixture"}}, fixedNow.Add(3*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Receipt.Producer.LaneID != wantB {
			t.Fatalf("receipt lane_id = %q, want advanced-declared %q", res.Receipt.Producer.LaneID, wantB)
		}
	})

	t.Run("correct at advance overrides the claim lane", func(t *testing.T) {
		s := newStore(t)
		seedLane(t, s, laneA)
		_, wantB := seedLane(t, s, laneB)
		w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "fixture proof exists"}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimWithLane(w.ID, "holder", laneA, fixedNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "holder", LaneRef: laneB}, fixedNow.Add(2*time.Minute)); err != nil {
			t.Fatalf("advance --lane %q correction: %v", laneB, err)
		}
		res, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "holder", Summary: "x", ProofRefs: []string{"proof:fixture"}}, fixedNow.Add(3*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Receipt.Producer.LaneID != wantB {
			t.Fatalf("receipt lane_id = %q, want corrected %q", res.Receipt.Producer.LaneID, wantB)
		}
	})

	t.Run("non-holder advance --lane refused (holder-scoped lease rule)", func(t *testing.T) {
		s := newStore(t)
		_, wantB := seedLane(t, s, laneB)
		w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "fixture proof exists"}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(w.ID, "holder", fixedNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		// A different actor tries to declare a lane without stealing the lease → refused.
		if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "intruder", LaneRef: laneB}, fixedNow.Add(2*time.Minute)); err == nil {
			t.Fatalf("non-holder advance --lane %q: want lease refusal, got nil", laneB)
		}
		// The declared lane must not have been recorded by the refused advance.
		lease, err := s.readLease(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if lease.LaneID == wantB {
			t.Fatalf("refused non-holder advance --lane recorded lane %q on the lease", wantB)
		}
	})
}
