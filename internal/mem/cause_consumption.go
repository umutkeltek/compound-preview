// cause_consumption.go — work_c13acdc090b5 (S3a): the observed-layer consumption counter.
//
// Authority: docs_private/plan/packets/2026-07-17-s3ab-consumption-counter-stranger-card.md.
// The packet's thesis: make it measurable whether anything in compound ever moves BECAUSE OF
// un-promoted memory. Transition.Cause.CausedByRefs (internal/envelope/envelope.go) is the only
// causal cross-ref compound carries today, and nothing joins it against the memory adjudication
// tier — this file is that join, read-only. It sweeps every Transition the eventstore holds,
// buckets each Cause.CausedByRefs entry into one of five tiers (D1 below), and reports the
// motion-from-unpromoted ratio: how much of what actually moved compound was cited via memory
// that had NOT been promoted to a Claim.
//
// Mint-side honesty (D6): only `compound source canon-observe` mints a Cause today
// (internal/capgraph/canon_evolution.go — read for pattern, never edited by this slice). This
// report counts over whatever exists; it never mints, backfills, or infers a Cause.
//
// Write boundary: this file NEVER writes to the ledger, candidates/, or claims/. The only
// artifact it can produce is a plain timestamped JSON dump under
// $COMPOUND_HOME/state/runs/cause-consumption/ (WriteCauseConsumptionRun), and only when the
// CLI caller passes --record (internal/mem/cause_consumption_cli.go). Mirrors the read-only sweep
// shape of internal/capgraph/injection_observation.go (comment at :10-13 there) without
// depending on capgraph — internal/mem already imports eventstore in non-test code (mem.go,
// reconcile.go) and must not gain an upward edge to capgraph.
package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
)

const (
	// CauseConsumptionSchemaVersion versions the report body.
	CauseConsumptionSchemaVersion = "compound.memory_cause_consumption.v0.1"

	// The five D1 tier buckets. Every ref in a Cause.CausedByRefs list resolves to exactly one.
	CauseConsumptionTierPromoted        = "promoted"         // resolves to a live Claim, or Candidate.State==promoted
	CauseConsumptionTierPreAdjudication = "pre_adjudication" // Candidate proposed|parked
	CauseConsumptionTierClosed          = "closed"           // Candidate rejected|no_promote
	CauseConsumptionTierObservedRaw     = "observed_raw"     // resolves to nothing in mem (raw, never-adjudicated)
	CauseConsumptionTierNonMemory       = "non_memory"       // known non-memory ref shape (commit:/work_/source:/decision:)
)

// causeConsumptionTiers is the fixed, always-zero-filled enumeration order for RefTierCounts so
// the report shape never depends on which tiers happened to occur.
var causeConsumptionTiers = []string{
	CauseConsumptionTierPromoted,
	CauseConsumptionTierPreAdjudication,
	CauseConsumptionTierClosed,
	CauseConsumptionTierObservedRaw,
	CauseConsumptionTierNonMemory,
}

// causeConsumptionNonMemoryPrefixes are ref shapes that are NOT memory refs at all — a commit
// hash, a work id, a source-manifest ref, or a decisions-authority ref. D1 is explicit that
// bucketing these as observed_raw is WRONG (they never had a chance to be memory-shaped), so
// they get their own tier and are excluded from the thesis ratio's denominator.
var causeConsumptionNonMemoryPrefixes = []string{"commit:", "work_", "source:", "decision:"}

// loadCauseConsumptionClaims is the report's test seam for proving claim-log I/O shape.
// Production uses the owned raw claim loader; tests may wrap it to count loads.
var loadCauseConsumptionClaims = func(s *Store) ([]Claim, error) {
	return s.loadClaimsRaw()
}

