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

// RuntimeConfigRecoveryOrigin names actual immutable native proof. It never
// supplies a replacement decision, observation or credential projection.
type RuntimeConfigRecoveryOrigin struct {
	PlanRef       string `json:"plan_ref"`
	WorkID        string `json:"work_id"`
	ActionID      string `json:"action_id"`
	AttemptID     string `json:"attempt_id"`
	ReceiptID     string `json:"receipt_id"`
	TransitionID  string `json:"transition_id"`
	RequestSHA256 string `json:"request_sha256"`
}

func (r RuntimeConfigRecoveryOrigin) proofRefs() []string {
	return []string{r.PlanRef, r.ActionID, r.AttemptID, "sha256:" + r.RequestSHA256, r.ReceiptID, r.TransitionID}
}
func runtimeConfigOwner(operation string) string {
	if operation == "install" {
		return runtimeConfigInstallOwner
	}
	return runtimeConfigRecoveryOwner
}
func runtimeConfigRecoveryWorkID(action, attempt string) string {
	return idFor("work_", "runtime-config-recovery", action, attempt)
}
func runtimeConfigOperationValid(operation string, origin *RuntimeConfigRecoveryOrigin) bool {
	return (operation == "install" && origin == nil) || ((operation == "reconcile" || operation == "rollback") && origin != nil)
}
func runtimeConfigValidateRequest(r policy.RuntimeConfigInstallRequest, operation string, now time.Time) error {
	if operation == "install" {
		return policy.ValidateRuntimeConfigInstallRequest(r, RuntimeConfigInstallProgramSHA256(), now)
	}
	return policy.ValidateRuntimeConfigRecoveryRequest(r, RuntimeConfigInstallProgramSHA256(), now)
}
func runtimeConfigOperationIntent(r policy.RuntimeConfigInstallRequest, workID, actor, workSHA, operation string, origin *RuntimeConfigRecoveryOrigin, now time.Time) envelope.ActionIntent {
	ai := runtimeConfigInstallIntent(r, workID, actor, workSHA, now)
	if operation != "install" && origin != nil {
		ai.Capability = policy.RuntimeConfigRecoveryCapability
		ai.Action = operation + "_inactive_runtime_configuration"
		ai.ArgumentsHash = "sha256:" + policy.RuntimeConfigRecoveryDigest(r, origin.ActionID, origin.AttemptID, operation)
		ai.ID = idFor("act_", workID, actor, ai.ArgumentsHash, ts(now))
		ai.EvidenceRefs = append(ai.EvidenceRefs, origin.proofRefs()...)
	}
	return ai
}
func runtimeConfigEvaluate(ai envelope.ActionIntent, r policy.RuntimeConfigInstallRequest, operation string, origin *RuntimeConfigRecoveryOrigin, cfg policy.Config, now time.Time) (envelope.PolicyDecision, bool) {
	if operation == "install" {
		return policy.EvaluateRuntimeConfigInstall(ai, r, RuntimeConfigInstallProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	}
	if origin == nil {
		return envelope.PolicyDecision{}, false
	}
	return policy.EvaluateRuntimeConfigRecovery(ai, r, origin.ActionID, origin.AttemptID, operation, RuntimeConfigInstallProgramSHA256(), cfg, policy.EvaluationContext{}, now)
}
func (v *RuntimeConfigInstaller) planAuthority(r policy.RuntimeConfigInstallRequest, operation string, origin *RuntimeConfigRecoveryOrigin, workID string, now time.Time) error {
	if !runtimeConfigOperationValid(operation, origin) {
		return runtimeConfigInstallError("operation_binding")
	}
	if operation == "install" {
		return v.witness(r, now)
	}
	return v.recoveryAuthority(r, *origin, workID)
}

// PlanRecovery first repairs only interrupted local native finalization. Remote
// recovery still requires a fresh exact approval on a separate claimed Work.
func (v *RuntimeConfigInstaller) PlanRecovery(originalPlan, operation, workID, actor string) (RuntimeConfigInstallPlanResult, error) {
	var out RuntimeConfigInstallPlanResult
	if operation != "reconcile" && operation != "rollback" {
		return out, runtimeConfigInstallError("recovery_operation")
	}
	p, ai, err := v.loadPlan(originalPlan, actor)
	if err != nil || p.Operation != "install" {
		return out, runtimeConfigInstallError("original_plan")
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
		return out, runtimeConfigInstallError("original_reservation")
	}
	attempts := runattempt.Open(v.home)
	attempt, err := attempts.Get(in.RequiredAttemptCompletion.AttemptID)
	if err != nil || attempt.ActionIntentID != ai.ID || attempt.WorkID != p.WorkID || attempt.Launch == nil || (attempt.Launch.CommittedAt == nil && attempt.Launch.StartErrorAt != nil) {
		return out, runtimeConfigInstallError("original_launch")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, runtimeConfigInstallError("original_terminal_without_outcome")
		}
		if in.PreEvaluatedDecision == nil {
			return out, runtimeConfigInstallError("original_policy")
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, _ := v.finish(RuntimeConfigInstallResult{RunAttemptID: attempt.ID, ApprovalConsumed: true, RecoveryWorkID: workID}, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &runtimeConfigInstallReservation{input: in, authorizedAt: at, launched: true}, cleanup)
		cancel()
		if result.ReceiptID == "" || result.TransitionID == "" {
			return out, runtimeConfigInstallError("original_finalization")
		}
		in, at, present, err = s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
		if err != nil || !present {
			return out, runtimeConfigInstallError("original_reservation")
		}
	}
	if in.Outcome != "failed" || in.ChangeKnowledge != "unknown" {
		return out, runtimeConfigInstallError("original_not_uncertain")
	}
	completed, err := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if !errors.Is(err, work.ErrRequiredAttemptCompletion) || completed.Receipt.ID == "" {
		return out, runtimeConfigInstallError("original_native_proof")
	}
	if _, err = attempts.CompleteWithOwnerProof(attempt.ID, runattempt.StatusFailed, runtimeConfigInstallOwner, completed.Receipt.ID, completed.Transition.ID, v.now().UTC()); err != nil {
		return out, runtimeConfigInstallError("original_attempt_proof")
	}
	origin := &RuntimeConfigRecoveryOrigin{PlanRef: originalPlan, WorkID: p.WorkID, ActionID: ai.ID, AttemptID: attempt.ID, ReceiptID: completed.Receipt.ID, TransitionID: completed.Transition.ID, RequestSHA256: p.RequestSHA256}
	return v.plan(p.Request, workID, actor, operation, origin)
}

func (v *RuntimeConfigInstaller) recoveryAuthority(request policy.RuntimeConfigInstallRequest, origin RuntimeConfigRecoveryOrigin, recoveryWork string) error {
	p, ai, err := v.loadPlan(origin.PlanRef, "")
	// The immutable plan carries the actor. Loading through an arbitrary actor
	// is forbidden, so recover that public metadata before exact plan validation.
	if err != nil {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return runtimeConfigInstallError("original_plan")
		}
		raw, e := store.Get(origin.PlanRef)
		if e != nil || len(raw) > 65536 || json.Unmarshal(raw, &p) != nil {
			return runtimeConfigInstallError("original_plan")
		}
		p, ai, err = v.loadPlan(origin.PlanRef, p.Actor)
	}
	if err != nil || p.Operation != "install" || p.WorkID != origin.WorkID || ai.ID != origin.ActionID || p.RequestSHA256 != origin.RequestSHA256 || policy.RuntimeConfigInstallDigest(request) != p.RequestSHA256 || recoveryWork != runtimeConfigRecoveryWorkID(ai.ID, origin.AttemptID) {
		return runtimeConfigInstallError("original_binding")
	}
	s, err := work.Open(v.home)
	if err != nil {
		return runtimeConfigInstallError("original_work")
	}
	in, _, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present || in.OutcomePending || in.Outcome != "failed" || in.ChangeKnowledge != "unknown" || in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.AttemptID != origin.AttemptID || in.RequiredAttemptCompletion.Owner != runtimeConfigInstallOwner || in.RequiredAttemptCompletion.Recovery == nil || in.RequiredAttemptCompletion.Recovery.WorkID != recoveryWork || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return runtimeConfigInstallError("original_reservation")
	}
	attempt, err := runattempt.Open(v.home).Get(origin.AttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed || attempt.TerminalizationOwner != runtimeConfigInstallOwner || attempt.ActionIntentID != origin.ActionID || attempt.WorkID != origin.WorkID || attempt.TerminalizationReceiptID != origin.ReceiptID || attempt.TerminalizationTransitionID != origin.TransitionID {
		return runtimeConfigInstallError("original_attempt")
	}
	events, err := eventstore.Open(v.home)
	if err != nil {
		return runtimeConfigInstallError("original_proof")
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	r, e1 := events.Read(origin.ReceiptID)
	t, e2 := events.Read(origin.TransitionID)
	if e1 != nil || e2 != nil || json.Unmarshal(r.Bytes, &receipt) != nil || json.Unmarshal(t.Bytes, &transition) != nil || receipt.Validate() != nil || transition.Validate() != nil || receipt.Outcome != "failed" || receipt.StateDelta.ChangeKnowledge != "unknown" || receipt.IDs.WorkID != origin.WorkID || transition.IDs.WorkID != origin.WorkID {
		return runtimeConfigInstallError("original_proof")
	}
	return nil
}
func (v *RuntimeConfigInstaller) clearRecoveredOriginal(origin RuntimeConfigRecoveryOrigin) error {
	s, err := work.Open(v.home)
	if err != nil {
		return runtimeConfigInstallError("original_work")
	}
	in, at, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present {
		return runtimeConfigInstallError("original_reservation")
	}
	if _, err = s.RecordReservedReceiptedAction(origin.WorkID, in, at); err != nil {
		return runtimeConfigInstallError("recovery_proof_incomplete")
	}
	return nil
}

// The sole production function is native owner finalization. Package tests can
// inject its persistence failure; neither request nor CLI selects an alternative.
// This descriptor-held lock is coordination, never outcome evidence. Its file
// stays in place so a second inode cannot admit a concurrent owner. A crash
// releases the lock; all outcome authority remains in the native receipt chain.
func (v *RuntimeConfigInstaller) executionLock(actionID string) (func(), error) {
	dir := filepath.Join(v.home, "locks", "runtime-config-owner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, runtimeConfigInstallError("execution_lock")
	}
	f, err := os.OpenFile(filepath.Join(dir, idFor("", actionID)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, runtimeConfigInstallError("execution_lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, runtimeConfigInstallError("execution_in_progress")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

var runtimeConfigCompleteAttempt = func(homePath, id string, status string, owner, receipt, transition string, now time.Time) error {
	_, err := runattempt.Open(homePath).CompleteWithOwnerProof(id, status, owner, receipt, transition, now)
	return err
}

// Exact retry resumes local proof publication only. It never re-enters transport
// or reuses the already consumed approval for another external operation.
func (v *RuntimeConfigInstaller) resumeFinalization(p RuntimeConfigInstallPlan, ai envelope.ActionIntent, cleanup context.Context) (out RuntimeConfigInstallResult, handled bool, err error) {
	s, e := work.Open(v.home)
	if e != nil {
		return out, true, runtimeConfigInstallError("work_unavailable")
	}
	in, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
	if e != nil {
		return out, true, runtimeConfigInstallError("reservation_unavailable")
	}
	if !present {
		return out, false, nil
	}
	handled = true
	if in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.Owner != runtimeConfigOwner(p.Operation) || policy.CanonicalFullIntentDigest(in.Action) != policy.CanonicalFullIntentDigest(ai) || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return out, true, runtimeConfigInstallError("reservation_binding")
	}
	out.RunAttemptID = in.RequiredAttemptCompletion.AttemptID
	out.ApprovalConsumed = true
	out.State = "recovery_required"
	if in.RequiredAttemptCompletion.Recovery != nil {
		out.RecoveryWorkID = in.RequiredAttemptCompletion.Recovery.WorkID
	}
	attempt, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || attempt.WorkID != p.WorkID || attempt.ActionIntentID != ai.ID {
		return out, true, runtimeConfigInstallError("attempt_binding")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, true, runtimeConfigInstallError("terminal_without_outcome")
		}
		result, e := v.finish(out, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &runtimeConfigInstallReservation{input: in, authorizedAt: at, launched: attempt.Launch != nil}, cleanup)
		return result, true, e
	}
	if in.Outcome != "completed" && in.Outcome != "failed" {
		return out, true, runtimeConfigInstallError("outcome_binding")
	}
	var observation *RuntimeConfigInstallObservation
	if in.Outcome == "completed" {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return out, true, runtimeConfigInstallError("observation_unavailable")
		}
		for _, ref := range in.AfterRefs {
			raw, e := store.Get(ref)
			if e != nil {
				continue
			}
			o, e := decodeRuntimeConfigInstallObservation(raw, p.Request)
			if e == nil && o.Operation == p.Operation && ((p.Operation == "rollback" && o.State == "rolled_back") || (p.Operation != "rollback" && o.State == "installed_inactive")) {
				observation = &o
				out.ObservationRef = ref
				break
			}
		}
		if observation == nil || in.ChangeKnowledge != "" || in.Changed != observation.Changed {
			return out, true, runtimeConfigInstallError("outcome_observation")
		}
	}
	completed, e := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if e != nil && !errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, runtimeConfigInstallError("receipt_unavailable")
	}
	out.ReceiptID, out.TransitionID = completed.Receipt.ID, completed.Transition.ID
	status := runattempt.StatusFailed
	if in.Outcome == "completed" {
		status = runattempt.StatusSucceeded
	}
	if e = runtimeConfigCompleteAttempt(v.home, out.RunAttemptID, status, runtimeConfigOwner(p.Operation), out.ReceiptID, out.TransitionID, v.now().UTC()); e != nil {
		return out, true, runtimeConfigInstallError("attempt_completion")
	}
	_, e = s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if in.Outcome == "failed" && in.ChangeKnowledge == "unknown" && errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, runtimeConfigInstallError("recovery_required")
	}
	if e != nil {
		return out, true, runtimeConfigInstallError("reservation_completion")
	}
	if in.Outcome == "failed" {
		out.State = "failed"
		return out, true, runtimeConfigInstallError("failed")
	}
	out.State = observation.State
	out.ConfigurationAccepted = observation.ConfigurationAccepted
	if p.Recovery != nil {
		if e = v.clearRecoveredOriginal(*p.Recovery); e != nil {
			return out, true, e
		}
	}
	return out, true, nil
}
