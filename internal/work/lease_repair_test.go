package work

// Governed lease repair tests (2026-07-31 incident —
// work_close_steal_lease_corruption_20260731).
//
// The atomic-steal fix prevents the brick from recurring. These tests pin the
// governed repair path so the two wave10 items bricked on 2026-07-31
// (work_minimax_wave10_tool-asc, work_minimax_wave10_tool-playtest-edge) can
// be restored to their original holder, then closed as clone-defects
// receipts. The repair itself must be auditable (ActionIntent +
// PolicyDecision + Receipt + Transition) and compare-and-swap (refuses if
// the corrupt holder has changed since operator review).

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Pin the exact work_id strings from the 2026-07-31 incident so the repair
// fixtures and any future operator-side repair scripts converge on the same
// identifiers.
const (
	brickedWave10ToolAsc          = "work_minimax_wave10_tool-asc"
	brickedWave10ToolPlaytestEdge = "work_minimax_wave10_tool-playtest-edge"
)

// SetupBrickedWave10Item simulates the partial re-point corruption from
// 2026-07-31: a work was claimed by actor-a, then a close --steal from
// actor-b passed the lease gate and re-pointed the lease to actor-b BEFORE
// the atomic-steal fix landed. A downstream validation refused the close
// (proof-missing policy deny in the original incident), so the work sat in
// actor-a's pre-close state with a lease naming actor-b. Returns the
// (claimed, then advanced) work, the actor names, and the state immediately
// before repair.
func setupBrickedWave10Item(t *testing.T, s *Store, workID string) (actorA, actorB string) {
	t.Helper()
	actorA = "lane-original"
	actorB = "lane-stealer"
	w, err := s.Create(CreateInput{
		Owner:            actorA,
		SuccessPredicate: "wave10 brick recovery",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Overwrite the minted ID with the bricked identifier so the fixtures
	// match the incident verbatim. CreateInput does not take WorkID, so we
	// rely on the test's workID only for downstream operator-side
	// diagnostics; the actual on-disk lease and work files use the minted
	// ID. The test's assertions are on the live store state, not on the
	// literal name string.
	_ = workID
	if _, err := s.Claim(w.ID, actorA, fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Leave the work in "claimed" — both "claimed" -> "cancelled" and the
	// lease-holder check are reachable without operator-provenance: claimed
	// state does not invoke heldReleaseOperatorError. This mirrors the
	// realistic 2026-07-31 partial re-point path: the close --steal happened
	// before any state advance committed, so the work never left "claimed".
	_ = fixedNow.Add(time.Minute)
	// Simulate the partial re-point: re-point the lease to actor-b exactly
	// the way the broken pre-fix close --steal would have. The work itself
	// stays in "running" because the close never landed — that is the brick.
	w, err = s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	originalHolder := lease.By
	lease.By = actorB
	if err := s.publishClaimJSON(s.leasePath(w.ID), &lease); err != nil {
		t.Fatalf("simulate partial re-point: %v", err)
	}
	// Sanity: the work state is unchanged (the close never landed); only the
	// lease was re-pointed.
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get after re-point: %v", err)
	}
	if after.State != w.State {
		t.Fatalf("simulation must not change work state: got %q want %q", after.State, w.State)
	}
	gotLease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read lease after re-point: %v", err)
	}
	if gotLease.By != actorB {
		t.Fatalf("simulation failed: lease holder = %q, want %q", gotLease.By, actorB)
	}
	if originalHolder != actorA {
		t.Fatalf("test invariant: original holder must be %q, got %q", actorA, originalHolder)
	}
	return actorA, actorB
}

// TestLeaseRepairDiagnosesAndGovernedlyRestoresExpectedHolder: a bricked
// wave10-style item is diagnosed without mutation, then repaired. The
// repair emits the full governed chain and links it back into the work
// envelope's ActionIntentIDs / PolicyDecisionID.
func TestLeaseRepairDiagnosesAndGovernedlyRestoresExpectedHolder(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "repair stale close-steal lease"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get claimed: %v", err)
	}
	// Reproduce the bricked state directly via the in-package seam (the
	// production path the atomic fix protects against).
	if err := s.recordLeaseSteal(claimed, "actor-b", "actor-a", "close later refused", "work.close", "close_lease_stolen", true, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("simulate brick via recordLeaseSteal: %v", err)
	}
	// Plan must be read-only and report repairable=true with the diagnosed
	// current holder matching the operator's reviewed corrupt value.
	plan, err := s.LeaseRepairPlan(w.ID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        "actor-a",
		ExpectedCurrentHolder: "actor-b",
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if !plan.Repairable || plan.CurrentHolder != "actor-b" || plan.ExpectedHolder != "actor-a" {
		t.Fatalf("unexpected repair plan: %+v", plan)
	}
	// Plan must not have mutated the lease.
	lease, err := s.readLease(w.ID)
	if err != nil || lease.By != "actor-b" {
		t.Fatalf("plan mutated lease: holder=%q err=%v", lease.By, err)
	}

	// RepairLease mints the full governed chain.
	result, err := s.RepairLease(w.ID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        "actor-a",
		ExpectedCurrentHolder: "actor-b",
		Reason:                "restore holder after refused close steal",
	}, fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if result.Lease.By != "actor-a" {
		t.Fatalf("lease not restored: holder=%q want actor-a", result.Lease.By)
	}
	if result.Action.ID == "" || result.Decision.ID == "" || result.Receipt.ID == "" || result.Transition.ID == "" {
		t.Fatalf("repair result missing governed chain: %+v", result)
	}
	if result.Decision.ActionIntentID != result.Action.ID {
		t.Fatalf("repair decision does not reference action: %+v", result.Decision)
	}
	if len(result.Transition.ActionRefs) == 0 || result.Transition.ActionRefs[0] != result.Action.ID {
		t.Fatalf("repair transition does not reference action: %+v", result.Transition)
	}
	repaired, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get repaired: %v", err)
	}
	if repaired.PolicyDecisionID != result.Decision.ID {
		t.Fatalf("work does not link repair policy decision: have=%q want=%q", repaired.PolicyDecisionID, result.Decision.ID)
	}
	if repaired.ActionIntentIDs[len(repaired.ActionIntentIDs)-1] != result.Action.ID {
		t.Fatalf("work does not link repair action: %+v", repaired.ActionIntentIDs)
	}
	if !policyAllowedRepair(result.Decision.Outcome) {
		t.Fatalf("repair policy must be allowed: outcome=%q", result.Decision.Outcome)
	}
}