// loadCauseConsumptionCandidates is the report's test seam for proving candidate-file I/O
// shape. Production uses the owned direct-by-id loader; tests may wrap it to count loads.
var loadCauseConsumptionCandidates = func(s *Store, ids []string) (map[string]Candidate, error) {
	out := make(map[string]Candidate, len(ids))
	for _, id := range ids {
		var candidate Candidate
		err := candidates.ReadCandidate(s.home, candidates.Memory, id, &candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read cause candidate %q: %w", id, err)
		}
		if candidate.ID == "" {
			candidate.ID = id
		}
		if _, err := tierForCandidateState(candidate.State); err != nil {
			return nil, fmt.Errorf("read cause candidate %q: %w", id, err)
		}
		out[id] = candidate
	}
	return out, nil
}

type causeConsumptionRefResolver struct {
	claimsByID     map[string]bool
	candidatesByID map[string]Candidate
}

// newCauseConsumptionRefResolver loads and indexes every persisted claim version plus every
// unique candidate-shaped ref once for one resolution pass. A claim:// ref may legitimately cite
// a superseded version, so the current fold alone is insufficient. Candidate misses are part of
// the batch snapshot: tierOfRef never retries a missing id per occurrence.
func newCauseConsumptionRefResolver(s *Store, refs []string) (*causeConsumptionRefResolver, error) {
	claims, err := loadCauseConsumptionClaims(s)
	if err != nil {
		return nil, err
	}
	claimsByID := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if id := strings.TrimSpace(claim.ID); id != "" {
			claimsByID[id] = true
		}
	}

	candidateIDs := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !causeConsumptionCandidateLookupRef(ref) || seen[ref] {
			continue
		}
		seen[ref] = true
		candidateIDs = append(candidateIDs, ref)
	}
	sort.Strings(candidateIDs)
	candidatesByID := map[string]Candidate{}
	if len(candidateIDs) > 0 {
		candidatesByID, err = loadCauseConsumptionCandidates(s, candidateIDs)
		if err != nil {
			return nil, err
		}
	}
	return &causeConsumptionRefResolver{claimsByID: claimsByID, candidatesByID: candidatesByID}, nil
}

func causeConsumptionCandidateLookupRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, claimRefPrefix) {
		return false
	}
	for _, prefix := range causeConsumptionNonMemoryPrefixes {
		if strings.HasPrefix(ref, prefix) {
			return false
		}
	}
	if ref == "." || ref == ".." || ref != filepath.Base(ref) || strings.ContainsAny(ref, `/\`) {
		return false
	}
	return true
}

// TierOfRef buckets one Transition.Cause.CausedByRefs entry into a D1 tier (F1). Ref forms:
//   - a known non-memory prefix (commit:/work_/source:/decision:) => non_memory, checked first
//     so these never fall through into observed_raw.
//   - "claim://<id>" => strip the scheme, look up the Claim by id => promoted if found, else
//     observed_raw (a claim-shaped ref that never resolved is still "resolves to nothing").
//   - anything else (bare "cand_*" ids and any other bare id) => try a Candidate lookup by the
//     exact ref first (covers the common cand_* shape), then a bare Claim-id lookup (a ref that
//     names a Claim.ID directly, no claim:// scheme) => observed_raw if neither resolves.
func (s *Store) TierOfRef(ref string) (string, error) {
	resolver, err := newCauseConsumptionRefResolver(s, []string{ref})
	if err != nil {
		return "", err
	}
	return resolver.tierOfRef(ref)
}

func (r *causeConsumptionRefResolver) tierOfRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return CauseConsumptionTierNonMemory, nil
	}
	for _, prefix := range causeConsumptionNonMemoryPrefixes {
		if strings.HasPrefix(ref, prefix) {
			return CauseConsumptionTierNonMemory, nil
		}
	}
	if strings.HasPrefix(ref, claimRefPrefix) {
		id := strings.TrimSpace(strings.TrimPrefix(ref, claimRefPrefix))
		if id == "" {
			return CauseConsumptionTierObservedRaw, nil
		}
		if r.claimsByID[id] {
			return CauseConsumptionTierPromoted, nil
		}
		return CauseConsumptionTierObservedRaw, nil
	}
	if c, ok := r.candidatesByID[ref]; ok {
		return tierForCandidateState(c.State)
	}
	if r.claimsByID[ref] {
		return CauseConsumptionTierPromoted, nil
	}
	return CauseConsumptionTierObservedRaw, nil
}

func tierForCandidateState(state string) (string, error) {
	switch state {
	case "promoted":
		return CauseConsumptionTierPromoted, nil
	case "rejected", "no_promote":
		return CauseConsumptionTierClosed, nil
	case "proposed", "parked":
		return CauseConsumptionTierPreAdjudication, nil
	default:
		return "", fmt.Errorf("candidate state %q is not a consumption tier", state)
	}
}

// CauseConsumptionTransitionRow is one Cause-carrying Transition the sweep found.
type CauseConsumptionTransitionRow struct {
	TransitionID string            `json:"transition_id"`
	WorkID       string            `json:"work_id,omitempty"`
	DeltaCause   string            `json:"delta_cause"`
	NextConsumer string            `json:"next_consumer,omitempty"`
	CreatedAt    string            `json:"created_at,omitempty"`
	CausedByRefs []string          `json:"caused_by_refs"`
	RefTiers     map[string]string `json:"ref_tiers"`
}

// CauseConsumptionDealtRow is one S3b dealt stranger card joined against the Cause sweep (D2,
// F8) — S3b's kill-condition meter: did a dealt weak-tie candidate ever go on to cause anything.
type CauseConsumptionDealtRow struct {
	CardID               string `json:"card_id"`
	CandidateRef         string `json:"candidate_ref"`
	Topic                string `json:"topic,omitempty"`
	DealtAt              string `json:"dealt_at"`
	CausalStatus         string `json:"causal_status"` // caused | same_second_ambiguous | not_caused
	Caused               bool   `json:"caused"`
	CausedByTransitionID string `json:"caused_by_transition_id,omitempty"`
}

// CauseConsumptionReport is the full derived, read-only report (D2).
type CauseConsumptionReport struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`

	TransitionsScanned   int `json:"transitions_scanned"`
	TransitionsWithCause int `json:"transitions_with_cause"`

	// RefTierCounts is always all five D1 tiers, zero-filled.
	RefTierCounts map[string]int `json:"ref_tier_counts"`

	// MemoryShapedRefs is the thesis denominator: promoted+pre_adjudication+observed_raw.
	// Closed refs remain visible in RefTierCounts but are excluded because rejected/no_promote
	// evidence is no longer eligible to move the system.
	// UnpromotedRefs = pre_adjudication+observed_raw. ThesisRatio = UnpromotedRefs/MemoryShapedRefs
	// (0 when MemoryShapedRefs is 0 — never divides by zero; ThesisNote says so explicitly).
	MemoryShapedRefs int     `json:"memory_shaped_refs"`
	UnpromotedRefs   int     `json:"unpromoted_refs"`
	ThesisRatio      float64 `json:"motion_from_unpromoted_ratio"`
	ThesisNote       string  `json:"thesis_note"`

	Transitions []CauseConsumptionTransitionRow `json:"transitions"`

	// DealtCards is the S3b join (D2/F8): every row ever appended to the stranger dealt-log,
	// each marked whether a causally eligible (not earlier at persisted timestamp precision)
	// Cause-transition cited its candidate_ref.
	DealtCards       []CauseConsumptionDealtRow `json:"dealt_cards"`
	DealtCount       int                        `json:"dealt_count"`
	DealtCausedCount int                        `json:"dealt_caused_count"`
	// DealtAmbiguousCount keeps equal-second matches visible without asserting an ordering the
	// persisted RFC3339 timestamps cannot prove.
	DealtAmbiguousCount int    `json:"dealt_ambiguous_count"`
	DealtCausedNote     string `json:"dealt_caused_note"`

	// CoverageNote is D6's honesty requirement: mint coverage is v1-limited.
	CoverageNote string `json:"coverage_note"`
}

const causeConsumptionCoverageNote = "mint coverage is v1-limited: only `compound source canon-observe` mints Transition.Cause today (D6); this report counts only what exists, it never infers or backfills a cause."

