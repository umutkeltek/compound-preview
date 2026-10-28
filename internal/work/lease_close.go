package work

// Close-gate lease enforcement. A WorkEnvelope lease (Claim, work.go:295-345) was advisory
// only: nothing stopped a non-holder actor from closing a work another actor's executor was
// still mid-build on. Tonight's twin-collisions (work_b1c9d3d624e1 consumed+closed by a lane
// that never held it; work_fc4a3b26a15a closed by a concurrent session mid-build) are the
// exact regression this file closes. See docs_private/plan/packets/2026-07-09-close-gate-lease.md.
//
// The gate lives here, in the Store, so every close door (Close, ClosePlan preview, and any
// future caller) inherits it — not in the CLI layer. Design:
//   - No lease (never claimed, or lease residue absent) = today's behavior, untouched.
//   - Same actor as the lease holder = today's behavior, untouched (exact string equality).
//   - A different actor with no --steal, or --steal with an empty reason, is REFUSED: the
//     refusal is recorded (ActionIntent + deny PolicyDecision + lifecycle event) so liveness
//     probes can see the contention, and the work is left exactly as it was.
//   - A different actor with --steal and a non-empty reason SUCCEEDS. A real Advance transition
//     transfers the live lease before continuing. A terminal Close instead binds the prior
//     holder and reason into its exact ActionIntent, retains the original lease until terminal
//     publication, publishes the successful override in that same lifecycle event, and releases
//     the original lease last. Coordination is advisory-with-teeth without creating a transient
//     close-only holder that can strand nonterminal work.
//
// P0 (2026-07-23): recordLeaseSteal used to record only the ActionIntent/PolicyDecision/Event
// trail and never touched work/leases/<id>.json — the file kept naming the original holder, so
// that holder's NEXT advance/close without --steal passed leaseHolderStatusFor's holder==actor
// short-circuit as if the steal had never happened (the exact twin-collision class this file
// exists to close, now against the stealer instead of a stranger). transferLeaseOnSteal closes
// that gap for continuing Advance transitions. Terminal Close has no next live mutation and
// therefore publishes the override atomically with terminal state instead of transferring.
//
// Follow-up: an advance-gate steal transfers only for a real state transition — a same-state
// advance steal is the
// land-check/proof-attachment class (LandCheck's same-state Advance in land_check.go), where
// --steal authorizes one evidence write and must NOT move the lease. See
// leaseHolderAdvanceGate's transitions computation in lease_advance.go and
// TestWorkLandCheckCLIStealOnlyOverridesProofAttachment (cmd/compound).

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// leaseHolderStatus is the read-only comparison of a close attempt against a work's live
// lease. Holder is empty when there is no lease (or no lease residue) to enforce.
type leaseHolderStatus struct {
	Holder  string // lease holder; "" means no lease = today's behavior
	Blocked bool   // actor is not Holder and no valid steal is presented
	Stolen  bool   // actor is not Holder but a valid --steal --reason is presented
	Message string // holder-named refusal / requirement message, set when Blocked
	Err     error  // non-missing lease authority could not be read; callers fail closed
}

// leaseHolderStatusFor reads workID's lease (if any) live and reports how an attempt by
// actor (to perform the mutation verb named by verb, e.g. "close" or "advance") compares to
// it. Pure/read-only: callers decide whether to record anything. verb only appears in the
// non-holder remedy message; the --steal-without-reason message never mentions it, so
// generalizing this from close-only costs close callers nothing (byte-identical when
// verb == "close").
func (s *Store) leaseHolderStatusFor(workID, actor string, steal bool, stealReason, verb string) leaseHolderStatus {
	lease, err := s.readLease(workID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return leaseHolderStatus{}
		}
		authorityErr := fmt.Errorf("read work %s lease authority: %w", workID, err)
		return leaseHolderStatus{
			Blocked: true,
			Message: authorityErr.Error(),
			Err:     authorityErr,
		}
	}
	holder := strings.TrimSpace(lease.By)
	if lease.WorkID != workID || holder == "" {
		authorityErr := fmt.Errorf(
			"work %s lease authority invalid: expected work_id %q and nonblank holder",
			workID,
			workID,
		)
		return leaseHolderStatus{
			Blocked: true,
			Message: authorityErr.Error(),
			Err:     authorityErr,
		}
	}
	if holder == actor {
		return leaseHolderStatus{}
	}
	reason := strings.TrimSpace(stealReason)
	if steal && reason != "" {
		return leaseHolderStatus{Holder: holder, Stolen: true}
	}
	if steal {
		return leaseHolderStatus{
			Holder:  holder,
			Blocked: true,
			Message: fmt.Sprintf("work %s is held by %q; --steal requires --reason <non-empty>", workID, holder),
		}
	}
	return leaseHolderStatus{
		Holder:  holder,
		Blocked: true,
		Message: fmt.Sprintf("work %s is held by %q; %s requires --by %s or --steal --reason \"...\"", workID, holder, verb, holder),
	}
}

