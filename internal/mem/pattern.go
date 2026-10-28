package mem

import "fmt"

// minPatternClaims is the recurrence floor — a single claim about a topic is not a pattern.
const minPatternClaims = 2

// Pattern is a recurring through-line — an (entity, topic) the ledger returns to across ≥
// minPatternClaims claims. It is a HYPOTHESIS with support (claim ids + how many distinct sessions),
// never canon — the spine-native convergence of the Gen-1 pattern.Build (the audit's "no equivalent
// anywhere in canonical"). The semantic version (an LLM naming the through-line) converges later.
type Pattern struct {
	ID            string   `json:"id"` // pat::entity::topic
	Entity        string   `json:"entity"`
	Topic         string   `json:"topic"`
	Description   string   `json:"description"`
	SupportClaims []string `json:"support_claims"` // the claim ids in the recurrence
	Sessions      int      `json:"sessions"`       // distinct source traces (recurrence breadth)
	Confidence    float64  `json:"confidence"`
	Status        string   `json:"status"` // hypothesis — not canon
}

// Patterns folds the ledger into recurrence hypotheses: per (entity, topic), if ≥ minPatternClaims
// claims exist, emit a Pattern annotated with its support claims and the number of distinct sessions
// that touched it. Derived (rebuildable), additive, mutation-free.
func (s *Store) Patterns() ([]Pattern, error) {
	claims, err := s.loadClaims()
	if err != nil {
		return nil, err
	}
	type key struct{ entity, topic string }
	type agg struct {
		claims   []string
		sessions map[string]bool
	}
	m := map[key]*agg{}
	var order []key
	for _, c := range claims {
		k := key{c.Entity, c.Topic()} // domain-aware topic axis (falls back to predicate)
		a := m[k]
		if a == nil {
			a = &agg{sessions: map[string]bool{}}
			m[k] = a
			order = append(order, k)
		}
		a.claims = append(a.claims, c.ID)
		a.sessions[claimSession(c)] = true
	}
	out := make([]Pattern, 0)
	for _, k := range order {
		a := m[k]
		if len(a.claims) < minPatternClaims {
			continue
		}
		out = append(out, Pattern{
			ID:            "pat::" + k.entity + "::" + k.topic,
			Entity:        k.entity,
			Topic:         k.topic,
			Description:   fmt.Sprintf("recurring: %d claims about %s/%s across %d session(s)", len(a.claims), k.entity, k.topic, len(a.sessions)),
			SupportClaims: a.claims,
			Sessions:      len(a.sessions),
			Confidence:    0.5,
			Status:        "hypothesis",
		})
	}
	return out, nil
}

// claimSession returns the source trace ("session") a claim came from — by construction the last
// evidence ref (EvidenceRefs = [candidate_id, source_trace_ref]). Distinct sessions = recurrence breadth.
func claimSession(c Claim) string {
	if n := len(c.EvidenceRefs); n > 0 {
		return c.EvidenceRefs[n-1]
	}
	return ""
}
