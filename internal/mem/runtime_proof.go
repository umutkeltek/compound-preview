package mem

import (
	"fmt"
	"strings"
	"time"
)

// RecordRuntimeProofCorroboration attaches runtime-proof corroboration marks to a candidate so a
// subsequent Adjudicate("promote") derives promotion_basis=runtime_proof through the EXISTING
// corroborationPromotionBasis path (mem.go) — never a hand-written claim, never a basis stamped out
// of band. Each distinct proof ref becomes one append-only EdgeKindCorroboration edge
// (From=proof ref, To=candidate); a ref already recorded is skipped, so a re-run is idempotent and
// adds nothing.
//
// This is the governed door a DETERMINISTIC runtime gate uses to present run / state-delta proof.
// The lane-belief cross-lane promotion gate (B3) is the first caller: it — not this method —
// decides whether the proof is ADMISSIBLE for the belief's valence (a cross-lane proof-bearing mark
// for a standing-raising belief; the subject lane's own deterministic receipt for a standing-
// lowering belief). This method only records the mark the gate already judged sufficient, keeping
// the runtime_proof basis (candidates.PromotionBases) derivable without weakening any existing
// promotion basis. Returns the number of NEW marks appended.
//
// Additive B3 premise-resolution: the runtime_proof basis constant already exists, but no exported
// door let a runtime gate attach it to a candidate whose producer (the B1 belief valve) forbids a
// pre-stamped basis. This is that door — minimal, and it reuses the corroboration-mark mechanism
// rather than introducing a new derivation.
func (s *Store) RecordRuntimeProofCorroboration(candID string, proofRefs []string, now time.Time) (int, error) {
	candID = strings.TrimSpace(candID)
	if candID == "" {
		return 0, fmt.Errorf("runtime-proof corroboration: candidate id required")
	}
	refs := make([]string, 0, len(proofRefs))
	for _, r := range proofRefs {
		if r = strings.TrimSpace(r); r != "" {
			refs = append(refs, r)
		}
	}
	refs = uniqueStrings(refs)
	if len(refs) == 0 {
		return 0, fmt.Errorf("runtime-proof corroboration: candidate %s needs at least one proof ref (a run/state-delta proof)", candID)
	}
	added := 0
	err := s.withLock(func() error {
		if _, err := s.getCandidate(candID); err != nil {
			return fmt.Errorf("runtime-proof corroboration: %w", err)
		}
		existing, err := s.candidateReviewMarksAsOf(0)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		if mark, ok := existing[candID]; ok {
			for _, r := range mark.Refs {
				have[r] = true
			}
		}
		for _, ref := range refs {
			if have[ref] {
				continue
			}
			seq, err := s.nextSeq()
			if err != nil {
				return err
			}
			edge := Edge{
				Seq:          seq,
				Kind:         EdgeKindCorroboration,
				From:         ref,
				To:           candID,
				Confidence:   1,
				EvidenceRefs: []string{ref},
				CreatedAt:    rfc3339(now),
			}
			if err := appendJSONL(s.edgesLog(), &edge); err != nil {
				return err
			}
			have[ref] = true
			added++
		}
		return nil
	})
	return added, err
}
