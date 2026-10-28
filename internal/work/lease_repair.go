package work

// Governed lease repair (2026-07-31 incident — work_close_steal_lease_corruption_20260731).
//
// The atomic-steal fix in work.go (see lease_close.go + closeLocked in work.go) prevents
// a refused close --steal from re-pointing the lease. The two wave10 items bricked on
// 2026-07-31 BEFORE the fix landed still carry the corrupt lease state from the
// partial re-point — their work/leases/<id>.json names a stealing actor that never
// actually landed the work, while the work envelope still sits in the original
// holder's pre-close state.
//
// This file is the governed repair door. It diagnoses the lease without mutating
// anything (LeaseRepairPlan), then on confirm rewrites the lease with compare-and-swap
// semantics so a racing claim/advance cannot be silently overwritten, and emits a
// complete ActionIntent + PolicyDecision + Receipt + Transition chain so the repair
// is auditable, not a silent overwrite. Once the lease is restored to the original
// holder, the bricked work can be closed normally (e.g. outcome=cancelled,
// move_type=control_plane_repair) as a clone-defects receipt.
//
// Repair is intentionally narrow:
//   - Capability: work.lease-repair, mutation class local_reversible. Default policy
//     allows this with the standard obligations (emit Receipt + record rollback plan).
//   - Both ExpectedHolder AND ExpectedCurrentHolder are required. The command never
//     guesses authority from history; it requires the operator to have reviewed the
//     corrupt holder value (the one currently on disk) so a racing state change since
//     review aborts the repair without overwriting good state.
//   - The repair does not advance the work; it only restores the lease so a normal
//     lifecycle mutation can proceed against the correct authority.

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// LeaseRepairInput is a compare-and-swap repair of one pre-existing lease projection
// mismatch. Both holder values are required: the command never guesses authority from
// history and refuses if the diagnosed corrupt value has changed since review.
type LeaseRepairInput struct {
	Actor                 string
	ExpectedHolder        string
	ExpectedCurrentHolder string
	Reason                string
}

// LeaseRepairPlan is the read-only diagnostic the operator reviews before confirming.
// No disk writes; safe to run on any work_id.
type LeaseRepairPlan struct {
	WorkID         string `json:"work_id"`
	WorkState      string `json:"work_state"`
	CurrentHolder  string `json:"current_holder"`
	ExpectedHolder string `json:"expected_holder"`
	Repairable     bool   `json:"repairable"`
	NoAction       string `json:"no_action,omitempty"`
}

// LeaseRepairResult carries the full governed chain emitted by a successful repair:
// the rewritten lease, the work envelope with the new ActionIntent/PolicyDecision IDs
// linked, the ActionIntent, PolicyDecision, Receipt, and Transition. Every id is
// content-addressed from the same atomic step, so any downstream consumer (close,
// doctor, projection) can prove the repair by id.
type LeaseRepairResult struct {
	Lease      Lease                   `json:"lease"`
	Work       envelope.WorkEnvelope   `json:"work"`
	Action     envelope.ActionIntent   `json:"action_intent"`
	Decision   envelope.PolicyDecision `json:"policy_decision"`
	Receipt    envelope.Receipt        `json:"receipt"`
	Transition envelope.Transition     `json:"transition"`
}

// LeaseRepairPlan diagnoses the current lease against the operator's expected holder
// and the corrupt current holder. Returns a plan that is either Repairable=true (caller
// may run RepairLease) or NoAction set (already correct, or racing since review).
//
// Reads only: never mutates state.
func (s *Store) LeaseRepairPlan(workID string, in LeaseRepairInput, now time.Time) (LeaseRepairPlan, error) {
	plan := LeaseRepairPlan{WorkID: workID}
	w, err := s.Get(workID)
	if err != nil {
		return plan, err
	}
	plan.WorkState = w.State
	lease, err := s.readLease(workID)
	if err != nil {
		return plan, fmt.Errorf("diagnose lease repair for %s: %w", workID, err)
	}
	plan.CurrentHolder = strings.TrimSpace(lease.By)
	plan.ExpectedHolder = strings.TrimSpace(in.ExpectedHolder)
	if plan.ExpectedHolder == "" {
		return plan, fmt.Errorf("lease repair expected holder required")
	}
	expectedCurrent := strings.TrimSpace(in.ExpectedCurrentHolder)
	if expectedCurrent == "" {
		return plan, fmt.Errorf("lease repair expected current holder required (operator must review the corrupt value currently on disk before confirming)")
	}
	if plan.CurrentHolder != expectedCurrent {
		plan.NoAction = fmt.Sprintf("current holder %q does not match reviewed corrupt holder %q (state changed since review; re-diagnose)", plan.CurrentHolder, expectedCurrent)
		return plan, nil
	}
	if plan.CurrentHolder == plan.ExpectedHolder {
		plan.NoAction = "lease already names expected holder"
		return plan, nil
	}
	plan.Repairable = true
	return plan, nil
}

