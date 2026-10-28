package policy

// Work-lease holder gate (close-gate lease enforcement): a WorkEnvelope lease becomes
// enforceable at close. internal/work's close gate reads the lease live and, when the
// closing actor is not the current holder, records this as a recordable deny — mirroring
// the autonomy kill-switch (autonomy_halt.go) and editable-surfaces (editable_surfaces.go)
// deny patterns — so a refused close (or an explicit --steal override) leaves an auditable
// envelope instead of a silent state change. The admit/deny decision itself is made by the
// caller (internal/work): this PolicyDecision is descriptive audit content, not a live gate
// evaluated by an adapter.

import (
	"time"

	"compound/internal/envelope"
)

// LeaseHolderAuthorityRef names the work lease as a policy authority for evidence refs.
const LeaseHolderAuthorityRef = "compound://registries/work-leases"

// LeaseHolderDenyDecision builds a recorded deny PolicyDecision for a close attempted by
// an actor who is not the work's current lease holder, so the refusal (or a recorded
// --steal override) leaves an auditable trail, not just an error.
func LeaseHolderDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, LeaseHolderAuthorityRef)
	return pd
}
