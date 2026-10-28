package lanebeliefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/mem"
)

// B4 — belief cooling (decay, NOT delete). A promoted tendency belief (object=observed) that gained
// zero reinforcement across COOL_AFTER_FOLDS consecutive fold runs is superseded to object=cooled.
//
// Mechanism (decision 3, option 2 — valve + adjudicate supersession, chosen because the sweep/decay
// path can NOT be driven here; see coolMechanismNote): the cool pass builds a TendencyBeliefDraft
// with object=cooled through the SAME B1 door + valve B2/B3 use, then drives it through the SAME
// governed adjudication path B3 uses (RecordStableCandidate → RecordRuntimeProofCorroboration →
// Adjudicate("promote")). Because the cooled draft shares the observed claim's cluster key (same
// lane + predicate + domain, MR-5), Adjudicate's promotedClaim automatically stamps
// Supersedes=<observed claim id> and appends the supersedes edge — the EXISTING supersession
// mechanic, no new decay primitive. The observed claim stays in ledger history (decay, not delete);
// the current stance becomes cooled; fresh evidence re-promotes observed over cooled (recovery lives
// in the fold, not here — there is no un-cool verb).
//
// Immunities (binding taxonomy): capability beliefs (predicate capable_of) ACCUMULATE and are NEVER
// evaluated. Parked candidates are B3's park bands, not promoted stances — the cool pass only reads
// CURRENT promoted claims (AllCurrent), so a parked candidate is structurally out of scope. Nothing
// here touches routing, capgraph, or peers rendering.
//
// Authority: docs_private/plan/packets/b4-belief-cooling.md.
//
// coolMechanismNote records WHY the valve+adjudicate path was chosen over driving mem's sweep/decay:
//   - sweep.go decay stage (decayExpiredCandidates) files PARKED CANDIDATES to no_promote on a
//     wall-clock ExpiresAt; it neither targets a promoted tendency claim nor triggers on fold-run
//     counts, and a contextual belief carries no ExpiresAt (the B1 valve forbids it).
//   - volatile_sweep.go DOES supersede a promoted claim to a cooled version, but only for
//     isVolatileVisibility claims driven by temperature (recall consumption) + wall clock; belief
//     claims are contextual visibility, and the packet forbids modifying sweep/temperature behavior —
//     so the volatile sweep would never select a belief claim, and B4 may not make it.
//   Neither existing stage can be driven to cool a contextual, fold-quiet tendency claim, so the
//   cool pass composes B3's adjudication path instead.

const coolReportSchemaVersion = "compound.lanebeliefs.cool.v0.1"
const coolingRecordSchemaVersion = "compound.lanebeliefs.cooling.v0.1"

// CoolReason is the durable reason token stamped on every cooling: the binding taxonomy's
// reason=zero_reinforcement_cooled. It is persisted in the coolings.jsonl artifact so a future reader
// can find WHY a belief cooled (the ledger carries the cooled STANCE; the artifact carries the why).
const CoolReason = "zero_reinforcement_cooled"

// coolWorkID tags the memory transition the cool adjudication emits.
const coolWorkID = "lanebeliefs_cool"

const (
	CoolDispositionCooled  = "cooled"
	CoolDispositionSkipped = "skipped"

	// Skip reasons (a belief evaluated but NOT cooled).
	ReasonReinforcedWithinWindow = "reinforced_within_window" // quiet folds < COOL_AFTER_FOLDS
	ReasonNoReinforcementRecord  = "no_reinforcement_record"  // no last_reinforced_run entry (never folded under B4)
	ReasonCoolDraftRefused       = "cool_draft_refused"       // the cooled draft could not be built (e.g. lane retired)
)

// CoolOptions controls a cooling pass. DryRun evaluates the gate and reports what WOULD cool while
// writing nothing (no candidate, no corroboration, no claim, no artifact). Now injects the clock.
type CoolOptions struct {
	DryRun bool
	Now    time.Time
}

// CoolReport mirrors the promote/fold reports: schema-versioned, counters, and a per-claim result.
type CoolReport struct {
	SchemaVersion  string       `json:"schema_version"`
	GeneratedAt    string       `json:"generated_at"`
	DryRun         bool         `json:"dry_run"`
	RunCount       int64        `json:"run_count"`
	CoolAfterFolds int          `json:"cool_after_folds"`
	EvaluatedCount int          `json:"evaluated_count"`
	CooledCount    int          `json:"cooled_count"`
	SkippedCount   int          `json:"skipped_count"`
	Results        []CoolResult `json:"results,omitempty"`
}