// leaseHolderCloseGate is the final authorization seam for a terminal close
// override. It re-reads the live holder after RunAttempt preparation and
// requires the exact holder retained by the read-only preflight. It deliberately
// does not rewrite the lease: the close ActionIntent binds holder/reason and the
// terminal lifecycle event publishes the only successful steal trail. The
// original lease remains in place until terminal close releases it last.
func (s *Store) leaseHolderCloseGate(w envelope.WorkEnvelope, actor string, in CloseInput, expected leaseHolderStatus) error {
	status := s.leaseHolderStatusFor(w.ID, actor, in.Steal, in.StealReason, "close")
	if status.Err != nil {
		return status.Err
	}
	if !status.Stolen || !expected.Stolen || status.Holder != expected.Holder {
		return fmt.Errorf(
			"close lease authority changed before terminal override: expected holder %q, got holder %q",
			expected.Holder,
			status.Holder,
		)
	}
	return nil
}

// recordLeaseHolderDeny writes the ActionIntent and a deny PolicyDecision for a mutation
// (capability/action name the caller's verb, e.g. work.close/close_denied_not_lease_holder
// or work.advance/advance_denied_not_lease_holder) attempted by an actor who is not the
// current lease holder (including an invalid steal attempt), plus a refusal lifecycle
// event, so the denial leaves an auditable trail while the work stays in place — a second
// identical refusal only appends another event line, it never corrupts state
// (idempotent-safe trail). The event Note prefix is derived from capability ("work.close" ->
// "close refused: ", "work.advance" -> "advance refused: ") so this stays one write path for
// every gated verb; close's Note stays byte-identical to before generalization.
func (s *Store) recordLeaseHolderDeny(w envelope.WorkEnvelope, actor, holder, message, capability, action string, now time.Time) error {
	ai := s.leaseGateActionIntent(w, actor, capability, action, now)
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := policy.LeaseHolderDenyDecision(ai, message, now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     refusalNotePrefix(capability) + message,
		Attached: []string{ai.ID, pd.ID},
	})
}

// recordLeaseSteal is the advance-path override recorder. When transfersLease is true, it
// transfers workID's live lease to actor
// (transferLeaseOnSteal) FIRST and fails closed before writing anything else: if the
// transfer errors, this returns immediately and writes none of the trail below, so the
// ledger never claims a steal that did not actually take the lease (and the caller's gate
// returns the error, blocking the underlying advance mutation too — see
// leaseHolderAdvanceGate). When transfersLease is false (a same-state
// advance steal — the land-check/proof-attachment class, see leaseHolderAdvanceGate), the
// lease file is left untouched: --steal there authorizes exactly one evidence-attachment
// write, not a hand-off, and the original holder must still be the recorded holder for every
// subsequent lifecycle command.
//
// Either way, this writes the ActionIntent and a recorded PolicyDecision for the explicit
// lease steal (--steal --reason) on the advance mutation, plus a lifecycle
// event carrying the prior holder, the reason, and the stealing actor — the same auditable
// trail a refusal leaves, so a steal is a loud, recorded override, not a silent bypass,
// whether or not it also transfers the lease.
func (s *Store) recordLeaseSteal(w envelope.WorkEnvelope, actor, holder, reason, capability, action string, transfersLease bool, now time.Time) error {
	if transfersLease {
		if err := s.transferLeaseOnSteal(w.ID, actor); err != nil {
			return err
		}
	}
	ai := s.leaseGateActionIntent(w, actor, capability, action, now)
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := policy.LeaseHolderDenyDecision(ai, fmt.Sprintf("lease held by %q stolen by %q: %s", holder, actor, reason), now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     fmt.Sprintf("lease stolen from %q: %s", holder, reason),
		Attached: []string{ai.ID, pd.ID},
	})
}