// BuildCauseConsumptionReport sweeps homePath's eventstore for Cause-carrying Transitions,
// buckets every caused_by_refs entry by adjudication tier (D1/F1), and joins the S3b dealt-log
// for the dealt->caused meter (D2/F8). Read-only: it opens the memory store only to resolve
// refs and never writes to the ledger, candidates/, or claims/ (F3).
func BuildCauseConsumptionReport(homePath string, now time.Time) (CauseConsumptionReport, error) {
	rep := CauseConsumptionReport{
		SchemaVersion: CauseConsumptionSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		RefTierCounts: map[string]int{},
		Transitions:   []CauseConsumptionTransitionRow{},
		DealtCards:    []CauseConsumptionDealtRow{},
		CoverageNote:  causeConsumptionCoverageNote,
	}
	for _, tier := range causeConsumptionTiers {
		rep.RefTierCounts[tier] = 0
	}

	store, err := OpenReadOnly(homePath)
	if err != nil {
		return rep, err
	}

	entries, err := store.eventStore.Scan(eventstore.ScanOptions{})
	if err != nil {
		if os.IsNotExist(err) {
			rep.ThesisNote = causeConsumptionThesisNote(0, 0)
			rep.DealtCausedNote = causeConsumptionDealtNote(0, 0, 0)
			return rep, nil
		}
		return rep, err
	}
	causeTransitions := make([]envelope.Transition, 0)
	var causedByRefs []string
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return rep, err
		}
		var head struct {
			SchemaVersion string `json:"schema_version"`
		}
		if json.Unmarshal(record.Bytes, &head) != nil {
			continue // torn/foreign file — skip, do not brick the sweep
		}
		if head.SchemaVersion != envelope.SVTransition {
			continue
		}
		var tr envelope.Transition
		if json.Unmarshal(record.Bytes, &tr) != nil {
			continue
		}
		if tr.Validate() != nil {
			continue
		}
		rep.TransitionsScanned++
		if tr.Cause == nil {
			continue
		}
		rep.TransitionsWithCause++
		causeTransitions = append(causeTransitions, tr)
		causedByRefs = append(causedByRefs, tr.Cause.CausedByRefs...)
	}

	resolver, err := newCauseConsumptionRefResolver(store, causedByRefs)
	if err != nil {
		return rep, err
	}

	for _, tr := range causeTransitions {
		row := CauseConsumptionTransitionRow{
			TransitionID: tr.ID,
			WorkID:       tr.IDs.WorkID,
			DeltaCause:   tr.Cause.DeltaCause,
			NextConsumer: tr.Cause.NextConsumer,
			CreatedAt:    strings.TrimSpace(tr.CreatedAt),
			CausedByRefs: append([]string{}, tr.Cause.CausedByRefs...),
			RefTiers:     map[string]string{},
		}
		for _, ref := range tr.Cause.CausedByRefs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			tier, err := resolver.tierOfRef(ref)
			if err != nil {
				return rep, err
			}
			row.RefTiers[ref] = tier
			rep.RefTierCounts[tier]++
		}
		rep.Transitions = append(rep.Transitions, row)
	}

	sort.Slice(rep.Transitions, func(i, j int) bool {
		if rep.Transitions[i].CreatedAt != rep.Transitions[j].CreatedAt {
			return rep.Transitions[i].CreatedAt < rep.Transitions[j].CreatedAt
		}
		return rep.Transitions[i].TransitionID < rep.Transitions[j].TransitionID
	})

	rep.MemoryShapedRefs = rep.RefTierCounts[CauseConsumptionTierPromoted] + rep.RefTierCounts[CauseConsumptionTierPreAdjudication] + rep.RefTierCounts[CauseConsumptionTierObservedRaw]
	rep.UnpromotedRefs = rep.RefTierCounts[CauseConsumptionTierPreAdjudication] + rep.RefTierCounts[CauseConsumptionTierObservedRaw]
	if rep.MemoryShapedRefs > 0 {
		rep.ThesisRatio = float64(rep.UnpromotedRefs) / float64(rep.MemoryShapedRefs)
	}
	rep.ThesisNote = causeConsumptionThesisNote(rep.MemoryShapedRefs, rep.UnpromotedRefs)

	dealt, err := ReadStrangerDealtLog(homePath)
	if err != nil {
		// A corrupt dealt-log must not brick the whole consumption report — the Cause-sweep
		// above is still valid and reportable; only the S3b join is unavailable this run.
		rep.DealtCausedNote = "stranger dealt-log unreadable; dealt->caused join skipped this run: " + err.Error()
		return rep, nil
	}
	rows, causedCount, ambiguousCount := joinDealtCauses(dealt, rep.Transitions)
	rep.DealtCards = rows
	rep.DealtCount = len(rows)
	rep.DealtCausedCount = causedCount
	rep.DealtAmbiguousCount = ambiguousCount
	rep.DealtCausedNote = causeConsumptionDealtNote(rep.DealtCount, rep.DealtCausedCount, rep.DealtAmbiguousCount)

	return rep, nil
}

