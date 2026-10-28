package work

// Advance-gate lease enforcement. Claim already refuses a second claimant (work.go, "already
// claimed by %q"), and Close got the same lease-holder gate (lease_close.go): a non-holder
// actor could still walk a claimed envelope running->verifying->done-adjacent states through
// Advance, corrupting the holder's liveness signal — the last ungated mutation verb. This file
// closes that door the same way Close's was closed, generalizing leaseHolderStatusFor and
// leaseGateActionIntent (lease_close.go) for the "advance" verb instead of copying them.
//
// Design mirrors leaseHolderCloseGate exactly:
//   - No lease (never claimed, or lease residue absent) = today's behavior, untouched.
//   - Same actor as the lease holder = today's behavior, untouched (exact string equality).
//   - A different actor with no --steal, or --steal with an empty reason, is REFUSED: the
//     refusal is recorded (ActionIntent + deny PolicyDecision + lifecycle event) and the work
//     is left exactly as it was.
//   - A different actor with --steal and a non-empty reason SUCCEEDS: the steal is recorded
//     (ActionIntent + PolicyDecision + lifecycle event carrying prior holder, reason, and the
//     stealing actor) and the caller proceeds.
//
// Lease TRANSFER on steal is conditional on the verb, unlike the trail above (see
// recordLeaseSteal/transferLeaseOnSteal in lease_close.go). A terminal Close does not
// transfer: it binds the override into terminal publication, retains the original lease
// through the recoverable boundary, and releases it last, so it cannot
// mis-authorize. An advance-gate steal transfers only when in.To != w.State — a real
// lifecycle transition (the E1 twin-collision class: running->verifying and friends). A
// SAME-STATE advance steal (in.To == w.State) is the land-check/proof-attachment class
// (LandCheck's same-state Advance in land_check.go, "Steal ... forwarded only to the
// same-state Advance that attaches a successful recorded proof. They do not transfer the
// lease..."): --steal there authorizes exactly one evidence-attachment write, not a
// hand-off, and every subsequent lifecycle command must still see the original holder —
// see TestWorkLandCheckCLIStealOnlyOverridesProofAttachment (cmd/compound).

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

// leaseHolderAdvanceGate is the enforcement seam Advance calls after its provenance/halt
// guards (autonomyHaltGuard, editableSurfaceGuard, heldReleaseOperatorError,
// directorProposedReleaseOperatorError) and before applyAdvance. It refuses (recording a
// deny) a non-holder advance, records an explicit steal, and is a no-op for an unclaimed
// work or a same-actor advance. applyAdvance itself stays ungated — AdmitAuto calls it
// directly, bypassing this gate on purpose (its own kill-switch/eligibility gates apply).
func (s *Store) leaseHolderAdvanceGate(w envelope.WorkEnvelope, actor string, in AdvanceInput, now time.Time) error {
	status := s.leaseHolderStatusFor(w.ID, actor, in.Steal, in.StealReason, "advance")
	if status.Err != nil {
		return status.Err
	}
	switch {
	case status.Blocked:
		if err := s.recordLeaseHolderDeny(w, actor, status.Holder, status.Message, "work.advance", "advance_denied_not_lease_holder", now); err != nil {
			return err
		}
		return fmt.Errorf("cannot advance %s: %s", w.ID, status.Message)
	case status.Stolen:
		// A same-state advance (in.To == w.State) is the land-check/proof-attachment class:
		// --steal there authorizes one evidence write, not a hand-off, so the lease stays
		// put. Any real transition (in.To != w.State) is the E1 twin-collision class and
		// does transfer.
		transitions := in.To != w.State
		return s.recordLeaseSteal(w, actor, status.Holder, strings.TrimSpace(in.StealReason), "work.advance", "advance_lease_stolen", transitions, now)
	default:
		return nil
	}
}
