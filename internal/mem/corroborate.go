package mem

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CorroborateClaim appends corroborating evidence to an existing current claim
// WITHOUT minting a new claim version: it records the evidence item (idempotent,
// content-addressed) and appends a `reinforces` edge (evidence -> claim) — the
// exact shape reinforceIfEcho already writes when a candidate echoes a current
// claim. This is the ruling harvester's "a restatement of accepted law appends
// evidence, no new card, no duplicate" path (grants-as-gates decisions-down,
// ruling-harvest falsifier a/b). Re-corroborating a claim with the same evidence
// is a no-op: the reinforces edge is deduped by (from, to), so replaying a
// transcript never multiplies edges.
//
// It returns the recorded evidence, whether a NEW reinforces edge was appended,
// and an error. It refuses to corroborate a claim id that is not on the ledger,
// so a typo can never create a dangling edge.
func (s *Store) CorroborateClaim(claimID string, ev EvidenceItem, confidence float64, now time.Time) (EvidenceItem, bool, error) {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return EvidenceItem{}, false, fmt.Errorf("corroborate: claim id required")
	}
	var recorded EvidenceItem
	var added bool
	err := s.withLock(func() error {
		claims, err := s.loadClaimsRaw()
		if err != nil {
			return err
		}
		found := false
		for _, c := range claims {
			if c.ID == claimID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("corroborate: claim %s not found on ledger", claimID)
		}
		rec, err := s.RecordEvidence(ev, now)
		if err != nil {
			return err
		}
		recorded = rec
		exists, err := s.hasReinforceEdge(rec.ID, claimID)
		if err != nil {
			return err
		}
		if exists {
			return nil // idempotent: this evidence already corroborates this claim
		}
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		edge := Edge{
			Seq:          seq,
			Kind:         EdgeKindReinforces,
			From:         rec.ID,
			To:           claimID,
			Confidence:   confidence,
			EvidenceRefs: []string{rec.ID},
			CreatedAt:    rfc3339(now),
		}
		if err := appendJSONL(s.edgesLog(), &edge); err != nil {
			return err
		}
		added = true
		return nil
	})
	return recorded, added, err
}

// CorroborationEvidence lists the evidence ids that reinforce a given claim via
// reinforces edges (the read side of CorroborateClaim). It is a bounded,
// metadata-only projection used to prove corroboration landed without exposing
// the ledger internals.
func (s *Store) CorroborationEvidence(claimID string) ([]string, error) {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return nil, fmt.Errorf("corroboration evidence: claim id required")
	}
	seen := map[string]bool{}
	var refs []string
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind == EdgeKindReinforces && e.To == claimID && e.From != "" && !seen[e.From] {
			seen[e.From] = true
			refs = append(refs, e.From)
		}
		return nil
	})
	return refs, err
}

func (s *Store) hasReinforceEdge(from, to string) (bool, error) {
	exists := false
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind == EdgeKindReinforces && e.From == from && e.To == to {
			exists = true
		}
		return nil
	})
	return exists, err
}
