package mem

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const CandidateListSchemaVersion = "compound.memory_candidate_list.v0.1"

type CandidateListOptions struct {
	State string
	Kind  string
	Limit int
	// CreatedAfter bounds the returned ITEMS to candidates created at or after
	// this instant (RFC3339; zero = unbounded). It never affects Counts:
	// ByState/Total/Matched are computed over the FULL snapshot so a bounded
	// caller still sees exact global state counts (the console briefing reads
	// its Mind Δ headline numbers from counts and its 14d weekly window from
	// the bounded items).
	CreatedAfter    time.Time
	IncludeEvidence bool
}

type CandidateListReport struct {
	SchemaVersion string                 `json:"schema_version"`
	AuthorityRef  string                 `json:"authority_ref"`
	GeneratedAt   string                 `json:"generated_at"`
	Counts        CandidateListCounts    `json:"counts"`
	Items         []CandidateListItem    `json:"items"`
	Warnings      []string               `json:"warnings,omitempty"`
	Filters       map[string]interface{} `json:"filters,omitempty"`
}

// CandidateListCounts separates "how many matched the filter" from "how many are in
// Items". Matched describes the CANDIDATE SET; Returned describes the page. They differ
// whenever Limit binds, and Truncated says so explicitly — a bounded listing must never
// read as a complete one.
type CandidateListCounts struct {
	Total   int `json:"total"`
	Corrupt int `json:"corrupt,omitempty"`
	Matched int `json:"matched"`
	// Returned is len(Items). Truncated is Matched > Returned.
	Returned  int            `json:"returned"`
	Truncated bool           `json:"truncated,omitempty"`
	ByState   map[string]int `json:"by_state"`
}

type CandidateListItem struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	ReviewState     string            `json:"review_state,omitempty"`
	Entity          string            `json:"entity,omitempty"`
	Predicate       string            `json:"predicate,omitempty"`
	Object          string            `json:"object,omitempty"`
	ObservationDate string            `json:"observation_date,omitempty"`
	Confidence      float64           `json:"confidence,omitempty"`
	Derivation      string            `json:"derivation,omitempty"`
	ClaimType       string            `json:"claim_type,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Visibility      string            `json:"visibility,omitempty"`
	PromotionBasis  string            `json:"promotion_basis,omitempty"`
	Qualifiers      map[string]string `json:"qualifiers,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	// RecurrenceCount + ConfidenceTrend are READ-ONLY DERIVED signals. They
	// are computed at projection time from existing ledger/inbox data and
	// never written back to the stored candidate JSON. The standing-law
	// console chip ("seen Nx, rising") uses them as a readiness cue, not a
	// promotion signal — promotion stays the governed memory-adjudicate
	// act.
	//
	// RecurrenceCount: distinct evidence refs across candidates that cite
	// the same normalized claim (entity|predicate|object). Includes the
	// current candidate. A lone candidate projects 1.
	//
	// ConfidenceTrend: "new" when only the current candidate exists for
	// the normalized key, "rising" when the current confidence is
	// strictly above any prior candidate's confidence (epsilon 1e-3),
	// "falling" when strictly below, "flat" otherwise.
	RecurrenceCount int    `json:"recurrence_count,omitempty"`
	ConfidenceTrend string `json:"confidence_trend,omitempty"`
	// AdjudicatedBy/AdjudicatedAt mirror Candidate's operator-ratification
	// provenance (see mem.Candidate) so `memory candidates --json` — the
	// projection the console standing-law queue reads — can distinguish a
	// candidate the operator explicitly ruled on (even "Keep aside") from one
	// still sitting untouched since ingestion.
	AdjudicatedBy  string                 `json:"adjudicated_by,omitempty"`
	AdjudicatedAt  string                 `json:"adjudicated_at,omitempty"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	ModifiedAt     string                 `json:"modified_at,omitempty"`
	SourceTraceRef string                 `json:"source_trace_ref,omitempty"`
	SnippetHash    string                 `json:"snippet_hash,omitempty"`
	EvidenceRefs   []string               `json:"evidence_refs,omitempty"`
	Evidence       *CandidateListEvidence `json:"evidence,omitempty"`
	BelievedBefore *CandidateListBelief   `json:"believed_before,omitempty"`
	Annotations    []CandidateAnnotation  `json:"annotations,omitempty"`
}

type CandidateListEvidence struct {
	Ref        string `json:"ref,omitempty"`
	SourceKind string `json:"source_kind,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Quote      string `json:"quote,omitempty"`
	EventTime  string `json:"event_time,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

type CandidateListBelief struct {
	ClaimID      string   `json:"claim_id,omitempty"`
	Entity       string   `json:"entity,omitempty"`
	Predicate    string   `json:"predicate,omitempty"`
	Object       string   `json:"object,omitempty"`
	ValidFrom    string   `json:"valid_from,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

