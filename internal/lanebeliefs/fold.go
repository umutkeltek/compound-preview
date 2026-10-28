package lanebeliefs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
)

// The lane-belief fold is the ONE belief producer over lane receipts (B2). It keeps the legacy
// organ — event cursoring, receipt grouping, observation aggregation, CLI — and rewires its two
// former duplicate layers onto their owners: lane IDENTITY resolves ONLY through the B0 store
// door (internal/capgraph, via resolveReceiptLane), and belief SHAPES are built ONLY by the B1
// constructors and pass the B1 valve (internal/mem, via draftForObservation). It is PROPOSE-ONLY
// (binding taxonomy: B3 cross-lane promotion + B4 cooling are out of scope) and read-only in
// influence: it writes its own cursor + observations + memory candidates, and NOTHING else.
//
// Authority: docs_private/plan/packets/2026-07-09-agent-lane-beliefs.md BINDING TAXONOMY.

const (
	MIN_LANE_RECEIPTS       = 5
	MIN_CORROBORATION_WORKS = 2
	BETA_PRIOR_ALPHA        = 2
	BETA_PRIOR_BETA         = 2
	PROMOTE_POSTERIOR_FLOOR = 0.66
	PER_CYCLE_CANDIDATE_CAP = 50
	COOL_AFTER_FOLDS        = 3
)

const foldReportSchemaVersion = "compound.lanebeliefs.fold.v0.1"
const listReportSchemaVersion = "compound.lanebeliefs.list.v0.1"

// foldProvenanceTag is the fold's OWN provenance tag (rule 4: provenance_lane is the fold's tag,
// not the subject, not the evidence producer). It is a lowercase slug (the B1 valve refuses a
// free-text provenance) and it is the single tag-free discriminator List uses to find fold output.
const foldProvenanceTag = "lanebeliefs-fold"

// belief kinds + the closed extractor vocabulary the fold maps closed receipt fields onto.
const (
	beliefKindTendency   = "tendency"
	beliefKindCapability = "capability"
	// beliefKindCompletion is the completion-posterior observation kind: one Beta trial per
	// terminal work attempt (completed vs failed/cancelled), conditioned on (lane, capability_class).
	// Unlike the two belief kinds it is NOT a promotable belief shape — it proposes no memory
	// candidate and never touches the B1 valve — it is an observation-derived read surface
	// (CompletionPosterior) the EV-card composer consumes. Its records live in their OWN store
	// (completions.jsonl), never commingled with the belief observations the promotion/cooling
	// passes read, so a completion trial can never leak into candidate promotion.
	beliefKindCompletion = "completion"

	tendencyReceiptFaithful = "receipt-faithful" // completed WITH proof — standing-raising
	tendencyCeremonySkipper = "ceremony-skipper" // completed WITHOUT proof — standing-lowering
	tendencyObjectObserved  = "observed"
	tendencyObjectCooled    = "cooled" // the decayed stance a zero-reinforcement belief supersedes to (B4)

	// completionObjectAttempt is the CONSTANT object every completion trial carries. The attempt's
	// outcome is NOT encoded here (that would make a record depend on which of a work's receipts was
	// discovered first) — it lives in Hits (1 completed, 0 failed/cancelled), AND-reduced so a record
	// is byte-identical under any receipt-discovery order.
	completionObjectAttempt = "attempt"
	// completionOutcomeCompleted is the single terminal work state that scores a completion hit; the
	// denominator (failed/cancelled) is every other terminal state (see laneBeliefTerminalState).
	completionOutcomeCompleted = "completed"
	// capabilityClassUnknown is the explicit sentinel class a completion trial buckets into when no
	// capability class is resolvable for the work. It is its OWN bucket: a KNOWN class is NEVER pooled
	// with another and NEVER merged into unknown, so a 2/2 prior over one class can never be mistaken
	// for evidence about another (see completionCapabilityClass for why this is unknown today).
	capabilityClassUnknown = "unknown"
)

type FoldOptions struct {
	DryRun bool
	Now    time.Time
}