func joinDealtCauses(dealt []StrangerDealtRow, transitions []CauseConsumptionTransitionRow) ([]CauseConsumptionDealtRow, int, int) {
	out := make([]CauseConsumptionDealtRow, 0, len(dealt))
	caused := 0
	ambiguous := 0
	for _, d := range dealt {
		row := CauseConsumptionDealtRow{
			CardID:       d.CardID,
			CandidateRef: d.CandidateRef,
			Topic:        d.Topic,
			DealtAt:      d.Ts,
			CausalStatus: "not_caused",
		}
		dealtAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(d.Ts))
		if perr == nil {
			for _, tr := range transitions {
				trAt, terr := time.Parse(time.RFC3339, strings.TrimSpace(tr.CreatedAt))
				if terr != nil || trAt.Before(dealtAt) {
					continue
				}
				matched := false
				for _, ref := range tr.CausedByRefs {
					if strings.TrimSpace(ref) == d.CandidateRef {
						matched = true
						break
					}
				}
				if matched {
					if trAt.Equal(dealtAt) {
						row.CausalStatus = "same_second_ambiguous"
						continue
					}
					row.Caused = true
					row.CausalStatus = "caused"
					row.CausedByTransitionID = tr.TransitionID
					break
				}
			}
		}
		if row.Caused {
			caused++
		} else if row.CausalStatus == "same_second_ambiguous" {
			ambiguous++
		}
		out = append(out, row)
	}
	return out, caused, ambiguous
}

func causeConsumptionThesisNote(memoryShaped, unpromoted int) string {
	if memoryShaped == 0 {
		return "no ratio-eligible memory-shaped caused_by_refs observed yet; motion_from_unpromoted_ratio is not yet meaningful"
	}
	return fmt.Sprintf("%d/%d ratio-eligible memory-shaped caused_by_refs currently resolve as un-promoted (pre_adjudication+observed_raw); closed refs remain reported but are excluded", unpromoted, memoryShaped)
}

func causeConsumptionDealtNote(dealtCount, causedCount, ambiguousCount int) string {
	if dealtCount == 0 {
		return "no stranger cards dealt yet; dealt->caused join not yet evaluable"
	}
	return fmt.Sprintf("%d/%d dealt stranger cards were cited by a strictly later Cause-transition; %d same-second match(es) remain causally ambiguous", causedCount, dealtCount, ambiguousCount)
}

// WriteCauseConsumptionRun persists the report as a plain timestamped artifact under
// homePath/state/runs/cause-consumption/ — the runtime home, never the repo, and never
// projections/ (D2: this is an observation artifact, not derived ledger state). It never
// mutates the ledger, candidates, claims, or any projection (F3).
func WriteCauseConsumptionRun(homePath string, report CauseConsumptionReport, now time.Time) (string, error) {
	dir := filepath.Join(homePath, "state", "runs", "cause-consumption")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, stamp+".json")
	b, err := json.MarshalIndent(&report, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