func (s *Store) ListCandidates(now time.Time, opts CandidateListOptions) (CandidateListReport, error) {
	state := strings.TrimSpace(opts.State)
	kind := strings.TrimSpace(opts.Kind)
	if state != "" && !validCandidateListState(state) {
		return CandidateListReport{}, fmt.Errorf("state must be one of proposed|parked|promoted|rejected|no_promote|needs_review, got %q", state)
	}
	if opts.Limit < 0 {
		return CandidateListReport{}, fmt.Errorf("limit must be non-negative")
	}

	snapshot, corruptions, err := s.candidateInboxSnapshot()
	if err != nil {
		return CandidateListReport{}, err
	}
	cands := make([]Candidate, 0, len(snapshot))
	rawKinds := make(map[string]string, len(snapshot))
	modifiedByCandidate := make(map[string]string, len(snapshot))
	for _, entry := range snapshot {
		candidate, _ := AdmitCandidate(entry.Candidate)
		cands = append(cands, candidate)
		if entry.RawKind != "" {
			rawKinds[candidate.ID] = entry.RawKind
		}
		if !entry.Modified.IsZero() {
			modifiedByCandidate[candidate.ID] = entry.Modified.UTC().Format(time.RFC3339Nano)
		}
	}
	reviewByCandidate, warnings := s.reviewStateByCandidate()
	for _, corruption := range corruptions {
		warnings = append(warnings, corruption.Error())
	}
	report := CandidateListReport{
		SchemaVersion: CandidateListSchemaVersion,
		AuthorityRef:  "compound://memory/candidates",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Counts: CandidateListCounts{
			Total:   len(cands),
			Corrupt: len(corruptions),
			ByState: map[string]int{},
		},
		Items:    []CandidateListItem{},
		Warnings: warnings,
	}
	if state != "" || kind != "" || opts.Limit > 0 || !opts.CreatedAfter.IsZero() || opts.IncludeEvidence {
		report.Filters = map[string]interface{}{}
		if state != "" {
			report.Filters["state"] = state
		}
		if kind != "" {
			report.Filters["kind"] = kind
		}
		if opts.Limit > 0 {
			report.Filters["limit"] = opts.Limit
		}
		if !opts.CreatedAfter.IsZero() {
			report.Filters["created_after"] = opts.CreatedAfter.UTC().Format(time.RFC3339)
		}
		if opts.IncludeEvidence {
			report.Filters["include_evidence"] = true
		}
	}

	for _, c := range cands {
		report.Counts.ByState[c.State]++
	}

	// Precompute recurrence + trend signals once over the FULL candidate set
	// so per-item reads are O(1). The signals are derived at projection time
	// and never written back to the inbox JSON.
	signals := candidateReadinessSignals(cands)

	matched := 0
	for _, c := range cands {
		reviewState := reviewByCandidate[c.ID]
		rawKind := rawKinds[c.ID]
		if state != "" {
			if state == ClaimStateNeedsReview {
				if reviewState != ClaimStateNeedsReview {
					continue
				}
			} else if c.State != state {
				continue
			}
		}
		if kind != "" && rawKind != kind && c.ClaimType != kind {
			continue
		}
		if !opts.CreatedAfter.IsZero() {
			created, err := time.Parse(time.RFC3339, strings.TrimSpace(c.CreatedAt))
			if err != nil || created.Before(opts.CreatedAfter) {
				continue
			}
		}
		// Count the match BEFORE the limit gate. Stopping the scan at Limit made
		// Counts.Matched describe the page instead of the set, so a bounded listing
		// over a large inbox reported a confident `matched: N` that a caller filtering
		// the items downstream read as proof of absence — the rows were on disk, just
		// past the limit in iteration order. Counting is cheap; only item projection
		// (which may read evidence) is gated.
		matched++
		if opts.Limit > 0 && len(report.Items) >= opts.Limit {
			continue
		}
		item, err := s.candidateListItem(c, reviewState, rawKind, modifiedByCandidate[c.ID], opts.IncludeEvidence)
		if err != nil {
			return report, err
		}
		if sig, ok := signals[c.ID]; ok {
			item.RecurrenceCount = sig.RecurrenceCount
			item.ConfidenceTrend = sig.ConfidenceTrend
		}
		report.Items = append(report.Items, item)
	}
	report.Counts.Matched = matched
	report.Counts.Returned = len(report.Items)
	report.Counts.Truncated = matched > len(report.Items)
	return report, nil
}

