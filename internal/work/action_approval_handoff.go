package work

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// ActionRecordResult separates a reviewable proposal from performed history.
type ActionRecordResult struct {
	Work     envelope.WorkEnvelope
	Decision envelope.PolicyDecision
	Recorded bool
}

// EvaluateAndRecordAction checks current authority, persists an immutable
// proposal, and records only an allowed action. Approval consumption and work
// recording share the admission lock; held proposals never advance work.
func (s *Store) EvaluateAndRecordAction(workID string, ai envelope.ActionIntent, now time.Time) (ActionRecordResult, error) {
	var result ActionRecordResult
	err := s.withLifecycleMutation(func() error {
		w, err := s.recoverPendingClaimBeforeMutation(workID)
		if err != nil {
			return err
		}
		result.Work = w
		switch w.State {
		case "claimed", "running", "verifying":
		default:
			return fmt.Errorf("work %s is %s; cannot propose or record an action", workID, w.State)
		}
		pending, err := s.pendingReceiptedActionID(workID)
		if err != nil {
			return err
		}
		if pending != "" {
			return fmt.Errorf("work %s has pending receipted action %s", workID, pending)
		}
		if err := ai.Validate(); err != nil {
			return fmt.Errorf("action intent invalid: %w", err)
		}
		if ai.IDs.WorkID != workID {
			return fmt.Errorf("action work_id %q does not match work %q", ai.IDs.WorkID, workID)
		}
		lease, err := s.readLease(workID)
		if err != nil {
			return fmt.Errorf("action work lease: %w", err)
		}
		if lease.WorkID != workID || strings.TrimSpace(ai.Actor) == "" || ai.Actor != lease.By {
			return fmt.Errorf("action actor %q does not hold work lease", ai.Actor)
		}
		if ai.IDs.ContextPackID != "" && ai.IDs.ContextPackID != w.IDs.ContextPackID {
			return fmt.Errorf("action context does not match current work")
		}
		for _, id := range w.ActionIntentIDs {
			if id == ai.ID {
				return fmt.Errorf("action %s already recorded", ai.ID)
			}
		}
		if err := s.validateSourceActionAdmission(w, ai); err != nil {
			return err
		}
		// This immutable put also rejects same-id/different-body retries BEFORE
		// looking for a grant, so a conflict cannot consume approval authority.
		if err := s.writeEventJSON(ai.ID, ai); err != nil {
			return fmt.Errorf("persist action proposal: %w", err)
		}
		cfg, _, err := policy.LoadConfig(s.home)
		if err != nil {
			return err
		}
		pd := policy.Evaluate(ai, cfg, now)
		for _, class := range []string{"build-implement", ai.Capability} {
			halted, reason, err := policy.AutonomyHalted(s.home, class)
			if err != nil {
				return err
			}
			if halted {
				result.Decision = policy.AutonomyHaltDecision(ai, reason, now)
				return nil
			}
		}
		if pd.Outcome == "hold" && ai.SecurityFloorOutcome == "" {
			pd, _, err = policy.NewExactActionApprovalStore(s.home).ApplyMatchingLocked(ai, cfg, pd, now)
			if err != nil {
				return err
			}
		}
		result.Decision = pd
		if !policy.Allowed(pd) {
			return nil
		}
		// RecordAction remains performed-and-allowed-only. Its lock-free helper
		// is safe here because this method owns the same admission lock.
		result.Work, err = s.recordActionLocked(workID, ai, pd, now)
		result.Recorded = err == nil
		return err
	})
	return result, err
}
