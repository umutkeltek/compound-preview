package work

// Dispatch-surface lease enforcement (2026-07-09 packet). WorkbenchItem already carried a
// live lease projection (ClaimedBy, workbench.go:678-681) for every state, not just
// "claimed" — a lease is not deleted when a work cycles claimed -> ready, or when it
// advances to running/verifying, so residue reliably survives. DispatchPlan/DispatchClaim
// never consulted it for "ready" candidates, and DispatchClosePlan/DispatchClose never
// consulted it for "verifying" candidates — the exact live incident (DispatchClose closes
// ANY actor's verifying work) named in docs_private/plan/packets/2026-07-09-close-gate-lease.md.

import (
	"strings"
	"testing"
)

// leaseResidueReadyWork creates a work, claims it as holder, then cycles it back to
// "ready" without releasing the lease — the realistic shape of lease residue on a ready
// item (Advance never deletes the lease file).
func leaseResidueReadyWork(t *testing.T, s *Store, holder string) string {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "lease residue on ready"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, holder, fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "ready", Actor: holder}, fixedNow); err != nil {
		t.Fatalf("advance back to ready: %v", err)
	}
	return w.ID
}

// Falsifier 6: dispatch-plan candidate enumeration excludes a ready item held by another
// actor from claim eligibility, keeps it visible (no silent shrinkage), and reports the
// skipped count.
func TestDispatchPlanExcludesReadyItemHeldByOtherActor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held := leaseResidueReadyWork(t, s, "actor-a")

	plan, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{Actor: "actor-b", Record: false})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selected != nil {
		t.Fatalf("held-by-other item must not be selected for claim: %+v", plan.Selected)
	}
	found := false
	for _, c := range plan.Candidates {
		if c.WorkID != held {
			continue
		}
		found = true
		if c.Action == "claim" {
			t.Fatalf("held-by-other candidate must not offer a claim action: %+v", c)
		}
		if c.ClaimedBy != "actor-a" {
			t.Fatalf("candidate must carry the live lease holder: %+v", c)
		}
	}
	if !found {
		t.Fatal("held-by-other item dropped from candidates (silent shrinkage) instead of staying visible")
	}
	skipReported := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "skipped") && strings.Contains(w, "held by other actors") {
			skipReported = true
		}
	}
	if !skipReported {
		t.Fatalf("dispatch-plan must report the skip count in Warnings: %+v", plan.Warnings)
	}

	// With no lease at all, the same item claims as today.
	s2 := newStore(t)
	ready, err := s2.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "no lease"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := s2.DispatchPlan(fixedNow, DispatchPlanOptions{Actor: "actor-b", Record: false})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Selected == nil || plan2.Selected.WorkID != ready.ID || plan2.Selected.Action != "claim" {
		t.Fatalf("unclaimed ready work must still be claimable: %+v", plan2.Selected)
	}
}

// Falsifier 5: dispatch-claim with a ready item held by another actor does NOT claim it
// and reports the skip.
func TestDispatchClaimSkipsReadyItemHeldByOtherActor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held := leaseResidueReadyWork(t, s, "actor-a")

	result, err := s.DispatchClaim(fixedNow, DispatchPlanOptions{Actor: "actor-b"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ClaimedWorkID != "" {
		t.Fatalf("dispatch-claim must not claim work held by another actor: %+v", result)
	}
	after, err := s.Get(held)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" || after.Authority.Owner == "actor-b" {
		t.Fatalf("held work must be left untouched by dispatch-claim: %+v", after)
	}
	skipReported := false
	for _, w := range result.Plan.Warnings {
		if strings.Contains(w, "skipped") && strings.Contains(w, "held by other actors") {
			skipReported = true
		}
	}
	if !skipReported {
		t.Fatalf("dispatch-claim must report the skip via its plan warnings: %+v", result.Plan.Warnings)
	}
}

// Bonus (decision 9): dispatch-close-plan/dispatch-close must respect the same lease for
// verifying work — this is the exact "closes ANY actor's verifying work" live incident
// mechanism the packet names (DispatchClose, dispatch_plan.go).
func TestDispatchCloseNeverAutoClosesVerifyingWorkHeldByOtherActor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "verifying held by other"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-a"}, fixedNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "actor-a", ArtifactID: "artifact_x", VerificationID: "ver_x"}, fixedNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	closePlan, err := s.DispatchClosePlan(fixedNow, DispatchClosePlanOptions{Actor: "actor-b"})
	if err != nil {
		t.Fatal(err)
	}
	if closePlan.Selected != nil {
		t.Fatalf("held-by-other verifying item must not be selected for close planning: %+v", closePlan.Selected)
	}
	skipReported := false
	for _, warn := range closePlan.Warnings {
		if strings.Contains(warn, "skipped") && strings.Contains(warn, "held by other actors") {
			skipReported = true
		}
	}
	if !skipReported {
		t.Fatalf("dispatch-close-plan must report the skip: %+v", closePlan.Warnings)
	}

	closed, err := s.DispatchClose(fixedNow, DispatchCloseOptions{Actor: "actor-b"})
	if err != nil {
		t.Fatal(err)
	}
	if closed.ClosedWorkID != "" {
		t.Fatalf("dispatch-close must never auto-close work held by another actor: %+v", closed)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "verifying" {
		t.Fatalf("held-by-other verifying work must be left untouched: got state %q", after.State)
	}
}
