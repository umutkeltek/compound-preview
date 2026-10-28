package act

import (
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const credentialPasswordAlignRecoveryOwner = "act.credential_password_alignment_recovery"

func credentialPasswordAlignOwnerFor(operation string) string {
	if operation == "inspect" {
		return "act.credential_password_alignment_metadata"
	}
	if operation == "align" {
		return credentialPasswordAlignOwner
	}
	return credentialPasswordAlignRecoveryOwner
}
func credentialPasswordAlignRecoveryWorkID(action, attempt string) string {
	return idFor("work_", "credential-password-recovery", action, attempt)
}
func (v *CredentialPasswordAligner) executionLock(actionID string) (func(), error) {
	dir := filepath.Join(v.home, "locks", "runtime-config-owner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, credentialPasswordAlignError("execution_lock")
	}
	f, err := os.OpenFile(filepath.Join(dir, idFor("", actionID)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, credentialPasswordAlignError("execution_lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, credentialPasswordAlignError("execution_in_progress")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

var credentialPasswordAlignCompleteAttempt = func(homePath, id string, status string, owner, receipt, transition string, now time.Time) error {
	_, err := runattempt.Open(homePath).CompleteWithOwnerProof(id, status, owner, receipt, transition, now)
	return err
}

// Exact retry resumes local proof publication only. It never re-enters transport
// or reuses the already consumed approval for another external operation.
func (v *CredentialPasswordAligner) resumeFinalization(p CredentialPasswordAlignmentPlan, ai envelope.ActionIntent, cleanup context.Context) (out CredentialPasswordAlignmentResult, handled bool, err error) {
	s, e := work.Open(v.home)
	if e != nil {
		return out, true, credentialPasswordAlignError("work_unavailable")
	}
	in, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
	if e != nil {
		return out, true, credentialPasswordAlignError("reservation_unavailable")
	}
	if !present {
		if len(p.Request.AttemptID) < 16 {
			return out, true, credentialPasswordAlignError("attempt_binding")
		}
		// A completed proof may have cleared its own reservation immediately
		// before a crash. Finish only local original-reservation reconciliation;
		// never re-enter transport or reuse the consumed grant.
		at, parseErr := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
		if parseErr == nil {
			id := runattempt.MintDeterministic("credential-password-align:"+p.Request.AttemptID, at)
			a, readErr := runattempt.Open(v.home).Get(id)
			if readErr == nil && a.ActionIntentID == ai.ID && a.WorkID == p.WorkID && a.Status == runattempt.StatusSucceeded {
				native, proofErr := credentialPasswordAlignmentReadOutcome(v.home, a)
				if proofErr != nil || policy.CredentialPasswordAlignmentDigest(native.Request) != p.RequestSHA256 {
					return out, true, credentialPasswordAlignError("completed_outcome_binding")
				}
				out = CredentialPasswordAlignmentResult{State: "completed", RunAttemptID: a.ID, ReceiptID: a.TerminalizationReceiptID, TransitionID: a.TerminalizationTransitionID, ApprovalConsumed: true}
				if p.Request.Origin != nil {
					if err = v.clearRecoveredOriginal(*p.Request.Origin); err != nil {
						return out, true, err
					}
				}
				return out, true, nil
			}
		}
		return out, false, nil
	}
	handled = true
	if in.RequiredAttemptCompletion == nil || in.RequiredAttemptCompletion.Owner != credentialPasswordAlignOwnerFor(p.Request.Operation) || policy.CanonicalFullIntentDigest(in.Action) != policy.CanonicalFullIntentDigest(ai) || in.PreEvaluatedDecision == nil || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, *in.PreEvaluatedDecision) != nil {
		return out, true, credentialPasswordAlignError("reservation_binding")
	}
	out.RunAttemptID = in.RequiredAttemptCompletion.AttemptID
	out.ApprovalConsumed = true
	out.State = "recovery_required"
	if in.RequiredAttemptCompletion.Recovery != nil {
		out.RecoveryWorkID = in.RequiredAttemptCompletion.Recovery.WorkID
	}
	attempt, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || attempt.WorkID != p.WorkID || attempt.ActionIntentID != ai.ID {
		return out, true, credentialPasswordAlignError("attempt_binding")
	}
	if in.OutcomePending {
		if runattempt.IsTerminal(attempt.Status) {
			return out, true, credentialPasswordAlignError("terminal_without_outcome")
		}
		result, e := v.finish(out, p, ai, *in.PreEvaluatedDecision, "failed", nil, "interrupted", &credentialPasswordAlignReservation{input: in, authorizedAt: at, launched: attempt.Launch != nil}, cleanup)
		return result, true, e
	}
	if in.Outcome != "completed" && in.Outcome != "failed" {
		return out, true, credentialPasswordAlignError("outcome_binding")
	}
	var observation *CredentialPasswordAlignmentObservation
	if in.Outcome == "completed" {
		store, e := NewArtifactStore(v.home)
		if e != nil {
			return out, true, credentialPasswordAlignError("observation_unavailable")
		}
		for _, ref := range in.AfterRefs {
			raw, e := store.Get(ref)
			if e != nil {
				continue
			}
			o, e := decodeCredentialPasswordAlignmentObservation(raw, p.Request)
			if e == nil && o.ActionID == ai.ID && o.Operation == p.Request.Operation && ((p.Request.Operation == "align" && o.Effect == "committed") || (p.Request.Operation == "inspect" && o.Effect == "observed") || (p.Request.Operation == "restore" && o.Effect == "restored") || (p.Request.Operation == "reconcile" && (o.Effect == "reconciled_old_old" || o.Effect == "reconciled_new_new"))) {
				observation = &o
				out.ObservationRef = ref
				break
			}
		}
		if observation == nil || in.ChangeKnowledge != "" || in.Changed != observation.Changed {
			return out, true, credentialPasswordAlignError("outcome_observation")
		}
	}
	completed, e := s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if e != nil && !errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, credentialPasswordAlignError("receipt_unavailable")
	}
	out.ReceiptID, out.TransitionID = completed.Receipt.ID, completed.Transition.ID
	status := runattempt.StatusFailed
	if in.Outcome == "completed" {
		status = runattempt.StatusSucceeded
	}
	if e = credentialPasswordAlignCompleteAttempt(v.home, out.RunAttemptID, status, credentialPasswordAlignOwnerFor(p.Request.Operation), out.ReceiptID, out.TransitionID, v.now().UTC()); e != nil {
		return out, true, credentialPasswordAlignError("attempt_completion")
	}
	_, e = s.RecordReservedReceiptedAction(p.WorkID, in, at)
	if in.Outcome == "failed" && in.ChangeKnowledge == "unknown" && errors.Is(e, work.ErrRequiredAttemptCompletion) {
		return out, true, credentialPasswordAlignError("recovery_required")
	}
	if e != nil {
		return out, true, credentialPasswordAlignError("reservation_completion")
	}
	if in.Outcome == "failed" {
		out.State = "failed"
		return out, true, credentialPasswordAlignError("failed")
	}
	out.State = "completed"
	out.ConfigurationAccepted = observation.ConfigurationAccepted
	if p.Request.Origin != nil {
		if e = v.clearRecoveredOriginal(*p.Request.Origin); e != nil {
			return out, true, e
		}
	}
	return out, true, nil
}

func (v *CredentialPasswordAligner) clearRecoveredOriginal(origin policy.CredentialPasswordAlignmentOrigin) error {
	s, err := work.Open(v.home)
	if err != nil {
		return credentialPasswordAlignError("original_work")
	}
	in, at, present, err := s.ReadReceiptedActionReservationInput(origin.WorkID, origin.ActionID)
	if err != nil {
		return credentialPasswordAlignError("original_reservation")
	}
	if !present {
		return nil
	}
	if _, err = s.RecordReservedReceiptedAction(origin.WorkID, in, at); err != nil {
		return credentialPasswordAlignError("recovery_proof_incomplete")
	}
	return nil
}
