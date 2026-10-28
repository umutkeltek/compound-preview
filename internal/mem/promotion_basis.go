package mem

import "compound/internal/candidates"

// PromotionBasisStrippedQualifier is the qualifier key under which Capture
// records the original promotion_basis it stripped from an intake-supplied
// candidate. The value is the basis string itself ("runtime_proof" /
// "curated_page" / "decision_record" / "trusted_deterministic_import"). The
// key is reserved for this purpose; no other organ reads it.
//
// Why audit-after-strip, not refuse-on-intake: a self-declared promotion
// basis is high-signal evidence (the producer THOUGHT a basis applied —
// wrong, but informative). Refusing the candidate on intake would drop both
// the candidate and the signal; recording it under this qualifier keeps the
// audit trail reviewable without inflating the durable ledger's basis
// value.
const PromotionBasisStrippedQualifier = "promotion_basis_stripped"

// systemDerivedPromotionBasis reports whether the given basis is one compound
// mints ONLY server-side via its owning derivation path:
//
//   - runtime_proof                → corroboration edges at Adjudicate time
//     (corroborationPromotionBasis).
//   - curated_page                 → knowledge-page import tooling at the
//     page-import door (not via Capture).
//   - decision_record              → Decisions authority (not via Capture).
//   - trusted_deterministic_import → a trusted deterministic adapter (not
//     via raw intake).
//
// A raw-intake candidate carrying one of these is the doctrine violation
// the packet work_2742cffbeb5f closes: "evidence is not truth; a model-
// supplied promotion basis is exactly the reject case." Only
// BasisExplicitConfirmation is self-attested — it is preserved through
// intake and gated separately at Adjudicate (intent.ExplicitConfirmationRef).
func systemDerivedPromotionBasis(basis string) bool {
	switch basis {
	case candidates.BasisRuntimeProof,
		candidates.BasisCuratedPage,
		candidates.BasisDecisionRecord,
		candidates.BasisTrustedDeterministic:
		return true
	}
	return false
}

// stripIntakePromotionBasis zeroes an intake-supplied promotion_basis when
// it is system-derived, recording the original in Qualifiers under
// PromotionBasisStrippedQualifier. Self-attested bases (explicit_confirmation)
// and any unrecognized value are preserved untouched. The Qualifiers map is
// shallow-copied before the qualifier is added so the caller's map is never
// mutated — important for tests and adapter callers that reuse candidate
// literals.
//
// Called only from Capture() (raw intake door); adapter-side doors such as
// RecordStableCandidate and the page_import validation path do not run it,
// because they are not raw intake (the packet carves out the page-import
// behavior explicitly).
func stripIntakePromotionBasis(c *Candidate) {
	if c.PromotionBasis == "" {
		return
	}
	if !systemDerivedPromotionBasis(c.PromotionBasis) {
		return
	}
	original := c.PromotionBasis
	next := make(map[string]string, len(c.Qualifiers)+1)
	for k, v := range c.Qualifiers {
		next[k] = v
	}
	next[PromotionBasisStrippedQualifier] = original
	c.Qualifiers = next
	c.PromotionBasis = ""
}
