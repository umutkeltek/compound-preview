package lanebeliefs

import (
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
)

// B3 — the cross-lane promotion gate (valence-split). Parked lane-belief candidates (B2 output)
// promote ONLY through this gate, and ONLY via the real mem adjudication API carrying basis
// runtime_proof. The gate is deterministic, idempotent (a promoted candidate leaves the parked set,
// so a re-run promotes nothing new), and dry-run writes NOTHING.
//
// The heart is the VALENCE split (binding taxonomy, VALENCE paragraph):
//   - standing-RAISING beliefs (all capability-beliefs + the raising tendencies) promote ONLY with
//     a cross-lane proof-bearing mark — a lane can NEVER install a flattering belief about itself.
//   - standing-LOWERING tendencies promote on the subject lane's OWN deterministic receipt
//     (self-incrimination valid); a third party can NEVER grief a lane (B2 attributes every
//     observation to its own producer, so a lowering belief is structurally self-sourced).
//
// Out of scope (binding taxonomy): B4 cooling/retirement; routing influence (the valve stays OFF —
// promoted beliefs surface in peers cards only, MR-4); any capgraph write; any storage beyond the
// report. `<0.50` simply does not promote (no cool action here).
//
// Authority: docs_private/plan/packets/b3-cross-lane-promotion-gate.md.

const (
	promoteReportSchemaVersion = "compound.lanebeliefs.promote.v0.1"

	// PARK_BAND_FLOOR is the lower edge of the park band: [0.50, 0.66) stays parked (park_band),
	// below 0.50 stays parked (below_band). PROMOTE_POSTERIOR_FLOOR (0.66) is the promote edge.
	PARK_BAND_FLOOR = 0.50

	ValenceStandingRaising  = "standing-raising"
	ValenceStandingLowering = "standing-lowering"

	DispositionPromoted = "promoted"
	DispositionParked   = "parked"

	ReasonUnderCorroborated  = "under_corroborated"
	ReasonParkBand           = "park_band"
	ReasonBelowBand          = "below_band"
	ReasonNoCrossLaneMark    = "no_cross_lane_mark"
	ReasonUnresolvedEvidence = "unresolved_evidence"
)

// standingLoweringTendencies is the closed set of tendency domains whose promotion is admissible on
// the subject lane's OWN receipt (self-incrimination). Everything else a tendency/capability belief
// can carry is standing-raising and requires a cross-lane mark — the conservative default (an
// unknown or flattering tendency must never self-promote). Verbatim from the taxonomy VALENCE table.
var standingLoweringTendencies = map[string]bool{
	"scope-creep":      true,
	"ceremony-skipper": true,
}

// standingRaisingTendencies is listed for legibility/symmetry with the taxonomy table; it is not the
// discriminator (anything not standing-lowering is standing-raising), but naming it keeps the closed
// vocabulary auditable next to its lowering counterpart.
var standingRaisingTendencies = map[string]bool{
	"falsifier-first":  true,
	"receipt-faithful": true,
	"adjacent-fixer":   true,
}

// PromoteOptions controls a promotion pass. DryRun evaluates the gate and reports what WOULD promote
// while writing nothing (F-dry-run). Now injects the clock.
type PromoteOptions struct {
	DryRun bool
	Now    time.Time
}

// PromoteReport mirrors the fold report: schema_versioned, counters, and a per-candidate result
// carrying the counted refusal reason — or, for a promotion, the promoted claim's basis/visibility
// readback.
type PromoteReport struct {
	SchemaVersion  string          `json:"schema_version"`
	GeneratedAt    string          `json:"generated_at"`
	DryRun         bool            `json:"dry_run"`
	EvaluatedCount int             `json:"evaluated_count"`
	PromotedCount  int             `json:"promoted_count"`
	RefusedCount   int             `json:"refused_count"`
	ReasonCounts   map[string]int  `json:"reason_counts,omitempty"`
	Results        []PromoteResult `json:"results,omitempty"`
}

// PromoteResult is one parked lane-belief candidate's disposition.
type PromoteResult struct {
	CandidateID      string  `json:"candidate_id"`
	Subject          string  `json:"subject_lane_node_id"`
	Domain           string  `json:"domain"`
	Kind             string  `json:"kind"`
	Valence          string  `json:"valence"`
	WorkCount        int     `json:"work_count"`
	Hits             int     `json:"hits"`
	Opportunities    int     `json:"opportunities"`
	PosteriorMean    float64 `json:"posterior_mean"`
	HasCrossLaneMark bool    `json:"has_cross_lane_mark"`
	Disposition      string  `json:"disposition"`
	Reason           string  `json:"reason,omitempty"`
	ClaimID          string  `json:"claim_id,omitempty"`
	PromotionBasis   string  `json:"promotion_basis,omitempty"`
	Visibility       string  `json:"visibility,omitempty"`
}

