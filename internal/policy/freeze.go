package policy

// Freeze registry (grants-as-gates, 2026-07-10): the sibling of the autonomy
// kill-switch (autonomy_halt.go) and the editable-surfaces whitelist
// (editable_surfaces.go), read at the SAME admission seam — but its source
// of truth is a decision RECORD (internal/decisions), never a config file or
// markdown prose (falsifier e). This file owns only:
//   - the plain FreezeRecord shape (a minimal projection of an active freeze
//     decision claim: id, owner globs, keywords, provenance)
//   - the pure literal matcher (FreezeMatch): no inference, no classifier
//     (settled design decision 2) — an owner glob or a keyword substring, or
//     the envelope passes untouched
//   - the recorded-deny PolicyDecision builder (FreezeDenyDecision),
//     mirroring AutonomyHaltDecision / EditableSurfaceDenyDecision exactly
//
// Loading ACTIVE freeze records from the decisions store lives in the
// caller (internal/work), which already imports both mem and policy.
// policy itself CANNOT import mem: internal/mem/admission.go already
// imports internal/policy, so the reverse edge would be an import cycle.

import (
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
)

// FreezeAuthorityRef names the freeze mechanism as a policy authority for
// evidence refs, mirroring AutonomyHaltAuthorityRef / EditableSurfacesAuthorityRef.
const FreezeAuthorityRef = "compound://registries/decision-freeze"

// FreezeRecord is the minimal projection of an active freeze decision record
// (internal/decisions.Lifecycle) this package needs to match a WorkEnvelope
// shape against. Scope is explicit, never inferred: owner globs and/or
// literal keyword substrings, matched literally against the envelope owner,
// canonical surface, and evidence refs.
type FreezeRecord struct {
	ID         string
	Title      string
	ReviewedBy string
	OwnerGlobs []string
	Keywords   []string
}

// FreezeMatch reports whether the given envelope shape matches ANY active
// freeze record via a literal owner glob (path.Match semantics) against
// owner, or a literal lowercase substring match of a keyword against the
// canonical surface or any evidence ref. No inference, no classifier — an
// envelope that matches nothing passes untouched.
func FreezeMatch(owner, canonicalSurface string, evidenceRefs []string, freezes []FreezeRecord) (bool, FreezeRecord) {
	owner = strings.TrimSpace(owner)
	surface := strings.ToLower(strings.TrimSpace(canonicalSurface))
	for _, f := range freezes {
		for _, glob := range f.OwnerGlobs {
			glob = strings.TrimSpace(glob)
			if glob == "" || owner == "" {
				continue
			}
			if ok, _ := filepath.Match(glob, owner); ok {
				return true, f
			}
		}
		for _, kw := range f.Keywords {
			kw = strings.ToLower(strings.TrimSpace(kw))
			if kw == "" {
				continue
			}
			if surface != "" && strings.Contains(surface, kw) {
				return true, f
			}
			for _, ref := range evidenceRefs {
				if strings.Contains(strings.ToLower(ref), kw) {
					return true, f
				}
			}
		}
	}
	return false, FreezeRecord{}
}

// FreezeDenyDecision builds a recorded deny PolicyDecision for an autonomous
// admission blocked by an active freeze record, so the denial leaves an
// auditable envelope, not just an error — mirrors AutonomyHaltDecision /
// EditableSurfaceDenyDecision exactly.
func FreezeDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, FreezeAuthorityRef)
	return pd
}
