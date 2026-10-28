package work

import "testing"

func TestProofChainForWorkScopesCanonicalChecksToRequestedWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	valid, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "target proof chain"}, fixedNow)
	if err != nil {
		t.Fatalf("create valid work: %v", err)
	}
	if _, err := s.Claim(valid.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim valid work: %v", err)
	}
	if _, err := s.Advance(valid.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow); err != nil {
		t.Fatalf("advance valid work: %v", err)
	}
	closed, err := s.Close(valid.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "target chain complete",
		ProofRefs: []string{"repair:target-proof-chain"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("close valid work: %v", err)
	}

	broken, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "unrelated broken proof chain"}, fixedNow)
	if err != nil {
		t.Fatalf("create broken work: %v", err)
	}
	broken.State = "completed"
	if err := s.writeJSON(s.workPath(broken.ID), &broken); err != nil {
		t.Fatalf("write broken work: %v", err)
	}

	item, err := s.ProofChainForWork(valid.ID)
	if err != nil {
		t.Fatalf("target proof: %v", err)
	}
	if !item.ProofComplete || item.WorkID != valid.ID || item.ReceiptID != closed.Receipt.ID || item.TransitionID != closed.Transition.ID || len(item.Problems) != 0 {
		t.Fatalf("valid target proof = %+v", item)
	}

	brokenItem, err := s.ProofChainForWork(broken.ID)
	if err != nil {
		t.Fatalf("broken target proof: %v", err)
	}
	if brokenItem.ProofComplete || len(brokenItem.Problems) == 0 {
		t.Fatalf("broken target must retain canonical findings: %+v", brokenItem)
	}
}