// CoolResult is one current observed tendency claim's disposition.
type CoolResult struct {
	ObservedClaimID   string `json:"observed_claim_id"`
	Subject           string `json:"subject_lane_node_id"`
	Domain            string `json:"domain"`
	Valence           string `json:"valence"`
	LastReinforcedRun int64  `json:"last_reinforced_run"`
	QuietFolds        int64  `json:"quiet_folds"`
	Disposition       string `json:"disposition"`
	Reason            string `json:"reason,omitempty"`
	CooledCandidateID string `json:"cooled_candidate_id,omitempty"`
	CooledClaimID     string `json:"cooled_claim_id,omitempty"`
	PromotionBasis    string `json:"promotion_basis,omitempty"`
	Visibility        string `json:"visibility,omitempty"`
}

// coolingRecord is one durable row in the coolings.jsonl artifact: the reason a belief cooled, kept
// findable in the lanebeliefs home area (the packet's report-artifact durability path). Closed
// vocabulary + ids only — no free text.
type coolingRecord struct {
	SchemaVersion             string `json:"schema_version"`
	SupersededObservedClaimID string `json:"superseded_observed_claim_id"`
	CooledClaimID             string `json:"cooled_claim_id"`
	CooledCandidateID         string `json:"cooled_candidate_id"`
	Subject                   string `json:"subject_lane_node_id"`
	Domain                    string `json:"domain"`
	Reason                    string `json:"reason"`
	RunCount                  int64  `json:"run_count"`
	LastReinforcedRun         int64  `json:"last_reinforced_run"`
	QuietFolds                int64  `json:"quiet_folds"`
	CreatedAt                 string `json:"created_at"`
}

// Cool evaluates every CURRENT observed tendency belief against the zero-reinforcement gate and cools
// the ones that have been quiet for COOL_AFTER_FOLDS folds — through the real mem adjudication API,
// carrying basis runtime_proof. It is the sole cooling path for lane beliefs.
func Cool(homePath string, opts CoolOptions) (CoolReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report := CoolReport{
		SchemaVersion:  coolReportSchemaVersion,
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		DryRun:         opts.DryRun,
		CoolAfterFolds: COOL_AFTER_FOLDS,
	}

	state, err := loadRunState(homePath)
	if err != nil {
		return report, err
	}
	report.RunCount = state.RunCount

	resolver, err := openLaneResolver(homePath)
	if err != nil {
		return report, err
	}
	store, err := openStoreForScan(homePath, opts.DryRun)
	if err != nil {
		return report, err
	}
	current, err := store.AllCurrent()
	if err != nil {
		return report, err
	}

	// Scope: CURRENT promoted tendency claims (object=observed) produced by the fold's provenance
	// tag. Capability beliefs (capable_of) and already-cooled stances are excluded by construction.
	observed := make([]mem.Claim, 0)
	for _, cl := range current {
		if cl.Predicate != mem.BeliefPredicateTendsTo || cl.ProvenanceLane != foldProvenanceTag {
			continue
		}
		if strings.TrimSpace(cl.Object) != tendencyObjectObserved {
			continue
		}
		if strings.TrimSpace(cl.Qualifiers[beliefDomainKey]) == "" {
			continue
		}
		observed = append(observed, cl)
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i].ID < observed[j].ID })

	for _, cl := range observed {
		res, err := evaluateCool(store, resolver, homePath, state, cl, opts.DryRun, now)
		if err != nil {
			return report, err
		}
		report.EvaluatedCount++
		if res.Disposition == CoolDispositionCooled {
			report.CooledCount++
		} else {
			report.SkippedCount++
		}
		report.Results = append(report.Results, res)
	}
	return report, nil
}