type FoldReport struct {
	SchemaVersion             string `json:"schema_version"`
	GeneratedAt               string `json:"generated_at"`
	DryRun                    bool   `json:"dry_run"`
	CursorBefore              int64  `json:"cursor_before"`
	CursorAfter               int64  `json:"cursor_after"`
	RawReceiptCount           int    `json:"raw_receipt_count"`
	UnresolvedReceiptCount    int    `json:"unresolved_receipt_count"`
	IgnoredReceiptCount       int    `json:"ignored_receipt_count"`
	NewObservationCount       int    `json:"new_observation_count"`
	DuplicateObservationCount int    `json:"duplicate_observation_count"`
	// NewCompletionObservationCount / DuplicateCompletionObservationCount mirror the belief-observation
	// counters for the completion kind, kept SEPARATE so the belief-observation counts (and every test
	// pinned to them) stay byte-unchanged when a completed receipt also mints a completion trial.
	NewCompletionObservationCount       int `json:"new_completion_observation_count"`
	DuplicateCompletionObservationCount int `json:"duplicate_completion_observation_count"`
	CandidateCount                      int `json:"candidate_count"`
	ReinforcementCount                  int `json:"reinforcement_count"`
	// RunCount is the persisted fold-RUN counter after this run (B4 cooling bookkeeping): every
	// non-dry-run Fold increments it. The cool pass reads current_run − last_reinforced_run against
	// COOL_AFTER_FOLDS. Zero on a dry-run (nothing persisted).
	RunCount int64 `json:"run_count"`
	// ReinforcedAggregateCount is how many (lane, kind, domain) aggregates gained ≥1 new observation
	// this run and therefore had their last_reinforced_run stamped to RunCount (B4 bookkeeping).
	ReinforcedAggregateCount int             `json:"reinforced_aggregate_count"`
	BelowFloorLaneCount      int             `json:"below_floor_lane_count"`
	ValveDroppedCount        int             `json:"valve_dropped_count"`
	OverflowCount            int             `json:"overflow_count"`
	CandidateIDs             []string        `json:"candidate_ids,omitempty"`
	ValveDrops               []ValveDrop     `json:"valve_drops,omitempty"`
	Aggregates               []FoldAggregate `json:"aggregates,omitempty"`
}

// ValveDrop records a belief draft the B1 valve refused: a first-class, counted outcome, never a
// silent swallow (a draft that fails the valve is DROPPED, never proposed — falsifier valve).
type ValveDrop struct {
	SubjectLaneNodeID string `json:"subject_lane_node_id"`
	Domain            string `json:"domain"`
	Kind              string `json:"kind"`
	Reason            string `json:"reason"`
}

type FoldAggregate struct {
	SubjectLaneNodeID   string  `json:"subject_lane_node_id"`
	Domain              string  `json:"domain"`
	Kind                string  `json:"kind"`
	WorkCount           int     `json:"work_count"`
	ReceiptCount        int     `json:"receipt_count"`
	OpportunityCount    int     `json:"opportunity_count"`
	HitCount            int     `json:"hit_count"`
	PosteriorMean       float64 `json:"posterior_mean"`
	PosteriorBand       string  `json:"posterior_band"`
	AbovePromotionFloor bool    `json:"above_promotion_floor"`
}

type ListReport struct {
	SchemaVersion  string          `json:"schema_version"`
	CandidateCount int             `json:"candidate_count"`
	Candidates     []mem.Candidate `json:"candidates"`
}

type foldCursor struct {
	MaxEventSeq int64  `json:"max_event_seq"`
	UpdatedAt   string `json:"updated_at"`
}

type workEvent struct {
	TS       string   `json:"ts"`
	WorkID   string   `json:"work_id"`
	To       string   `json:"to"`
	Attached []string `json:"attached"`
}

type sequencedEvent struct {
	Seq   int64
	Event workEvent
}

// observationRecord is one (lane, domain, kind, work) trial: the atom that satisfies rule 6
// (ONE observation per lane/domain/work — receipt spam within a work does not inflate n). It
// carries the closed structured counts B3's Beta will need (Hits/Opportunities per work-trial;
// ReceiptCount raw volume for the noise floor) — the fold RECORDS these rates, it does not
// compute promotion posteriors.
type observationRecord struct {
	DedupKey          string `json:"dedup_key"`
	GroupKey          string `json:"group_key"`
	SubjectLaneNodeID string `json:"subject_lane_node_id"`
	Domain            string `json:"domain"`
	Kind              string `json:"kind"`
	Object            string `json:"object"`
	WorkID            string `json:"work_id"`
	EvidenceEventID   string `json:"evidence_event_id"`
	MoveType          string `json:"move_type"`
	Outcome           string `json:"outcome"`
	ReceiptCount      int    `json:"receipt_count"`
	Hits              int    `json:"hits"`
	Opportunities     int    `json:"opportunities"`
	TransitionCount   int    `json:"transition_count"`
	FileChangeCount   int    `json:"file_change_count"`
	CreatedAt         string `json:"created_at"`
}

type receiptGroup struct {
	SubjectLaneNodeID string
	Domain            string
	Kind              string
	Object            string
	WorkID            string
	EvidenceEventID   string
	MoveType          string
	Outcome           string
	ReceiptCount      int
	Hits              int
	Opportunities     int
	TransitionCount   int
	FileChangeCount   int
}

