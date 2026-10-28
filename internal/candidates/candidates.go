// Package candidates is the SINGLE source of truth for candidate kinds and where each one
// lives under COMPOUND_HOME. It exists because the same concept had three folder names — the
// home created `inbox/capability-observations`, the distiller wrote `inbox/capability_observation`,
// and the capability graph read a third spelling — which is "same concept, three folder names,
// no single contract": the fragmentation disease in miniature. Every producer (distiller, mem)
// and consumer (capgraph, the memory adjudicator, HabitatCatalog) now derives its inbox path from
// THIS map, so a candidate kind can have exactly one home. It is a pure leaf: it imports nothing
// internal, so nothing can create an import cycle through it.
package candidates

import "sort"

// Kind is a typed candidate kind. These are the ONLY values that may key an inbox path — a Kind
// keys a filesystem directory, so an unknown kind must never reach disk (path-traversal guard).
type Kind string

const (
	Memory            Kind = "memory"
	Skill             Kind = "skill"
	Decision          Kind = "decision"
	FailurePattern    Kind = "failure_pattern"
	Taste             Kind = "taste"
	ProductHypothesis Kind = "product_hypothesis"
	RouteObservation  Kind = "route_observation"
	CapabilityObs     Kind = "capability_observation"
	MethodUpgrade     Kind = "method_upgrade"
	EvalCase          Kind = "eval_case"
	PolicyPatch       Kind = "policy_patch"
	ContentDraft      Kind = "content_draft"
)

// Adjudication verdicts — the shared vocabulary a candidate can receive. `no_promote` is the
// explicit "seen, deliberately not promoted" state (distinct from park/reject) so a candidate
// does not re-enter the loop as "new".
const (
	Reject         = "reject"
	Park           = "park"
	Promote        = "promote"
	Operationalize = "operationalize"
	Retire         = "retire"
	NoPromote      = "no_promote"
)

// PromotionBasis — the ONLY grounds on which a memory candidate may promote to canon. "the model
// extracted it with high confidence" is deliberately NOT one of them: high confidence is not a
// promotion basis (memory is evidence, not truth). This is the CANONICAL home for the taxonomy;
// Gen-1 internal/memory.PromotionBasis converged here; this is now the canonical taxonomy.
const (
	BasisExplicitConfirmation = "explicit_confirmation"        // the user decided it
	BasisCuratedPage          = "curated_page"                 // it is in a curated source
	BasisDecisionRecord       = "decision_record"              // backed by a Decision
	BasisRuntimeProof         = "runtime_proof"                // proven by a run / state delta
	BasisTrustedDeterministic = "trusted_deterministic_import" // a trusted deterministic importer/adapter
)

// PromotionBases is the allowlist; an empty or unknown basis means "no basis" (parks in live mode).
var PromotionBases = []string{BasisExplicitConfirmation, BasisCuratedPage, BasisDecisionRecord, BasisRuntimeProof, BasisTrustedDeterministic}

// ValidBasis reports whether s is a recognized promotion basis.
func ValidBasis(s string) bool {
	for _, b := range PromotionBases {
		if b == s {
			return true
		}
	}
	return false
}

// Spec is everything the kernel needs to know about one candidate kind: where it lands, which
// verdicts it may receive, and who consumes it. Consumer is what the architecture doctor uses to
// prove "every inbox dir has a producer or consumer" — no orphan directories.
type Spec struct {
	Kind     Kind
	InboxDir string   // canonical directory under COMPOUND_HOME (one name, forever)
	Verdicts []string // allowed adjudication verdicts
	Consumer string   // who consumes it (doctor's producer/consumer check)
}

// specs — the canonical table. Directory names are human-readable hyphen-plural; the Kind value
// stays underscore-singular (it is the typed enum carried in candidate JSON records).
var specs = map[Kind]Spec{
	Memory:            {Memory, "inbox/memory-candidates", []string{Park, Promote, Reject, NoPromote}, "compound memory adjudicate"},
	Skill:             {Skill, "inbox/skill-candidates", []string{Park, Operationalize, Reject, NoPromote}, "compound capability skill-plan"},
	Decision:          {Decision, "inbox/decision-candidates", []string{Park, Promote, Reject, NoPromote}, "compound decisions candidate-plan"},
	FailurePattern:    {FailurePattern, "inbox/failure-patterns", []string{Park, Operationalize, Reject}, "compound distiller rule-plan"},
	Taste:             {Taste, "inbox/taste-observations", []string{Park, Operationalize, Reject}, "compound distiller taste-plan"},
	ProductHypothesis: {ProductHypothesis, "inbox/product-hypotheses", []string{Park, Operationalize, Reject}, "compound product hypothesis-plan"},
	RouteObservation:  {RouteObservation, "inbox/route-observations", []string{Park, Operationalize}, "compound route plan"},
	CapabilityObs:     {CapabilityObs, "inbox/capability-observations", []string{Operationalize, Park}, "capgraph.Consume"},
	MethodUpgrade:     {MethodUpgrade, "inbox/method-upgrades", []string{Park, Operationalize}, "compound distiller improvement-plan"},
	EvalCase:          {EvalCase, "inbox/eval-cases", []string{Operationalize, Park}, "compound distiller eval-plan"},
	PolicyPatch:       {PolicyPatch, "inbox/policy-patches", []string{Park, Operationalize}, "compound distiller improvement-plan"},
	ContentDraft:      {ContentDraft, "inbox/content-drafts", []string{Park, Operationalize, Reject}, "compound product content-draft"},
}

// InboxDir returns the canonical directory for a kind, or "" if the kind is unknown (callers
// MUST treat "" as a hard error before touching disk).
func InboxDir(k Kind) string {
	if s, ok := specs[k]; ok {
		return s.InboxDir
	}
	return ""
}

// Known reports whether k is a recognized candidate kind.
func Known(k Kind) bool { _, ok := specs[k]; return ok }

// SpecFor returns the full spec and whether it exists.
func SpecFor(k Kind) (Spec, bool) { s, ok := specs[k]; return s, ok }

// AllInboxDirs returns every canonical inbox directory, sorted (for HabitatCatalog to create).
func AllInboxDirs() []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.InboxDir)
	}
	sort.Strings(out)
	return out
}

// AllKinds returns every kind, sorted by value (deterministic for tests and the doctor).
func AllKinds() []Kind {
	out := make([]Kind, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
