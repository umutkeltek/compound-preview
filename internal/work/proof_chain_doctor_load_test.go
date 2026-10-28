package work

import "testing"

// TestProofChainDoctorLoadsWorkLogExactlyOnce is the slice-5 counting-seam
// falsifier: ProofChainDoctor must read the authoritative events/work.jsonl
// exactly once per invocation, no matter how many work items exist. Before the
// eventsByWorkID batch cutover it read the whole log once per terminal item
// (proofChainItem -> eventsFor) PLUS once per work item (the orphan scan), so
// the delta grew with the ledger. The counter (Store.eventLogReads, exposed via
// eventLogReadCount) is the same seam workbench.go already batches through.
//
// Red at base: with 3 works (2 terminal, 1 open) the base delta is 5
// (2 terminal proofChainItem reads + 3 orphan-scan reads). Green after: 1.
func TestProofChainDoctorLoadsWorkLogExactlyOnce(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	// Two terminal works (each drives a proofChainItem read at base) and one
	// open work (drives only an orphan-scan read at base) so the base delta is
	// strictly greater than 1 and grows with the ledger.
	for i, pred := range []string{"terminal one", "terminal two"} {
		w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: pred}, fixedNow)
		if err != nil {
			t.Fatalf("create terminal %d: %v", i, err)
		}
		if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
			t.Fatalf("claim terminal %d: %v", i, err)
		}
		if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
			t.Fatalf("advance terminal %d: %v", i, err)
		}
		if _, err := s.Close(w.ID, CloseInput{
			Outcome:   "completed",
			MoveType:  "control_plane_repair",
			Summary:   "closed",
			ProofRefs: []string{"repair:proof-chain-load"},
		}, fixedNow); err != nil {
			t.Fatalf("close terminal %d: %v", i, err)
		}
	}
	if _, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "open one"}, fixedNow); err != nil {
		t.Fatalf("create open: %v", err)
	}

	before := s.eventLogReadCount()
	if _, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{}); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	delta := s.eventLogReadCount() - before
	if delta != 1 {
		t.Fatalf("ProofChainDoctor read events/work.jsonl %d times, want exactly 1 (batch seam not used)", delta)
	}
}