// beliefSignal is a single (kind, domain, object) belief the extractor reads off ONE receipt's
// closed structured fields. A receipt may yield several (a capability + a tendency + a completion
// trial); each becomes its own observation trial. Hits/Opportunities carry the Beta-trial numerator
// and denominator: the belief kinds are always a hit-of-one (1/1 — a completed work observed once),
// while a completion trial is 1/1 for a completed work and 0/1 for a failed/cancelled one, so the
// same aggregation yields P(completed).
type beliefSignal struct {
	Kind          string
	Domain        string
	Object        string
	Hits          int
	Opportunities int
}

func Fold(homePath string, opts FoldOptions) (FoldReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report := FoldReport{
		SchemaVersion: foldReportSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		DryRun:        opts.DryRun,
	}
	resolver, err := openLaneResolver(homePath)
	if err != nil {
		return report, err
	}
	cursor, err := loadCursor(homePath)
	if err != nil {
		return report, err
	}
	report.CursorBefore = cursor.MaxEventSeq
	events, maxSeq, err := loadWorkEventsAfter(homePath, cursor.MaxEventSeq)
	if err != nil {
		return report, err
	}
	report.CursorAfter = maxSeq

	existing, err := loadObservations(homePath)
	if err != nil {
		return report, err
	}
	existingGroups := map[string]bool{}
	for _, obs := range existing {
		existingGroups[obs.GroupKey] = true
	}
	// Completion trials live in their own store and dedup against their own group set (rule 5/6
	// applied per kind): a re-fold over unchanged events adds no completion observation either.
	existingCompletions, err := loadCompletionObservations(homePath)
	if err != nil {
		return report, err
	}
	existingCompletionGroups := map[string]bool{}
	for _, obs := range existingCompletions {
		existingCompletionGroups[obs.GroupKey] = true
	}

	groups := map[string]*receiptGroup{}
	for _, ev := range events {
		if !laneBeliefTerminalState(ev.Event.To) {
			continue
		}
		transitionCount := countAttachedPrefix(ev.Event.Attached, "tr_")
		for _, id := range ev.Event.Attached {
			if !strings.HasPrefix(id, "rec_") {
				continue
			}
			rec, err := loadReceipt(homePath, id)
			if err != nil {
				return report, err
			}
			report.RawReceiptCount++
			resolution := resolveReceiptLane(resolver, rec)
			if !resolution.Resolved() {
				// Rule 1 + falsifier l: an unattributable receipt is a first-class zero-evidence
				// outcome, counted here and dropped — never guessed onto a synthetic node.
				report.UnresolvedReceiptCount++
				continue
			}
			if strings.TrimSpace(rec.MoveType) == "audit_only" {
				report.IgnoredReceiptCount++
				continue
			}
			workID := firstNonEmpty(rec.IDs.WorkID, ev.Event.WorkID)
			if workID == "" {
				report.IgnoredReceiptCount++
				continue
			}
			// Belief signals (capability + tendency) are minted ONLY for a completed receipt; the
			// completion trial is minted for EVERY terminal work attempt (completed/failed/cancelled)
			// keyed on the WORK's terminal state (ev.Event.To), so a non-completion is a first-class
			// miss (Hits=0), not an ignored receipt. audit_only and work-id-less receipts were already
			// dropped above, so every receipt reaching here is one legitimate attempt.
			signals := beliefSignalsFromReceipt(rec)
			signals = append(signals, completionSignal(ev.Event.To, completionCapabilityClass(rec)))
			for _, sig := range signals {
				groupKey := hashParts(resolution.NodeID, sig.Kind, sig.Domain, workID)
				group := groups[groupKey]
				if group == nil {
					group = &receiptGroup{
						SubjectLaneNodeID: resolution.NodeID,
						Domain:            sig.Domain,
						Kind:              sig.Kind,
						Object:            sig.Object,
						WorkID:            workID,
						EvidenceEventID:   rec.ID,
						MoveType:          strings.TrimSpace(rec.MoveType),
						Outcome:           strings.TrimSpace(rec.Outcome),
						// rule 6: one work is one Beta trial. Receipt spam inside the work bumps
						// ReceiptCount (volume) but NEVER n (Opportunities). Hits/Opportunities come
						// from the signal: 1/1 for a belief, {0,1}/1 for a completion trial.
						Hits:          sig.Hits,
						Opportunities: sig.Opportunities,
					}
					groups[groupKey] = group
				}
				group.ReceiptCount++
				group.TransitionCount += transitionCount
				group.FileChangeCount += len(rec.StateDelta.FilesChanged)
				if group.Kind == beliefKindCompletion {
					// Order-independent reducers so a work's completion record is byte-identical under
					// any receipt-discovery order: AND the hit (any non-completed terminal for this
					// attempt makes it a miss) and pin the evidence ref to the smallest receipt id.
					if sig.Hits < group.Hits {
						group.Hits = sig.Hits
					}
					if rec.ID < group.EvidenceEventID {
						group.EvidenceEventID = rec.ID
					}
				}
			}
		}
	}

	newObservations := make([]observationRecord, 0, len(groups))
	newCompletions := make([]observationRecord, 0, len(groups))
	for _, key := range sortedReceiptGroupKeys(groups) {
		group := groups[key]
		obs := newObservationRecord(group, key, now)
		if group.Kind == beliefKindCompletion {
			if existingCompletionGroups[key] {
				report.DuplicateCompletionObservationCount++
				continue
			}
			newCompletions = append(newCompletions, obs)
			continue
		}
		if existingGroups[key] {
			report.DuplicateObservationCount++
			continue
		}
		newObservations = append(newObservations, obs)
	}
	report.NewObservationCount = len(newObservations)
	report.NewCompletionObservationCount = len(newCompletions)
	// Aggregates + candidate emission stay belief-only: completion trials are never folded into the
	// belief observations the promotion/cooling passes read, and they mint no candidate.
	allObservations := append(append([]observationRecord{}, existing...), newObservations...)
	report.Aggregates = aggregateObservations(allObservations)

	if len(newObservations) > 0 {
		store, err := openStoreForScan(homePath, opts.DryRun)
		if err != nil {
			return report, err
		}
		// B4 recovery: a belief slot whose CURRENT claim is object=cooled is re-openable — fresh
		// evidence must re-promote observed OVER cooled (decay is undoable). The landed fold+B3
		// freeze a promoted slot's candidate (writeStableCandidate no-ops), so a recovery draft needs
		// a DISTINCT id keyed on the cooled claim it supersedes. When no cooled claim is present the
		// candidate id is byte-unchanged (normal path), so this arm is inert on every non-recovery run.
		cooledSlots, err := cooledBeliefSlots(store)
		if err != nil {
			return report, err
		}
		candidates, drops, belowFloor, err := candidatesForNewObservations(resolver, newObservations, allObservations, cooledSlots, now)
		if err != nil {
			return report, err
		}
		report.ValveDrops = drops
		report.ValveDroppedCount = len(drops)
		report.BelowFloorLaneCount = belowFloor
		if len(candidates) > PER_CYCLE_CANDIDATE_CAP {
			report.OverflowCount = len(candidates) - PER_CYCLE_CANDIDATE_CAP
			candidates = candidates[:PER_CYCLE_CANDIDATE_CAP]
		}
		if opts.DryRun {
			report.CandidateCount = len(candidates)
			for _, candidate := range candidates {
				report.CandidateIDs = append(report.CandidateIDs, candidate.ID)
			}
		} else {
			for _, candidate := range candidates {
				created, err := store.RecordStableCandidate(candidate)
				if err != nil {
					return report, err
				}
				if created {
					report.CandidateCount++
					report.CandidateIDs = append(report.CandidateIDs, candidate.ID)
				}
			}
		}
	}

	if !opts.DryRun {
		if err := appendObservations(homePath, newObservations); err != nil {
			return report, err
		}
		// Completion trials are persisted to their own store regardless of whether any belief
		// observation formed this run (a run of only failed/cancelled attempts still records misses).
		if err := appendCompletionObservations(homePath, newCompletions); err != nil {
			return report, err
		}
		if maxSeq > cursor.MaxEventSeq {
			if err := saveCursor(homePath, foldCursor{MaxEventSeq: maxSeq, UpdatedAt: now.UTC().Format(time.RFC3339)}); err != nil {
				return report, err
			}
		}
		// B4 run-bookkeeping (additive): every non-dry-run fold is one RUN. Increment the run counter
		// and stamp last_reinforced_run = current run for each aggregate that gained a new observation
		// this run. Persisted beside the cursor. Quiet runs bump the counter without reinforcing, so
		// the cool pass can measure zero-reinforcement folds since a belief's last evidence.
		reinforced, err := recordFoldRun(homePath, newObservations, now)
		if err != nil {
			return report, err
		}
		report.RunCount = reinforced.RunCount
		report.ReinforcedAggregateCount = reinforced.ReinforcedAggregateCount
	}
	return report, nil
}