func validCandidateListState(state string) bool {
	switch state {
	case "proposed", "parked", "promoted", "rejected", "no_promote", ClaimStateNeedsReview:
		return true
	default:
		return false
	}
}

func (s *Store) candidateListItem(c Candidate, reviewState, rawKind, modifiedAt string, includeEvidence bool) (CandidateListItem, error) {
	item := CandidateListItem{
		ID:              c.ID,
		State:           c.State,
		ReviewState:     reviewState,
		Entity:          c.Entity,
		Predicate:       c.Predicate,
		Object:          c.Object,
		ObservationDate: c.ObservationDate,
		Confidence:      c.Confidence,
		Derivation:      c.Derivation,
		ClaimType:       c.ClaimType,
		Kind:            rawKind,
		Visibility:      c.Visibility,
		PromotionBasis:  c.PromotionBasis,
		Qualifiers:      c.Qualifiers,
		Reason:          c.Reason,
		AdjudicatedBy:   c.AdjudicatedBy,
		AdjudicatedAt:   c.AdjudicatedAt,
		CreatedAt:       c.CreatedAt,
		ModifiedAt:      modifiedAt,
	}
	if includeEvidence {
		item.SourceTraceRef = c.SourceTraceRef
		item.SnippetHash = c.SnippetHash
		item.EvidenceRefs = []string{c.ID}
		if c.SourceTraceRef != "" {
			item.EvidenceRefs = append(item.EvidenceRefs, c.SourceTraceRef)
			if ev, ok, err := s.GetEvidence(c.SourceTraceRef); err != nil {
				return item, err
			} else if ok {
				item.Evidence = &CandidateListEvidence{
					Ref:        ev.ID,
					SourceKind: ev.SourceKind,
					SourceID:   ev.SourceID,
					SessionID:  ev.SessionID,
					Quote:      ev.QuoteText,
					EventTime:  ev.EventTime,
					ObservedAt: ev.ObservedAt,
					Actor:      ev.Actor,
					Visibility: ev.Visibility,
				}
			}
		}
		item.Annotations = append([]CandidateAnnotation(nil), c.Annotations...)
		if c.State != ClaimStatePromoted {
			if prior, ok, err := s.currentClusterClaim(c, 0); err != nil {
				return item, err
			} else if ok {
				item.BelievedBefore = &CandidateListBelief{
					ClaimID:      prior.ID,
					Entity:       prior.Entity,
					Predicate:    prior.Predicate,
					Object:       prior.Object,
					ValidFrom:    prior.ValidFrom,
					CreatedAt:    prior.CreatedAt,
					EvidenceRefs: append([]string(nil), prior.EvidenceRefs...),
				}
			}
		}
	}
	return item, nil
}

func (s *Store) reviewStateByCandidate() (map[string]string, []string) {
	out := map[string]string{}
	claims, err := s.loadClaimsAsOf(0)
	if err != nil {
		return out, []string{"claim review state unavailable: " + err.Error()}
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].Seq < claims[j].Seq
	})
	for _, claim := range claims {
		if claim.State != ClaimStateNeedsReview {
			continue
		}
		for _, ref := range claim.EvidenceRefs {
			if strings.HasPrefix(ref, "cand_") {
				out[ref] = ClaimStateNeedsReview
			}
		}
	}
	candidateMarks, err := s.candidateReviewMarksAsOf(0)
	if err != nil {
		return out, []string{"candidate review state unavailable: " + err.Error()}
	}
	for candidateID, mark := range candidateMarks {
		if len(mark.Refs) == 0 {
			continue
		}
		out[candidateID] = ClaimStateNeedsReview
	}
	return out, nil
}

