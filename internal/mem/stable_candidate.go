package mem

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
)

// RecordStableCandidate writes a caller-supplied memory candidate using the same
// stable candidate path used by knowledge page import. Admission decides whether
// the candidate enters operator review or stays parked as source-index or
// annotated-evidence material.
func (s *Store) RecordStableCandidate(c Candidate) (bool, error) {
	if strings.TrimSpace(c.ID) == "" {
		return false, fmt.Errorf("candidate id required")
	}
	if strings.TrimSpace(c.Entity) == "" || strings.TrimSpace(c.Predicate) == "" || strings.TrimSpace(c.Object) == "" {
		return false, fmt.Errorf("candidate %s needs entity, predicate, object", c.ID)
	}
	if c.Derivation == "" {
		c.Derivation = "extracted"
	}
	if !oneOf(c.Derivation, Derivations) {
		return false, fmt.Errorf("candidate %s: bad derivation %q", c.ID, c.Derivation)
	}
	if c.ClaimType != "" && !oneOf(c.ClaimType, ClaimTypes) {
		return false, fmt.Errorf("candidate %s: bad claim_type %q", c.ID, c.ClaimType)
	}
	if c.PromotionBasis != "" && !candidates.ValidBasis(c.PromotionBasis) {
		return false, fmt.Errorf("candidate %s: bad promotion_basis %q", c.ID, c.PromotionBasis)
	}
	if c.Visibility == "" {
		c.Visibility = "operational"
	}
	c.EvidenceRefs = uniqueStrings(c.EvidenceRefs)
	if !oneOf(c.Visibility, Visibilities) {
		return false, fmt.Errorf("candidate %s: bad visibility %q", c.ID, c.Visibility)
	}
	if err := validateVolatileWrite("candidate "+c.ID, c.Visibility, c.ExpiresAt, c.ClaimType, c.CanonClass); err != nil {
		return false, err
	}
	c, _ = AdmitCandidateAt(c, time.Now().UTC())
	if !validStableCandidateState(c.State) {
		return false, fmt.Errorf("candidate %s state must be proposed, parked, or no_promote, got %q", c.ID, c.State)
	}
	return s.writeStableCandidate(c)
}

func validStableCandidateState(state string) bool {
	return state == "proposed" || state == "parked" || state == "no_promote"
}