// beliefSignalsFromReceipt derives lane-belief signals from a receipt's CLOSED structured fields
// ONLY (move_type, outcome, proof_refs PRESENCE) — never free text (rule 7: no StateDelta.Summary,
// FailureCause, snippets). A completed terminal receipt yields (a) a capability signal for the
// task class it completed and (b) exactly one tendency signal keyed on proof discipline:
// proof-bearing → the standing-raising `receipt-faithful` tendency, proof-absent → the
// standing-lowering `ceremony-skipper` tendency (self-incriminating and admissible per the
// taxonomy's valence rule). A non-completed terminal receipt (failed/cancelled/no_op) yields no
// positive belief signal — the fold does not infer a capability or a raising tendency from a
// non-completion.
func beliefSignalsFromReceipt(rec envelope.Receipt) []beliefSignal {
	if strings.TrimSpace(rec.Outcome) != "completed" {
		return nil
	}
	tendency := tendencyCeremonySkipper
	if len(rec.ProofRefs) > 0 {
		tendency = tendencyReceiptFaithful
	}
	return []beliefSignal{
		{Kind: beliefKindCapability, Domain: taskClassDomain(rec), Object: mem.CapabilityObject, Hits: 1, Opportunities: 1},
		{Kind: beliefKindTendency, Domain: tendency, Object: tendencyObjectObserved, Hits: 1, Opportunities: 1},
	}
}