func evaluateCool(store *mem.Store, resolver mem.LaneResolver, homePath string, state foldRunState, cl mem.Claim, dryRun bool, now time.Time) (CoolResult, error) {
	domain := strings.TrimSpace(cl.Qualifiers[beliefDomainKey])
	subject := strings.TrimSpace(cl.Entity)
	res := CoolResult{
		ObservedClaimID: cl.ID,
		Subject:         subject,
		Domain:          domain,
		Valence:         beliefValence(cl.Predicate, domain),
		Disposition:     CoolDispositionSkipped,
	}

	key := reinforcementKey(subject, beliefKindTendency, domain)
	lastReinforced, ok := state.LastReinforced[key]
	if !ok {
		// No reinforcement clock (belief predates B4 bookkeeping) — conservatively never cool: we
		// cannot prove COOL_AFTER_FOLDS quiet folds, so do not decay on unknown history.
		res.Reason = ReasonNoReinforcementRecord
		return res, nil
	}
	res.LastReinforcedRun = lastReinforced
	quiet := state.RunCount - lastReinforced
	res.QuietFolds = quiet
	if quiet < COOL_AFTER_FOLDS {
		res.Reason = ReasonReinforcedWithinWindow
		return res, nil
	}

	// The gate passes → this belief cools.
	res.Disposition = CoolDispositionCooled
	res.Reason = CoolReason
	if dryRun {
		// Dry-run writes NOTHING (F-dry-run): report the disposition, leave the ledger, candidates,
		// and cooling artifact untouched.
		return res, nil
	}

	draft, buildErr := mem.TendencyBeliefDraft(resolver, subject, domain, tendencyObjectCooled, foldProvenanceTag)
	if buildErr != nil {
		// e.g. the lane node was retired since promotion → the resolver refuses. Never guess an
		// entity; skip this claim with a counted reason rather than aborting the whole pass.
		res.Disposition = CoolDispositionSkipped
		res.Reason = ReasonCoolDraftRefused
		return res, nil
	}
	draft.ID = coolCandidateID(cl.ID)
	draft.Confidence = cl.Confidence
	draft.ObservationDate = now.UTC().Format(time.RFC3339)
	draft.CreatedAt = now.UTC().Format(time.RFC3339)
	proofRefs := coolingProofRefs(cl)
	draft.EvidenceRefs = proofRefs
	// Defense in depth: the cooled draft must pass the valve (F-valve; object=cooled is ratified
	// vocabulary). A path that bypasses this governed construction is unrepresentable in cool.go.
	if err := mem.ValidateBeliefDraft(draft); err != nil {
		return res, err
	}
	if _, err := store.RecordStableCandidate(draft); err != nil {
		return res, err
	}
	// Attach the belief's own evidence trail as the deterministic runtime proof so the cooled claim
	// derives basis=runtime_proof through the EXISTING corroboration path — never a hand-stamped basis.
	if _, err := store.RecordRuntimeProofCorroboration(draft.ID, proofRefs, now); err != nil {
		return res, err
	}
	adj, err := store.Adjudicate(draft.ID, "promote", coolWorkID, now)
	if err != nil {
		return res, err
	}
	res.CooledCandidateID = draft.ID
	if adj.Claim != nil {
		res.CooledClaimID = adj.Claim.ID
		res.PromotionBasis = adj.Claim.PromotionBasis
		res.Visibility = adj.Claim.Visibility
	}
	rec := coolingRecord{
		SchemaVersion:             coolingRecordSchemaVersion,
		SupersededObservedClaimID: cl.ID,
		CooledClaimID:             res.CooledClaimID,
		CooledCandidateID:         draft.ID,
		Subject:                   subject,
		Domain:                    domain,
		Reason:                    CoolReason,
		RunCount:                  state.RunCount,
		LastReinforcedRun:         lastReinforced,
		QuietFolds:                quiet,
		CreatedAt:                 now.UTC().Format(time.RFC3339),
	}
	if err := appendCoolingRecord(homePath, rec); err != nil {
		return res, err
	}
	return res, nil
}

// coolCandidateID is the deterministic id of the candidate that cools an observed belief. It is keyed
// on the observed claim being superseded, so re-running Cool over unchanged state re-derives the same
// id — but the observed claim is by then superseded and no longer selected, so cooling is idempotent
// (F-idempotent) on two counts.
func coolCandidateID(observedClaimID string) string {
	return "cand_lanebelief_cool_" + hashParts(observedClaimID)[:20]
}

// coolingProofRefs is the belief's own evidence trail (the observed claim's evidence refs), used as
// the deterministic runtime proof for the cooled stance: the zero-reinforcement gate presents the
// belief's receipt trail as proof it existed and went quiet. Self-sourced is appropriate — cooling is
// a decay, not a standing-raising claim. Falls back to the observed claim id if the trail is empty
// (RecordRuntimeProofCorroboration requires ≥1 non-empty ref).
func coolingProofRefs(cl mem.Claim) []string {
	refs := uniqueNonEmpty(cl.EvidenceRefs)
	if len(refs) == 0 {
		refs = []string{cl.ID}
	}
	return refs
}

func coolingsPath(homePath string) string {
	return filepath.Join(laneBeliefsDir(homePath), "coolings.jsonl")
}

func appendCoolingRecord(homePath string, rec coolingRecord) error {
	if err := os.MkdirAll(laneBeliefsDir(homePath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(coolingsPath(homePath), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