// promoteAggregate is the belief's rate-based evidence, summed over its observations (rule 6: one
// Beta trial per work). WorkSet is distinct works (corroboration); Hits/Opportunities feed the
// posterior; EvidenceEventIDs are the subject's OWN receipt refs (self-proof for a lowering belief).
type promoteAggregate struct {
	workSet          map[string]bool
	hits             int
	opportunities    int
	evidenceEventIDs []string
}

func (a promoteAggregate) workIDs() []string {
	out := make([]string, 0, len(a.workSet))
	for w := range a.workSet {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// posteriorMean is E[Beta(a0+hits, b0+misses)] with a0=b0=2 and misses = opportunities − hits.
func posteriorMean(hits, opportunities int) float64 {
	return float64(BETA_PRIOR_ALPHA+hits) / float64(BETA_PRIOR_ALPHA+BETA_PRIOR_BETA+opportunities)
}

// beliefValence classifies a candidate by its shape + domain. Capability beliefs and every tendency
// not in the closed standing-lowering set are standing-raising; only the two standing-lowering
// tendencies admit self-incriminating promotion.
func beliefValence(predicate, domain string) string {
	if predicate == mem.BeliefPredicateTendsTo && standingLoweringTendencies[strings.TrimSpace(domain)] {
		return ValenceStandingLowering
	}
	return ValenceStandingRaising
}

func candidateKind(predicate string) string {
	if predicate == mem.BeliefPredicateCapableOf {
		return beliefKindCapability
	}
	return beliefKindTendency
}

// Promote evaluates every parked lane-belief candidate against the gate and promotes the ones that
// pass — through the real mem adjudication API, carrying basis runtime_proof. Refusals are counted
// by reason. It is the sole promotion path for lane beliefs.
func Promote(homePath string, opts PromoteOptions) (PromoteReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report := PromoteReport{
		SchemaVersion: promoteReportSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		DryRun:        opts.DryRun,
		ReasonCounts:  map[string]int{},
	}

	resolver, err := openLaneResolver(homePath)
	if err != nil {
		return report, err
	}
	observations, err := loadObservations(homePath)
	if err != nil {
		return report, err
	}
	aggregates := buildPromoteAggregates(observations)

	store, err := openStoreForScan(homePath, opts.DryRun)
	if err != nil {
		return report, err
	}
	all, err := store.AllCandidates()
	if err != nil {
		return report, err
	}
	candidates := make([]mem.Candidate, 0, len(all))
	for _, c := range all {
		if isLaneBeliefCandidate(c) && c.State == "parked" {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	for _, cand := range candidates {
		res, err := evaluatePromotion(store, resolver, homePath, cand, aggregates, opts.DryRun, now)
		if err != nil {
			return report, err
		}
		report.EvaluatedCount++
		if res.Disposition == DispositionPromoted {
			report.PromotedCount++
		} else {
			report.RefusedCount++
			report.ReasonCounts[res.Reason]++
		}
		report.Results = append(report.Results, res)
	}
	return report, nil
}

func evaluatePromotion(store *mem.Store, resolver mem.LaneResolver, homePath string, cand mem.Candidate, aggregates map[string]promoteAggregate, dryRun bool, now time.Time) (PromoteResult, error) {
	kind := candidateKind(cand.Predicate)
	domain := strings.TrimSpace(cand.Qualifiers[beliefDomainKey])
	subject := strings.TrimSpace(cand.Entity)
	valence := beliefValence(cand.Predicate, domain)

	res := PromoteResult{
		CandidateID: cand.ID,
		Subject:     subject,
		Domain:      domain,
		Kind:        kind,
		Valence:     valence,
		Disposition: DispositionParked,
	}

	agg, ok := aggregates[aggregateKey(subject, kind, domain)]
	if !ok || agg.opportunities == 0 {
		// No resolvable evidence backs this candidate (orphan candidate / evidence never folded).
		res.Reason = ReasonUnresolvedEvidence
		return res, nil
	}
	res.WorkCount = len(agg.workSet)
	res.Hits = agg.hits
	res.Opportunities = agg.opportunities
	res.PosteriorMean = posteriorMean(agg.hits, agg.opportunities)

	// a. Corroboration: ≥ MIN_CORROBORATION_WORKS DISTINCT works (F-corroboration).
	if res.WorkCount < MIN_CORROBORATION_WORKS {
		res.Reason = ReasonUnderCorroborated
		return res, nil
	}
	// b. Posterior band (F-floor): below 0.50 below_band; [0.50, 0.66) park_band; ≥ 0.66 proceeds.
	if res.PosteriorMean < PARK_BAND_FLOOR {
		res.Reason = ReasonBelowBand
		return res, nil
	}
	if res.PosteriorMean < PROMOTE_POSTERIOR_FLOOR {
		res.Reason = ReasonParkBand
		return res, nil
	}

	// c. Valence (F-a, F-h). A standing-raising belief needs a cross-lane proof-bearing mark; a
	// standing-lowering belief promotes on the subject lane's own deterministic receipts.
	var proofRefs []string
	if valence == ValenceStandingRaising {
		marks, err := crossLaneProofMarks(resolver, homePath, subject, agg.workIDs())
		if err != nil {
			return res, err
		}
		res.HasCrossLaneMark = len(marks) > 0
		if len(marks) == 0 {
			res.Reason = ReasonNoCrossLaneMark
			return res, nil
		}
		proofRefs = marks
	} else {
		// Self-incrimination: the subject lane's own receipt evidence IS the runtime proof.
		proofRefs = uniqueNonEmpty(agg.evidenceEventIDs)
	}

	// d. All pass → promote through the real adjudication API with basis runtime_proof.
	res.Disposition = DispositionPromoted
	if dryRun {
		// Dry-run writes NOTHING (F-dry-run): report the disposition, leave the ledger untouched.
		return res, nil
	}
	if _, err := store.RecordRuntimeProofCorroboration(cand.ID, proofRefs, now); err != nil {
		return res, err
	}
	promoteWorkID := ""
	if works := agg.workIDs(); len(works) > 0 {
		promoteWorkID = works[0]
	}
	adj, err := store.Adjudicate(cand.ID, "promote", promoteWorkID, now)
	if err != nil {
		return res, err
	}
	if adj.Claim != nil {
		res.ClaimID = adj.Claim.ID
		res.PromotionBasis = adj.Claim.PromotionBasis
		res.Visibility = adj.Claim.Visibility
	}
	return res, nil
}

// buildPromoteAggregates sums observation trials into per (subject, kind, domain) evidence, keyed by
// the SAME aggregateKey the fold uses.
func buildPromoteAggregates(observations []observationRecord) map[string]promoteAggregate {
	byKey := map[string]*promoteAggregate{}
	for _, obs := range observations {
		key := aggregateKey(obs.SubjectLaneNodeID, obs.Kind, obs.Domain)
		agg := byKey[key]
		if agg == nil {
			agg = &promoteAggregate{workSet: map[string]bool{}}
			byKey[key] = agg
		}
		agg.hits += obs.Hits
		agg.opportunities += obs.Opportunities
		if w := strings.TrimSpace(obs.WorkID); w != "" {
			agg.workSet[w] = true
		}
		if ref := strings.TrimSpace(obs.EvidenceEventID); ref != "" {
			agg.evidenceEventIDs = append(agg.evidenceEventIDs, ref)
		}
	}
	out := make(map[string]promoteAggregate, len(byKey))
	for key, agg := range byKey {
		out[key] = *agg
	}
	return out
}

// crossLaneProofMarks returns the receipt ids of cross-lane proof-bearing marks on the candidate's
// works: a receipt whose producer lane resolves (store-backed, B0) to a node OTHER than the subject,
// with outcome completed, non-empty proof refs, and a real non-audit_only move_type. An unresolvable
// producer, an audit_only move, a missing proof ref, or a non-completed outcome is NOT a mark
// (F-mark-quality). A referenced-but-absent receipt is skipped, never guessed.
func crossLaneProofMarks(resolver mem.LaneResolver, homePath, subjectNode string, workIDs []string) ([]string, error) {
	if len(workIDs) == 0 {
		return nil, nil
	}
	wanted := make(map[string]bool, len(workIDs))
	for _, w := range workIDs {
		wanted[w] = true
	}
	events, _, err := loadWorkEventsAfter(homePath, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var marks []string
	for _, ev := range events {
		if !wanted[strings.TrimSpace(ev.Event.WorkID)] {
			continue
		}
		for _, id := range ev.Event.Attached {
			if !strings.HasPrefix(id, "rec_") || seen[id] {
				continue
			}
			seen[id] = true
			rec, err := loadReceipt(homePath, id)
			if err != nil {
				// A work event may reference a receipt whose body is absent; that is not a mark.
				continue
			}
			if isCrossLaneProofMark(resolver, rec, subjectNode) {
				marks = append(marks, rec.ID)
			}
		}
	}
	sort.Strings(marks)
	return marks, nil
}

func isCrossLaneProofMark(resolver mem.LaneResolver, rec envelope.Receipt, subjectNode string) bool {
	resolution := resolveReceiptLane(resolver, rec)
	if !resolution.Resolved() || resolution.NodeID == subjectNode {
		return false // unresolvable producer, or the subject's own lane — never a cross-lane mark
	}
	if strings.TrimSpace(rec.Outcome) != "completed" {
		return false
	}
	if len(rec.ProofRefs) == 0 {
		return false
	}
	return isProofBearingMoveType(rec.MoveType)
}

// isProofBearingMoveType reports whether mt is a real move type that counts as a proof-bearing move —
// a member of the envelope move-type enum, minus audit_only (which never counts, per the transition
// doctrine: audit_only does not count as a move).
func isProofBearingMoveType(mt string) bool {
	mt = strings.TrimSpace(mt)
	if mt == "audit_only" || mt == "" {
		return false
	}
	for _, m := range envelope.MoveTypes {
		if m == mt {
			return true
		}
	}
	return false
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
