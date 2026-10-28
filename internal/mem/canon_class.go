package mem

// CanonClasses is the closed kind-of-truth taxonomy. It decides (later slices)
// how a claim may surface and whether it may ever change silently.
//
// Ambient-tier mapping (ambient-everywhere / matching-context / recall-only) is
// documented here for consumers but NOT applied in this slice — pack-assembly
// filtering is a deliberate later behavior change.
//
// Risk tiers (CanonRiskTier):
//   - operator_permanent: decision, principle, behavior — never auto-promote or
//     silently supersede (permanently operator-gated).
//   - low_risk: research_fact, product_metric, status — the only classes eligible
//     for future silent supersede/promote (behavior ships OFF until clean-window).
//   - gated: product_fact, unclassified, and any unknown input.
var CanonClasses = []string{
	"decision",
	"principle",
	"behavior",
	"product_fact",
	"product_metric",
	"research_fact",
	"status",
}

// CanonClassUnclassified is the derived-only fallback when neither an explicit
// canon_class nor a ClaimType mapping applies. It is never accepted as a stored value.
const CanonClassUnclassified = "unclassified"

// Canon risk-tier labels returned by CanonRiskTier.
const (
	CanonRiskOperatorPermanent = "operator_permanent"
	CanonRiskGated             = "gated"
	CanonRiskLowRisk           = "low_risk"
)

// claimTypeToCanonClass maps the legacy ClaimType vocabulary onto canon classes.
// Total over ClaimTypes; unknown/empty → unclassified (never earns a low-risk lane).
var claimTypeToCanonClass = map[string]string{
	"decision":     "decision",
	"preference":   "principle", // preferences are standing rules — operator-permanent
	"constraint":   "principle",
	"architecture": "decision", // architecture choices are decisions — never silent
	"status":       "status",
}

// EffectiveCanonClass: explicit stored field wins; else deterministic ClaimType
// mapping; else unclassified. Does not mutate storage.
func EffectiveCanonClass(claimType, canonClass string) string {
	if canonClass != "" {
		if oneOf(canonClass, CanonClasses) {
			return canonClass
		}
		// Explicit but invalid is treated as unclassified for consumers; Capture
		// already refuses invalid values on write.
		return CanonClassUnclassified
	}
	if mapped, ok := claimTypeToCanonClass[claimType]; ok {
		return mapped
	}
	return CanonClassUnclassified
}

// CanonRiskTier returns operator_permanent | gated | low_risk for a class.
func CanonRiskTier(class string) string {
	switch class {
	case "decision", "principle", "behavior":
		return CanonRiskOperatorPermanent
	case "research_fact", "product_metric", "status":
		return CanonRiskLowRisk
	default:
		// product_fact, unclassified, unknown — gated
		return CanonRiskGated
	}
}

// CanonClassLowRisk reports whether class is eligible for future silent
// supersede/promote. Eligibility only — the behavior ships OFF until a later slice.
func CanonClassLowRisk(class string) bool {
	return CanonRiskTier(class) == CanonRiskLowRisk
}

// CanonClassOperatorPermanent reports whether class may never auto-promote or
// silently supersede.
func CanonClassOperatorPermanent(class string) bool {
	return CanonRiskTier(class) == CanonRiskOperatorPermanent
}

// CanonClassReport is the read-only JSON shape for `compound memory canon`.
type CanonClassReport struct {
	SchemaVersion string            `json:"schema_version"`
	Vocabulary    []string          `json:"vocabulary"`
	Mapping       map[string]string `json:"mapping"`
	Tiers         map[string]string `json:"tiers"`
	Distribution  map[string]int    `json:"distribution"`
	Total         int               `json:"total"`
}

// CanonClassReportSchemaVersion is the wire schema for memory canon reports.
const CanonClassReportSchemaVersion = "compound.memory.canon.v0.1"

// CanonDistribution folds current (non-superseded) claims into EffectiveCanonClass counts.
// Read-only: no ledger or registry writes.
func (s *Store) CanonDistribution() (CanonClassReport, error) {
	claims, err := s.AllCurrent()
	if err != nil {
		return CanonClassReport{}, err
	}
	dist := map[string]int{}
	for _, class := range CanonClasses {
		dist[class] = 0
	}
	dist[CanonClassUnclassified] = 0
	for _, c := range claims {
		class := EffectiveCanonClass(c.ClaimType, c.CanonClass)
		dist[class]++
	}
	mapping := map[string]string{}
	for _, ct := range ClaimTypes {
		mapping[ct] = claimTypeToCanonClass[ct]
	}
	tiers := map[string]string{}
	for _, class := range CanonClasses {
		tiers[class] = CanonRiskTier(class)
	}
	tiers[CanonClassUnclassified] = CanonRiskTier(CanonClassUnclassified)
	return CanonClassReport{
		SchemaVersion: CanonClassReportSchemaVersion,
		Vocabulary:    append([]string(nil), CanonClasses...),
		Mapping:       mapping,
		Tiers:         tiers,
		Distribution:  dist,
		Total:         len(claims),
	}, nil
}