// TestLeaseRepairRefusesStaleExpectedCurrentHolderWithoutMutation: a
// diagnosis whose reviewed corrupt holder no longer matches the live lease
// is refused. The refusal must leave the lease byte-identical so a racing
// Claim/Advance that legitimately took the lease is never overwritten by
// an operator running on stale review.
func TestLeaseRepairRefusesStaleExpectedCurrentHolderWithoutMutation(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "repair compare and swap"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := s.recordLeaseSteal(claimed, "actor-b", "actor-a", "first steal", "work.close", "close_lease_stolen", true, fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("simulate brick: %v", err)
	}
	before := snapshotLifecycleAuthority(t, s)
	// Stale review: ExpectedCurrentHolder names a holder that is no longer
	// the live one (someone else legitimately re-claimed in between).
	_, err = s.RepairLease(w.ID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        "actor-a",
		ExpectedCurrentHolder: "actor-zzz-not-current",
		Reason:                "stale diagnosis",
	}, fixedNow.Add(2*time.Minute))
	if err == nil {
		t.Fatal("expected compare-and-swap refusal")
	}
	assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
}

// TestLeaseRepairThenCancelAsCloneDefects: the full wave10 recovery
// path. A partial-re-pointed item is repaired, then the original holder
// can close it as cancelled with a clone-defects receipt so the brick is
// auditable end to end. This is the operator-facing recovery the 2026-07-31
// incident motivated.
func TestLeaseRepairThenCancelAsCloneDefects(t *testing.T) {
	s := newStore(t)
	actorA, actorB := setupBrickedWave10Item(t, s, brickedWave10ToolAsc)
	workID := mustOnlyWorkID(t, s)

	// Step 1: governedly restore the lease.
	repair, err := s.RepairLease(workID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        actorA,
		ExpectedCurrentHolder: actorB,
		Reason:                "2026-07-31 partial re-point brick recovery for " + brickedWave10ToolAsc,
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repair.Lease.By != actorA {
		t.Fatalf("repair did not restore lease: holder=%q want=%q", repair.Lease.By, actorA)
	}

	// Step 2: original holder cancels as a clone-defects receipt. After the
	// repair, actor-a is once again the legitimate holder, so this close is
	// allowed by the lease gate (no --steal needed). The receipt carries
	// the brick recovery note and links the repair chain so any future
	// doctor/audit/projection can trace the brick -> repair -> cancel arc.
	res, err := s.Close(workID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "control_plane_repair",
		Actor:    actorA,
		Changed:  true,
		Summary:  "wave10 item bricked on 2026-07-31 by partial re-point; restored via lease repair and cancelled as clone-defects receipt",
		ProofRefs: []string{
			"repair:" + repair.Action.ID,
			"receipt:" + repair.Receipt.ID,
		},
	}, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("close after repair: %v", err)
	}
	if res.Work.State != "cancelled" {
		t.Fatalf("close after repair did not land terminal: state=%q want=cancelled", res.Work.State)
	}
	if !res.Receipt.StateDelta.Changed {
		t.Fatalf("cancel receipt must mark changed=true (the repair DID change state): %+v", res.Receipt.StateDelta)
	}
	if !strings.Contains(res.Receipt.StateDelta.Summary, "wave10") {
		t.Fatalf("clone-defects receipt summary must reference the wave10 incident for traceability: %q", res.Receipt.StateDelta.Summary)
	}
	// Final invariant: lease released on terminal close, work terminal, no
	// residue (same posture as any clean close).
	if _, err := s.readLease(workID); err == nil {
		t.Fatalf("terminal close must release lease; found residue for %s", workID)
	}
	if !strings.Contains(res.Receipt.MoveType, "control_plane_repair") {
		t.Fatalf("clone-defects receipt must use control_plane_repair move_type: %q", res.Receipt.MoveType)
	}
}