// newObservationRecord flattens one grouped Beta trial into its persisted record. Completion trials
// drop the per-receipt move_type/outcome (an attempt's outcome lives in Hits, AND-reduced and
// order-independent); carrying the raw fields would make the record depend on receipt-discovery
// order and break the fold's byte-identical determinism guarantee.
func newObservationRecord(group *receiptGroup, key string, now time.Time) observationRecord {
	moveType, outcome := group.MoveType, group.Outcome
	if group.Kind == beliefKindCompletion {
		moveType, outcome = "", ""
	}
	return observationRecord{
		// rule 5: the per-observation dedup key hash(subject_lane, domain, work_id,
		// evidence_event_id); GroupKey (lane, kind, domain, work) is the rule-6 identity the
		// dedup check runs on, so a re-fold over unchanged events adds no observation.
		DedupKey:          hashParts(group.SubjectLaneNodeID, group.Domain, group.WorkID, group.EvidenceEventID),
		GroupKey:          key,
		SubjectLaneNodeID: group.SubjectLaneNodeID,
		Domain:            group.Domain,
		Kind:              group.Kind,
		Object:            group.Object,
		WorkID:            group.WorkID,
		EvidenceEventID:   group.EvidenceEventID,
		MoveType:          moveType,
		Outcome:           outcome,
		ReceiptCount:      group.ReceiptCount,
		Hits:              group.Hits,
		Opportunities:     group.Opportunities,
		TransitionCount:   group.TransitionCount,
		FileChangeCount:   group.FileChangeCount,
		CreatedAt:         now.UTC().Format(time.RFC3339),
	}
}

// taskClassDomain is the capability domain: the move_type this lane completed, as a lowercase
// task-class slug. move_type is a closed enum field, never free text.
func taskClassDomain(rec envelope.Receipt) string {
	return slug(strings.TrimSpace(rec.MoveType))
}

func laneBeliefTerminalState(state string) bool {
	state = strings.TrimSpace(state)
	return state == "completed" || state == "failed" || state == "cancelled"
}

