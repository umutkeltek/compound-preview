package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/landlease"
	"compound/internal/policy"
)

// LandingResourceKey resolves the repository/ref from the durable delivery
// contract and revalidates it against the bound checkout. Callers cannot choose
// a fingerprint or substitute their current working directory.
func (s *Store) LandingResourceKey(workID string) (landlease.ResourceKey, error) {
	w, err := s.Get(workID)
	if err != nil {
		return landlease.ResourceKey{}, err
	}
	if w.GitDelivery == nil {
		return landlease.ResourceKey{}, fmt.Errorf("work %s has no git_delivery contract", workID)
	}
	if err := s.validateBoundGitDelivery(w); err != nil {
		return landlease.ResourceKey{}, err
	}
	return landlease.LandingResourceKey(w.GitDelivery.RepositoryFingerprint, w.GitDelivery.TargetLocalRef)
}

type LandingResourceMutation struct {
	Actor, Holder, Operation, Reason string
	TTL                              time.Duration
}
type LandingResourceMutationResult struct {
	Lease   landlease.Lease
	Release landlease.ReleaseResult
}

// MutateLandingResource binds the advisory mutation to current work authority.
// Waiting happens outside this method so a waiter never holds work admission.
func (s *Store) MutateLandingResource(workID string, in LandingResourceMutation, now time.Time) (LandingResourceMutationResult, error) {
	var result LandingResourceMutationResult
	err := s.withLifecycleMutation(func() error {
		if in.Operation != "acquire" && in.Operation != "release" && in.Operation != "steal" {
			return fmt.Errorf("unsupported landing lease operation %q", in.Operation)
		}
		if !holderNamesWork(in.Holder, workID) || strings.TrimSpace(in.Holder) == "" || (in.Operation == "steal" && strings.TrimSpace(in.Reason) == "") {
			return fmt.Errorf("holder must name exact work_id; steal also requires an explicit reason")
		}
		w, err := s.resourceMutationOwner(workID, in.Actor)
		if err != nil {
			return err
		}
		key, err := s.LandingResourceKey(workID)
		if err != nil {
			return err
		}
		resource, err := landlease.OpenResource(s.home, key)
		if err != nil {
			return err
		}
		var predecessor landlease.StatusResult
		priorSnapshot := ""
		if in.Operation == "steal" {
			status, err := resource.StatusReadOnly(now)
			if err != nil {
				return err
			}
			if status.Holder == "" {
				return fmt.Errorf("landing steal requires an existing predecessor")
			}
			predecessor = status
			// Derived clock fields are excluded; the durable lease image is exact.
			status.Held, status.IsExpired, status.RemainingSeconds = false, false, 0
			encoded, err := json.Marshal(status)
			if err != nil {
				return err
			}
			priorSnapshot = string(encoded)
		}
		action := s.resourceMutationAction(w, in.Actor, "landing."+in.Operation,
			fmt.Sprintf("repository=%s ref=%s holder=%q ttl=%s reason=%q", key.RepositoryFingerprint, key.TargetRef, in.Holder, in.TTL, in.Reason)+" predecessor="+priorSnapshot, w.GitDelivery.TargetWorktreeRoot, now)
		if in.Operation == "steal" {
			action.MutationClass = "security"
		}
		action.ArgumentsHash, err = workEnvelopeSHA256(w)
		if err != nil {
			return err
		}
		decision, err := s.authorizeResourceMutationLocked(w, action, now)
		if err != nil {
			return err
		}
		switch in.Operation {
		case "acquire":
			result.Lease, err = resource.Acquire(in.Holder, in.TTL, now)
		case "release":
			result.Release, err = resource.Release(in.Holder, now)
		case "steal":
			result.Lease, err = resource.StealObserved(in.Holder, predecessor, in.Reason, now)
		}
		return s.finishResourceMutationLocked(w, action, decision, err, now)
	})
	return result, err
}

func (s *Store) resourceMutationOwner(workID, actor string) (envelope.WorkEnvelope, error) {
	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return w, err
	}
	if w.State != "claimed" && w.State != "running" && w.State != "verifying" {
		return w, fmt.Errorf("resource mutation requires claimed, running, or verifying work")
	}
	claim, err := s.readLease(workID)
	if err != nil {
		return w, err
	}
	if strings.TrimSpace(actor) == "" || claim.By != actor {
		return w, fmt.Errorf("actor %q does not hold work %s", actor, workID)
	}
	return w, w.Validate()
}

func (s *Store) resourceMutationAction(w envelope.WorkEnvelope, actor, operation, preview, path string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, idFor("act_", w.ID, actor, operation, preview, path, ts(now)), now, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:  actor, Capability: "coordination.resources", Action: operation,
		Target:        envelope.ActionTarget{Kind: "work", ID: w.ID, Path: path},
		MutationClass: "local_reversible", Reversibility: "manual", BlastRadius: "local",
		Preview: preview, RollbackPlan: "retain coordination records; release the exact owned lease or reconcile the cutover under writer quiescence",
	}
}

func (s *Store) authorizeResourceMutationLocked(w envelope.WorkEnvelope, action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
	var empty envelope.PolicyDecision

	if err := action.Validate(); err != nil {
		return empty, err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return empty, err
	}
	for _, class := range []string{"build-implement", action.Capability} {
		halted, reason, err := policy.AutonomyHalted(s.home, class)
		if err != nil {
			return empty, err
		}
		if halted {
			return empty, fmt.Errorf("resource mutation halted: %s", reason)
		}
	}
	if err := s.writeEventJSON(action.ID, action); err != nil {
		return empty, err
	}
	decision := policy.Evaluate(action, cfg, now)
	// These one-way operations require approval of their exact current inputs,
	// even when a standing grant covers security writes to the whole home.
	// Keep the original config for approval binding, and never relax a deny.
	if action.MutationClass == "security" && action.Reversibility == "irreversible" && policy.Allowed(decision) {
		decision.Outcome = "hold"
		decision.StandingGrantID = ""
		decision.ExpiresAt = ""
		decision.Obligations = nil
		decision.Reasons = append(decision.Reasons, "one-way coordination mutation requires exact one-use operator approval")
	}
	if decision.Outcome == "hold" {
		decision, _, err = policy.NewExactActionApprovalStore(s.home).ApplyMatchingLocked(action, cfg, decision, now)
		if err != nil {
			return empty, fmt.Errorf("resource mutation action %s: %w", action.ID, err)
		}
	}
	if !policy.Allowed(decision) {
		return empty, fmt.Errorf("resource mutation action %s policy %s", action.ID, decision.Outcome)
	}
	if err := s.writeEventJSON(decision.ID, decision); err != nil {
		return empty, err
	}
	// Intent and allowing decision are durable before the resource side effect.
	// Performed work history is attached only after a successful transition.
	return decision, nil
}

// A result event distinguishes an unsuccessful attempt from a performed action.
// A missing result after a crash is unknown; it is never success evidence.
func (s *Store) finishResourceMutationLocked(w envelope.WorkEnvelope, action envelope.ActionIntent, decision envelope.PolicyDecision, transitionErr error, now time.Time) error {
	outcome := "succeeded"
	if transitionErr != nil {
		outcome = "failed: " + transitionErr.Error()
	}
	resultErr := s.appendEvent(Event{TS: ts(now), WorkID: w.ID, From: w.State, To: w.State,
		Note: "resource transition " + outcome, Attached: []string{action.ID, decision.ID}})
	if transitionErr != nil || resultErr != nil {
		return errors.Join(transitionErr, resultErr)
	}
	_, err := s.recordActionLocked(w.ID, action, decision, now)
	return err
}