// TestLeaseRepairPlanIsReadOnly: LeaseRepairPlan must not mutate the
// lifecycle bytes regardless of outcome. This is the operator's pre-confirm
// gate; if it ever wrote, the read-only contract would be violated.
func TestLeaseRepairPlanIsReadOnly(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "plan read-only"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := s.recordLeaseSteal(claimed, "actor-b", "actor-a", "diag", "work.close", "close_lease_stolen", true, fixedNow); err != nil {
		t.Fatalf("simulate brick: %v", err)
	}
	before := snapshotLifecycleAuthority(t, s)
	for i := 0; i < 3; i++ {
		plan, err := s.LeaseRepairPlan(w.ID, LeaseRepairInput{
			Actor:                 "op",
			ExpectedHolder:        "actor-a",
			ExpectedCurrentHolder: "actor-b",
		}, fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil || !plan.Repairable {
			t.Fatalf("plan iteration %d failed: plan=%+v err=%v", i, plan, err)
		}
	}
	assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
}

// policyAllowedRepair reports whether the policy outcome returned by the
// repair seam is allowed. The default policy maps mutation_class
// local_reversible to allow_with_obligations, which is allowed.
func policyAllowedRepair(outcome string) bool {
	return outcome == "allow" || outcome == "allow_with_obligations"
}

// mustOnlyWorkID returns the single work_id the test fixture has on disk.
// The setup helper does not take a workID, so the test discovers it after
// seeding. Tests that need the literal bricked name keep the constant for
// diagnostics.
func mustOnlyWorkID(t *testing.T, s *Store) string {
	t.Helper()
	leases, err := s.ListLeases()
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("expected exactly one lease on disk, got %d", len(leases))
	}
	return leases[0].WorkID
}

func TestLeaseRepairRefusesFrozenCloseAuthorityWithoutMutation(t *testing.T) {
	s := newStore(t)
	const holder = "actor-a"
	workID := prepareClaimedWork(t, s, holder, "", "a frozen close fences lease repair")
	frozen := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    holder,
		Summary:  "freeze close before lease repair",
	}, fixedNow.Add(time.Minute))
	beforeAuthority := snapshotLifecycleAuthority(t, s)
	beforeAttempts := runAttemptsByID(t, s)

	_, err := s.RepairLease(workID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        "actor-b",
		ExpectedCurrentHolder: holder,
		Reason:                "must not overtake the frozen close generation",
	}, fixedNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "close recovery_required") {
		t.Fatalf("repair over frozen close %s/%s error = %v, want close recovery_required", frozen.Action.ID, frozen.Decision.ID, err)
	}
	if !strings.Contains(err.Error(), frozen.Action.ID) {
		t.Fatalf("repair fence error %q does not bind frozen close action %s", err, frozen.Action.ID)
	}
	assertLifecycleAuthorityUnchanged(t, beforeAuthority, snapshotLifecycleAuthority(t, s))
	afterAttempts := runAttemptsByID(t, s)
	if !reflect.DeepEqual(beforeAttempts, afterAttempts) {
		t.Fatalf("repair over frozen close changed RunAttempts\nbefore=%+v\nafter=%+v", beforeAttempts, afterAttempts)
	}
}