// List returns the fold's proposed lane-belief candidates. Discrimination is TAG-FREE: the B1
// valve refuses tags, so List cannot filter by tag; it filters by the fold's own provenance tag
// (rule 4) intersected with the belief predicates — the fold is the only writer of that tag.
func List(homePath string) (ListReport, error) {
	store, err := mem.OpenScan(homePath)
	if err != nil {
		return ListReport{}, err
	}
	all, err := store.AllCandidates()
	if err != nil {
		return ListReport{}, err
	}
	items := make([]mem.Candidate, 0, len(all))
	for _, candidate := range all {
		if isLaneBeliefCandidate(candidate) {
			items = append(items, candidate)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return ListReport{SchemaVersion: listReportSchemaVersion, CandidateCount: len(items), Candidates: items}, nil
}

func isLaneBeliefCandidate(c mem.Candidate) bool {
	if c.ProvenanceLane != foldProvenanceTag {
		return false
	}
	for _, p := range mem.BeliefPredicates {
		if c.Predicate == p {
			return true
		}
	}
	return false
}

// candidatesForNewObservations turns new observation trials into proposed belief candidates,
// building each draft ONLY via the B1 constructors + valve (draftForObservation). A lane/domain
// below the noise floor yields no candidate (counted in belowFloor); a draft the valve refuses is
// DROPPED and counted with its reason (never proposed, never silently swallowed).
func candidatesForNewObservations(resolver mem.LaneResolver, newObservations, allObservations []observationRecord, cooledSlots map[string]string, now time.Time) (cands []mem.Candidate, drops []ValveDrop, belowFloor int, err error) {
	aggregates := aggregateObservationMap(allObservations)
	seen := map[string]bool{}
	for _, obs := range newObservations {
		aggKey := aggregateKey(obs.SubjectLaneNodeID, obs.Kind, obs.Domain)
		if seen[aggKey] {
			continue
		}
		seen[aggKey] = true
		agg := aggregates[aggKey]
		if agg.ReceiptCount < MIN_LANE_RECEIPTS {
			// Noise floor (falsifier floor): a lane below MIN_LANE_RECEIPTS yields no candidate.
			belowFloor++
			continue
		}
		draft, buildErr := draftForObservation(resolver, obs, agg, now)
		if buildErr != nil {
			drops = append(drops, ValveDrop{
				SubjectLaneNodeID: obs.SubjectLaneNodeID,
				Domain:            obs.Domain,
				Kind:              obs.Kind,
				Reason:            buildErr.Error(),
			})
			continue
		}
		// B4 recovery: if the slot's current claim is cooled, this new (object=observed) draft is a
		// re-promotion over the cooled claim. Re-key it off the cooled claim id so it is a FRESH
		// candidate the gate can promote (the frozen genesis candidate stays promoted and untouched).
		if cooledClaimID, ok := cooledSlots[aggKey]; ok {
			draft.ID = recoveryCandidateID(cooledClaimID)
			if err := mem.ValidateBeliefDraft(draft); err != nil {
				drops = append(drops, ValveDrop{
					SubjectLaneNodeID: obs.SubjectLaneNodeID,
					Domain:            obs.Domain,
					Kind:              obs.Kind,
					Reason:            err.Error(),
				})
				continue
			}
		}
		cands = append(cands, draft)
	}
	return cands, drops, belowFloor, nil
}

// draftForObservation builds one belief draft through the B1 door (mem.TendencyBeliefDraft /
// mem.CapabilityBeliefDraft — which mint the entity ONLY via the resolver and self-validate), then
// stamps the fold's deterministic identity + evidence refs + observation dates and runs the
// exported valve one final time before the draft is proposed. Propose-only: promotion_basis stays
// EMPTY (promotion is B3). A valve refusal is returned as an error and counted upstream.
func draftForObservation(resolver mem.LaneResolver, obs observationRecord, agg FoldAggregate, now time.Time) (mem.Candidate, error) {
	var (
		draft mem.Candidate
		err   error
	)
	switch obs.Kind {
	case beliefKindTendency:
		draft, err = mem.TendencyBeliefDraft(resolver, obs.SubjectLaneNodeID, obs.Domain, obs.Object, foldProvenanceTag)
	case beliefKindCapability:
		draft, err = mem.CapabilityBeliefDraft(resolver, obs.SubjectLaneNodeID, obs.Domain, foldProvenanceTag)
	default:
		return mem.Candidate{}, fmt.Errorf("belief draft: unknown kind %q", obs.Kind)
	}
	if err != nil {
		return mem.Candidate{}, err
	}
	draft.ID = beliefCandidateID(draft)
	draft.Confidence = agg.PosteriorMean
	draft.ObservationDate = now.UTC().Format(time.RFC3339)
	draft.CreatedAt = now.UTC().Format(time.RFC3339)
	if ref := strings.TrimSpace(obs.EvidenceEventID); ref != "" {
		draft.EvidenceRefs = []string{ref}
	}
	// Final gate before proposing (defense in depth after the fold's stamps): the valve is law.
	if err := mem.ValidateBeliefDraft(draft); err != nil {
		return mem.Candidate{}, err
	}
	return draft, nil
}

// beliefCandidateID is the deterministic candidate id for a (lane, predicate, domain) belief slot,
// stable across folds so RecordStableCandidate is idempotent. Object is excluded so a future
// stance change supersedes in-slot rather than minting a new candidate.
func beliefCandidateID(c mem.Candidate) string {
	h := hashParts(c.Entity, c.Predicate, c.Qualifiers[beliefDomainKey])
	return "cand_lanebelief_" + h[:20]
}

const beliefDomainKey = "domain"

// recoveryCandidateID is the deterministic id of the FRESH candidate that re-promotes an observed
// belief over a cooled one (B4 recovery). It is keyed on the cooled claim being superseded, so it is
// stable while that cooled claim is current (idempotent) and distinct from both the frozen genesis
// candidate and any prior cool/recovery generation.
func recoveryCandidateID(cooledClaimID string) string {
	return "cand_lanebelief_recover_" + hashParts(cooledClaimID)[:20]
}

// reinforcementKey is the JSON-map-safe key for a (subject, kind, domain) aggregate in the fold
// run-state's last_reinforced map. Both the fold (stamping) and the cool pass (reading) compute it
// the same way, so they agree on which aggregate a belief's reinforcement clock belongs to.
func reinforcementKey(subject, kind, domain string) string {
	return hashParts(subject, kind, domain)
}

// foldRunState is B4's additive fold-RUN bookkeeping, persisted beside the cursor. run_count is the
// number of non-dry-run folds; last_reinforced maps a reinforcementKey to the run in which its
// aggregate last gained a new observation. The cursor (event seq) is untouched — this is a separate,
// additive projection so the landed cursor semantics do not change.
type foldRunState struct {
	RunCount       int64            `json:"run_count"`
	LastReinforced map[string]int64 `json:"last_reinforced"`
	UpdatedAt      string           `json:"updated_at,omitempty"`
}

type foldRunSummary struct {
	RunCount                 int64
	ReinforcedAggregateCount int
}

func runStatePath(homePath string) string {
	return filepath.Join(laneBeliefsDir(homePath), "runstate.json")
}

func loadRunState(homePath string) (foldRunState, error) {
	state := foldRunState{LastReinforced: map[string]int64{}}
	b, err := os.ReadFile(runStatePath(homePath))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("parse lane belief run state: %w", err)
	}
	if state.LastReinforced == nil {
		state.LastReinforced = map[string]int64{}
	}
	return state, nil
}

func saveRunState(homePath string, state foldRunState) error {
	if err := os.MkdirAll(laneBeliefsDir(homePath), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(runStatePath(homePath), append(b, '\n'), 0o644)
}

// recordFoldRun increments the run counter and stamps last_reinforced_run for each aggregate that
// gained a new observation this run. It runs only on a non-dry-run fold (the caller gates it), so a
// dry-run leaves the run state byte-identical.
func recordFoldRun(homePath string, newObservations []observationRecord, now time.Time) (foldRunSummary, error) {
	state, err := loadRunState(homePath)
	if err != nil {
		return foldRunSummary{}, err
	}
	state.RunCount++
	reinforced := map[string]bool{}
	for _, obs := range newObservations {
		key := reinforcementKey(obs.SubjectLaneNodeID, obs.Kind, obs.Domain)
		state.LastReinforced[key] = state.RunCount
		reinforced[key] = true
	}
	state.UpdatedAt = now.UTC().Format(time.RFC3339)
	if err := saveRunState(homePath, state); err != nil {
		return foldRunSummary{}, err
	}
	return foldRunSummary{RunCount: state.RunCount, ReinforcedAggregateCount: len(reinforced)}, nil
}

// cooledBeliefSlots returns, per (subject, kind, domain) aggregate key, the id of the CURRENT cooled
// tendency claim in that slot — the slots B4 recovery may re-promote over. Only fold-produced
// tendency claims (provenance = the fold tag) with object=cooled qualify. Read-only.
func cooledBeliefSlots(store *mem.Store) (map[string]string, error) {
	current, err := store.AllCurrent()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, cl := range current {
		if cl.Predicate != mem.BeliefPredicateTendsTo || cl.ProvenanceLane != foldProvenanceTag {
			continue
		}
		if strings.TrimSpace(cl.Object) != tendencyObjectCooled {
			continue
		}
		domain := strings.TrimSpace(cl.Qualifiers[beliefDomainKey])
		if domain == "" {
			continue
		}
		out[aggregateKey(cl.Entity, beliefKindTendency, domain)] = cl.ID
	}
	return out, nil
}

func aggregateObservations(observations []observationRecord) []FoldAggregate {
	m := aggregateObservationMap(observations)
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]FoldAggregate, 0, len(keys))
	for _, key := range keys {
		out = append(out, m[key])
	}
	return out
}

