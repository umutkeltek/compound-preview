// probe_group.go — probe-group label parsing shared by capgraph's receipt-sweep
// projections (work_8713c4206232, S4 of the probe-group/temperature program).
//
// internal/contract's probe-group selector (probe_contracts.go,
// loadProbeGroupedRefsFromView) stamps a "probe:<class>/<group>; " prefix onto a
// probed ref's Reason before it lands in ContextPack.Included[] (contract.go's
// projectRuntimePack copies ref.Reason verbatim into IncludedRef.Reason for both
// memory refs and position refs — see contract.go:815-830). This file duplicates the
// label's SHAPE ONLY (never contract's selection logic) so capgraph's projections can
// attribute injected/consumed/ignored counts per probe group without importing
// internal/contract: capgraph is a lower-layer projection consumer of receipts+envelope,
// and internal/contract already imports capgraph (capability offers/authority revision),
// so importing back here would cycle. This is the same "duplicate the constant, not the
// layer" discipline internal/mem/temperature.go already uses for
// capgraph.CapabilityUsageRelPath.
//
// Fail-safe by construction: ANY reason that does not carry a well-formed label — no
// "probe:" prefix, no "/" class/group separator, no "; " terminator, or an empty class or
// group component — parses as (ok=false). The caller then treats the ref exactly like a
// legacy pack that predates probe-group selection: this can never surface as an error, a
// malformed label is just another shape of "no label".
package capgraph

import "strings"

// probeGroupLabelPrefix is the literal prefix internal/contract's probe-group selector
// stamps onto a probed ref's Reason (see probe_contracts.go:
// `label := "probe:" + plan.Class + "/" + group.Group + "; "`, prepended to the ref's
// original reason).
const probeGroupLabelPrefix = "probe:"

// probeGroupLabelSeparator terminates the class/group label before the ref's original
// reason text resumes.
const probeGroupLabelSeparator = "; "

// parseProbeGroupLabel extracts "<class>/<group>" from a probe-group-stamped
// IncludedRef.Reason. ok=false for any reason that lacks the "probe:" prefix, lacks the
// "/" class/group separator, lacks the "; " terminator, or has an empty class or group
// component — every one of those counts as unlabeled, never an error.
func parseProbeGroupLabel(reason string) (label string, ok bool) {
	if !strings.HasPrefix(reason, probeGroupLabelPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(reason, probeGroupLabelPrefix)
	head, _, found := strings.Cut(rest, probeGroupLabelSeparator)
	if !found {
		return "", false
	}
	class, group, found := strings.Cut(head, "/")
	if !found {
		return "", false
	}
	class = strings.TrimSpace(class)
	group = strings.TrimSpace(group)
	if class == "" || group == "" {
		return "", false
	}
	return class + "/" + group, true
}
