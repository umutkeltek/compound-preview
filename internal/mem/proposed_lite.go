package mem

import (
	"encoding/json"
	"fmt"

	"compound/internal/candidates"
)

// CandidateLite is the narrow decode shape the promote-driver walker needs:
// enough to decide basis-gated promotion and exact duplicate collapse
// WITHOUT the full Candidate decode. Fields mirror the candidate JSON;
// anything absent here is re-read live by the mutation doors (Adjudicate
// re-reads before writing, so a lite-driven decision never writes on stale
// data — the door re-validates against the live file).
type CandidateLite struct {
	ID             string            `json:"id"`
	State          string            `json:"state"`
	PromotionBasis string            `json:"promotion_basis,omitempty"`
	Entity         string            `json:"entity"`
	Predicate      string            `json:"predicate"`
	Object         string            `json:"object"`
	CreatedAt      string            `json:"created_at"`
	Qualifiers     map[string]string `json:"qualifiers,omitempty"`
}

// CandidateLitePageEntry is one ordered walk outcome. A valid entry carries
// both the narrow decision projection and the descriptor-pinned Record that a
// later mutation must use as its compare-and-swap precondition. A corrupt
// entry carries its typed custody finding instead and is never actionable.
type CandidateLitePageEntry struct {
	Cursor     candidates.EntryCursor
	Candidate  *CandidateLite
	Record     *candidates.Record
	Corruption *candidates.Corruption
}

// CandidateLitePage preserves the one authoritative directory order across
// decoded and corrupt entries. Consumers must advance cursors from Entries,
// never from a filtered candidate slice.
type CandidateLitePage struct {
	Entries    []CandidateLitePageEntry
	Walked     int
	LastCursor candidates.EntryCursor
	AtEnd      bool
}

// ListCandidatesLite walks the descriptor-rooted memory-candidate inbox and
// decodes only the fields the promote driver and disposition sweep need.
func (s *Store) ListCandidatesLite(afterCursor candidates.EntryCursor, scanLimit int) (CandidateLitePage, error) {
	page, err := candidates.ReadPage(s.home, candidates.Memory, afterCursor, scanLimit)
	if err != nil {
		return CandidateLitePage{}, err
	}
	result := CandidateLitePage{
		Entries:    make([]CandidateLitePageEntry, 0, len(page.Entries)),
		Walked:     page.Walked,
		LastCursor: page.LastCursor,
		AtEnd:      page.AtEnd,
	}
	for _, entry := range page.Entries {
		outcome := CandidateLitePageEntry{Cursor: entry.EntryCursor}
		switch {
		case entry.Corruption != nil:
			copyOfCorruption := *entry.Corruption
			outcome.Corruption = &copyOfCorruption
		case entry.Record == nil:
			outcome.Corruption = &candidates.Corruption{EntryCursor: entry.EntryCursor, Code: candidates.CorruptionRaced, Detail: "page entry has no custody outcome"}
		default:
			var lite CandidateLite
			if err := json.Unmarshal(entry.Record.Body, &lite); err != nil {
				outcome.Corruption = &candidates.Corruption{EntryCursor: entry.EntryCursor, Code: candidates.CorruptionInvalidJSON, Detail: fmt.Sprintf("decode candidate projection: %v", err)}
			} else if lite.ID != entry.Record.ID {
				outcome.Corruption = &candidates.Corruption{EntryCursor: entry.EntryCursor, Code: candidates.CorruptionIDMismatch, Detail: fmt.Sprintf("decoded id %q, want %q", lite.ID, entry.Record.ID)}
			} else {
				copyOfLite := lite
				copyOfRecord := *entry.Record
				outcome.Candidate = &copyOfLite
				outcome.Record = &copyOfRecord
			}
		}
		result.Entries = append(result.Entries, outcome)
	}
	return result, nil
}