// RepairLease executes the diagnosed repair under withLifecycleMutation's home-level
// lock (same lock Close/Advance/Claim take, so a racing Claim/Advance is serialized
// either before or after the repair, never interleaved). It mints a complete
// ActionIntent + PolicyDecision + Receipt + Transition chain — the chain is the
// receipt of the clone-defects repair; downstream close as cancelled writes the
// final receipt on top.
//
// Compare-and-swap semantics: re-diagnoses inside the lock and refuses if the live
// holder has changed since the operator's reviewed ExpectedCurrentHolder, so a
// racing Claim/Advance that legitimately took the lease cannot be silently
// overwritten by an operator running on stale review.
func (s *Store) RepairLease(workID string, in LeaseRepairInput, now time.Time) (LeaseRepairResult, error) {
	var out LeaseRepairResult
	err := s.withLifecycleMutation(func() error {
		var err error
		out, err = s.repairLeaseLocked(workID, in, now)
		return err
	})
	return out, err
}

func (s *Store) repairLeaseLocked(workID string, in LeaseRepairInput, now time.Time) (LeaseRepairResult, error) {
	var out LeaseRepairResult
	if err := s.fenceClosePendingMutation(workID); err != nil {
		return out, err
	}
	if err := s.fenceSweepStalePendingMutation(workID); err != nil {
		return out, err
	}
	actor := strings.TrimSpace(in.Actor)
	reason := strings.TrimSpace(in.Reason)
	if actor == "" {
		return out, fmt.Errorf("lease repair actor required")
	}
	if reason == "" {
		return out, fmt.Errorf("lease repair reason required")
	}
	plan, err := s.LeaseRepairPlan(workID, in, now)
	if err != nil {
		return out, err
	}
	if !plan.Repairable {
		return out, fmt.Errorf("cannot repair lease %s: %s", workID, plan.NoAction)
	}
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	lease, err := s.readLease(workID)
	if err != nil {
		return out, err
	}
	summary := fmt.Sprintf("repaired lease holder from %q to %q: %s", plan.CurrentHolder, plan.ExpectedHolder, reason)
	proofRefs := []string{"work:" + workID, "lease:" + workID}
	action := envelope.ActionIntent{
		Header:        newHeader(envelope.SVActionIntent, idFor("act_", "work.lease-repair", workID, actor, ts(now)), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:         actor,
		Capability:    "work.lease-repair",
		Action:        "repair_lease_holder",
		Target:        envelope.ActionTarget{Kind: "work", ID: workID, Path: s.leasePath(workID)},
		MutationClass: "local_reversible",
		Reversibility: "manual",
		BlastRadius:   "local",
		Preview:       summary,
		RollbackPlan:  fmt.Sprintf("restore lease holder to %q", plan.CurrentHolder),
	}
	action.Header.EvidenceRefs = proofRefs
	if err := action.Validate(); err != nil {
		return out, err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return out, err
	}
	decision := policy.Evaluate(action, cfg, now)
	if err := decision.Validate(); err != nil {
		return out, err
	}
	if !policy.Allowed(decision) {
		return out, fmt.Errorf("cannot repair lease %s: policy %s (%s)", workID, decision.Outcome, strings.Join(decision.Reasons, "; "))
	}
	receipt := envelope.Receipt{
		Header:   newHeader(envelope.SVReceipt, idFor("rec_", action.ID, decision.ID), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		StateDelta: envelope.StateDelta{
			Changed:      true,
			Summary:      summary,
			FilesChanged: []string{},
			Artifacts:    []string{},
			Verification: []string{},
		},
		Cost:      envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:    envelope.ReceiptSkills{Used: []string{"cap:work.lease-repair"}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}, Rejected: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: proofRefs,
	}
	if err := receipt.Validate(); err != nil {
		return out, err
	}
	transition := envelope.Transition{
		Header:     newHeader(envelope.SVTransition, idFor("tr_", action.ID, decision.ID), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		BeforeRefs: []string{"lease-holder:" + plan.CurrentHolder},
		ActionRefs: []string{action.ID},
		AfterRefs:  []string{receipt.ID, "lease-holder:" + plan.ExpectedHolder},
		StateDelta: summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: []string{"cap:work.lease-repair"}, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{}, Typed: []envelope.ProjectionRef{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	if err := transition.Validate(); err != nil {
		return out, err
	}

	lease.By = plan.ExpectedHolder
	w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	w.PolicyDecisionID = decision.ID
	if err := s.writeEventJSON(action.ID, &action); err != nil {
		return out, err
	}
	if err := s.writeEventJSON(decision.ID, &decision); err != nil {
		return out, err
	}
	if err := s.writeEventJSON(receipt.ID, &receipt); err != nil {
		return out, err
	}
	if err := s.writeEventJSON(transition.ID, &transition); err != nil {
		return out, err
	}
	if err := s.publishClaimJSON(s.leasePath(workID), &lease); err != nil {
		return out, err
	}
	if err := s.writeJSON(s.workPath(workID), &w); err != nil {
		return out, err
	}
	if err := s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   workID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     summary,
		Attached: []string{action.ID, decision.ID, receipt.ID, transition.ID},
	}); err != nil {
		return out, err
	}
	return LeaseRepairResult{
		Lease:      lease,
		Work:       w,
		Action:     action,
		Decision:   decision,
		Receipt:    receipt,
		Transition: transition,
	}, nil
}
