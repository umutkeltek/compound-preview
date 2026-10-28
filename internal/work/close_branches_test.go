package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// Falsifier (write door): Close carries CloseInput.OpensBranches onto the
// emitted Transition AND into the persisted tr_*.json record — the model's
// declared branch survives the whole mint path.
func TestCloseAttachesOpensBranchesToTransition(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "branch-lifecycle", SuccessPredicate: "baseline close stays branchless"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	closeWorkbenchTestWork(t, s, w.ID, fixedNow, "baseline close without branches")

	// Re-open a second work and close it WITH branches.
	w2, err := s.Create(CreateInput{Owner: "branch-lifecycle", SuccessPredicate: "close mints declared branches"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w2.ID, "codex", fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w2.ID, AdvanceInput{To: "running"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w2.ID, AdvanceInput{To: "verifying", VerificationID: "ver_" + w2.ID}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Close(w2.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "rule_delta",
		Summary:   "branch declared at close",
		Changed:   true,
		ProofRefs: []string{"check:branch-write-door"},
		OpensBranches: []envelope.BranchRef{
			{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "opens"},
		},
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transition.OpensBranches) != 1 || res.Transition.OpensBranches[0].Kind != "opens" {
		t.Fatalf("close result transition lost opens_branches: %+v", res.Transition.OpensBranches)
	}

	// The persisted record must carry the branch too (coverage reads records, not results).
	var persisted envelope.Transition
	if err := s.readEventJSON(res.Transition.ID, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.OpensBranches) != 1 || persisted.OpensBranches[0].Branch != "branch-lifecycle" {
		t.Fatalf("persisted transition lost opens_branches: %+v", persisted.OpensBranches)
	}
	if err := persisted.Validate(); err != nil {
		t.Fatalf("persisted branched transition invalid: %v", err)
	}
}

// Falsifier (fail-closed door): an invalid branch ref (bogus kind) makes Close
// refuse — the mint validates before anything is persisted.
func TestCloseRefusesInvalidBranchRef(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "branch-lifecycle", SuccessPredicate: "invalid branch refused"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", VerificationID: "ver_" + w.ID}, fixedNow); err != nil {
		t.Fatal(err)
	}
	_, err = s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "bogus branch kind",
		Changed:   true,
		ProofRefs: []string{"proof:" + w.ID},
		OpensBranches: []envelope.BranchRef{
			{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "frobnicate"},
		},
	}, fixedNow)
	if err == nil {
		t.Fatalf("close must refuse an invalid branch kind")
	}
	if !strings.Contains(err.Error(), "branch") {
		t.Fatalf("error should name the branch problem: %v", err)
	}
	// Fail-closed: the refused close must not have terminalized the work.
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State == "completed" {
		t.Fatalf("refused close must not terminalize the work")
	}
}