// candidateReadinessSignal is the per-candidate projection of two read-only
// derived fields (see CandidateListItem.RecurrenceCount / ConfidenceTrend).
type candidateReadinessSignal struct {
	RecurrenceCount int
	ConfidenceTrend string
}

// trendEpsilon is the half-width of the "equal" band for trend classification
// (a difference strictly below this is not a momentum signal).
const trendEpsilon = 1e-3

// normalizeClaimKey builds the projection-time join key for the two derived
// signals: lowercased + whitespace-trimmed entity|predicate|object. Empty
// fields collapse to "" so an entity-only row still joins with its peers
// (rather than silently dropping from the cohort).
func normalizeClaimKey(c Candidate) string {
	return strings.ToLower(strings.TrimSpace(c.Entity)) +
		"|" + strings.ToLower(strings.TrimSpace(c.Predicate)) +
		"|" + strings.ToLower(strings.TrimSpace(c.Object))
}

// candidateReadinessSignals computes the derived (recurrence_count,
// confidence_trend) pair for every candidate in the input set, keyed by the
// normalized claim key. The trend compares the candidate's confidence with
// the maximum confidence of any PRIOR candidate (earlier ObservationDate) for
// the same key. The function is pure: it does not read or write disk, and it
// never mutates its input.
func candidateReadinessSignals(cands []Candidate) map[string]candidateReadinessSignal {
	if len(cands) == 0 {
		return map[string]candidateReadinessSignal{}
	}

	// Bucket by normalized claim key; sort each bucket in temporal order so
	// the trend sees PRIOR observations (ObservationDate ascending), not
	// random ID order. Ties (same ObservationDate + same CreatedAt) break
	// on ID for determinism.
	byKey := map[string][]int{}
	for i, c := range cands {
		byKey[normalizeClaimKey(c)] = append(byKey[normalizeClaimKey(c)], i)
	}

	out := make(map[string]candidateReadinessSignal, len(cands))
	for _, indices := range byKey {
		sort.SliceStable(indices, func(a, b int) bool {
			ca, cb := cands[indices[a]], cands[indices[b]]
			if ca.ObservationDate != cb.ObservationDate {
				return ca.ObservationDate < cb.ObservationDate
			}
			if ca.CreatedAt != cb.CreatedAt {
				return ca.CreatedAt < cb.CreatedAt
			}
			return ca.ID < cb.ID
		})

		// Distinct evidence refs across the cohort: each candidate contributes
		// its SourceTraceRef (when present) so two candidates citing the
		// same trace share one ref, but independent traces count twice.
		refs := map[string]struct{}{}
		for _, idx := range indices {
			c := cands[idx]
			if ref := strings.TrimSpace(c.SourceTraceRef); ref != "" {
				refs[ref] = struct{}{}
			}
		}
		recurrence := len(refs)
		// A cohort that cites NO trace still has a "recurrence" of 1 — it
		// has been observed once. (We deliberately do NOT count refs=0
		// cohorts as zero, which would let a stray empty trace suppress the
		// chip for a single-observation candidate.)
		if recurrence == 0 {
			recurrence = 1
		}

		// Two passes: first to compute each candidate's prior max, then to
		// assign. Walking in temporal order means "prior" is the max of all
		// earlier observations' confidences.
		priorMax := make([]float64, len(indices))
		priorHave := make([]bool, len(indices))
		var maxPrior float64
		havePrior := false
		for j, idx := range indices {
			if j == 0 {
				priorMax[j] = 0
				priorHave[j] = false
			} else {
				priorMax[j] = maxPrior
				priorHave[j] = havePrior
			}
			if !havePrior || cands[idx].Confidence > maxPrior {
				maxPrior = cands[idx].Confidence
				havePrior = true
			}
		}

		for j, idx := range indices {
			c := cands[idx]
			trend := "new"
			if priorHave[j] {
				switch {
				case c.Confidence > priorMax[j]+trendEpsilon:
					trend = "rising"
				case c.Confidence < priorMax[j]-trendEpsilon:
					trend = "falling"
				default:
					trend = "flat"
				}
			}
			out[c.ID] = candidateReadinessSignal{
				RecurrenceCount: recurrence,
				ConfidenceTrend: trend,
			}
		}
	}
	return out
}
