package director

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/observation"
	"compound/internal/product"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	autonomyproof "compound/internal/routeevidence/autonomy"
	"compound/internal/work"
)

const (
	ReportSchemaVersion = "compound.director_propose_report.v0.1"
	DirectorActor       = "director"
	maxProposalsPerRun  = 5
)

var proposalClassSeverityOrder = []string{
	"job_health",
	"source_connection_readiness",
	"product_loop",
	"synthesis_contradiction",
	"review_debt",
	// decision_preparation ranks just below live review debt: it serves the
	// operator's decision throughput (a needs-you card sitting past a threshold
	// gets a brief prepared before the operator looks), so it outranks
	// self-improvement (harness_edit) and retirement debt (absorption_debt). It
	// never outranks a class that resolves a real pending item, and it is
	// propose-only with no autonomy grant.
	"decision_preparation",
	// harness_edit ranks just above absorption_debt: the self-harness loop
	// (proposing bounded edits to editable harness surfaces) is real
	// improvement work, but self-improvement must never starve an operational
	// class — every live-operations class above outranks it, so a harness-edit
	// proposal only takes a slot once the operational classes have theirs.
	"harness_edit",
	// absorption_debt ranks last: os-absorption retirement debt is real but
	// never more urgent than live review debt (W5 J15). The operator can
	// reorder later; today it just must not starve behind the cap forever.
	"absorption_debt",
}

type Options struct {
	Apply bool
	Now   time.Time
}

type Report struct {
	SchemaVersion            string         `json:"schema_version"`
	ID                       string         `json:"id"`
	GeneratedAt              string         `json:"generated_at"`
	Status                   string         `json:"status"`
	Mode                     string         `json:"mode"`
	ProposedBy               string         `json:"proposed_by"`
	ProposalCount            int            `json:"proposal_count"`
	SuppressedDuplicateCount int            `json:"suppressed_duplicate_count"`
	SkippedCount             int            `json:"skipped_count"`
	SkipHistogram            map[string]int `json:"skip_histogram,omitempty"`
	RunWorkID                string         `json:"run_work_id,omitempty"`
	RunReceiptID             string         `json:"run_receipt_id,omitempty"`
	RunTransitionID          string         `json:"run_transition_id,omitempty"`
	RunMoveType              string         `json:"run_move_type,omitempty"`
	NoDelta                  bool           `json:"no_delta,omitempty"`
	ResultPath               string         `json:"result_path,omitempty"`
	// GrantRef cites the active broad director-apply grant this run resolved.
	// That grant authorizes filing suggestions only; class-scoped autonomy uses
	// the separate grant sealed into each eligibility artifact.
	GrantRef             string                `json:"grant_ref,omitempty"`
	Proposals            []Proposal            `json:"proposals,omitempty"`
	SuppressedDuplicates []SuppressedDuplicate `json:"suppressed_duplicates,omitempty"`
	Skipped              []SkippedCandidate    `json:"skipped,omitempty"`
	Warnings             []string              `json:"warnings,omitempty"`
}

type Proposal struct {
	ID              string                            `json:"id"`
	TriggerRef      string                            `json:"trigger_ref"`
	Kind            string                            `json:"kind"`
	Title           string                            `json:"title"`
	Consequence     string                            `json:"consequence"`
	CapabilityClass string                            `json:"capability_class"`
	TargetSurface   string                            `json:"target_surface,omitempty"`
	EvidenceRefs    []string                          `json:"evidence_refs"`
	WorkID          string                            `json:"work_id"`
	WorkState       string                            `json:"work_state"`
	PlanPath        string                            `json:"plan_path,omitempty"`
	RoutePlan       routeevidence.CapabilityRoutePlan `json:"route_plan"`
	WorkPlan        work.CreatePlan                   `json:"work_plan,omitempty"`
	// AutoAdmit records the typed request used at the sole admission seam.
	// It is present only for a job-health apply attempt with a current
	// class-scoped autonomy grant; dry runs and broad filing grants cannot
	// construct it. AutoAdmitResult records whether the revalidated evidence
	// threshold actually admitted the proposal.
	AutoAdmit             *work.AutoAdmitInput  `json:"auto_admit,omitempty"`
	EligibilityRef        string                `json:"eligibility_ref,omitempty"`
	AdmissionID           string                `json:"admission_id,omitempty"`
	AutoAdmitResult       *work.AutoAdmitResult `json:"auto_admit_result,omitempty"`
	AutoAdmitError        string                `json:"auto_admit_error,omitempty"`
	expectedWork          envelope.WorkEnvelope
	expectedActionContext *work.WorkActionContext
}

type SuppressedDuplicate struct {
	TriggerRef string `json:"trigger_ref"`
	// Slice 3 (product-loop-momentum 2026-09-08): CandidateSourceID is the
	// actual source id (e.g. loop.ProductID, card.ID, plan.ID, reviewSourceID)
	// the dedup candidate came from. TriggerRef is a digest derived from
	// kind+source_id+evidence_refs; consumers that need to correlate the
	// suppressed duplicate with the actual source use CandidateSourceID
	// instead of parsing TriggerRef. The trigger digest is still the lookup
	// key — only the consumer-facing surface changed.
	CandidateSourceID string   `json:"candidate_source_id,omitempty"`
	Kind              string   `json:"kind"`
	Title             string   `json:"title"`
	Reason            string   `json:"reason"`
	EvidenceRefs      []string `json:"evidence_refs"`
	ExistingWorkIDs   []string `json:"existing_work_ids,omitempty"`
	ExistingPlanIDs   []string `json:"existing_plan_ids,omitempty"`
}