// transferLeaseOnSteal is the actual hand-off a steal performs: it rewrites workID's live
// lease so By becomes actor, preserving every other field (WorkID, ClaimAt, LaneID,
// WorkSHA256) exactly as the original claim/advance-lane recorded them. Without this write,
// work/leases/<id>.json keeps naming the original holder, so that holder's next unguarded
// advance/close would pass leaseHolderStatusFor's holder==actor short-circuit as if the steal
// never happened — the P0 this closes. It reuses the same atomic stage+rename publish path
// the claim flow uses (publishClaimJSON, via stageJSON + commitStagedJSON), so a torn write is
// not observable: either the file ends up wholly rewritten, or (rename never reached) it is
// left wholly as it was.
//
// Read-then-write is race-free here: every caller reaches this seam only from inside
// withLifecycleMutation's home-level lock (Advance/Close both take it before ever calling
// leaseHolderAdvanceGate/leaseHolderCloseGate), so no concurrent writer can observe or produce
// a torn intermediate state.
//
// Failure is returned as-is, deliberately BEFORE recordLeaseSteal writes any of the
// ActionIntent/PolicyDecision/lifecycle-event trail: a write failure here must fail the whole
// steal closed rather than let the ledger claim a steal that did not actually take the lease.
// TestAdvanceStealFailsClosedWhenLeaseTransferWriteFails fault-injects exactly this path
// (chmod work/leases read-only) and asserts no trail entry and no mutation follow through.
func (s *Store) transferLeaseOnSteal(workID, actor string) error {
	lease, err := s.readLease(workID)
	if err != nil {
		return fmt.Errorf("steal %s: read live lease before transfer: %w", workID, err)
	}
	lease.By = actor
	if err := s.publishClaimJSON(s.leasePath(workID), &lease); err != nil {
		return fmt.Errorf("steal %s: transfer lease to %q: %w", workID, actor, err)
	}
	return nil
}

// refusalNotePrefix keeps the close refusal event Note byte-identical ("close refused: ...")
// while giving other gated verbs (advance: "advance refused: ...") their own prefix, derived
// from the capability ("work.<verb>") rather than adding yet another parameter everywhere.
func refusalNotePrefix(capability string) string {
	return strings.TrimPrefix(capability, "work.") + " refused: "
}

// leaseGateActionIntent builds the ActionIntent shared by recordLeaseHolderDeny and
// recordLeaseSteal — a distinct, non-mutating "audit only" action against the work, kept
// separate from the actual mutation's own ActionIntent (closeActionIntent in close_plan.go,
// advanceActionIntent in advance_plan.go) so the lease-gate record never masquerades as the
// mutation itself. capability/action are supplied by the caller (e.g. "work.close" /
// "close_denied_not_lease_holder", "work.advance" / "advance_denied_not_lease_holder") so
// this one builder serves every gated verb.
func (s *Store) leaseGateActionIntent(w envelope.WorkEnvelope, actor, capability, action string, now time.Time) envelope.ActionIntent {
	targetPath := s.workPath(w.ID)
	return envelope.ActionIntent{
		Header: newHeader(
			envelope.SVActionIntent,
			idFor("act_", capability, action, "work", w.ID, targetPath, actor, ts(now)),
			now,
			w.ID,
			w.IDs.ContextPackID,
			w.Privacy,
			w.Risk,
		),
		Actor:         actor,
		Capability:    capability,
		Action:        action,
		Target:        envelope.ActionTarget{Kind: "work", ID: w.ID, Path: targetPath},
		MutationClass: "local_reversible",
		Reversibility: "manual",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("%s work %s", action, w.ID),
		RollbackPlan:  "no mutation recorded; work state unchanged",
	}
}
