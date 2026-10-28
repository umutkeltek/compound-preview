package act

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// Only this exact historical request can be reconciled with current transport
// code. New retirement requests always pin the current program.
const sourceMarkerLegacyProgramSHA256 = "b517afa4728bf99bc9a1e347b186641ea921f2cbb2ae7df9adb81ba854c51487"

func sourceMarkerRequestProgram(r policy.SourceMarkerRetirementRequest, operation string) string {
	if operation == "reconcile" && r.ProgramSHA256 == sourceMarkerLegacyProgramSHA256 {
		return sourceMarkerLegacyProgramSHA256
	}
	return SourceMarkerRetirementProgramSHA256()
}

// SourceMarkerRecoveryOrigin names actual immutable native proof. It never
// supplies a replacement decision, observation or credential projection.
type SourceMarkerRecoveryOrigin struct {
	PlanRef                string `json:"plan_ref"`
	WorkID                 string `json:"work_id"`
	ActionID               string `json:"action_id"`
	AttemptID              string `json:"attempt_id"`
	ReceiptID              string `json:"receipt_id"`
	TransitionID           string `json:"transition_id"`
	RequestSHA256          string `json:"request_sha256"`
	TransportProgramSHA256 string `json:"transport_program_sha256,omitempty"`
}

func (r SourceMarkerRecoveryOrigin) proofRefs() []string {
	refs := []string{r.PlanRef, r.ActionID, r.AttemptID, "sha256:" + r.RequestSHA256, r.ReceiptID, r.TransitionID}
	if r.TransportProgramSHA256 != "" {
		refs = append(refs, "transport-program-sha256:"+r.TransportProgramSHA256)
	}
	return refs
}
func sourceMarkerOwner(operation string) string {
	if operation == "retire" {
		return sourceMarkerRetirementOwner
	}
	return sourceMarkerRecoveryOwner
}
func sourceMarkerRecoveryWorkID(action, attempt string) string {
	return idFor("work_", "source-marker-recovery", action, attempt)
}
func sourceMarkerOperationValid(operation string, origin *SourceMarkerRecoveryOrigin) bool {
	return (operation == "retire" && origin == nil) || ((operation == "reconcile") && origin != nil)
}
func sourceMarkerValidateRequest(r policy.SourceMarkerRetirementRequest, operation string, now time.Time) error {
	if operation == "retire" {
		return policy.ValidateSourceMarkerRetirementRequest(r, SourceMarkerRetirementProgramSHA256(), now)
	}
	return policy.ValidateSourceMarkerRecoveryRequest(r, sourceMarkerRequestProgram(r, operation), now)
}
func sourceMarkerOperationIntent(r policy.SourceMarkerRetirementRequest, workID, actor, workSHA, operation string, origin *SourceMarkerRecoveryOrigin, now time.Time) envelope.ActionIntent {
	ai := sourceMarkerRetirementIntent(r, workID, actor, workSHA, now)
	if operation != "retire" && origin != nil {
		ai.Capability = policy.SourceMarkerRecoveryCapability
		ai.Action = "reconcile_source_comparison_marker"
		ai.ArgumentsHash = "sha256:" + policy.SourceMarkerRecoveryDigest(r, origin.ActionID, origin.AttemptID, operation)
		ai.ID = idFor("act_", workID, actor, ai.ArgumentsHash, ts(now))
		ai.EvidenceRefs = append(ai.EvidenceRefs, origin.proofRefs()...)
	}
	return ai
}
func sourceMarkerEvaluate(ai envelope.ActionIntent, r policy.SourceMarkerRetirementRequest, operation string, origin *SourceMarkerRecoveryOrigin, cfg policy.Config, now time.Time) (envelope.PolicyDecision, bool) {
	if operation == "retire" {
		return policy.EvaluateSourceMarkerRetirement(ai, r, SourceMarkerRetirementProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	}
	if origin == nil || origin.TransportProgramSHA256 != SourceMarkerRetirementProgramSHA256() {
		return envelope.PolicyDecision{}, false
	}
	return policy.EvaluateSourceMarkerRecovery(ai, r, origin.ActionID, origin.AttemptID, operation, sourceMarkerRequestProgram(r, operation), cfg, policy.EvaluationContext{}, now)
}
func (v *SourceMarkerRetirer) planAuthority(r policy.SourceMarkerRetirementRequest, operation string, origin *SourceMarkerRecoveryOrigin, workID string, now time.Time) error {
	if !sourceMarkerOperationValid(operation, origin) {
		return sourceMarkerRetirementError("operation_binding")
	}
	if operation == "retire" {
		return v.witness(r, now)
	}
	if origin.TransportProgramSHA256 != SourceMarkerRetirementProgramSHA256() {
		return sourceMarkerRetirementError("transport_program_binding")
	}
	return v.recoveryAuthority(r, *origin, workID)
}

// PlanRecovery first repairs only interrupted local native finalization. Remote
// recovery still requires a fresh exact approval on a separate claimed Work.
func (v *SourceMarkerRetirer) PlanRecovery(originalPlan, operation, workID, actor string) (SourceMarkerRetirementPlanResult, error) {
	var out SourceMarkerRetirementPlanResult
	if operation != "reconcile" {
		return out, sourceMarkerRetirementError("recovery_operation")
	}
	p, ai, err := v.loadPlan(originalPlan, actor)
	if err != nil || p.Operation != "retire" {
		return out, sourceMarkerRetirementError("original_plan")
	}
	release, err := v.executionLock(ai.ID)
	if err != nil {
		return out, err
	}
	defer release()
	s, err := work.Open(v.home)
	if err != nil {
		return out, err
	}
	in, at, present, err := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
	if err != nil || !present || in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.Recovery == nil || in.RequiredAttemptCompletion.Recovery.WorkID != workID {
		return out, sourceMarkerRetirementError("original_reservation")
	}
	attempts := runattempt.Open(v.home)
	attempt, err := attempts.Get(in.RequiredAttemptCompletion.AttemptID)
	if err != nil || attempt.ActionIntentID != ai.ID || attempt.WorkID != p.WorkID || attempt.Launch == nil || (attempt.Launch.CommittedAt == nil && attempt.Launch.StartErrorAt != nil) {
		return out, sourceMarkerRetirementError("original_launch")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, sourceMarkerRetirementError("original_terminal_without_outcome")
		}
		if in.PreEvaluatedDecision == nil {
			return out, sourceMarkerRetirementError("original_policy")
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, _ := v.finish(SourceMarkerRetirementResult{RunAttemptID: attempt.ID, ApprovalConsumed: true, RecoveryWorkID: workID}, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &sourceMarkerRetirementReservation{input: in, authorizedAt: at, launched: true}, cleanup)
		cancel()
		if result.ReceiptID == "" || result.TransitionID == "" {
			return out, sourceMarkerRetirementError("original_finalization")
		}
		in, at, present, err = s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
		if err != nil || !present {
			return out, sourceMarkerRetirementError("original_reservation")
		}
	}
	if in.Outcome != "failed" || in.ChangeKnowledge != "unknown" {
		return out, sourceMarkerRetirementError("original_not_uncertain")
	}
	completed, err := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if !errors.Is(err, work.ErrRequiredAttemptCompletion) || completed.Receipt.ID == "" {
		return out, sourceMarkerRetirementError("original_native_proof")
	}
	if _, err = attempts.CompleteWithOwnerProof(attempt.ID, runattempt.StatusFailed, sourceMarkerRetirementOwner, completed.Receipt.ID, completed.Transition.ID, v.now().UTC()); err != nil {
		return out, sourceMarkerRetirementError("original_attempt_proof")
	}
	origin := &SourceMarkerRecoveryOrigin{PlanRef: originalPlan, WorkID: p.WorkID, ActionID: ai.ID, AttemptID: attempt.ID, ReceiptID: completed.Receipt.ID, TransitionID: completed.Transition.ID, RequestSHA256: p.RequestSHA256, TransportProgramSHA256: SourceMarkerRetirementProgramSHA256()}
	return v.plan(p.Request, workID, actor, operation, origin)
}

func (v *SourceMarkerRetirer) recoveryAuthority(request policy.SourceMarkerRetirementRequest, origin SourceMarkerRecoveryOrigin, recoveryWork string) error {
	p, ai, err := v.loadPlan(origin.PlanRef, "")
	// The immutable plan carries the actor. Loading through an arbitrary actor
	// is forbidden, so recover that public metadata before exact plan validation.
	if err != nil {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return sourceMarkerRetirementError("original_plan")
		}
		raw, e := store.Get(origin.PlanRef)
		if e != nil || len(raw) > 65536 || json.Unmarshal(raw, &p) != nil {
			return sourceMarkerRetirementError("original_plan")
		}
		p, ai, err = v.loadPlan(origin.PlanRef, p.Actor)
	}
	if err != nil || p.Operation != "retire" || p.WorkID != origin.WorkID || ai.ID != origin.ActionID || p.RequestSHA256 != origin.RequestSHA256 || policy.SourceMarkerRetirementDigest(request) != p.RequestSHA256 || recoveryWork != sourceMarkerRecoveryWorkID(ai.ID, origin.AttemptID) {
		return sourceMarkerRetirementError("original_binding")
	}
	s, err := work.Open(v.home)
	if err != nil {
		return sourceMarkerRetirementError("original_work")
	}
	in, _, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present || in.OutcomePending || in.Outcome != "failed" || in.ChangeKnowledge != "unknown" || in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.AttemptID != origin.AttemptID || in.RequiredAttemptCompletion.Owner != sourceMarkerRetirementOwner || in.RequiredAttemptCompletion.Recovery == nil || in.RequiredAttemptCompletion.Recovery.WorkID != recoveryWork || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return sourceMarkerRetirementError("original_reservation")
	}
	attempt, err := runattempt.Open(v.home).Get(origin.AttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed || attempt.TerminalizationOwner != sourceMarkerRetirementOwner || attempt.ActionIntentID != origin.ActionID || attempt.WorkID != origin.WorkID || attempt.TerminalizationReceiptID != origin.ReceiptID || attempt.TerminalizationTransitionID != origin.TransitionID {
		return sourceMarkerRetirementError("original_attempt")
	}
	events, err := eventstore.Open(v.home)
	if err != nil {
		return sourceMarkerRetirementError("original_proof")
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	r, e1 := events.Read(origin.ReceiptID)
	t, e2 := events.Read(origin.TransitionID)
	if e1 != nil || e2 != nil || json.Unmarshal(r.Bytes, &receipt) != nil || json.Unmarshal(t.Bytes, &transition) != nil || receipt.Validate() != nil || transition.Validate() != nil || receipt.Outcome != "failed" || receipt.StateDelta.ChangeKnowledge != "unknown" || receipt.IDs.WorkID != origin.WorkID || transition.IDs.WorkID != origin.WorkID {
		return sourceMarkerRetirementError("original_proof")
	}
	return nil
}
func (v *SourceMarkerRetirer) clearRecoveredOriginal(origin SourceMarkerRecoveryOrigin) error {
	s, err := work.Open(v.home)
	if err != nil {
		return sourceMarkerRetirementError("original_work")
	}
	in, at, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present {
		return sourceMarkerRetirementError("original_reservation")
	}
	if _, err = s.RecordReservedReceiptedAction(origin.WorkID, in, at); err != nil {
		return sourceMarkerRetirementError("recovery_proof_incomplete")
	}
	return nil
}

// The sole production function is native owner finalization. Package tests can
// inject its persistence failure; neither request nor CLI selects an alternative.
// This descriptor-held lock is coordination, never outcome evidence. Its file
// stays in place so a second inode cannot admit a concurrent owner. A crash
// releases the lock; all outcome authority remains in the native receipt chain.
func (v *SourceMarkerRetirer) executionLock(actionID string) (func(), error) {
	dir := filepath.Join(v.home, "locks", "source-marker-retirement-owner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, sourceMarkerRetirementError("execution_lock")
	}
	f, err := os.OpenFile(filepath.Join(dir, idFor("", actionID)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, sourceMarkerRetirementError("execution_lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, sourceMarkerRetirementError("execution_in_progress")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

var sourceMarkerCompleteAttempt = func(homePath, id string, status string, owner, receipt, transition string, now time.Time) error {
	_, err := runattempt.Open(homePath).CompleteWithOwnerProof(id, status, owner, receipt, transition, now)
	return err
}

// Exact retry resumes local proof publication only. It never re-enters transport
// or reuses the already consumed approval for another external operation.
func (v *SourceMarkerRetirer) resumeFinalization(p SourceMarkerRetirementPlan, ai envelope.ActionIntent, cleanup context.Context) (out SourceMarkerRetirementResult, handled bool, err error) {
	s, e := work.Open(v.home)
	if e != nil {
		return out, true, sourceMarkerRetirementError("work_unavailable")
	}
	in, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
	if e != nil {
		return out, true, sourceMarkerRetirementError("reservation_unavailable")
	}
	if !present {
		return out, false, nil
	}
	handled = true
	if in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.Owner != sourceMarkerOwner(p.Operation) || policy.CanonicalFullIntentDigest(in.Action) != policy.CanonicalFullIntentDigest(ai) || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return out, true, sourceMarkerRetirementError("reservation_binding")
	}
	out.RunAttemptID = in.RequiredAttemptCompletion.AttemptID
	out.ApprovalConsumed = true
	out.State = "recovery_required"
	if in.RequiredAttemptCompletion.Recovery != nil {
		out.RecoveryWorkID = in.RequiredAttemptCompletion.Recovery.WorkID
	}
	attempt, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || attempt.WorkID != p.WorkID || attempt.ActionIntentID != ai.ID {
		return out, true, sourceMarkerRetirementError("attempt_binding")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, true, sourceMarkerRetirementError("terminal_without_outcome")
		}
		result, e := v.finish(out, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &sourceMarkerRetirementReservation{input: in, authorizedAt: at, launched: attempt.Launch != nil}, cleanup)
		return result, true, e
	}
	if in.Outcome != "completed" && in.Outcome != "failed" {
		return out, true, sourceMarkerRetirementError("outcome_binding")
	}
	var observation *SourceMarkerRetirementObservation
	if in.Outcome == "completed" {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return out, true, sourceMarkerRetirementError("observation_unavailable")
		}
		for _, ref := range in.AfterRefs {
			raw, e := store.Get(ref)
			if e != nil {
				continue
			}
			o, e := decodeSourceMarkerRetirementObservation(raw, p.Request)
			if e == nil && o.Operation == p.Operation && (o.State == "source_marker_retired") {
				observation = &o
				out.ObservationRef = ref
				break
			}
		}
		if observation == nil || in.ChangeKnowledge != "" || in.Changed != observation.Changed {
			return out, true, sourceMarkerRetirementError("outcome_observation")
		}
	}
	completed, e := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if e != nil && !errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, sourceMarkerRetirementError("receipt_unavailable")
	}
	out.ReceiptID, out.TransitionID = completed.Receipt.ID, completed.Transition.ID
	status := runattempt.StatusFailed
	if in.Outcome == "completed" {
		status = runattempt.StatusSucceeded
	}
	if e = sourceMarkerCompleteAttempt(v.home, out.RunAttemptID, status, sourceMarkerOwner(p.Operation), out.ReceiptID, out.TransitionID, v.now().UTC()); e != nil {
		return out, true, sourceMarkerRetirementError("attempt_completion")
	}
	_, e = s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if in.Outcome == "failed" && in.ChangeKnowledge == "unknown" && errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, sourceMarkerRetirementError("recovery_required")
	}
	if e != nil {
		return out, true, sourceMarkerRetirementError("reservation_completion")
	}
	if in.Outcome == "failed" {
		out.State = "failed"
		return out, true, sourceMarkerRetirementError("failed")
	}
	out.State = observation.State
	out.SourceMarkerRetired = observation.SourceMarkerRetired
	if p.Recovery != nil {
		if e = v.clearRecoveredOriginal(*p.Recovery); e != nil {
			return out, true, e
		}
	}
	return out, true, nil
}
