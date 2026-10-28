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

// BackupCredentialRecoveryOrigin names actual immutable native proof. It never
// supplies a replacement decision, observation or credential projection.
type BackupCredentialRecoveryOrigin struct {
	PlanRef       string `json:"plan_ref"`
	WorkID        string `json:"work_id"`
	ActionID      string `json:"action_id"`
	AttemptID     string `json:"attempt_id"`
	ReceiptID     string `json:"receipt_id"`
	TransitionID  string `json:"transition_id"`
	RequestSHA256 string `json:"request_sha256"`
}

func (r BackupCredentialRecoveryOrigin) proofRefs() []string {
	return []string{r.PlanRef, r.ActionID, r.AttemptID, "sha256:" + r.RequestSHA256, r.ReceiptID, r.TransitionID}
}
func backupCredentialOwner(operation string) string {
	if operation == "stage" {
		return backupCredentialStageOwner
	}
	return backupCredentialRecoveryOwner
}
func backupCredentialRecoveryWorkID(action, attempt string) string {
	return idFor("work_", "backup-credential-recovery", action, attempt)
}
func backupCredentialOperationValid(operation string, origin *BackupCredentialRecoveryOrigin) bool {
	return (operation == "stage" && origin == nil) || ((operation == "reconcile") && origin != nil)
}
func backupCredentialValidateRequest(r policy.BackupCredentialStageRequest, operation string, now time.Time) error {
	if operation == "stage" {
		return policy.ValidateBackupCredentialStageRequest(r, BackupCredentialStageProgramSHA256(), now)
	}
	return policy.ValidateBackupCredentialRecoveryRequest(r, BackupCredentialStageProgramSHA256(), now)
}
func backupCredentialOperationIntent(r policy.BackupCredentialStageRequest, workID, actor, workSHA, operation string, origin *BackupCredentialRecoveryOrigin, now time.Time) envelope.ActionIntent {
	ai := backupCredentialStageIntent(r, workID, actor, workSHA, now)
	if operation != "stage" && origin != nil {
		ai.Capability = policy.BackupCredentialRecoveryCapability
		ai.Action = operation + "_existing_backup_credentials"
		ai.ArgumentsHash = "sha256:" + policy.BackupCredentialRecoveryDigest(r, origin.ActionID, origin.AttemptID, operation)
		ai.ID = idFor("act_", workID, actor, ai.ArgumentsHash, ts(now))
		ai.EvidenceRefs = append(ai.EvidenceRefs, origin.proofRefs()...)
	}
	return ai
}
func backupCredentialEvaluate(ai envelope.ActionIntent, r policy.BackupCredentialStageRequest, operation string, origin *BackupCredentialRecoveryOrigin, cfg policy.Config, now time.Time) (envelope.PolicyDecision, bool) {
	if operation == "stage" {
		return policy.EvaluateBackupCredentialStage(ai, r, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	}
	if origin == nil {
		return envelope.PolicyDecision{}, false
	}
	return policy.EvaluateBackupCredentialRecovery(ai, r, origin.ActionID, origin.AttemptID, operation, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now)
}
func (v *BackupCredentialStager) planAuthority(r policy.BackupCredentialStageRequest, operation string, origin *BackupCredentialRecoveryOrigin, workID string, now time.Time) error {
	if !backupCredentialOperationValid(operation, origin) {
		return backupCredentialStageError("operation_binding")
	}
	if operation == "stage" {
		return v.witness(r, now)
	}
	return v.recoveryAuthority(r, *origin, workID)
}

// PlanRecovery first repairs only interrupted local native finalization. Remote
// recovery still requires a fresh exact approval on a separate claimed Work.
func (v *BackupCredentialStager) PlanRecovery(originalPlan, operation, workID, actor string) (BackupCredentialStagePlanResult, error) {
	var out BackupCredentialStagePlanResult
	if operation != "reconcile" {
		return out, backupCredentialStageError("recovery_operation")
	}
	p, ai, err := v.loadPlan(originalPlan, actor)
	if err != nil || p.Operation != "stage" {
		return out, backupCredentialStageError("original_plan")
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
		return out, backupCredentialStageError("original_reservation")
	}
	attempts := runattempt.Open(v.home)
	attempt, err := attempts.Get(in.RequiredAttemptCompletion.AttemptID)
	if err != nil || attempt.ActionIntentID != ai.ID || attempt.WorkID != p.WorkID || attempt.Launch == nil || (attempt.Launch.CommittedAt == nil && attempt.Launch.StartErrorAt != nil) {
		return out, backupCredentialStageError("original_launch")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, backupCredentialStageError("original_terminal_without_outcome")
		}
		if in.PreEvaluatedDecision == nil {
			return out, backupCredentialStageError("original_policy")
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, _ := v.finish(BackupCredentialStageResult{RunAttemptID: attempt.ID, ApprovalConsumed: true, RecoveryWorkID: workID}, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &backupCredentialStageReservation{input: in, authorizedAt: at, launched: true}, cleanup)
		cancel()
		if result.ReceiptID == "" || result.TransitionID == "" {
			return out, backupCredentialStageError("original_finalization")
		}
		in, at, present, err = s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
		if err != nil || !present {
			return out, backupCredentialStageError("original_reservation")
		}
	}
	if in.Outcome != "failed" || in.ChangeKnowledge != "unknown" {
		return out, backupCredentialStageError("original_not_uncertain")
	}
	completed, err := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if !errors.Is(err, work.ErrRequiredAttemptCompletion) || completed.Receipt.ID == "" {
		return out, backupCredentialStageError("original_native_proof")
	}
	if _, err = attempts.CompleteWithOwnerProof(attempt.ID, runattempt.StatusFailed, backupCredentialStageOwner, completed.Receipt.ID, completed.Transition.ID, v.now().UTC()); err != nil {
		return out, backupCredentialStageError("original_attempt_proof")
	}
	origin := &BackupCredentialRecoveryOrigin{PlanRef: originalPlan, WorkID: p.WorkID, ActionID: ai.ID, AttemptID: attempt.ID, ReceiptID: completed.Receipt.ID, TransitionID: completed.Transition.ID, RequestSHA256: p.RequestSHA256}
	return v.plan(p.Request, workID, actor, operation, origin)
}

func (v *BackupCredentialStager) recoveryAuthority(request policy.BackupCredentialStageRequest, origin BackupCredentialRecoveryOrigin, recoveryWork string) error {
	p, ai, err := v.loadPlan(origin.PlanRef, "")
	// The immutable plan carries the actor. Loading through an arbitrary actor
	// is forbidden, so recover that public metadata before exact plan validation.
	if err != nil {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return backupCredentialStageError("original_plan")
		}
		raw, e := store.Get(origin.PlanRef)
		if e != nil || len(raw) > 65536 || json.Unmarshal(raw, &p) != nil {
			return backupCredentialStageError("original_plan")
		}
		p, ai, err = v.loadPlan(origin.PlanRef, p.Actor)
	}
	if err != nil || p.Operation != "stage" || p.WorkID != origin.WorkID || ai.ID != origin.ActionID || p.RequestSHA256 != origin.RequestSHA256 || policy.BackupCredentialStageDigest(request) != p.RequestSHA256 || recoveryWork != backupCredentialRecoveryWorkID(ai.ID, origin.AttemptID) {
		return backupCredentialStageError("original_binding")
	}
	s, err := work.Open(v.home)
	if err != nil {
		return backupCredentialStageError("original_work")
	}
	in, _, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present || in.OutcomePending || in.Outcome != "failed" || in.ChangeKnowledge != "unknown" || in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.AttemptID != origin.AttemptID || in.RequiredAttemptCompletion.Owner != backupCredentialStageOwner || in.RequiredAttemptCompletion.Recovery == nil || in.RequiredAttemptCompletion.Recovery.WorkID != recoveryWork || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return backupCredentialStageError("original_reservation")
	}
	attempt, err := runattempt.Open(v.home).Get(origin.AttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed || attempt.TerminalizationOwner != backupCredentialStageOwner || attempt.ActionIntentID != origin.ActionID || attempt.WorkID != origin.WorkID || attempt.TerminalizationReceiptID != origin.ReceiptID || attempt.TerminalizationTransitionID != origin.TransitionID {
		return backupCredentialStageError("original_attempt")
	}
	events, err := eventstore.Open(v.home)
	if err != nil {
		return backupCredentialStageError("original_proof")
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	r, e1 := events.Read(origin.ReceiptID)
	t, e2 := events.Read(origin.TransitionID)
	if e1 != nil || e2 != nil || json.Unmarshal(r.Bytes, &receipt) != nil || json.Unmarshal(t.Bytes, &transition) != nil || receipt.Validate() != nil || transition.Validate() != nil || receipt.Outcome != "failed" || receipt.StateDelta.ChangeKnowledge != "unknown" || receipt.IDs.WorkID != origin.WorkID || transition.IDs.WorkID != origin.WorkID {
		return backupCredentialStageError("original_proof")
	}
	return nil
}
func (v *BackupCredentialStager) clearRecoveredOriginal(origin BackupCredentialRecoveryOrigin) error {
	s, err := work.Open(v.home)
	if err != nil {
		return backupCredentialStageError("original_work")
	}
	in, at, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil || !present {
		return backupCredentialStageError("original_reservation")
	}
	if _, err = s.RecordReservedReceiptedAction(origin.WorkID, in, at); err != nil {
		return backupCredentialStageError("recovery_proof_incomplete")
	}
	return nil
}

// The sole production function is native owner finalization. Package tests can
// inject its persistence failure; neither request nor CLI selects an alternative.
// This descriptor-held lock is coordination, never outcome evidence. Its file
// stays in place so a second inode cannot admit a concurrent owner. A crash
// releases the lock; all outcome authority remains in the native receipt chain.
func (v *BackupCredentialStager) executionLock(actionID string) (func(), error) {
	dir := filepath.Join(v.home, "locks", "backup-credential-stage-owner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, backupCredentialStageError("execution_lock")
	}
	f, err := os.OpenFile(filepath.Join(dir, idFor("", actionID)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, backupCredentialStageError("execution_lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, backupCredentialStageError("execution_in_progress")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

var backupCredentialCompleteAttempt = func(homePath, id string, status string, owner, receipt, transition string, now time.Time) error {
	_, err := runattempt.Open(homePath).CompleteWithOwnerProof(id, status, owner, receipt, transition, now)
	return err
}

// Exact retry resumes local proof publication only. It never re-enters transport
// or reuses the already consumed approval for another external operation.
func (v *BackupCredentialStager) resumeFinalization(p BackupCredentialStagePlan, ai envelope.ActionIntent, cleanup context.Context) (out BackupCredentialStageResult, handled bool, err error) {
	s, e := work.Open(v.home)
	if e != nil {
		return out, true, backupCredentialStageError("work_unavailable")
	}
	in, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
	if e != nil {
		return out, true, backupCredentialStageError("reservation_unavailable")
	}
	if !present {
		return out, false, nil
	}
	handled = true
	if in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.Owner != backupCredentialOwner(p.Operation) || policy.CanonicalFullIntentDigest(in.Action) != policy.CanonicalFullIntentDigest(ai) || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return out, true, backupCredentialStageError("reservation_binding")
	}
	out.RunAttemptID = in.RequiredAttemptCompletion.AttemptID
	out.ApprovalConsumed = true
	out.State = "recovery_required"
	if in.RequiredAttemptCompletion.Recovery != nil {
		out.RecoveryWorkID = in.RequiredAttemptCompletion.Recovery.WorkID
	}
	attempt, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || attempt.WorkID != p.WorkID || attempt.ActionIntentID != ai.ID {
		return out, true, backupCredentialStageError("attempt_binding")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, true, backupCredentialStageError("terminal_without_outcome")
		}
		result, e := v.finish(out, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &backupCredentialStageReservation{input: in, authorizedAt: at, launched: attempt.Launch != nil}, cleanup)
		return result, true, e
	}
	if in.Outcome != "completed" && in.Outcome != "failed" {
		return out, true, backupCredentialStageError("outcome_binding")
	}
	var observation *BackupCredentialStageObservation
	if in.Outcome == "completed" {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return out, true, backupCredentialStageError("observation_unavailable")
		}
		for _, ref := range in.AfterRefs {
			raw, e := store.Get(ref)
			if e != nil {
				continue
			}
			o, e := decodeBackupCredentialStageObservation(raw, p.Request)
			if e == nil && o.Operation == p.Operation && (o.State == "staged_backup_credentials") {
				observation = &o
				out.ObservationRef = ref
				break
			}
		}
		if observation == nil || in.ChangeKnowledge != "" || in.Changed != observation.Changed {
			return out, true, backupCredentialStageError("outcome_observation")
		}
	}
	completed, e := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if e != nil && !errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, backupCredentialStageError("receipt_unavailable")
	}
	out.ReceiptID, out.TransitionID = completed.Receipt.ID, completed.Transition.ID
	status := runattempt.StatusFailed
	if in.Outcome == "completed" {
		status = runattempt.StatusSucceeded
	}
	if e = backupCredentialCompleteAttempt(v.home, out.RunAttemptID, status, backupCredentialOwner(p.Operation), out.ReceiptID, out.TransitionID, v.now().UTC()); e != nil {
		return out, true, backupCredentialStageError("attempt_completion")
	}
	_, e = s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if in.Outcome == "failed" && in.ChangeKnowledge == "unknown" && errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, backupCredentialStageError("recovery_required")
	}
	if e != nil {
		return out, true, backupCredentialStageError("reservation_completion")
	}
	if in.Outcome == "failed" {
		out.State = "failed"
		return out, true, backupCredentialStageError("failed")
	}
	out.State = observation.State
	out.CredentialsStaged = observation.CredentialsStaged
	if p.Recovery != nil {
		if e = v.clearRecoveredOriginal(*p.Recovery); e != nil {
			return out, true, e
		}
	}
	return out, true, nil
}
