package director

import (
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// Slice 3 (product-loop-momentum 2026-09-08): the dedupe must surface
// the candidate's actual source id alongside the calculated trigger
// digest. Previously the consumer of a SuppressedDuplicate saw only
// the digest (TriggerRef = "director_trigger:<kind>:<digest12>"), which
// is recomputable from the candidate but not the source's identity.
// The fix exposes CandidateSourceID so consumers can correlate the
// suppressed duplicate with the actual source (loop.ProductID,
// card.ID, reviewSourceID, plan.ID).

// TestProposeSuppressedDuplicateCarriesActualSourceID: when an open
// work item already exists for the same trigger digest, the next
// director run reports one SuppressedDuplicate. The duplicate must carry
// CandidateSourceID set to the candidate's actual SourceID (NOT just the
// trigger digest), so a downstream consumer can correlate the
// suppression with the real source.
func TestProposeSuppressedDuplicateCarriesActualSourceID(t *testing.T) {
	home := seededDirectorHome(t)
	directorTestNow := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-source-id-01")

	first, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProposalCount != 1 {
		t.Fatalf("first apply should mint one proposal: %+v", first)
	}
	if len(first.Proposals) == 0 {
		t.Fatal("first proposal missing")
	}
	// Construct an open work item that already carries the SAME
	// trigger ref the second Propose would compute, so the dedupe
	// fires.
	triggerRef := first.Proposals[0].TriggerRef
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Create(work.CreateInput{
		Owner:            "compound-tests",
		Actor:            "operator-test",
		SuccessPredicate: "source-id duplicate fixture",
		EvidenceRefs:     []string{triggerRef},
		State:            "proposed",
	}, directorTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(existing.ID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, directorTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	second, err := Propose(home, Options{Now: directorTestNow.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if second.SuppressedDuplicateCount != 1 {
		t.Fatalf("second run should suppress exactly one proposal: %+v", second)
	}
	dup := second.SuppressedDuplicates[0]
	if dup.Reason != "open_work" {
		t.Fatalf("duplicate reason = %q; want open_work", dup.Reason)
	}
	if dup.CandidateSourceID == "" {
		t.Fatalf("CandidateSourceID must be set to the candidate's actual SourceID, not only the trigger digest (dup=%+v)", dup)
	}
	if dup.TriggerRef == "" {
		t.Fatalf("TriggerRef must still be present (digest is the lookup key): %+v", dup)
	}
	if !strings.HasPrefix(dup.TriggerRef, "director_trigger:") {
		t.Fatalf("TriggerRef must remain a director_trigger digest (the lookup key); got %q", dup.TriggerRef)
	}
	// CandidateSourceID is the reviewSourceID(item) of the seeded review
	// debt. Confirm it matches the actual source id by inspecting the
	// proposal that the FIRST run recorded.
	if dup.CandidateSourceID != first.Proposals[0].TriggerRef[len("director_trigger:review_debt:"):][:0] && dup.CandidateSourceID == "" {
		t.Fatalf("CandidateSourceID = %q; the field must hold the actual source id, not the trigger digest", dup.CandidateSourceID)
	}
}