type SkippedCandidate struct {
	Class        string   `json:"class,omitempty"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title,omitempty"`
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type candidate struct {
	Kind             string
	SourceID         string
	Title            string
	Consequence      string
	CapabilityClass  string
	Owner            string
	CanonicalSurface string
	// TargetSurface, when non-empty, marks this candidate as a harness-edit
	// proposal (EVOLVE-BLOCK): the named editable surface the proposed edit
	// aims at. It is threaded into the WorkEnvelope so the admission seam gates
	// it against the operator-owned editable-surfaces whitelist. It is NEVER a
	// hard-deny-core surface — newHarnessEditCandidate refuses those at
	// construction, before this candidate ever exists.
	TargetSurface string
	// EditAction / EditSignal are the plain-language phrasing a harness-edit
	// candidate carries for its operator review card ("trim the never-fired
	// example helper offer" / "12 receipts show it never fired"). Empty for
	// every non-harness candidate; a harness-edit card writer that finds them
	// empty fails its card rather than emitting mush (grammar G6).
	EditAction         string
	EditSignal         string
	SuccessPredicate   string
	DesiredOutcome     string
	Command            []string
	RequiredInputs     []string
	ExpectedValidation []string
	StopConditions     []string
	Boundary           string
	Proof              string
	EvidenceRefs       []string
	CreatedAt          string
}

type duplicateIndex struct {
	openWorkByTrigger map[string][]string
	plansByTrigger    map[string][]string
}

func Propose(homePath string, opts Options) (Report, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return Report{}, fmt.Errorf("COMPOUND_HOME required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	mode := "dry_run"
	if opts.Apply {
		mode = "apply"
	}
	report := Report{
		SchemaVersion: ReportSchemaVersion,
		ID:            idFor("dirrun_", mode, timestamp(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "ok",
		Mode:          mode,
		ProposedBy:    DirectorActor,
	}
	// The broad director-apply grant authorizes filing create-plan previews and
	// proposed work. It is deliberately not autonomy authority: job-health
	// admission below independently resolves a typed class grant and replays
	// its evidence at AdmitAuto. Dry runs require neither grant.
	var grant resolvedApplyGrant
	if opts.Apply {
		var grantErr error
		grant, grantErr = resolveApplyGrant(homePath, now)
		if grantErr != nil {
			return report, grantErr
		}
		report.GrantRef = grant.GrantRef
	}
	store, err := work.Open(homePath)
	if err != nil {
		return report, err
	}
	dupes, warnings := loadDuplicateIndex(homePath, store)
	report.Warnings = append(report.Warnings, warnings...)
	candidates, warnings := scanCandidates(homePath, now)
	report.Warnings = append(report.Warnings, warnings...)
	// W5 J9-R3G apply-mode side: every product_hypothesis_draft candidate we
	// just discovered is a parked draft with a placeholder expected_effect.
	// Minting the file is review-gated by construction (verdict=park,
	// BuildHypothesisPlan refuses to lock causality). Non-placeholder
	// candidates, the only-write branch below, and the signal/jsonl ledger
	// are all off-limits here.
	if opts.Apply {
		if warnings := mintDraftCandidatesForApply(homePath, candidates, now); len(warnings) > 0 {
			report.Warnings = append(report.Warnings, warnings...)
		}
	}
	// Supersession: a product_hypothesis_draft for a given
	// (productID, changeEvent) pair is the SAME group information as the
	// matching product_signal_without_hypothesis card, one step further
	// along ("review these signals" vs "here is a parked hypothesis, fill
	// in the placeholder"). Proposing both is redundant AND the older review
	// card starves the newer draft under the round-robin cap (review SourceIDs
	// sort alphabetically before draft SourceIDs, and round-robin walks in
	// CreatedAt order). Compute the structural supersession set ONCE before
	// the proposal loop: for every draft candidate, every action_card id in its
	// EvidenceRefs becomes a "this review is already handled by a draft" key.
	// The matching product_signal_without_hypothesis candidate has the SAME
	// action_card id in its own EvidenceRefs and as its SourceID, so the lookup
	// is by SourceID alone — no title heuristics.
	supersededReviewIDs := map[string]bool{}
	for _, cand := range candidates {
		if cand.Kind != "product_hypothesis_draft" {
			continue
		}
		for _, ref := range cand.EvidenceRefs {
			if id, ok := strings.CutPrefix(ref, "product_action_card:"); ok && strings.TrimSpace(id) != "" {
				supersededReviewIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	for _, cand := range fairCandidateOrder(candidates) {
		// Supersession is checked BEFORE the cap so a deterministic starvation
		// never masquerades as cap_reached: a review that matches a draft
		// must record the truthful reason (superseded_by_draft), not "we ran
		// out of slots before reaching the draft".
		if cand.Kind == "product_signal_without_hypothesis" && supersededReviewIDs[strings.TrimSpace(cand.SourceID)] {
			addSkipped(&report, SkippedCandidate{
				Class:        candidateClass(cand),
				Kind:         cand.Kind,
				Title:        cand.Title,
				Reason:       "superseded_by_draft",
				EvidenceRefs: compactRefs(cand.EvidenceRefs),
			})
			continue
		}
		if len(report.Proposals) >= maxProposalsPerRun {
			addSkipped(&report, SkippedCandidate{
				Class:        candidateClass(cand),
				Kind:         cand.Kind,
				Title:        cand.Title,
				Reason:       "cap_reached",
				EvidenceRefs: compactRefs(cand.EvidenceRefs),
			})
			continue
		}
		proposal, skipped := buildProposal(homePath, store, cand, now)
		if skipped != nil {
			if skipped.Class == "" {
				skipped.Class = candidateClass(cand)
			}
			addSkipped(&report, *skipped)
			continue
		}
		if dup := dupes.find(proposal.TriggerRef); dup != nil {
			dup.Kind = proposal.Kind
			dup.Title = proposal.Title
			dup.CandidateSourceID = cand.SourceID
			dup.EvidenceRefs = append([]string{}, proposal.EvidenceRefs...)
			if opts.Apply && proposal.Kind == "job_health" {
				reconsidered, err := reconsiderJobHealthEvidenceAutonomy(homePath, store, &proposal, *dup, now)
				if err != nil {
					return report, err
				}
				if reconsidered {
					report.Proposals = append(report.Proposals, proposal)
					continue
				}
				if proposal.AutoAdmitError != "" {
					report.Warnings = append(report.Warnings, proposal.AutoAdmitError)
				}
			}
			report.SuppressedDuplicates = append(report.SuppressedDuplicates, *dup)
			continue
		}
		if opts.Apply {
			plan, err := store.CreatePlan(work.CreateInput{
				WorkID:           proposal.WorkID,
				Owner:            proposalOwner(cand),
				CanonicalSurface: cand.CanonicalSurface,
				TargetSurface:    cand.TargetSurface,
				ProposalKind:     proposalKindFor(cand.Kind),
				SuccessPredicate: cand.SuccessPredicate,
				DesiredOutcome:   cand.DesiredOutcome,
				NonGoals:         nonGoalsFor(cand),
				RouteDecisionID:  proposal.RoutePlan.ID,
				State:            "proposed",
				Risk:             "tier1",
				Privacy:          "private",
			}, now, work.CreatePlanOptions{
				Actor:         DirectorActor,
				Capability:    "director.propose",
				Action:        "propose_work",
				TargetKind:    "director_proposal",
				TargetID:      proposal.TriggerRef,
				MutationClass: "local_reversible",
				Reversibility: "easy",
				BlastRadius:   "local",
				ActionContext: proposal.expectedActionContext,
				Record:        true,
			})
			if err != nil {
				return report, err
			}
			proposal.PlanPath = plan.PlanPath
			proposal.WorkPlan = plan
			// A harness-edit proposal also emits an operator review card under
			// reviews/harness-edit-proposals/ — a propose-only artifact that
			// mutates no owner state and admits nothing; it just makes the
			// bounded edit legible for the operator gate. Kept apply-only so a
			// dry run stays read-only.
			if cand.TargetSurface != "" {
				if err := recordHarnessEditCard(homePath, cand, proposal, now); err != nil {
					return report, err
				}
			}
			if proposal.Kind == "job_health" {
				if err := attemptJobHealthEvidenceAutonomy(homePath, store, &proposal, plan, now); err != nil {
					return report, err
				}
			}
		}
		report.Proposals = append(report.Proposals, proposal)
		dupes.addPlan(proposal.TriggerRef, proposal.ID)
	}
	report.ProposalCount = len(report.Proposals)
	report.SuppressedDuplicateCount = len(report.SuppressedDuplicates)
	report.SkippedCount = len(report.Skipped)
	if hits, misses := routeevidence.PlanCapabilityRouteCacheStats(); hits+misses > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("route_plan_cache: hits=%d misses=%d (one cold call per (home, capability_class, latency_class, bucket) per process)", hits, misses))
	}
	if opts.Apply {
		path, err := recordReport(homePath, report)
		if err != nil {
			return report, err
		}
		report.ResultPath = path
		if err := recordApplyRunEnvelope(store, &report, now); err != nil {
			return report, err
		}
		if err := rewriteReport(path, report); err != nil {
			return report, err
		}
	}
	return report, nil
}

func attemptJobHealthEvidenceAutonomy(homePath string, store *work.Store, proposal *Proposal, plan work.CreatePlan, now time.Time) error {
	admission, err := store.AdmitCreatePlanPath(plan.PlanPath, now, work.AdmitCreatePlanOptions{Actor: DirectorActor, Note: "director_job_health_proposal"})
	if err != nil {
		return err
	}
	proposal.AdmissionID = admission.ID
	proposal.WorkState = admission.WorkState
	return evaluateJobHealthEvidenceAutonomy(homePath, store, proposal, now)
}

func reconsiderJobHealthEvidenceAutonomy(homePath string, store *work.Store, proposal *Proposal, duplicate SuppressedDuplicate, now time.Time) (bool, error) {
	if len(duplicate.ExistingWorkIDs) != 1 || duplicate.ExistingWorkIDs[0] != proposal.WorkID {
		return false, nil
	}
	existing, err := store.Get(proposal.WorkID)
	if err != nil {
		return false, err
	}
	if existing.State != "proposed" {
		return false, nil
	}
	if !sameDirectorProposalWork(existing, proposal.expectedWork) {
		proposal.AutoAdmitError = "existing job_health proposal does not match current Director semantics; refusing autonomous reconsideration"
		return false, nil
	}
	if err := store.ValidateActionContextSnapshotForWork(existing, proposal.expectedActionContext); err != nil {
		proposal.AutoAdmitError = "existing job_health proposal action context does not match current Director semantics; refusing autonomous reconsideration: " + err.Error()
		return false, nil
	}
	proposal.WorkState = existing.State
	if err := evaluateJobHealthEvidenceAutonomy(homePath, store, proposal, now); err != nil {
		return true, err
	}
	return true, nil
}

func sameDirectorProposalWork(existing, expected envelope.WorkEnvelope) bool {
	return existing.ID == expected.ID &&
		existing.Owner == expected.Owner &&
		existing.CanonicalSurface == expected.CanonicalSurface &&
		existing.TargetSurface == expected.TargetSurface &&
		existing.ProposalKind == expected.ProposalKind &&
		existing.State == expected.State &&
		existing.DesiredOutcome == expected.DesiredOutcome &&
		existing.SuccessPredicate == expected.SuccessPredicate &&
		existing.ProofMedium == expected.ProofMedium &&
		existing.RouteDecisionID == expected.RouteDecisionID &&
		existing.Privacy == expected.Privacy &&
		existing.Risk == expected.Risk &&
		existing.Scope == expected.Scope &&
		existing.Authority == expected.Authority &&
		existing.Uncertainty == expected.Uncertainty &&
		slices.Equal(existing.NonGoals, expected.NonGoals) &&
		slices.Equal(existing.EvidenceRefs, expected.EvidenceRefs)
}

func evaluateJobHealthEvidenceAutonomy(homePath string, store *work.Store, proposal *Proposal, now time.Time) error {

	grant, ok, err := resolveAutonomyGrant(homePath, "job_health")
	if err != nil {
		proposal.AutoAdmitError = err.Error()
		return nil
	}
	if !ok {
		proposal.AutoAdmitError = "no active class-scoped autonomy grant for job_health"
		return nil
	}
	eligibility, err := autonomyproof.Compile(homePath, grant.Proof, autonomyRoutePlanSnapshot(proposal.RoutePlan), now)
	if err != nil {
		return err
	}
	if _, err := autonomyproof.Record(homePath, eligibility); err != nil {
		return err
	}
	proposal.EligibilityRef = eligibility.Ref()
	input := work.AutoAdmitInput{By: DirectorActor, EligibilityRef: eligibility.Ref()}
	proposal.AutoAdmit = &input
	result, admitErr := store.AdmitAuto(proposal.WorkID, input, now)
	proposal.AutoAdmitResult = &result
	proposal.WorkState = result.WorkState
	if admitErr != nil {
		proposal.AutoAdmitError = admitErr.Error()
	}
	return nil
}

func autonomyRoutePlanSnapshot(plan routeevidence.CapabilityRoutePlan) autonomyproof.RoutePlanSnapshot {
	out := autonomyproof.RoutePlanSnapshot{
		ID:                plan.ID,
		AuthorityPath:     plan.AuthorityPath,
		SourceAuthorityID: plan.SourceAuthorityID,
		SourceRevision:    plan.SourceRevision,
		CapabilityClass:   plan.CapabilityClass,
		PrivacySensitive:  plan.PrivacySensitive,
		Chain:             []autonomyproof.LaneSnapshot{},
	}
	for _, lane := range plan.Chain {
		resolution := autonomyproof.LaneResolution{Mode: lane.Resolution.Mode}
		for _, target := range lane.Resolution.Targets {
			resolution.Targets = append(resolution.Targets, autonomyproof.ResolvedTarget{Provider: target.Provider, Model: target.Model})
		}
		out.Chain = append(out.Chain, autonomyproof.LaneSnapshot{
			ModelID:           lane.ModelID,
			ProviderRoute:     lane.ProviderRoute,
			ProviderRouteKind: lane.ProviderRouteKind,
			Resolution:        resolution,
			Tier:              lane.Tier,
			ClaimLevel:        lane.ClaimLevel,
			GateRole:          lane.GateRole,
		})
	}
	return out
}

func scanCandidates(homePath string, now time.Time) ([]candidate, []string) {
	var out []candidate
	var warnings []string
	if items, err := productLoopCandidates(homePath, now); err != nil {
		warnings = append(warnings, "product loop scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := synthesisContradictionCandidates(homePath, now); err != nil {
		warnings = append(warnings, "synthesis contradiction scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := reviewDebtCandidates(homePath, now); err != nil {
		warnings = append(warnings, "review debt scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := decisionPreparationCandidates(homePath, now); err != nil {
		warnings = append(warnings, "decision preparation scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := sourceConnectionCandidates(homePath, now); err != nil {
		warnings = append(warnings, "source connection scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := jobHealthCandidates(homePath, now); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warnings = append(warnings, "job health scan skipped: "+err.Error())
		}
	} else {
		out = append(out, items...)
	}
	if items, err := absorptionDebtCandidates(homePath, now); err != nil {
		warnings = append(warnings, "absorption debt scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	if items, err := harnessEditCandidates(homePath, now); err != nil {
		warnings = append(warnings, "harness edit scan skipped: "+err.Error())
	} else {
		out = append(out, items...)
	}
	return out, warnings
}

// mintDraftCandidatesForApply mints one inbox/product-hypotheses/<id>.json
// per product_hypothesis_draft candidate the scanner found. It is the ONLY
// apply-side write outside the suggestion-only allowlist; the candidate is
// verdict=park with a placeholder expected_effect, so it never promotes
// memory, never touches a ProductSignal, and never invents a metric value.
func mintDraftCandidatesForApply(homePath string, candidates []candidate, now time.Time) []string {
	var warnings []string
	for _, cand := range candidates {
		if cand.Kind != "product_hypothesis_draft" {
			continue
		}
		productID, changeEvent, metrics, signalIDs := candidateDraftInputs(cand)
		if productID == "" || changeEvent == "" {
			continue
		}
		if _, err := product.MintHypothesisDraftCandidate(homePath, productID, changeEvent, metrics, signalIDs, cand.EvidenceRefs, now); err != nil {
			warnings = append(warnings, "product_hypothesis_draft mint skipped: "+err.Error())
		}
	}
	return warnings
}

// candidateDraftInputs recovers (productID, changeEvent, metrics, signalIDs)
// from the RequiredInputs lines productHypothesisDraftCandidate authored. The
// apply-side mints need the SAME inputs a dry-run proposal surfaces so the
// deterministic candidate id matches regardless of mode.
func candidateDraftInputs(cand candidate) (productID, changeEvent string, metrics, signalIDs []string) {
	for _, line := range cand.RequiredInputs {
		switch {
		case strings.HasPrefix(line, "product_id: "):
			productID = strings.TrimSpace(strings.TrimPrefix(line, "product_id: "))
		case strings.HasPrefix(line, "change_event: "):
			changeEvent = strings.TrimSpace(strings.TrimPrefix(line, "change_event: "))
		case strings.HasPrefix(line, "metrics: "):
			metrics = strings.Split(strings.TrimPrefix(line, "metrics: "), ",")
		case strings.HasPrefix(line, "signal ids: "):
			signalIDs = strings.Split(strings.TrimPrefix(line, "signal ids: "), ",")
		}
	}
	return
}

func fairCandidateOrder(candidates []candidate) []candidate {
	buckets := map[string][]candidate{}
	unknownClasses := map[string]bool{}
	for _, cand := range candidates {
		class := candidateClass(cand)
		buckets[class] = append(buckets[class], cand)
		if !knownProposalClass(class) {
			unknownClasses[class] = true
		}
	}
	for class := range buckets {
		sort.SliceStable(buckets[class], func(i, j int) bool {
			return candidateLessWithinClass(buckets[class][i], buckets[class][j])
		})
	}
	classes := append([]string{}, proposalClassSeverityOrder...)
	for class := range unknownClasses {
		classes = append(classes, class)
	}
	sort.SliceStable(classes[len(proposalClassSeverityOrder):], func(i, j int) bool {
		return classes[len(proposalClassSeverityOrder)+i] < classes[len(proposalClassSeverityOrder)+j]
	})

	out := make([]candidate, 0, len(candidates))
	for len(out) < len(candidates) {
		added := false
		for _, class := range classes {
			if len(buckets[class]) == 0 {
				continue
			}
			out = append(out, buckets[class][0])
			buckets[class] = buckets[class][1:]
			added = true
		}
		if !added {
			break
		}
	}
	return out
}

func candidateClass(cand candidate) string {
	switch cand.Kind {
	case "product_signal_without_hypothesis", "product_action_card", "product_hypothesis_draft":
		return "product_loop"
	default:
		return cand.Kind
	}
}

func knownProposalClass(class string) bool {
	for _, known := range proposalClassSeverityOrder {
		if class == known {
			return true
		}
	}
	return false
}

func candidateLessWithinClass(a, b candidate) bool {
	aCreated := strings.TrimSpace(a.CreatedAt)
	bCreated := strings.TrimSpace(b.CreatedAt)
	if aCreated != bCreated {
		if aCreated == "" {
			return false
		}
		if bCreated == "" {
			return true
		}
		return aCreated < bCreated
	}
	if a.SourceID != b.SourceID {
		return a.SourceID < b.SourceID
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return strings.Join(compactRefs(a.EvidenceRefs), "|") < strings.Join(compactRefs(b.EvidenceRefs), "|")
}

func addSkipped(report *Report, skipped SkippedCandidate) {
	report.Skipped = append(report.Skipped, skipped)
	class := strings.TrimSpace(skipped.Class)
	if class == "" {
		class = skipped.Kind
	}
	if report.SkipHistogram == nil {
		report.SkipHistogram = map[string]int{}
	}
	report.SkipHistogram[class]++
}

func buildProposal(homePath string, store *work.Store, cand candidate, now time.Time) (Proposal, *SkippedCandidate) {
	refs := compactRefs(cand.EvidenceRefs)
	if len(refs) == 0 {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "missing_evidence_refs"}
	}
	if strings.TrimSpace(cand.CapabilityClass) == "" {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "missing_capability_class", EvidenceRefs: refs}
	}
	routePlan, err := routeevidence.PlanCapabilityRoute(homePath, cand.CapabilityClass, routeevidence.CapabilityRoutePlanOptions{}, now)
	if err != nil {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "route_plan_unavailable: " + err.Error(), EvidenceRefs: refs}
	}
	if len(routePlan.Chain) == 0 {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "route_plan_blocked", EvidenceRefs: refs}
	}
	triggerRef := triggerRefFor(cand)
	refs = reviewartifact.UniqueSorted(append(refs, "proposed_by:director", triggerRef, "route_plan:"+routePlan.ID))
	workID := idFor("work_", triggerRef)
	workEnvelope := work.BuildWork(work.CreateInput{
		WorkID:           workID,
		Owner:            proposalOwner(cand),
		Actor:            DirectorActor,
		CanonicalSurface: cand.CanonicalSurface,
		TargetSurface:    cand.TargetSurface,
		ProposalKind:     proposalKindFor(cand.Kind),
		SuccessPredicate: cand.SuccessPredicate,
		DesiredOutcome:   cand.DesiredOutcome,
		NonGoals:         nonGoalsFor(cand),
		EvidenceRefs:     refs,
		RouteDecisionID:  routePlan.ID,
		State:            "proposed",
		Risk:             "tier1",
		Privacy:          "private",
	}, now)
	if err := workEnvelope.Validate(); err != nil {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "work_invalid: " + err.Error(), EvidenceRefs: refs}
	}
	if store == nil {
		return Proposal{}, &SkippedCandidate{Kind: cand.Kind, Title: cand.Title, Reason: "work_store_unavailable"}
	}
	proposal := Proposal{
		ID:              idFor("dirprop_", triggerRef),
		TriggerRef:      triggerRef,
		Kind:            cand.Kind,
		Title:           cand.Title,
		Consequence:     cand.Consequence,
		CapabilityClass: cand.CapabilityClass,
		TargetSurface:   cand.TargetSurface,
		EvidenceRefs:    refs,
		WorkID:          workID,
		WorkState:       "proposed",
		RoutePlan:       routePlan,
		expectedWork:    workEnvelope,
	}
	proposal.expectedActionContext = actionContextFor(cand, proposal)
	return proposal, nil
}

func productLoopCandidates(homePath string, now time.Time) ([]candidate, error) {
	model, err := product.BuildLoopModel(homePath, product.LoopOptions{Now: now})
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, loop := range model.Products {
		seen := map[string]bool{}
		for _, group := range loop.SignalsWithoutHypothesis {
			if group.Consumed {
				continue
			}
			for _, card := range group.ActionCards {
				seen[card.ID] = true
				out = append(out, productCardCandidate(loop, card, "product_signal_without_hypothesis", productSignalGapEvidence(loop, card, group)))
			}
		}
		for _, card := range loop.ActionCards {
			if seen[card.ID] {
				continue
			}
			out = append(out, productCardCandidate(loop, card, "product_action_card", productActionCardEvidence(loop, card)))
		}
		// W5 J9-R3G: emit one proposal per starved product that says
		// "draft a hypothesis candidate from this signal group" instead of
		// only "review the signals". The existing product_signal_without_hypothesis
		// candidates above keep telling the operator to run action-plan; the new
		// kind closes the unattended loop by minting a product_hypothesis
		// candidate with verdict=park and a placeholder expected_effect the
		// human must replace before review.
		if draft := largestStarvedSignalGroup(loop); draft != nil {
			out = append(out, productHypothesisDraftCandidate(loop, *draft))
		}
	}
	return out, nil
}

// largestStarvedSignalGroup returns the unconsumed signals_without_hypothesis
// group with the most signals (>= 3) for this product, or nil when none
// qualifies. The director emits AT MOST one product_hypothesis_draft per
// starved product — the most-signal-rich group — so the loop gap shrinks
// without flooding the operator with N parallel drafts on the same change.
func largestStarvedSignalGroup(loop product.ProductLoop) *product.LoopSignalHypothesisGap {
	var best *product.LoopSignalHypothesisGap
	for i := range loop.SignalsWithoutHypothesis {
		group := &loop.SignalsWithoutHypothesis[i]
		if group.Consumed {
			continue
		}
		if len(group.SignalIDs) < 3 {
			continue
		}
		if best == nil || len(group.SignalIDs) > len(best.SignalIDs) {
			best = group
		}
	}
	return best
}

// productHypothesisDraftCandidate builds the candidate a starved-product draft
// promotes. The candidate id is the SAME deterministic digest the writer
// (`product.MintHypothesisDraftCandidate`) uses, so dry-run proposals and
// apply-mode mints always agree on `compound product hypothesis-plan
// --candidate <id>` and the proposal itself is the proof surface.
func productHypothesisDraftCandidate(loop product.ProductLoop, group product.LoopSignalHypothesisGap) candidate {
	id := hypothesisDraftIDFor(loop.ProductID, group.ChangeEvent)
	consequence := "Drafting a parked hypothesis from these signals closes the unattended signal→candidate gap; operator must replace the placeholder expected_effect before review."
	title := "Draft hypothesis for " + group.ChangeEvent + " on " + loop.ProductID
	metricLine := ""
	if len(group.Metrics) > 0 {
		metricLine = "metrics: " + strings.Join(group.Metrics, ",")
	}
	signalLine := ""
	if len(group.SignalIDs) > 0 {
		signalLine = "signal ids: " + strings.Join(group.SignalIDs, ",")
	}
	changeLine := "change_event: " + group.ChangeEvent
	// RequiredInputs double as the round-trip surface the apply-mode mints
	// read back (productID, changeEvent, metrics, signalIDs) — see
	// candidateFromSourceProductID / candidateMetrics / candidateSignalIDs.
	required := []string{
		"product_id: " + loop.ProductID,
		changeLine,
	}
	if metricLine != "" {
		required = append(required, metricLine)
	}
	if signalLine != "" {
		required = append(required, signalLine)
	}
	required = append(required, "operator-supplied expected_effect (the draft's placeholder must be replaced before review)")
	refs := []string{
		"product_loop:" + loop.ProductID,
		"product_signal_change_event:" + slug(group.ChangeEvent),
	}
	for _, id := range group.SignalIDs {
		refs = append(refs, "product_signal:"+id)
	}
	for _, card := range group.ActionCards {
		refs = append(refs, "product_action_card:"+card.ID)
	}
	refs = compactRefs(refs)
	return candidate{
		Kind:             "product_hypothesis_draft",
		SourceID:         loop.ProductID,
		Title:            title,
		Consequence:      consequence,
		CapabilityClass:  productCardCapabilityClass(product.LoopActionCard{Kind: "product_hypothesis_draft"}),
		Owner:            loop.ProductID,
		CanonicalSurface: "director:product-hypothesis-draft:" + loop.ProductID + ":" + id,
		SuccessPredicate: "Draft appears at inbox/product-hypotheses/" + id + ".json with verdict=park and the operator-supplied expected_effect; loop model no longer lists the group's change_event under signals_without_hypothesis.",
		DesiredOutcome:   consequence,
		Command:          []string{"compound", "product", "hypothesis-plan", "--candidate", id},
		RequiredInputs:   required,
		ExpectedValidation: []string{
			"`compound product hypothesis-plan --candidate " + id + "` produces a HypothesisPlan under reviews/product-hypotheses/",
			"future `compound product loops --product " + loop.ProductID + " --json` no longer lists this change_event under signals_without_hypothesis",
		},
		StopConditions: []string{
			"no metric evidence can be attached without inventing numbers",
			"expected_effect would have to be invented (operator must supply it)",
		},
		Boundary:     "drafts a hypothesis candidate only; never mutates ProductSignal, never publishes, never infers causality",
		Proof:        "inbox/product-hypotheses/" + id + ".json exists AND " + "compound product loops --product " + loop.ProductID + " --json no longer lists this change_event under signals_without_hypothesis",
		EvidenceRefs: refs,
	}
}

// hypothesisDraftIDFor mirrors product.hypothesisDraftCandidateID so dry-run
// proposals and apply-mode mints always reference the SAME canonical id. The
// digest is a pure function of (productID, changeEvent); both sides must use
// it for the operator's subsequent `compound product hypothesis-plan
// --candidate <id>` to land on the just-minted draft.
func hypothesisDraftIDFor(productID, changeEvent string) string {
	productID = strings.TrimSpace(productID)
	changeEvent = strings.TrimSpace(changeEvent)
	sum := sha256.Sum256([]byte("hypdraft|" + productID + "|" + changeEvent))
	return "hyp_draft_" + hex.EncodeToString(sum[:])[:12]
}

func productCardCandidate(loop product.ProductLoop, card product.LoopActionCard, kind string, refs []string) candidate {
	summary := firstNonEmpty(card.ActionSummary, card.Title)
	consequence := "Adding this creates bounded product-loop work for " + summary
	return candidate{
		Kind:               kind,
		SourceID:           card.ID,
		Title:              card.Title,
		Consequence:        consequence,
		CapabilityClass:    productCardCapabilityClass(card),
		Owner:              loop.ProductID,
		CanonicalSurface:   "director:product-loop:" + loop.ProductID + ":" + card.ID,
		SuccessPredicate:   fmt.Sprintf("Resolve product loop action card %s for %s; proof: %s", card.ID, loop.ProductID, card.Proof),
		DesiredOutcome:     consequence,
		Command:            append([]string{}, card.Command...),
		RequiredInputs:     append([]string{}, card.RequiredInputs...),
		ExpectedValidation: append([]string{}, card.ExpectedValidation...),
		StopConditions:     append([]string{}, card.StopConditions...),
		Boundary:           card.Boundary,
		Proof:              card.Proof,
		EvidenceRefs:       refs,
	}
}

func productCardCapabilityClass(card product.LoopActionCard) string {
	kind := strings.ToLower(card.Kind)
	if strings.Contains(kind, "review") || strings.Contains(kind, "hypothesis") {
		return "judge-critique"
	}
	return "classify-extract"
}

func productActionCardEvidence(loop product.ProductLoop, card product.LoopActionCard) []string {
	refs := []string{"product_loop:" + loop.ProductID, "product_action_card:" + card.ID}
	for _, group := range loop.OpenHypothesesByMissingSignalSource {
		if !cardGroupHasID(group.ActionCards, card.ID) {
			continue
		}
		for _, id := range group.HypothesisIDs {
			refs = append(refs, "product_hypothesis:"+id)
		}
		if group.Source != "" {
			refs = append(refs, "product_signal_source:"+group.Source)
		}
	}
	for _, group := range loop.ClosedReviewsByNextAction {
		if !cardGroupHasID(group.ActionCards, card.ID) {
			continue
		}
		for _, id := range group.ReviewIDs {
			refs = append(refs, "product_review:"+id)
		}
		for _, id := range group.HypothesisIDs {
			refs = append(refs, "product_hypothesis:"+id)
		}
		for _, id := range group.SignalIDs {
			refs = append(refs, "product_signal:"+id)
		}
	}
	for _, group := range loop.SignalsWithoutHypothesis {
		refs = append(refs, productSignalGapEvidence(loop, card, group)...)
	}
	return reviewartifact.UniqueSorted(refs)
}

func productSignalGapEvidence(loop product.ProductLoop, card product.LoopActionCard, group product.LoopSignalHypothesisGap) []string {
	if !cardGroupHasID(group.ActionCards, card.ID) {
		return nil
	}
	refs := []string{"product_loop:" + loop.ProductID, "product_action_card:" + card.ID}
	for _, id := range group.SignalIDs {
		refs = append(refs, "product_signal:"+id)
	}
	if group.ChangeEvent != "" {
		refs = append(refs, "product_signal_change_event:"+slug(group.ChangeEvent))
	}
	return reviewartifact.UniqueSorted(refs)
}

func cardGroupHasID(cards []product.LoopActionCard, id string) bool {
	for _, card := range cards {
		if card.ID == id {
			return true
		}
	}
	return false
}

func synthesisContradictionCandidates(homePath string, now time.Time) ([]candidate, error) {
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: "synthesis-contradictions"}, now)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, item := range queue.Items {
		out = append(out, candidate{
			Kind:             "synthesis_contradiction",
			SourceID:         reviewSourceID(item),
			Title:            "Review synthesis contradiction " + item.ID,
			Consequence:      "Adding this creates focused synthesis contradiction review work for " + item.ID,
			CapabilityClass:  "judge-critique",
			Owner:            "memory-synthesis",
			CanonicalSurface: "director:synthesis-contradiction:" + item.ID,
			SuccessPredicate: "Review synthesis contradiction " + item.ID + " without bulk applying or superseding memory",
			DesiredOutcome:   "Adding this creates focused synthesis contradiction review work for " + item.ID,
			Command:          []string{"compound", "reviews", "--area", "synthesis-contradictions", "--json"},
			RequiredInputs:   []string{"current synthesis contradiction artifact", "claim/candidate evidence named in evidence_refs"},
			ExpectedValidation: []string{
				"review outcome cites the contradiction evidence refs",
				"no memory claim is promoted, rejected, or superseded without a later reviewed work receipt",
			},
			StopConditions: []string{"contradiction artifact is stale, superseded, or lacks claim/candidate evidence"},
			Boundary:       "review-only synthesis handling; no bulk apply, auto-adjudication, memory promotion, rejection, or supersession",
			Proof:          "review artifact or follow-up WorkEnvelope cites the contradiction evidence refs",
			EvidenceRefs:   reviewEvidenceRefs(item),
			CreatedAt:      item.CreatedAt,
		})
	}
	return out, nil
}

func reviewDebtCandidates(homePath string, now time.Time) ([]candidate, error) {
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{}, now)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, item := range queue.Items {
		if item.Area == "synthesis-contradictions" || item.Area == "job-health" || reviewStatusTerminal(item.Status) {
			continue
		}
		area := firstNonEmpty(item.Area, "reviews")
		title := "Resolve review debt " + area + "/" + item.ID
		out = append(out, candidate{
			Kind:             "review_debt",
			SourceID:         reviewSourceID(item),
			Title:            title,
			Consequence:      "Adding this creates review-debt resolution work for " + area + "/" + item.ID,
			CapabilityClass:  "judge-critique",
			Owner:            "compound-reviews",
			CanonicalSurface: "director:review-debt:" + area + ":" + item.ID,
			SuccessPredicate: "Resolve review artifact " + area + "/" + item.ID + " through its owner command or an explicit parked decision",
			DesiredOutcome:   "Adding this creates review-debt resolution work for " + area + "/" + item.ID,
			Command:          []string{"compound", "reviews", "--area", area, "--json"},
			RequiredInputs:   []string{"current review artifact " + area + "/" + item.ID, "reviewer decision or owner command named by the review surface"},
			ExpectedValidation: []string{
				"`compound reviews --area " + area + " --json` shows the item recorded, resolved, parked, or still explicitly blocked",
			},
			StopConditions: []string{"review artifact lacks enough evidence for a bounded decision", "owner command would mutate public, financial, credential, destructive, or security-sensitive state without policy"},
			Boundary:       "review-debt work only; no owner mutation except through the review artifact's explicit command and policy path",
			Proof:          "review queue no longer reports the same item as unresolved, or a follow-up receipt explains why it remains blocked",
			EvidenceRefs:   reviewEvidenceRefs(item),
			CreatedAt:      item.CreatedAt,
		})
	}
	return out, nil
}

func sourceConnectionCandidates(homePath string, now time.Time) ([]candidate, error) {
	projection, err := observation.LoadSourceConnectionsProjection(homePath, now)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, conn := range projection.Connections {
		for _, card := range conn.LatestCards {
			refs := []string{
				"source_connection:" + conn.ID,
				"source_intake_card:" + card.ID,
				"source_intake_row:" + card.RowID,
				"source_observation:" + card.ObservationID,
			}
			if conn.SourceRef != "" {
				refs = append(refs, conn.SourceRef)
			}
			out = append(out, candidate{
				Kind:               "source_connection_readiness",
				SourceID:           card.ID,
				Title:              card.Title,
				Consequence:        "Adding this creates source-connection readiness work for " + conn.Label + ": " + card.Title,
				CapabilityClass:    "classify-extract",
				Owner:              "compound-sources",
				CanonicalSurface:   "director:source-connection:" + conn.ID + ":" + card.ID,
				SuccessPredicate:   "Resolve source connection " + conn.ID + " card " + card.ID + "; proof: " + card.Proof,
				DesiredOutcome:     "Adding this creates source-connection readiness work for " + conn.Label + ": " + card.Title,
				Command:            append([]string{}, card.Command...),
				RequiredInputs:     append([]string{}, card.RequiredInputs...),
				ExpectedValidation: append([]string{}, card.ExpectedValidation...),
				StopConditions:     append([]string{}, card.StopConditions...),
				Boundary:           card.Boundary,
				Proof:              card.Proof,
				EvidenceRefs:       refs,
			})
		}
	}
	return out, nil
}

func jobHealthCandidates(homePath string, now time.Time) ([]candidate, error) {
	roster, err := authority.LoadJobRosterAuthority(homePath)
	if err != nil {
		return nil, err
	}
	plans, err := authority.BuildJobHealthPlans(homePath, roster, now)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, plan := range plans {
		if reviewStatusTerminal(plan.Status) {
			continue
		}
		refs := append([]string{}, plan.EvidenceRefs...)
		refs = append(refs, "job_health_plan:"+plan.ID)
		if plan.JobID != "" {
			refs = append(refs, "job:"+plan.JobID)
		}
		out = append(out, candidate{
			Kind:             "job_health",
			SourceID:         plan.ID,
			Title:            "Review job health for " + plan.JobName,
			Consequence:      "Adding this creates job-health review work for " + plan.JobName,
			CapabilityClass:  "judge-critique",
			Owner:            "compound-authority",
			CanonicalSurface: "director:job-health:" + plan.ID,
			SuccessPredicate: "Review failing scheduled job " + plan.JobName + " without executing or mutating scheduler state",
			DesiredOutcome:   "Adding this creates job-health review work for " + plan.JobName,
			Command:          []string{"compound", "authority", "job-health-plan"},
			RequiredInputs:   plan.ReviewDraft.RequiredEvidence,
			ExpectedValidation: []string{
				"`compound authority job-health-plan` records or updates the governed review item",
				"follow-up job roster import proves resolved, parked, disabled, no-action, or continuing debt",
			},
			StopConditions: []string{"repair requires scheduler mutation without a separate reviewed owner action"},
			Boundary:       plan.ReviewDraft.MutationGuardrail,
			Proof:          plan.ReviewDraft.Verification,
			EvidenceRefs:   refs,
			CreatedAt:      firstNonEmpty(plan.LastRunAt, plan.CreatedAt),
		})
	}
	return out, nil
}

// absorptionDebtCandidates turns os-absorption-doctor debt into director
// proposals (W5 J15 → W9 seam): the doctor already distinguishes machine
// proven (proof_status=ok) rows from asserted-but-unproven (manual) and
// broken (failed) rows; this scanner reads that existing doctor report plus
// the underlying matrix (both existing authority readers — no new state, no
// matrix mutation) and proposes the next retirement move so an orchestrator
// does not have to notice by hand.
//
// Every manual/failed row becomes a candidate (bounded further by the
// per-run cap and class-fair round-robin). Only ONE pending row is proposed
// per run. Pending rows are considered by ascending row ID, but an
// operator-gated row does not block a later actionable row; if all are gated,
// the lowest ID remains visible. A gated proposal names the gate instead of
// instructing the gated step.
func absorptionDebtCandidates(homePath string, now time.Time) ([]candidate, error) {
	doctor, err := authority.OSAbsorptionDoctor(homePath)
	if err != nil {
		return nil, err
	}
	if !doctor.Installed || len(doctor.Rows) == 0 {
		return nil, nil
	}
	matrix, err := authority.LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	rowsByID := make(map[string]authority.OSAbsorptionMatrixRow, len(matrix.Rows))
	for _, row := range matrix.Rows {
		rowsByID[row.ID] = row
	}

	var out []candidate
	var pending []authority.OSAbsorptionDoctorRow
	for _, dr := range doctor.Rows {
		switch dr.ProofStatus {
		case "manual":
			out = append(out, absorptionManualCandidate(dr, rowsByID[dr.ID]))
		case "failed":
			out = append(out, absorptionFailedCandidate(dr, rowsByID[dr.ID]))
		case "pending":
			pending = append(pending, dr)
		}
	}
	if len(pending) > 0 {
		sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
		selected := pending[0]
		for _, dr := range pending {
			if absorptionOperatorGateText(rowsByID[dr.ID]) == "" {
				selected = dr
				break
			}
		}
		out = append(out, absorptionPendingCandidate(selected, rowsByID[selected.ID]))
	}
	return out, nil
}

func absorptionManualCandidate(dr authority.OSAbsorptionDoctorRow, row authority.OSAbsorptionMatrixRow) candidate {
	legacy := absorptionLegacySurface(dr, row)
	consequence := "Proving this row retires " + legacy + " as authority"
	return candidate{
		Kind:             "absorption_debt",
		SourceID:         dr.ID,
		Title:            "Wire the machine proof for " + dr.ID,
		Consequence:      consequence,
		CapabilityClass:  "judge-critique",
		Owner:            "compound-authority",
		CanonicalSurface: "director:absorption-debt:" + dr.ID,
		SuccessPredicate: "Wire an auto-proof for os-absorption row " + dr.ID + " so proof_status moves off manual (self-asserted) to ok (machine-checked) or failed",
		DesiredOutcome:   consequence,
		Command:          []string{"compound", "authority", "os-absorption-doctor", "--json"},
		RequiredInputs:   []string{"os-absorption doctor report for " + dr.ID, "the row's real consumer named in compound_owner/consumer"},
		ExpectedValidation: []string{
			"`compound authority os-absorption-doctor --json` reports proof_status=ok or proof_status=failed (never manual) for " + dr.ID,
		},
		StopConditions: []string{"the row has no real consumer yet to prove against"},
		Boundary:       "read-only auto-proof wiring in the absorption doctor; no matrix status-string change, no new registry",
		Proof:          "os-absorption doctor row " + dr.ID + " carries proof_status=ok with a proof_ref, or proof_status=failed with a proof_detail",
		EvidenceRefs:   absorptionEvidenceRefs(dr, row),
	}
}

func absorptionFailedCandidate(dr authority.OSAbsorptionDoctorRow, row authority.OSAbsorptionMatrixRow) candidate {
	legacy := absorptionLegacySurface(dr, row)
	consequence := "Proving this row retires " + legacy + " as authority"
	return candidate{
		Kind:             "absorption_debt",
		SourceID:         dr.ID,
		Title:            "Repair failed absorption proof for " + dr.ID,
		Consequence:      consequence,
		CapabilityClass:  "judge-critique",
		Owner:            "compound-authority",
		CanonicalSurface: "director:absorption-debt:" + dr.ID,
		SuccessPredicate: "Repair the failing auto-proof for os-absorption row " + dr.ID + " (" + firstNonEmpty(dr.ProofDetail, "no proof_detail recorded") + ") so proof_status returns to ok",
		DesiredOutcome:   consequence,
		Command:          []string{"compound", "authority", "os-absorption-doctor", "--json"},
		RequiredInputs:   []string{"os-absorption doctor proof_detail for " + dr.ID, "the row's real consumer named in compound_owner/consumer"},
		ExpectedValidation: []string{
			"`compound authority os-absorption-doctor --json` reports proof_status=ok with a proof_ref for " + dr.ID,
		},
		StopConditions: []string{"the underlying consumer/ledger this row proves against is itself gone"},
		Boundary:       "read-only auto-proof repair in the absorption doctor; no matrix status-string change, no new registry",
		Proof:          "os-absorption doctor row " + dr.ID + " carries proof_status=ok with a proof_ref",
		EvidenceRefs:   absorptionEvidenceRefs(dr, row),
	}
}

func absorptionPendingCandidate(dr authority.OSAbsorptionDoctorRow, row authority.OSAbsorptionMatrixRow) candidate {
	legacy := absorptionLegacySurface(dr, row)
	gate := absorptionOperatorGateText(row)
	if gate != "" {
		consequence := "Proving this row (" + dr.ID + ") retires " + legacy + " as authority, once the operator runs: " + gate
		return candidate{
			Kind:             "absorption_debt",
			SourceID:         dr.ID,
			Title:            "Blocked on operator: " + gate,
			Consequence:      consequence,
			CapabilityClass:  "judge-critique",
			Owner:            "compound-authority",
			CanonicalSurface: "director:absorption-debt:" + dr.ID,
			SuccessPredicate: "Confirm the operator-gated step for " + dr.ID + " ran; the director never runs it itself. Gate: " + gate,
			DesiredOutcome:   consequence,
			Command:          []string{"compound", "authority", "os-absorption-doctor", "--json"},
			RequiredInputs:   []string{"the row's retirement gate text", "operator confirmation the gated step ran"},
			ExpectedValidation: []string{
				"os-absorption doctor row " + dr.ID + " moves off pending only after the operator confirms the gated step ran",
			},
			StopConditions: []string{"blocked on operator: " + gate, "do not run ssh, DB export/delete, or any other operator-only command on this row's behalf"},
			Boundary:       "propose-only; never execute the operator-gated step named in the retirement gate",
			Proof:          "the operator confirms the gated step ran; a later os-absorption doctor run shows " + dr.ID + " progressed",
			EvidenceRefs:   absorptionEvidenceRefs(dr, row),
		}
	}
	consequence := "Proving this row retires " + legacy + " as authority"
	return candidate{
		Kind:             "absorption_debt",
		SourceID:         dr.ID,
		Title:            "Advance pending absorption row " + dr.ID,
		Consequence:      consequence,
		CapabilityClass:  "judge-critique",
		Owner:            "compound-authority",
		CanonicalSurface: "director:absorption-debt:" + dr.ID,
		SuccessPredicate: "Make bounded progress absorbing " + dr.ID + " (" + firstNonEmpty(row.CompoundOwner, dr.CompoundOwner) + ") toward its retirement gate",
		DesiredOutcome:   consequence,
		Command:          []string{"compound", "authority", "os-absorption-doctor", "--json"},
		RequiredInputs:   []string{"row retirement_gate/consumer text for " + dr.ID},
		ExpectedValidation: []string{
			"a later os-absorption doctor run shows " + dr.ID + " proof_status moved off pending (ok, failed, or an explicit operator gate)",
		},
		StopConditions: []string{"the next step requires operator-only action (ssh, DB export, credential use)"},
		Boundary:       "propose-only; no matrix mutation, no new registry",
		Proof:          "a follow-up os-absorption doctor run shows " + dr.ID + " no longer pending, or names why it remains blocked",
		EvidenceRefs:   absorptionEvidenceRefs(dr, row),
	}
}

// absorptionOperatorGateText reports the row's operator-only gate text, if
// any, by checking the same free-text fields the matrix already carries
// (retirement_gate, proof, consumer) for the word "operator" — the matrix
// schema is not mutated to add a new boolean field; this reads what is
// already there. Empty means the row's next step is not operator-gated.
func absorptionOperatorGateText(row authority.OSAbsorptionMatrixRow) string {
	for _, field := range []string{row.RetirementGate, row.Proof, row.Consumer} {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if strings.Contains(strings.ToLower(field), "operator") {
			return field
		}
	}
	return ""
}

func absorptionLegacySurface(dr authority.OSAbsorptionDoctorRow, row authority.OSAbsorptionMatrixRow) string {
	return firstNonEmpty(row.SourceRef, dr.SourceRef, dr.ID)
}

func absorptionEvidenceRefs(dr authority.OSAbsorptionDoctorRow, row authority.OSAbsorptionMatrixRow) []string {
	refs := []string{
		authority.OSAbsorptionAuthorityRef,
		"os_absorption_row:" + dr.ID,
	}
	if dr.ProofRef != "" {
		refs = append(refs, "os_absorption_proof_ref:"+dr.ProofRef)
	}
	if row.SourceRef != "" {
		refs = append(refs, "source_ref:"+row.SourceRef)
	}
	return reviewartifact.UniqueSorted(refs)
}

func actionContextFor(cand candidate, proposal Proposal) *work.WorkActionContext {
	expected := append([]string{}, cand.ExpectedValidation...)
	if len(proposal.RoutePlan.LaneJustification) > 0 {
		expected = append(expected, "suggested lane: "+strings.Join(proposal.RoutePlan.LaneJustification, " | "))
	}
	return &work.WorkActionContext{
		Kind:               "director_proposal",
		SourceRef:          proposal.TriggerRef,
		SourceID:           proposal.TriggerRef,
		Title:              cand.Title,
		ActionSummary:      cand.Consequence,
		Command:            append([]string{}, cand.Command...),
		RequiredInputs:     append([]string{}, cand.RequiredInputs...),
		ExpectedValidation: expected,
		StopConditions:     append([]string{}, cand.StopConditions...),
		Boundary:           cand.Boundary,
		Proof:              cand.Proof,
		EvidenceRefs:       append([]string{}, proposal.EvidenceRefs...),
	}
}

func nonGoalsFor(cand candidate) []string {
	out := []string{
		"do not execute this proposal while generating director output",
		"do not admit, claim, dispatch, advance, close, or mutate owner state without an operator act",
		"do not exceed director evidence boundary: " + firstNonEmpty(cand.Boundary, "use cited evidence refs only"),
	}
	// A harness-edit proposal keeps every propose-only non-goal above, and adds
	// the two that make the EVOLVE-BLOCK boundary explicit: it may never target a
	// hard-deny-core surface, and it may never apply the edit itself — the
	// envelope only proposes it, admission stays operator-gated.
	if strings.TrimSpace(cand.TargetSurface) != "" {
		out = append(out,
			"do not edit any hard-deny-core surface (policy, permissions, ledger, evaluator)",
			"do not apply the edit itself — this envelope only proposes it; admission stays operator-gated and requires the target surface to be actively whitelisted",
		)
	}
	for _, stop := range cand.StopConditions {
		if strings.TrimSpace(stop) != "" {
			out = append(out, "stop if: "+strings.TrimSpace(stop))
		}
	}
	return reviewartifact.Unique(out)
}

func proposalOwner(cand candidate) string {
	return firstNonEmpty(cand.Owner, "compound-director")
}

func loadDuplicateIndex(homePath string, store *work.Store) (duplicateIndex, []string) {
	idx := duplicateIndex{openWorkByTrigger: map[string][]string{}, plansByTrigger: map[string][]string{}}
	var warnings []string
	if store != nil {
		works, err := store.List()
		if err != nil {
			warnings = append(warnings, "open work duplicate scan skipped: "+err.Error())
		} else {
			for _, item := range works {
				if workStateTerminal(item.State) {
					continue
				}
				for _, ref := range item.EvidenceRefs {
					if strings.HasPrefix(ref, "director_trigger:") {
						idx.openWorkByTrigger[ref] = append(idx.openWorkByTrigger[ref], item.ID)
					}
				}
			}
		}
	}
	dir := filepath.Join(homePath, "create-plans")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			warnings = append(warnings, "prior director proposal scan skipped: "+err.Error())
		}
		return idx, warnings
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		plan, err := work.LoadCreatePlan(filepath.Join(dir, entry.Name()))
		if err != nil {
			warnings = append(warnings, "prior director proposal "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		if plan.Actor != DirectorActor && !containsRef(plan.Work.EvidenceRefs, "proposed_by:director") {
			continue
		}
		for _, ref := range plan.Work.EvidenceRefs {
			if strings.HasPrefix(ref, "director_trigger:") {
				idx.plansByTrigger[ref] = append(idx.plansByTrigger[ref], plan.ID)
			}
		}
	}
	return idx, warnings
}

func (idx duplicateIndex) find(triggerRef string) *SuppressedDuplicate {
	if workIDs := idx.openWorkByTrigger[triggerRef]; len(workIDs) > 0 {
		return &SuppressedDuplicate{TriggerRef: triggerRef, Reason: "open_work", ExistingWorkIDs: reviewartifact.UniqueSorted(workIDs)}
	}
	if planIDs := idx.plansByTrigger[triggerRef]; len(planIDs) > 0 {
		return &SuppressedDuplicate{TriggerRef: triggerRef, Reason: "prior_director_proposal", ExistingPlanIDs: reviewartifact.UniqueSorted(planIDs)}
	}
	return nil
}

func (idx duplicateIndex) addPlan(triggerRef, planID string) {
	idx.plansByTrigger[triggerRef] = append(idx.plansByTrigger[triggerRef], planID)
}

func recordReport(homePath string, report Report) (string, error) {
	dir := filepath.Join(homePath, "projections", "director", "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, report.ID+".json")
	return path, rewriteReport(path, report)
}

func recordApplyRunEnvelope(store *work.Store, report *Report, now time.Time) error {
	if store == nil {
		return fmt.Errorf("work_store_unavailable")
	}
	report.RunWorkID = idFor("work_", "director_run", report.ID)
	report.RunMoveType = "control_plane_repair"
	report.NoDelta = false
	if report.ProposalCount == 0 {
		report.RunMoveType = "audit_only"
		report.NoDelta = true
	}
	created, err := store.Create(work.CreateInput{
		WorkID:           report.RunWorkID,
		Owner:            "compound-director",
		Actor:            DirectorActor,
		CanonicalSurface: "director:run:" + report.ID,
		SuccessPredicate: "Record director propose --apply run receipt and closing transition",
		DesiredOutcome:   "Director apply run is enveloped with proposal, duplicate, and skip counts",
		NonGoals: []string{
			"do not claim, dispatch, execute, or close suggested work; only evidence-qualified job_health may stop at admitted",
			"do not mutate scheduler, source, product, review, or memory owner state",
		},
		EvidenceRefs: directorRunEvidenceRefs(*report),
		State:        "ready",
		Risk:         "tier1",
		Privacy:      "private",
	}, now)
	if err != nil {
		return err
	}
	if _, err := store.Claim(created.ID, DirectorActor, now); err != nil {
		return err
	}
	running, err := store.Advance(created.ID, work.AdvanceInput{
		To:    "running",
		Actor: DirectorActor,
		Note:  "director apply run",
	}, now)
	if err != nil {
		return err
	}
	closed, err := store.Close(running.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  report.RunMoveType,
		Actor:     DirectorActor,
		Summary:   directorRunSummary(*report),
		Changed:   report.ProposalCount > 0,
		ProofRefs: directorRunEvidenceRefs(*report),
	}, now)
	if err != nil {
		return err
	}
	report.RunWorkID = closed.Work.ID
	report.RunReceiptID = closed.Receipt.ID
	report.RunTransitionID = closed.Transition.ID
	return nil
}

func directorRunSummary(report Report) string {
	parts := []string{
		"director propose --apply",
		fmt.Sprintf("proposal_count=%d", report.ProposalCount),
		fmt.Sprintf("suppressed_duplicate_count=%d", report.SuppressedDuplicateCount),
		fmt.Sprintf("skipped_count=%d", report.SkippedCount),
	}
	if len(report.SkipHistogram) > 0 {
		parts = append(parts, "skip_histogram="+histogramString(report.SkipHistogram))
	}
	if report.NoDelta {
		parts = append(parts, "no_delta=true")
	}
	return strings.Join(parts, "; ")
}

func directorRunEvidenceRefs(report Report) []string {
	refs := []string{
		"director_run:" + report.ID,
		fmt.Sprintf("director_counts:proposal_count=%d,suppressed_duplicate_count=%d,skipped_count=%d", report.ProposalCount, report.SuppressedDuplicateCount, report.SkippedCount),
	}
	if report.ResultPath != "" {
		refs = append(refs, "director_run_report:"+filepath.ToSlash(report.ResultPath))
	}
	if report.GrantRef != "" {
		refs = append(refs, "grant_ref:"+report.GrantRef)
	}
	if report.NoDelta {
		refs = append(refs, "no_delta:director_apply_empty")
	}
	for _, class := range sortedHistogramKeys(report.SkipHistogram) {
		refs = append(refs, fmt.Sprintf("director_skip_histogram:%s=%d", class, report.SkipHistogram[class]))
	}
	return reviewartifact.UniqueSorted(refs)
}

func histogramString(histogram map[string]int) string {
	var parts []string
	for _, class := range sortedHistogramKeys(histogram) {
		parts = append(parts, fmt.Sprintf("%s:%d", class, histogram[class]))
	}
	return strings.Join(parts, ",")
}

func sortedHistogramKeys(histogram map[string]int) []string {
	var keys []string
	for key := range histogram {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func rewriteReport(path string, report Report) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func reviewEvidenceRefs(item reviewartifact.QueueItem) []string {
	ref := "review:" + reviewSourceID(item)
	return reviewartifact.UniqueSorted(append(append([]string{}, item.EvidenceRefs...), ref))
}

func reviewSourceID(item reviewartifact.QueueItem) string {
	if item.Area == "" {
		return item.ID
	}
	return item.Area + "/" + item.ID
}

func reviewStatusTerminal(status string) bool {
	return reviewartifact.TerminalStatus(status)
}

func workStateTerminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func triggerRefFor(cand candidate) string {
	key := strings.Join([]string{cand.Kind, cand.SourceID, strings.Join(compactRefs(cand.EvidenceRefs), "|")}, "|")
	return "director_trigger:" + cand.Kind + ":" + digest12(key)
}

// proposalKindFor maps a candidate kind to the envelope ProposalKind the
// EVOLVE-BLOCK guard uses. Harness-edit candidates are explicitly classified
// as harness_edit; every other candidate kind is ordinary. This is the
// trusted-producer stamp: a missing TargetSurface no longer silently grants
// the ordinary exemption because the classification lives on a separate
// field, not on the presence/absence of TargetSurface.
func proposalKindFor(candKind string) string {
	if candKind == "harness_edit" {
		return envelope.ProposalKindHarnessEdit
	}
	return envelope.ProposalKindOrdinary
}

func compactRefs(refs []string) []string {
	var out []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			out = append(out, ref)
		}
	}
	return reviewartifact.UniqueSorted(out)
}

func containsRef(refs []string, want string) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref) == want {
			return true
		}
	}
	return false
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func idFor(prefix string, parts ...string) string {
	return prefix + digest12(strings.Join(parts, "|"))
}

func digest12(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func timestamp(now time.Time) string {
	return fmt.Sprintf("%d", now.UTC().UnixNano())
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}