func aggregateObservationMap(observations []observationRecord) map[string]FoldAggregate {
	byAggregate := map[string]*FoldAggregate{}
	works := map[string]map[string]bool{}
	for _, obs := range observations {
		key := aggregateKey(obs.SubjectLaneNodeID, obs.Kind, obs.Domain)
		agg := byAggregate[key]
		if agg == nil {
			agg = &FoldAggregate{SubjectLaneNodeID: obs.SubjectLaneNodeID, Kind: obs.Kind, Domain: obs.Domain}
			byAggregate[key] = agg
			works[key] = map[string]bool{}
		}
		agg.ReceiptCount += obs.ReceiptCount
		agg.HitCount += obs.Hits
		agg.OpportunityCount += obs.Opportunities
		works[key][obs.WorkID] = true
	}
	out := map[string]FoldAggregate{}
	for key, agg := range byAggregate {
		agg.WorkCount = len(works[key])
		agg.PosteriorMean = float64(BETA_PRIOR_ALPHA+agg.HitCount) / float64(BETA_PRIOR_ALPHA+BETA_PRIOR_BETA+agg.OpportunityCount)
		// Bands are DISPLAY only — the fold does NOT gate promotion on the posterior (that is B3).
		// Candidate emission gates on the noise floor + the valve, never on this band.
		agg.AbovePromotionFloor = agg.PosteriorMean >= PROMOTE_POSTERIOR_FLOOR && agg.WorkCount >= MIN_CORROBORATION_WORKS
		switch {
		case agg.AbovePromotionFloor:
			agg.PosteriorBand = "propose"
		case agg.PosteriorMean >= 0.50:
			agg.PosteriorBand = "park"
		default:
			agg.PosteriorBand = "ignore"
		}
		out[key] = *agg
	}
	return out
}

