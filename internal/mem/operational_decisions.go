package mem

import "strings"

// AuthoritativeActiveOperationalDecisions is the single pure fold for
// operational decisions that may authorize an autonomous action. It admits
// only accepted, reviewed, authority-stamped records and applies explicit
// decision-id supersession before returning live claims.
func AuthoritativeActiveOperationalDecisions(claims []Claim) []Claim {
	accepted := make([]Claim, 0, len(claims))
	superseded := map[string]bool{}
	for _, claim := range claims {
		q := claim.Qualifiers
		if claim.ClaimType != "decision" || !HasOperationalDecisionAuthority(q) ||
			strings.TrimSpace(q["lifecycle_status"]) != "accepted" || strings.TrimSpace(q["reviewed_by"]) == "" {
			continue
		}
		accepted = append(accepted, claim)
		for _, id := range strings.Split(q["supersedes"], ",") {
			if id = strings.TrimSpace(id); id != "" {
				superseded[id] = true
			}
		}
	}
	out := make([]Claim, 0, len(accepted))
	for _, claim := range accepted {
		decisionID := strings.TrimSpace(claim.Qualifiers["decision_id"])
		if decisionID == "" || superseded[decisionID] {
			continue
		}
		out = append(out, claim)
	}
	return out
}