// openLaneResolver returns the B0 identity door for this home: a read-only capability-graph store
// when one exists, else a resolver that resolves nothing (a fresh home with no imported lanes
// attributes every receipt to nothing — zero evidence, never a guess). It never creates capgraph
// state (rule 8: read-only influence).
func openLaneResolver(homePath string) (mem.LaneResolver, error) {
	if !capgraph.Exists(homePath) {
		return nullLaneResolver{}, nil
	}
	return capgraph.OpenReadOnly(homePath)
}

// openStoreForScan is the store door the belief pipeline uses. An APPLY pass
// needs the writer's scaffolding (it promotes through the real adjudication
// API); a DRY-RUN pass must not scaffold ledger/ or events/ just by looking —
// the plan pass reads the inbox and claims through the scan opener, which
// treats an absent ledger as an empty view instead of creating one.
func openStoreForScan(homePath string, dryRun bool) (*mem.Store, error) {
	if dryRun {
		return mem.OpenScan(homePath)
	}
	return mem.Open(homePath)
}

type nullLaneResolver struct{}

func (nullLaneResolver) ResolveLaneRef(string) (string, bool) { return "", false }

func loadWorkEventsAfter(homePath string, cursor int64) ([]sequencedEvent, int64, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return nil, cursor, err
	}
	f, err := store.OpenWorkLog()
	if os.IsNotExist(err) {
		return nil, cursor, nil
	}
	if err != nil {
		return nil, cursor, err
	}
	defer f.Close()
	var events []sequencedEvent
	seq := int64(0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		seq++
		if line == "" || seq <= cursor {
			continue
		}
		var ev workEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, seq, fmt.Errorf("parse work event seq %d: %w", seq, err)
		}
		events = append(events, sequencedEvent{Seq: seq, Event: ev})
	}
	if err := scanner.Err(); err != nil {
		return nil, seq, err
	}
	return events, seq, nil
}

func loadReceipt(homePath, id string) (envelope.Receipt, error) {
	var rec envelope.Receipt
	store, err := eventstore.Open(homePath)
	if err != nil {
		return rec, err
	}
	record, err := store.Read(id)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(record.Bytes, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func loadCursor(homePath string) (foldCursor, error) {
	var cursor foldCursor
	b, err := os.ReadFile(cursorPath(homePath))
	if os.IsNotExist(err) {
		return cursor, nil
	}
	if err != nil {
		return cursor, err
	}
	return cursor, json.Unmarshal(b, &cursor)
}

func saveCursor(homePath string, cursor foldCursor) error {
	if err := os.MkdirAll(laneBeliefsDir(homePath), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cursor, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cursorPath(homePath), append(b, '\n'), 0o644)
}

func loadObservations(homePath string) ([]observationRecord, error) {
	return loadObservationRecords(observationsPath(homePath), "observation")
}

// loadCompletionObservations reads the completion-trial store — a store SEPARATE from the belief
// observations, so the promotion/cooling passes (which read loadObservations) never see a
// completion trial.
func loadCompletionObservations(homePath string) ([]observationRecord, error) {
	return loadObservationRecords(completionsPath(homePath), "completion observation")
}

func loadObservationRecords(path, label string) ([]observationRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []observationRecord
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obs observationRecord
		if err := json.Unmarshal([]byte(line), &obs); err != nil {
			return nil, fmt.Errorf("parse lane belief %s %d: %w", label, lineNo, err)
		}
		out = append(out, obs)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func appendObservations(homePath string, observations []observationRecord) error {
	return appendObservationRecords(observationsPath(homePath), observations)
}

func appendCompletionObservations(homePath string, observations []observationRecord) error {
	return appendObservationRecords(completionsPath(homePath), observations)
}

func appendObservationRecords(path string, observations []observationRecord) error {
	if len(observations) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, obs := range observations {
		b, err := json.Marshal(obs)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func sortedReceiptGroupKeys(groups map[string]*receiptGroup) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func aggregateKey(subject, kind, domain string) string {
	return subject + "\x00" + kind + "\x00" + domain
}

func countAttachedPrefix(values []string, prefix string) int {
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			count++
		}
	}
	return count
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(strings.TrimSpace(part)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func laneBeliefsDir(homePath string) string { return filepath.Join(homePath, "lanebeliefs") }
func cursorPath(homePath string) string {
	return filepath.Join(laneBeliefsDir(homePath), "cursor.json")
}
func observationsPath(homePath string) string {
	return filepath.Join(laneBeliefsDir(homePath), "observations.jsonl")
}
func completionsPath(homePath string) string {
	return filepath.Join(laneBeliefsDir(homePath), "completions.jsonl")
}
