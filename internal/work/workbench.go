package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/home"
)

// WorkbenchOptions controls the read-only workbench projection.
type WorkbenchOptions struct {
	RecentLimit                     int
	Actor                           string
	ActivePacketSurface             string
	ActivePacketExpectedEvidenceRef string
	IncludeArchived                 bool
	PacketDir                       string
}

// WorkbenchReport is a read model over WorkEnvelope, Receipt, and Transition
// state. It does not dispatch, claim, close, or repair work.
type WorkbenchReport struct {
	GeneratedAt       string                      `json:"generated_at"`
	Counts            WorkbenchCounts             `json:"counts"`
	Open              []WorkbenchItem             `json:"open"`
	Recent            []WorkbenchItem             `json:"recent,omitempty"`
	ActivePacketProof *ActivePacketProofReport    `json:"active_packet_proof,omitempty"`
	OfferedPackets    *OfferedPacketSurface       `json:"offered_packets,omitempty"`
	AdmissionPreviews []WorkbenchAdmissionPreview `json:"admission_previews,omitempty"`
	DispatchNext      WorkbenchDispatchNext       `json:"dispatch_next"`
	DispatchStepRuns  []DispatchStepRunSummary    `json:"dispatch_step_runs,omitempty"`
	ContinueRuns      []ContinueRunSummary        `json:"continue_runs,omitempty"`
	// BranchDebt is the branch-lifecycle read model (opens vs covered vs stale
	// over the transition stream): the anti-freeze meter. Bounded lists; full
	// counts. Nil only when the coverage fold itself failed (see Integrity).
	BranchDebt *BranchCoverageReport `json:"branch_debt,omitempty"`
	Integrity  []string              `json:"integrity,omitempty"`
	// eventsByWork is the single event-log snapshot already loaded to build this
	// report. In-process consumers may reuse it for adjacent projections without
	// paying a second full-ledger read; it is deliberately absent from JSON.
	eventsByWork map[string][]Event
}

type WorkbenchCounts struct {
	Total             int            `json:"total"`
	Open              int            `json:"open"`
	Terminal          int            `json:"terminal"`
	NeedsClaim        int            `json:"needs_claim"`
	NeedsReview       int            `json:"needs_review"`
	InProgress        int            `json:"in_progress"`
	RunObservations   int            `json:"run_observations"`
	RunArtifacts      int            `json:"run_artifacts"`
	DispatchStepRuns  int            `json:"dispatch_step_runs"`
	ContinueRuns      int            `json:"continue_runs"`
	AdmissionPreviews int            `json:"admission_previews"`
	MissingReceipt    int            `json:"missing_receipt"`
	MissingTransition int            `json:"missing_transition"`
	ByState           map[string]int `json:"by_state"`
}

type DispatchStepRunSummary struct {
	ID          string   `json:"id"`
	GeneratedAt string   `json:"generated_at"`
	Status      string   `json:"status"`
	Actor       string   `json:"actor"`
	StepCount   int      `json:"step_count"`
	CanContinue bool     `json:"can_continue"`
	WaitReason  string   `json:"wait_reason,omitempty"`
	BlockedOn   []string `json:"blocked_on,omitempty"`
	NextAction  string   `json:"next_action,omitempty"`
	ResultPath  string   `json:"result_path,omitempty"`
	StepActions []string `json:"step_actions,omitempty"`
	StepWorkIDs []string `json:"step_work_ids,omitempty"`
}

type ContinueRunSummary struct {
	ID          string   `json:"id"`
	GeneratedAt string   `json:"generated_at"`
	Status      string   `json:"status"`
	Actor       string   `json:"actor"`
	StepCount   int      `json:"step_count"`
	CanContinue bool     `json:"can_continue"`
	WaitReason  string   `json:"wait_reason,omitempty"`
	BlockedOn   []string `json:"blocked_on,omitempty"`
	NextAction  string   `json:"next_action,omitempty"`
	ResultPath  string   `json:"result_path,omitempty"`
	StepActions []string `json:"step_actions,omitempty"`
	StepWorkIDs []string `json:"step_work_ids,omitempty"`
}

type WorkbenchDispatchNext struct {
	Actor            string                     `json:"actor"`
	CanRun           bool                       `json:"can_run"`
	Command          []string                   `json:"command,omitempty"`
	Action           string                     `json:"action,omitempty"`
	Reason           string                     `json:"reason,omitempty"`
	WorkID           string                     `json:"work_id,omitempty"`
	WaitReason       string                     `json:"wait_reason,omitempty"`
	BlockedOn        []string                   `json:"blocked_on,omitempty"`
	AdjacentProgress *WorkbenchAdjacentProgress `json:"adjacent_progress,omitempty"`
}

type WorkbenchAdjacentProgress struct {
	Available                bool                      `json:"available"`
	Reason                   string                    `json:"reason"`
	NextAction               string                    `json:"next_action"`
	PacketCandidateCount     int                       `json:"packet_candidate_count"`
	PacketRefs               []string                  `json:"packet_refs,omitempty"`
	Command                  []string                  `json:"command,omitempty"`
	HeldWorkIDs              []string                  `json:"held_work_ids,omitempty"`
	NonPacketActions         []WorkbenchAdjacentAction `json:"non_packet_actions,omitempty"`
	ConsumedNonPacketActions []WorkbenchAdjacentAction `json:"consumed_non_packet_actions,omitempty"`
}

type WorkbenchAdjacentAction struct {
	ID                   string               `json:"id"`
	Kind                 string               `json:"kind"`
	Title                string               `json:"title"`
	NextAction           string               `json:"next_action,omitempty"`
	Command              []string             `json:"command,omitempty"`
	RequiredInputs       []string             `json:"required_inputs,omitempty"`
	ExpectedValidation   []string             `json:"expected_validation,omitempty"`
	StopConditions       []string             `json:"stop_conditions,omitempty"`
	Boundary             string               `json:"boundary"`
	Proof                string               `json:"proof"`
	RequiresOperator     bool                 `json:"requires_operator,omitempty"`
	Consumed             bool                 `json:"consumed,omitempty"`
	ConsumedEvidenceRefs []string             `json:"consumed_evidence_refs,omitempty"`
	ReviewCard           *WorkbenchReviewCard `json:"review_card,omitempty"`
}

type WorkbenchAdmissionPreview struct {
	PlanID                 string             `json:"plan_id"`
	PlanSchemaVersion      string             `json:"plan_schema_version,omitempty"`
	GeneratedAt            string             `json:"generated_at,omitempty"`
	AgeSeconds             int64              `json:"age_seconds,omitempty"`
	Age                    string             `json:"age,omitempty"`
	Status                 string             `json:"status"`
	Mode                   string             `json:"mode,omitempty"`
	Staleness              string             `json:"staleness,omitempty"`
	StaleReason            string             `json:"stale_reason,omitempty"`
	NextAction             string             `json:"next_action,omitempty"`
	Actor                  string             `json:"actor,omitempty"`
	WorkID                 string             `json:"work_id"`
	WorkOwner              string             `json:"work_owner,omitempty"`
	WorkState              string             `json:"work_state,omitempty"`
	SuccessPredicate       string             `json:"success_predicate,omitempty"`
	DesiredOutcome         string             `json:"desired_outcome,omitempty"`
	ActionSummary          string             `json:"action_summary,omitempty"`
	PlanPath               string             `json:"plan_path,omitempty"`
	Command                []string           `json:"command,omitempty"`
	AdmitCommand           []string           `json:"admit_command,omitempty"`
	RetireCommand          []string           `json:"retire_command,omitempty"`
	RetireDryRun           []string           `json:"retire_dry_run_command,omitempty"`
	PolicyAllowed          bool               `json:"policy_allowed"`
	PolicyOutcome          string             `json:"policy_outcome,omitempty"`
	RequiresOperator       bool               `json:"requires_operator,omitempty"`
	AdmissionBlockedReason string             `json:"admission_blocked_reason,omitempty"`
	EvidenceRefs           []string           `json:"evidence_refs,omitempty"`
	ActionContext          *WorkActionContext `json:"action_context,omitempty"`
}

type WorkbenchItem struct {
	// Captured with State/PolicyDecisionID; in-process dispatch must not reload
	// a newer generation and mistake it for the one that was selected.
	sourceActionIntentID  string
	WorkID                string                  `json:"work_id"`
	State                 string                  `json:"state"`
	Owner                 string                  `json:"owner,omitempty"`
	AccountableOwner      string                  `json:"accountable_owner,omitempty"`
	CreatorHarness        string                  `json:"creator_harness,omitempty"`
	CreatorRequestedModel string                  `json:"creator_requested_model,omitempty"`
	CreatorLaneID         string                  `json:"creator_lane_id,omitempty"`
	GitDeliveryBound      bool                    `json:"git_delivery_bound,omitempty"`
	TargetWorktreeRoot    string                  `json:"target_worktree_root,omitempty"`
	RepositoryFingerprint string                  `json:"repository_fingerprint,omitempty"`
	TargetLocalRef        string                  `json:"target_local_ref,omitempty"`
	ExpectedBaseSHA       string                  `json:"expected_base_sha,omitempty"`
	PublicationMode       string                  `json:"publication_mode,omitempty"`
	PublicationRef        string                  `json:"publication_ref,omitempty"`
	CandidateTipSHA       string                  `json:"candidate_tip_sha,omitempty"`
	LandingStatus         string                  `json:"landing_status,omitempty"`
	ClaimedBy             string                  `json:"claimed_by,omitempty"`
	ClaimedAt             string                  `json:"claimed_at,omitempty"`
	SuccessPredicate      string                  `json:"success_predicate"`
	DesiredOutcome        string                  `json:"desired_outcome,omitempty"`
	CanonicalSurface      string                  `json:"canonical_surface,omitempty"`
	ContextPackID         string                  `json:"context_pack_id,omitempty"`
	PolicyDecisionID      string                  `json:"policy_decision_id,omitempty"`
	ReceiptID             string                  `json:"receipt_id,omitempty"`
	TransitionID          string                  `json:"transition_id,omitempty"`
	MoveType              string                  `json:"move_type,omitempty"`
	Changed               bool                    `json:"changed,omitempty"`
	ReviewTrigger         string                  `json:"review_trigger,omitempty"`
	ObservatoryTags       []string                `json:"observatory_tags,omitempty"`
	NextAction            string                  `json:"next_action"`
	OpenReason            string                  `json:"open_reason,omitempty"`
	EvidenceRefs          []string                `json:"evidence_refs,omitempty"`
	ArtifactIDs           []string                `json:"artifact_ids,omitempty"`
	VerificationIDs       []string                `json:"verification_ids,omitempty"`
	MemoryCandidateIDs    []string                `json:"memory_candidate_ids,omitempty"`
	ActionContext         *WorkActionContext      `json:"action_context,omitempty"`
	ReviewCards           []WorkbenchReviewCard   `json:"review_cards,omitempty"`
	RunObservations       []RunObservationSummary `json:"run_observations,omitempty"`
	RunArtifacts          []WorkbenchRunArtifact  `json:"run_artifacts,omitempty"`
	RouteOutcomes         []RouteOutcomeSummary   `json:"route_outcomes,omitempty"`
	recentAt              string
}

type WorkbenchReviewCard struct {
	ID                     string   `json:"id"`
	Kind                   string   `json:"kind"`
	Title                  string   `json:"title"`
	NextAction             string   `json:"next_action,omitempty"`
	ConditionStatus        string   `json:"condition_status,omitempty"`
	TriggeringEvidenceRef  string   `json:"triggering_evidence_ref,omitempty"`
	MatchedConditionRefs   []string `json:"matched_condition_refs,omitempty"`
	WorkID                 string   `json:"work_id"`
	GateID                 string   `json:"gate_id,omitempty"`
	SourceKind             string   `json:"source_kind,omitempty"`
	CandidateID            string   `json:"candidate_id,omitempty"`
	CandidateState         string   `json:"candidate_state,omitempty"`
	CandidateEntity        string   `json:"candidate_entity,omitempty"`
	CandidatePredicate     string   `json:"candidate_predicate,omitempty"`
	CandidateObject        string   `json:"candidate_object,omitempty"`
	CandidateConfidence    *float64 `json:"candidate_confidence,omitempty"`
	CandidateVisibility    string   `json:"candidate_visibility,omitempty"`
	CandidateSourceRef     string   `json:"candidate_source_ref,omitempty"`
	CandidateItemRef       string   `json:"candidate_item_ref,omitempty"`
	CandidateExtraction    string   `json:"candidate_extraction_id,omitempty"`
	CandidateExtractedBy   string   `json:"candidate_extracted_by,omitempty"`
	CandidateSourceKind    string   `json:"candidate_source_kind,omitempty"`
	CandidateSourceTitle   string   `json:"candidate_source_title,omitempty"`
	CandidateSourceExcerpt string   `json:"candidate_source_excerpt,omitempty"`
	PanelRunID             string   `json:"panel_run_id,omitempty"`
	PanelRunRef            string   `json:"panel_run_ref,omitempty"`
	PanelDecision          string   `json:"panel_decision,omitempty"`
	PanelConfidence        *float64 `json:"panel_confidence,omitempty"`
	PanelRationale         string   `json:"panel_rationale,omitempty"`
	PanelFindingCount      int      `json:"panel_finding_count,omitempty"`
	PanelRiskCount         int      `json:"panel_risk_count,omitempty"`
	PanelFailedCount       int      `json:"panel_failed_count,omitempty"`
	PromotionSummaryRef    string   `json:"promotion_summary_ref,omitempty"`
	PromotionCount         *int     `json:"promotion_count,omitempty"`
	Command                []string `json:"command,omitempty"`
	RequiredInputs         []string `json:"required_inputs,omitempty"`
	ExpectedValidation     []string `json:"expected_validation,omitempty"`
	StopConditions         []string `json:"stop_conditions,omitempty"`
	Boundary               string   `json:"boundary"`
	Proof                  string   `json:"proof"`
	RequiresOperator       bool     `json:"requires_operator,omitempty"`
}

type WorkbenchRunArtifact struct {
	Ref          string   `json:"ref"`
	Kind         string   `json:"kind"`
	Bytes        int64    `json:"bytes,omitempty"`
	Schema       string   `json:"schema_version,omitempty"`
	ID           string   `json:"id,omitempty"`
	Status       string   `json:"status,omitempty"`
	TargetKind   string   `json:"target_kind,omitempty"`
	TargetRefs   []string `json:"target_refs,omitempty"`
	CandidateIDs []string `json:"candidate_ids,omitempty"`
	Count        *int     `json:"count,omitempty"`
}

type RouteOutcomeSummary struct {
	ID                 string `json:"id"`
	CreatedAt          string `json:"created_at,omitempty"`
	Status             string `json:"status"`
	ReviewRef          string `json:"review_ref"`
	RouteEvidenceID    string `json:"route_evidence_id"`
	ReceiptID          string `json:"receipt_id"`
	AssessmentClass    string `json:"assessment_class"`
	ShouldReviewPolicy bool   `json:"should_review_policy"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Protocol           string `json:"protocol,omitempty"`
}

type RunObservationSummary struct {
	ID               string `json:"id"`
	GeneratedAt      string `json:"generated_at,omitempty"`
	Actor            string `json:"actor,omitempty"`
	Note             string `json:"note,omitempty"`
	TraceID          string `json:"trace_id"`
	State            string `json:"state"`
	ActionIntentID   string `json:"action_intent_id,omitempty"`
	PolicyDecisionID string `json:"policy_decision_id,omitempty"`
	ResultPath       string `json:"result_path,omitempty"`
}

func (s *Store) Workbench(now time.Time, opts WorkbenchOptions) (WorkbenchReport, error) {
	if opts.RecentLimit <= 0 {
		opts.RecentLimit = 5
	}
	profileWorkbench := os.Getenv("COMPOUND_PROFILE_WORKBENCH") == "1"
	wbT0 := time.Now()
	wbLogStage := func(label string, t time.Time) {
		if !profileWorkbench {
			return
		}
		fmt.Fprintf(os.Stderr, "[WB-PROFILE] %s: %s (cum=%s)\n", label, time.Since(t), time.Since(wbT0))
	}
	report := WorkbenchReport{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Counts:      WorkbenchCounts{ByState: map[string]int{}},
		Open:        []WorkbenchItem{},
	}
	if strings.TrimSpace(opts.PacketDir) != "" {
		offered, err := OfferedPackets(opts.PacketDir)
		if err != nil {
			report.Integrity = append(report.Integrity, "offered packets unavailable: "+err.Error())
		} else {
			report.OfferedPackets = &offered
		}
	}
	wbLogStage("setup", time.Now())
	// list() (not List/ListWithArchived) so a work item that fails to parse is
	// reported through report.Integrity instead of silently vanishing from
	// every count built below.
	works, listWarnings, err := s.list(opts.IncludeArchived)
	if err != nil {
		return report, err
	}
	report.Integrity = append(report.Integrity, listWarnings...)
	wbLogStage("list_works", time.Now())
	// Loaded once and threaded through the rest of this build: every Get/
	// archivedWorkPath lookup on the workbench path uses this snapshot instead
	// of re-reading the archive index per work item. A malformed/missing index
	// is treated as "no archived entries" here, matching archivedWorkPath's own
	// swallowed-error behavior, so report content is unchanged either way.
	archiveIndex, _ := home.LoadOperationalArchiveIndex(s.home)
	holdEvidence, holdEvidenceWarnings := s.workHoldEvidenceIndex()
	for _, warning := range holdEvidenceWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	routeOutcomes, routeWarnings := s.workbenchRouteOutcomeIndex()
	for _, warning := range routeWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	wbLogStage("indices", time.Now())
	admissionPreviews, admissionWarnings := s.workbenchAdmissionPreviews(now, opts.RecentLimit, archiveIndex)
	report.AdmissionPreviews = admissionPreviews
	report.Counts.AdmissionPreviews = len(admissionPreviews)
	for _, warning := range admissionWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	report.Counts.Total = len(works)
	// One event-log read for the whole build: eventsFor stays the correct call
	// for single-work callers, but re-reading+re-decoding the full log once per
	// work item here made this loop O(works x log size) instead of O(log size).
	eventsByWork, err := s.eventsByWorkID()
	if err != nil {
		return report, err
	}
	report.eventsByWork = eventsByWork
	wbLogStage("events_by_work", time.Now())
	// Pre-index run-observations by work_id once. The per-item path
	// (workbenchRunObservations) used to ReadDir+read+parse every file in
	// run-observations/ for every work item — O(works × run-observations).
	// The index makes the per-item lookup O(1) so the loop scales linearly
	// with the actual evidence, not with the full directory.
	runObsByWork, runObsWarnings := s.workbenchRunObservationsIndex()
	for _, warning := range runObsWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	// Cache the ASC product-signal state. The held/blocked review-card path
	// used to read product-signals.jsonl and parse every row per held/blocked
	// work item; the ledger is independent of the work item, so this is a
	// single per-workbench load.
	ascSignals := s.ascProductSignalState()
	// Per-call memo for envelope.Read (Transition/Receipt payloads). Without
	// this every terminal item walks the segment files independently; with
	// it the unique-ID cost is paid exactly once per Workbench call.
	envelopeMemo := map[string]envelopeMemoEntry{}
	// One request-local Git facts session for the whole board. Every Work still
	// evaluates its own delivery verdict (contract, artifact, proof,
	// publication, lineage); only the repository-scoped read facts are shared,
	// so a board full of distinct candidate tips in one repository pays for the
	// common reads once. The session dies with this call.
	landingFacts := newLandingGitFacts()
	wbLogStage("obs_asc_indices", time.Now())
	for _, w := range works {
		events := eventsByWork[w.ID]
		wbItemT := time.Now()
		observations := runObsByWork[w.ID]
		if observations == nil {
			// The index already checked this work, including an empty result or
			// unreadable input reported above. Nil means no index to standalone
			// callers; pass a non-nil empty result so this loop never rescans it.
			observations = []RunObservationSummary{}
		}
		item := s.workbenchItemWithCaches(w, events, &report, holdEvidence, now, workbenchItemCaches{
			RunObservations: observations,
			ASCSignals:      &ascSignals,
			EnvelopeMemo:    &envelopeMemo,
			LandingGitFacts: landingFacts,
		})
		wbLogStage("item", wbItemT)
		routeOutcomes.attach(&item)
		item.ObservatoryTags = workbenchObservatoryTags(item)
		report.Counts.RunObservations += len(item.RunObservations)
		report.Counts.ByState[w.State]++
		if terminalStates[w.State] {
			report.Counts.Terminal++
			if terminalProofRequired(w.State) && item.ReceiptID == "" {
				report.Counts.MissingReceipt++
			}
			if terminalProofRequired(w.State) && item.TransitionID == "" {
				report.Counts.MissingTransition++
			}
			report.Recent = append(report.Recent, item)
			continue
		}
		report.Counts.Open++
		switch w.State {
		case "ready", "admitted", "proposed":
			report.Counts.NeedsClaim++
		case "held", "blocked":
			report.Counts.NeedsReview++
		case "claimed", "running", "verifying":
			report.Counts.InProgress++
		}
		report.Open = append(report.Open, item)
	}
	wbLogStage("item_loop", time.Now())
	runs, warnings := s.workbenchDispatchStepRuns(opts.RecentLimit, archiveIndex)
	wbLogStage("dispatch_step_runs", time.Now())
	report.DispatchStepRuns = runs
	report.Counts.DispatchStepRuns = len(runs)
	for _, warning := range warnings {
		report.Integrity = append(report.Integrity, warning)
	}
	continueRuns, continueWarnings := s.workbenchContinueRuns(opts.RecentLimit, archiveIndex)
	wbLogStage("continue_runs", time.Now())
	report.ContinueRuns = continueRuns
	report.Counts.ContinueRuns = len(continueRuns)
	for _, warning := range continueWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	activePacket, activeWarnings := s.workbenchActivePacketProof(now, opts.ActivePacketSurface, opts.ActivePacketExpectedEvidenceRef)
	wbLogStage("active_packet_proof", time.Now())
	report.ActivePacketProof = activePacket
	for _, warning := range activeWarnings {
		report.Integrity = append(report.Integrity, warning)
	}
	sort.Slice(report.Open, func(i, j int) bool {
		pi, pj := statePriority(report.Open[i].State), statePriority(report.Open[j].State)
		if pi != pj {
			return pi < pj
		}
		return report.Open[i].WorkID < report.Open[j].WorkID
	})
	report.DispatchNext = workbenchDispatchNext(report, opts.Actor)
	if warnings := s.applyAdjacentActionProofConsumption(report.DispatchNext.AdjacentProgress); len(warnings) > 0 {
		report.Integrity = append(report.Integrity, warnings...)
	}
	sort.SliceStable(report.Recent, func(i, j int) bool {
		if report.Recent[i].recentAt != report.Recent[j].recentAt {
			return workbenchTimestampAfter(report.Recent[i].recentAt, report.Recent[j].recentAt)
		}
		return report.Recent[i].WorkID > report.Recent[j].WorkID
	})
	if len(report.Recent) > opts.RecentLimit {
		report.Recent = report.Recent[:opts.RecentLimit]
	}
	// Branch debt is folded from the transition event stream (the SAME store
	// this workbench already serves): a coverage failure degrades to an
	// integrity note, never a workbench failure.
	if coverage, err := s.BranchCoverage(now, BranchDebtStaleWindowDays); err != nil {
		report.Integrity = append(report.Integrity, "branch coverage unavailable: "+err.Error())
	} else {
		report.BranchDebt = &coverage
	}
	wbLogStage("branch_coverage", time.Now())
	return report, nil
}

func (s *Store) workbenchActivePacketProof(now time.Time, surface, expectedPacketEvidenceRef string) (*ActivePacketProofReport, []string) {
	proof, err := s.ActivePacketProof(now, surface, expectedPacketEvidenceRef)
	if err != nil {
		return nil, []string{"active-packet-proof unreadable: " + err.Error()}
	}
	if strings.TrimSpace(proof.WorkID) == "" {
		return nil, nil
	}
	return &proof, nil
}

func (s *Store) workbenchAdmissionPreviews(now time.Time, limit int, archiveIndex []home.OperationalArchiveEntry) ([]WorkbenchAdmissionPreview, []string) {
	dir := filepath.Join(s.home, "create-plans")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{"create plans unreadable: " + err.Error()}
	}

	var previews []WorkbenchAdmissionPreview
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var plan CreatePlan
		if err := s.readJSON(path, &plan); err != nil {
			warnings = append(warnings, "create plan "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		if !workbenchCreatePlanIsAdmissionPreview(plan) {
			continue
		}
		if _, err := s.getWithArchiveIndex(plan.WorkID, archiveIndex); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			warnings = append(warnings, "create plan "+entry.Name()+" work lookup failed: "+err.Error())
			continue
		}

		planPath := strings.TrimSpace(plan.PlanPath)
		if planPath == "" {
			planPath = path
		}
		workOwner := strings.TrimSpace(plan.WorkOwner)
		if workOwner == "" {
			workOwner = plan.Work.Owner
		}
		workState := strings.TrimSpace(plan.WorkState)
		if workState == "" {
			workState = plan.Work.State
		}
		ageSeconds, ageLabel, staleness, staleReason := workbenchAdmissionPreviewAge(now, plan.GeneratedAt)
		nextAction := "review proposal-only admission preview; admit only if the planned work is still intended"
		if staleness == "stale" {
			nextAction = "stale proposal-only admission preview; revalidate intent before admitting, otherwise retire through operational archive or replace with a fresh plan"
		}
		planRel := filepath.ToSlash(strings.TrimPrefix(path, s.home+string(os.PathSeparator)))
		var retireCommand []string
		var retireDryRun []string
		if planRel != "" && !strings.HasPrefix(planRel, "..") {
			retireCommand = []string{"compound", "home", "archive-history", "--kind", "create-plan", "--path", planRel}
			retireDryRun = append(append([]string{}, retireCommand...), "--dry-run")
		}
		actionContext := cloneWorkActionContext(plan.ActionContext)
		actionSummary := ""
		if actionContext != nil {
			actionSummary = strings.TrimSpace(actionContext.ActionSummary)
		}
		requiresOperator := workActionContextRequiresOperator(actionContext)
		if requiresOperator && actionContext != nil {
			actionContext.RequiresOperator = true
		}
		admissionBlockedReason := ""
		admitCommand := []string{"compound", "work", "admit-plan", "--plan", planPath}
		if plan.SchemaVersion == LegacyCreatePlanSchemaVersion {
			staleness = "legacy"
			staleReason = "legacy create-plan id is not content-bound"
			nextAction = "legacy admission preview; create a fresh content-bound plan, then retire this preview through operational archive"
			admissionBlockedReason = "legacy create-plan requires re-plan before admission"
			admitCommand = nil
		}
		if requiresOperator && admissionBlockedReason == "" {
			admissionBlockedReason = "operator required inputs must be supplied before admission"
			nextAction = "operator-only admission preview; do not admit until required private/operator inputs are supplied, otherwise retire through operational archive"
			admitCommand = nil
		}
		previews = append(previews, WorkbenchAdmissionPreview{
			PlanID:                 plan.ID,
			PlanSchemaVersion:      plan.SchemaVersion,
			GeneratedAt:            plan.GeneratedAt,
			AgeSeconds:             ageSeconds,
			Age:                    ageLabel,
			Status:                 plan.Status,
			Mode:                   plan.Mode,
			Staleness:              staleness,
			StaleReason:            staleReason,
			NextAction:             nextAction,
			Actor:                  plan.Actor,
			WorkID:                 plan.WorkID,
			WorkOwner:              workOwner,
			WorkState:              workState,
			SuccessPredicate:       plan.Work.SuccessPredicate,
			DesiredOutcome:         plan.Work.DesiredOutcome,
			ActionSummary:          actionSummary,
			PlanPath:               planPath,
			Command:                append([]string{}, plan.CreateCommand...),
			AdmitCommand:           admitCommand,
			RetireCommand:          retireCommand,
			RetireDryRun:           retireDryRun,
			PolicyAllowed:          plan.Policy.Allowed,
			PolicyOutcome:          plan.Policy.Outcome,
			RequiresOperator:       requiresOperator,
			AdmissionBlockedReason: admissionBlockedReason,
			EvidenceRefs:           append([]string{}, plan.Work.EvidenceRefs...),
			ActionContext:          actionContext,
		})
	}

	sort.SliceStable(previews, func(i, j int) bool {
		if previews[i].GeneratedAt != previews[j].GeneratedAt {
			return workbenchTimestampAfter(previews[i].GeneratedAt, previews[j].GeneratedAt)
		}
		return previews[i].PlanID > previews[j].PlanID
	})
	if limit > 0 && len(previews) > limit {
		previews = previews[:limit]
	}
	return previews, warnings
}

func workbenchCreatePlanIsAdmissionPreview(plan CreatePlan) bool {
	return (plan.SchemaVersion == CreatePlanSchemaVersion || plan.SchemaVersion == LegacyCreatePlanSchemaVersion) &&
		plan.Status == "create_policy_allowed" &&
		plan.Policy.Allowed &&
		strings.TrimSpace(plan.ID) != "" &&
		strings.TrimSpace(plan.WorkID) != ""
}

func workActionContextRequiresOperator(ctx *WorkActionContext) bool {
	if ctx == nil {
		return false
	}
	if ctx.RequiresOperator {
		return true
	}
	if strings.TrimSpace(ctx.Kind) != "workbench_review_card" {
		return false
	}
	parts := []string{ctx.SourceID, ctx.SourceRef, ctx.Title, ctx.ActionSummary, ctx.Boundary, ctx.Proof}
	parts = append(parts, ctx.RequiredInputs...)
	parts = append(parts, ctx.StopConditions...)
	joined := strings.ToLower(strings.Join(parts, " "))
	return strings.Contains(joined, "operator-gate-") ||
		strings.Contains(joined, "memory-review-") ||
		strings.Contains(joined, "operator supplies") ||
		strings.Contains(joined, "operator review") ||
		strings.Contains(joined, "operator adjudication") ||
		strings.Contains(joined, "private config") ||
		strings.Contains(joined, "private/operator")
}

const workbenchAdmissionPreviewStaleAfter = 24 * time.Hour

func workbenchAdmissionPreviewAge(now time.Time, generatedAt string) (int64, string, string, string) {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(generatedAt))
	if err != nil {
		return 0, "", "unknown", "generated_at unreadable"
	}
	if now.IsZero() {
		now = time.Now()
	}
	age := now.UTC().Sub(at.UTC())
	if age < 0 {
		age = 0
	}
	seconds := int64(age.Round(time.Second).Seconds())
	label := workbenchDurationLabel(seconds)
	if age >= workbenchAdmissionPreviewStaleAfter {
		return seconds, label, "stale", "proposal-only create-plan older than 24h"
	}
	return seconds, label, "fresh", ""
}

func workbenchDurationLabel(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds >= 86400:
		return strconv.FormatInt(seconds/86400, 10) + "d"
	case seconds >= 3600:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	case seconds >= 60:
		return strconv.FormatInt(seconds/60, 10) + "m"
	default:
		return strconv.FormatInt(seconds, 10) + "s"
	}
}

func (s *Store) actionContextForWork(w envelope.WorkEnvelope) (*WorkActionContext, []string) {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return nil, []string{"action context lifecycle unreadable: " + err.Error()}
	}
	return s.actionContextFromCreatePlan(w, events)
}

func (s *Store) actionContextFromCreatePlan(w envelope.WorkEnvelope, events []Event) (*WorkActionContext, []string) {
	if snapshotContext, found, warnings := s.actionContextSnapshotForWork(w, events); found {
		return snapshotContext, warnings
	}
	// Backward compatibility for work admitted before immutable snapshots were
	// introduced. New admissions always attach a content-addressed snapshot.
	planID := lastCreatePlanID(events)
	if planID == "" {
		return nil, nil
	}
	planPath := filepath.Join(s.home, "create-plans", planID+".json")
	plan, err := LoadCreatePlan(planPath)
	if err != nil {
		return nil, []string{"create plan " + planID + " action context unreadable: " + err.Error()}
	}
	if plan.WorkID != w.ID {
		return nil, []string{"create plan " + planID + " belongs to " + plan.WorkID + ", not " + w.ID}
	}
	return cloneWorkActionContext(plan.ActionContext), nil
}

func lastCreatePlanID(events []Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		for j := len(events[i].Attached) - 1; j >= 0; j-- {
			ref := strings.TrimSpace(events[i].Attached[j])
			if strings.HasPrefix(ref, "create_plan:") {
				return strings.TrimSpace(strings.TrimPrefix(ref, "create_plan:"))
			}
		}
	}
	return ""
}

// workbenchItemCaches carries pre-resolved per-workbench evidence shared by
// the whole loop. Each field is optional: nil means "fall back to a per-item
// lookup" so the standalone callers (active_packet_proof, tests) still work
// without precomputing the full board.
type workbenchItemCaches struct {
	RunObservations []RunObservationSummary
	ASCSignals      *ascProductSignalState

	// envelopeMemo memoizes readEventJSON (the segment-walking read of
	// transition / receipt payloads) for the duration of one Workbench
	// call. A live home can reference the same envelope ID from many
	// terminal items (a Transition closes exactly one work_id, but the
	// receipt it links frequently re-links to future audits), so a
	// single-call cache turns the O(items × segment-walk) cost into
	// O(unique IDs × segment-walk). Failures are cached too so a
	// missing id never re-asks the segment engine.
	EnvelopeMemo *map[string]envelopeMemoEntry

	// LandingGitFacts is the request-local Git read session shared by every
	// item of one Workbench call. It reuses repository-scoped READ FACTS ONLY
	// (canonical root, normalized refs, ref resolution, current branch, status,
	// worktree list, object existence, ancestry, repository identity keyed by
	// canonical root + exact expected base, owned-path dirt). It never shares a
	// LandCheckReport, a verdict, a proof, or any Work-specific outcome: each
	// Work keeps its own contract, artifact, proof, publication, and lineage
	// evaluation, and a failed shared read is reported for every affected Work.
	// Nil means fresh reads per item (standalone callers).
	LandingGitFacts *landingGitFacts
}

type envelopeMemoEntry struct {
	value []byte
	err   error
}

// memoEnvelope returns the bytes for ref via the per-call cache, falling back
// to readEventJSON on a miss and writing the result back into the cache.
func (s *Store) memoEnvelope(memo *map[string]envelopeMemoEntry, ref string) ([]byte, error) {
	if memo == nil {
		record, err := s.events.Read(ref)
		if err != nil {
			return nil, err
		}
		return record.Bytes, nil
	}
	if entry, ok := (*memo)[ref]; ok {
		return entry.value, entry.err
	}
	record, err := s.events.Read(ref)
	var entry envelopeMemoEntry
	if err != nil {
		entry = envelopeMemoEntry{err: err}
	} else {
		entry = envelopeMemoEntry{value: record.Bytes}
	}
	(*memo)[ref] = entry
	return entry.value, entry.err
}

// readEnvelopeJSON is the unified read for terminal items: the memoized path
// when EnvelopeMemo is wired (the Workbench loop), and the plain readEventJSON
// when called from a single-shot path (active_packet_proof, tests).
func (s *Store) readEnvelopeJSON(memo *map[string]envelopeMemoEntry, ref string, v any) error {
	if memo == nil {
		return s.readEventJSON(ref, v)
	}
	bytes, err := s.memoEnvelope(memo, ref)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

func (s *Store) workbenchItem(w envelope.WorkEnvelope, events []Event, report *WorkbenchReport, holdEvidence holdEvidenceIndex, now time.Time) WorkbenchItem {
	return s.workbenchItemWithCaches(w, events, report, holdEvidence, now, workbenchItemCaches{})
}

func (s *Store) workbenchItemWithCaches(w envelope.WorkEnvelope, events []Event, report *WorkbenchReport, holdEvidence holdEvidenceIndex, now time.Time, caches workbenchItemCaches) WorkbenchItem {
	// COMPOUND_PROFILE_WORKBENCH_SLOW_ITEMS_MS=<n>: only log items whose
	// total wall exceeds the threshold. Per-stage logging would serialise
	// through stderr's mutex and inflate the very cost we're measuring.
	slowThresholdMS := 0
	if v := os.Getenv("COMPOUND_PROFILE_WORKBENCH_SLOW_ITEMS_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			slowThresholdMS = n
		}
	}
	profileSlow := slowThresholdMS > 0
	itemT := time.Now()
	subLog := func(label string) {
		if profileSlow {
			fmt.Fprintf(os.Stderr, "[WB-SLOW-STG] %s %s=%s\n", w.ID, label, time.Since(itemT))
		}
	}
	defer func() {
		if profileSlow {
			d := time.Since(itemT)
			if d >= time.Duration(slowThresholdMS)*time.Millisecond {
				fmt.Fprintf(os.Stderr, "[WB-SLOW] %s state=%s total=%s\n", w.ID, w.State, d)
			}
		}
	}()
	item := WorkbenchItem{
		WorkID:             w.ID,
		State:              w.State,
		Owner:              w.Owner,
		ExpectedBaseSHA:    w.ExpectedBaseSHA,
		SuccessPredicate:   w.SuccessPredicate,
		DesiredOutcome:     w.DesiredOutcome,
		CanonicalSurface:   w.CanonicalSurface,
		ContextPackID:      w.IDs.ContextPackID,
		PolicyDecisionID:   w.PolicyDecisionID,
		TransitionID:       w.IDs.TransitionID,
		NextAction:         nextWorkAction(w.State),
		OpenReason:         openReason(w.State),
		EvidenceRefs:       append([]string{}, w.EvidenceRefs...),
		ArtifactIDs:        append([]string{}, w.ArtifactIDs...),
		VerificationIDs:    append([]string{}, w.VerificationIDs...),
		MemoryCandidateIDs: append([]string{}, w.MemoryCandidateIDs...),
		recentAt:           w.CreatedAt,
	}
	if n := len(w.ActionIntentIDs); n > 0 {
		item.sourceActionIntentID = w.ActionIntentIDs[n-1]
	}
	if w.Origin != nil {
		item.AccountableOwner = w.Origin.AccountableOwner
		item.CreatorHarness = w.Origin.Harness
		item.CreatorRequestedModel = w.Origin.RequestedModel
		item.CreatorLaneID = w.Origin.LaneID
	}
	s.applyWorkbenchGitDeliveryWithFacts(&item, w, report, now, caches.LandingGitFacts)
	subLog("git_delivery")
	if caches.RunObservations != nil {
		if len(caches.RunObservations) > 0 {
			item.RunObservations = caches.RunObservations
		}
	} else if observations, warnings := s.workbenchRunObservations(w.ID); len(observations) > 0 || len(warnings) > 0 {
		item.RunObservations = observations
		for _, warning := range warnings {
			report.Integrity = append(report.Integrity, w.ID+": "+warning)
		}
	}
	if artifacts, warnings := s.workbenchRunArtifacts(w.ID); len(artifacts) > 0 || len(warnings) > 0 {
		item.RunArtifacts = artifacts
		report.Counts.RunArtifacts += len(artifacts)
		for _, warning := range warnings {
			report.Integrity = append(report.Integrity, w.ID+": "+warning)
		}
	}
	artifactCandidateIDs, warnings := s.workbenchArtifactMemoryCandidateIDs(item.RunArtifacts)
	item.MemoryCandidateIDs = mergeUnique(item.MemoryCandidateIDs, artifactCandidateIDs)
	for _, warning := range warnings {
		report.Integrity = append(report.Integrity, w.ID+": "+warning)
	}
	memoryCandidates, warnings := s.workbenchMemoryCandidates(item.RunArtifacts)
	for _, candidate := range memoryCandidates {
		item.MemoryCandidateIDs = mergeUnique(item.MemoryCandidateIDs, []string{candidate.ID})
	}
	for _, warning := range warnings {
		report.Integrity = append(report.Integrity, w.ID+": "+warning)
	}
	if reviewCards := s.workbenchReviewCards(w.ID, item.RunArtifacts, memoryCandidates); len(reviewCards) > 0 {
		item.ReviewCards = reviewCards
	}
	item.ReviewCards = append(item.ReviewCards, s.workbenchOperatorGateCardsWithCache(w, item, caches.ASCSignals)...)
	item.ReviewCards = markStaleHoldReviewCards(w, item, item.ReviewCards, holdEvidence)
	if len(item.ReviewCards) > 0 && (w.State == "held" || w.State == "blocked") {
		item.NextAction = firstNonEmpty(item.ReviewCards[0].NextAction, item.ReviewCards[0].Title)
	}
	if actionContext, warnings := s.actionContextFromCreatePlan(w, events); actionContext != nil || len(warnings) > 0 {
		item.ActionContext = actionContext
		if actionContext != nil {
			item.EvidenceRefs = mergeUnique(item.EvidenceRefs, actionContext.EvidenceRefs)
			if w.State == "proposed" || w.State == "admitted" {
				item.NextAction = "prepare ready with reviewed action context; do not execute action until claimed/running"
			}
		}
		for _, warning := range warnings {
			report.Integrity = append(report.Integrity, w.ID+": "+warning)
		}
	}
	if lease, err := s.readLease(w.ID); err == nil {
		item.ClaimedBy = lease.By
		item.ClaimedAt = lease.ClaimAt
	}
	if terminalStates[w.State] {
		if item.TransitionID != "" {
			var tr envelope.Transition
			readErr := s.readEnvelopeJSON(caches.EnvelopeMemo, item.TransitionID, &tr)
			if readErr != nil {
				report.Integrity = append(report.Integrity, w.ID+": transition "+item.TransitionID+" unreadable: "+readErr.Error())
			} else if err := tr.Validate(); err != nil {
				report.Integrity = append(report.Integrity, w.ID+": transition "+item.TransitionID+" invalid: "+err.Error())
			} else if tr.IDs.WorkID != w.ID {
				report.Integrity = append(report.Integrity, w.ID+": transition "+item.TransitionID+" belongs to work "+tr.IDs.WorkID)
			} else {
				receiptRefs := compactPrefix(tr.AfterRefs, "rec_")
				switch len(receiptRefs) {
				case 0:
					report.Integrity = append(report.Integrity, w.ID+": transition "+item.TransitionID+" has no closing receipt ref")
				case 1:
					item.ReceiptID = receiptRefs[0]
				default:
					report.Integrity = append(report.Integrity, w.ID+": transition "+item.TransitionID+" has multiple closing receipt refs")
				}
				item.recentAt = workbenchLatestTimestamp(item.recentAt, tr.CreatedAt)
			}
		}
		if item.ReceiptID != "" {
			var rec envelope.Receipt
			if err := s.readEnvelopeJSON(caches.EnvelopeMemo, item.ReceiptID, &rec); err != nil {
				report.Integrity = append(report.Integrity, w.ID+": receipt "+item.ReceiptID+" unreadable: "+err.Error())
			} else if err := rec.Validate(); err != nil {
				report.Integrity = append(report.Integrity, w.ID+": receipt "+item.ReceiptID+" invalid: "+err.Error())
			} else if rec.IDs.WorkID != w.ID {
				report.Integrity = append(report.Integrity, w.ID+": receipt "+item.ReceiptID+" belongs to work "+rec.IDs.WorkID)
				item.ReceiptID = ""
			} else {
				item.MoveType = rec.MoveType
				item.Changed = rec.EffectiveChanged()
				item.EvidenceRefs = mergeUnique(item.EvidenceRefs, rec.ProofRefs)
				item.ReviewTrigger = rec.Next.ReviewTrigger
				item.recentAt = workbenchLatestTimestamp(item.recentAt, rec.CreatedAt)
			}
		}
	}
	item.ObservatoryTags = workbenchObservatoryTags(item)
	return item
}

func (s *Store) applyWorkbenchGitDelivery(item *WorkbenchItem, w envelope.WorkEnvelope, report *WorkbenchReport, now time.Time) {
	s.applyWorkbenchGitDeliveryWithFacts(item, w, report, now, nil)
}

// applyWorkbenchGitDeliveryWithFacts projects ONE Work's Git delivery verdict.
// The evaluation is per-Work: envelope validation, the bound contract, the
// attached candidate artifact, proof resolution, publication, and lineage are
// never shared. Only the repository-scoped Git read facts may come from the
// request-local session, so a board of Works over one repository does not
// repeat the common reads per item.
func (s *Store) applyWorkbenchGitDeliveryWithFacts(item *WorkbenchItem, w envelope.WorkEnvelope, report *WorkbenchReport, now time.Time, facts *landingGitFacts) {
	if item == nil {
		return
	}
	terminal := terminalStates[w.State]
	_, tip, found, valid := latestCommitArtifact(w.ArtifactIDs)
	item.CandidateTipSHA = tip
	if w.GitDelivery == nil {
		if found && !terminal {
			item.LandingStatus = "missing_git_delivery_contract"
			item.NextAction = "replace this incomplete code work with a WorkEnvelope carrying a complete git_delivery contract"
		}
		return
	}
	contract := *w.GitDelivery
	item.TargetWorktreeRoot = contract.TargetWorktreeRoot
	item.RepositoryFingerprint = contract.RepositoryFingerprint
	item.TargetLocalRef = contract.TargetLocalRef
	item.ExpectedBaseSHA = contract.ExpectedBaseSHA
	item.PublicationMode = contract.PublicationMode
	item.PublicationRef = contract.PublicationRef
	if gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery) {
		if !terminal {
			item.LandingStatus = "repository_identity_migration_required"
			item.NextAction = repositoryIdentityMigrationNextAction(w)
		}
		return
	}
	item.GitDeliveryBound = true
	if terminal {
		return
	}
	switch {
	case !found:
		item.LandingStatus = "awaiting_candidate"
		item.NextAction = "attach the final verified candidate as commit:<full-sha>"
		return
	case !valid:
		item.LandingStatus = "invalid_candidate_artifact"
		item.NextAction = "replace the malformed latest commit artifact with commit:<full-sha>"
		return
	}

	live, err := s.evaluateLandCheckWithFacts(w, LandCheckOptions{CandidateTipSHA: tip}, now, facts)
	if err != nil {
		item.LandingStatus = "delivery_check_error"
		item.NextAction = "repair the bound Git delivery target, then rerun compound work land-check " + w.ID + " --tip " + tip
		report.Integrity = append(report.Integrity, w.ID+": Git delivery check failed: "+err.Error())
		return
	}
	switch live.Status {
	case "missing_source_bound_proof":
		item.LandingStatus = "awaiting_proof"
		item.NextAction = live.NextAction
	case "awaiting_publication":
		item.LandingStatus = "awaiting_publication"
		item.NextAction = live.NextAction
	case "landed":
		if _, err := s.recordedLandedProofForClose(w, landCheckEvidenceRefs(w.EvidenceRefs)); err != nil {
			item.LandingStatus = "awaiting_proof_recording"
			item.NextAction = "record the current delivery proof with compound work land-check " + w.ID + " --tip " + tip
			return
		}
		item.LandingStatus = "ready_to_close"
		item.NextAction = "close with the attached current Git delivery proof"
	default:
		item.LandingStatus = "awaiting_local_land"
		item.NextAction = firstNonEmpty(live.NextAction, "land the candidate in the WorkEnvelope-bound target and rerun land-check")
	}
}

func repositoryIdentityMigrationNextAction(w envelope.WorkEnvelope) string {
	if w.GitDelivery == nil {
		return ""
	}
	contract := w.GitDelivery
	command := fmt.Sprintf("compound work advance %s --to %s --by operator:<id> --target-worktree-root %q --target-local-ref %s --expected-base-sha %s",
		w.ID, w.State, contract.TargetWorktreeRoot, contract.TargetLocalRef, contract.ExpectedBaseSHA)
	if contract.PublicationMode == "local_only" {
		command += " --local-only"
	} else {
		command += " --publication-ref " + contract.PublicationRef
	}
	return "review the legacy repository identity, then run exact operator same-state migration: " + command
}

type routeOutcomeIndex struct {
	byWork    map[string][]RouteOutcomeSummary
	byReceipt map[string][]RouteOutcomeSummary
}

type routeOutcomeArtifact struct {
	SchemaVersion   string `json:"schema_version"`
	ID              string `json:"id"`
	CreatedAt       string `json:"created_at"`
	Status          string `json:"status"`
	WorkID          string `json:"work_id,omitempty"`
	ReceiptID       string `json:"receipt_id"`
	RouteEvidenceID string `json:"route_evidence_id"`
	SelectedTarget  struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	} `json:"selected_target"`
	Assessment struct {
		Class              string `json:"class"`
		ShouldReviewPolicy bool   `json:"should_review_policy"`
	} `json:"assessment"`
}

func (s *Store) workbenchRouteOutcomeIndex() (routeOutcomeIndex, []string) {
	index := routeOutcomeIndex{
		byWork:    map[string][]RouteOutcomeSummary{},
		byReceipt: map[string][]RouteOutcomeSummary{},
	}
	dir := filepath.Join(s.home, "reviews", "route-remorse")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return index, nil
		}
		return index, []string{"route outcome reviews unreadable: " + err.Error()}
	}
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, "route outcome review "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		var review routeOutcomeArtifact
		if err := json.Unmarshal(data, &review); err != nil {
			warnings = append(warnings, "route outcome review "+entry.Name()+" invalid: "+err.Error())
			continue
		}
		if review.SchemaVersion != "compound.route_outcome_review.v0.1" || strings.TrimSpace(review.ID) == "" {
			warnings = append(warnings, "route outcome review "+entry.Name()+" invalid: missing route outcome schema/id")
			continue
		}
		summary := RouteOutcomeSummary{
			ID:                 review.ID,
			CreatedAt:          review.CreatedAt,
			Status:             review.Status,
			ReviewRef:          "route_outcome_review:" + review.ID,
			RouteEvidenceID:    review.RouteEvidenceID,
			ReceiptID:          review.ReceiptID,
			AssessmentClass:    review.Assessment.Class,
			ShouldReviewPolicy: review.Assessment.ShouldReviewPolicy,
			Provider:           review.SelectedTarget.Provider,
			Model:              review.SelectedTarget.Model,
			Protocol:           review.SelectedTarget.Protocol,
		}
		if review.WorkID != "" {
			index.byWork[review.WorkID] = append(index.byWork[review.WorkID], summary)
		}
		if review.ReceiptID != "" {
			index.byReceipt[review.ReceiptID] = append(index.byReceipt[review.ReceiptID], summary)
		}
	}
	index.sort()
	return index, warnings
}

func (idx routeOutcomeIndex) sort() {
	for key := range idx.byWork {
		sortRouteOutcomeSummaries(idx.byWork[key])
	}
	for key := range idx.byReceipt {
		sortRouteOutcomeSummaries(idx.byReceipt[key])
	}
}

func sortRouteOutcomeSummaries(out []RouteOutcomeSummary) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return workbenchTimestampAfter(out[i].CreatedAt, out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
}

func (idx routeOutcomeIndex) attach(item *WorkbenchItem) {
	if item == nil {
		return
	}
	seen := map[string]bool{}
	for _, existing := range item.RouteOutcomes {
		seen[existing.ID] = true
	}
	add := func(out RouteOutcomeSummary) {
		if out.ID == "" || seen[out.ID] {
			return
		}
		seen[out.ID] = true
		item.RouteOutcomes = append(item.RouteOutcomes, out)
		item.EvidenceRefs = mergeUnique(item.EvidenceRefs, []string{out.ReviewRef, out.RouteEvidenceID})
		item.recentAt = workbenchLatestTimestamp(item.recentAt, out.CreatedAt)
	}
	for _, out := range idx.byWork[item.WorkID] {
		add(out)
	}
	for _, out := range idx.byReceipt[item.ReceiptID] {
		add(out)
	}
	sortRouteOutcomeSummaries(item.RouteOutcomes)
}

func (s *Store) workbenchRunObservations(workID string) ([]RunObservationSummary, []string) {
	dir := filepath.Join(s.home, "run-observations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{"run observations unreadable: " + err.Error()}
	}
	out := []RunObservationSummary{}
	warnings := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var result RunObservationResult
		s.runObservationFileReads.Add(1)
		if err := s.readJSON(path, &result); err != nil {
			warnings = append(warnings, "run observation "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		if result.WorkID != workID {
			continue
		}
		out = append(out, RunObservationSummary{
			ID:               result.ID,
			GeneratedAt:      result.GeneratedAt,
			Actor:            result.Actor,
			Note:             result.Note,
			TraceID:          result.TraceID,
			State:            result.State,
			ActionIntentID:   result.ActionIntentID,
			PolicyDecisionID: result.PolicyDecisionID,
			ResultPath:       path,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GeneratedAt != out[j].GeneratedAt {
			return out[i].GeneratedAt > out[j].GeneratedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, warnings
}

// workbenchRunObservationsIndex reads run-observations/ ONCE and groups the
// summaries by work_id. Replaces the per-item ReadDir-and-parse loop in
// workbenchRunObservations, which was the dominant cost of the item loop on a
// live home (790 items × 856 observation files = ~677k reads+parses per
// workbench call).
func (s *Store) workbenchRunObservationsIndex() (map[string][]RunObservationSummary, []string) {
	dir := filepath.Join(s.home, "run-observations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]RunObservationSummary{}, nil
		}
		return nil, []string{"run observations unreadable: " + err.Error()}
	}
	byWork := make(map[string][]RunObservationSummary, len(entries))
	warnings := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var result RunObservationResult
		s.runObservationFileReads.Add(1)
		if err := s.readJSON(path, &result); err != nil {
			warnings = append(warnings, "run observation "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		if strings.TrimSpace(result.WorkID) == "" {
			continue
		}
		byWork[result.WorkID] = append(byWork[result.WorkID], RunObservationSummary{
			ID:               result.ID,
			GeneratedAt:      result.GeneratedAt,
			Actor:            result.Actor,
			Note:             result.Note,
			TraceID:          result.TraceID,
			State:            result.State,
			ActionIntentID:   result.ActionIntentID,
			PolicyDecisionID: result.PolicyDecisionID,
			ResultPath:       path,
		})
	}
	for workID := range byWork {
		summaries := byWork[workID]
		sort.Slice(summaries, func(i, j int) bool {
			if summaries[i].GeneratedAt != summaries[j].GeneratedAt {
				return summaries[i].GeneratedAt > summaries[j].GeneratedAt
			}
			return summaries[i].ID > summaries[j].ID
		})
		byWork[workID] = summaries
	}
	return byWork, warnings
}

func (s *Store) workbenchRunArtifacts(workID string) ([]WorkbenchRunArtifact, []string) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return nil, nil
	}
	dir := filepath.Join(s.home, "runs", workID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{"run artifacts unreadable: " + err.Error()}
	}
	var out []WorkbenchRunArtifact
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			warnings = append(warnings, "run artifact "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		artifact := WorkbenchRunArtifact{
			Ref:   filepath.ToSlash(filepath.Join("runs", workID, entry.Name())),
			Kind:  workbenchRunArtifactKind(entry.Name()),
			Bytes: info.Size(),
		}
		if strings.HasSuffix(entry.Name(), ".json") && info.Size() > 0 {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				warnings = append(warnings, "run artifact "+entry.Name()+" unreadable: "+err.Error())
				continue
			}
			artifact = enrichWorkbenchRunArtifact(artifact, data)
		}
		out = append(out, artifact)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Ref < out[j].Ref
	})
	return out, warnings
}

func workbenchRunArtifactKind(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case name == "knowledge-extract-summary.json":
		return "knowledge_extract_summary"
	case strings.HasPrefix(name, "knowledge-extract-"):
		return "knowledge_extract"
	case strings.HasPrefix(name, "knowledge-import-"):
		return "knowledge_import"
	case strings.Contains(name, "knowledge-export"):
		return "knowledge_export"
	case strings.Contains(name, "manifest"):
		return "source_manifest"
	case strings.HasPrefix(name, "panel-plan"):
		return "panel_plan"
	case strings.HasPrefix(name, "panel-run"):
		return "panel_run"
	case strings.Contains(name, "promoted") && strings.Contains(name, "claims"):
		return "promotion_summary"
	default:
		return "artifact"
	}
}

func enrichWorkbenchRunArtifact(artifact WorkbenchRunArtifact, data []byte) WorkbenchRunArtifact {
	var payload struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		Status        string `json:"status"`
		TargetKind    string `json:"target_kind"`
		Targets       []struct {
			Ref string `json:"ref"`
		} `json:"targets"`
		CandidateIDs []string `json:"candidate_ids"`
		TotalCreated int      `json:"total_created"`
		Count        *int     `json:"count"`
		Refs         []string `json:"refs"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return artifact
	}
	artifact.Schema = strings.TrimSpace(payload.SchemaVersion)
	artifact.ID = strings.TrimSpace(payload.ID)
	artifact.Status = strings.TrimSpace(payload.Status)
	artifact.TargetKind = strings.TrimSpace(payload.TargetKind)
	for _, target := range payload.Targets {
		artifact.TargetRefs = appendUniqueExact(artifact.TargetRefs, target.Ref)
	}
	artifact.CandidateIDs = appendUniqueExact(artifact.CandidateIDs, payload.CandidateIDs...)
	if artifact.Kind == "knowledge_extract_summary" && payload.TotalCreated > 0 {
		count := payload.TotalCreated
		artifact.Count = &count
	}
	if payload.Count != nil {
		count := *payload.Count
		artifact.Count = &count
	}
	if len(payload.Refs) > 0 {
		artifact.TargetRefs = appendUniqueExact(artifact.TargetRefs, payload.Refs...)
	}
	return artifact
}

func appendUniqueExact(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" || slices.Contains(dst, value) {
			continue
		}
		dst = append(dst, value)
	}
	return dst
}

type workbenchMemoryCandidate struct {
	ID      string
	Panel   WorkbenchRunArtifact
	Summary memoryCandidateReviewSummary
}

func (s *Store) workbenchArtifactMemoryCandidateIDs(artifacts []WorkbenchRunArtifact) ([]string, []string) {
	var ids []string
	var warnings []string
	for _, artifact := range artifacts {
		for _, id := range artifact.CandidateIDs {
			if _, err := candidates.ReadRecord(s.home, candidates.Memory, id); err != nil {
				warnings = append(warnings, "memory candidate artifact corrupt: "+err.Error())
				continue
			}
			ids = mergeUnique(ids, []string{id})
		}
	}
	return ids, warnings
}

func (s *Store) workbenchMemoryCandidates(artifacts []WorkbenchRunArtifact) ([]workbenchMemoryCandidate, []string) {
	var out []workbenchMemoryCandidate
	var warnings []string
	for _, artifact := range artifacts {
		if artifact.Kind != "panel_run" || artifact.Status != "applied" || artifact.TargetKind != "memory_candidate" {
			continue
		}
		candidateID, err := workbenchMemoryCandidateRef(artifact.TargetRefs)
		if err != nil {
			warnings = append(warnings, "memory candidate artifact corrupt: "+err.Error())
			continue
		}
		summary, err := s.memoryCandidateReviewSummary(candidateID)
		if err != nil {
			warnings = append(warnings, "memory candidate artifact corrupt: "+err.Error())
			continue
		}
		out = append(out, workbenchMemoryCandidate{ID: candidateID, Panel: artifact, Summary: summary})
	}
	return out, warnings
}

func (s *Store) workbenchReviewCards(workID string, artifacts []WorkbenchRunArtifact, memoryCandidates []workbenchMemoryCandidate) []WorkbenchReviewCard {
	var promotion WorkbenchRunArtifact
	for _, artifact := range artifacts {
		if artifact.Kind == "promotion_summary" && promotion.Ref == "" && artifact.Count != nil && *artifact.Count == 0 {
			promotion = artifact
		}
	}
	if promotion.Ref == "" {
		return nil
	}
	for _, candidate := range memoryCandidates {
		if candidate.Summary.State == "promoted" || candidate.Summary.State == "rejected" || candidate.Summary.State == "no_promote" {
			continue
		}
		panelSummary := s.panelRunReviewSummary(candidate.Panel.Ref)
		sourceSummary := s.memoryCandidateSourceSummary(workID, candidate.Summary.ItemRef, candidate.Summary.Object)
		command := []string{"compound", "workbench", "--json"}
		if candidate.Panel.ID != "" {
			command = []string{"compound", "panel", "show", candidate.Panel.ID, "--json"}
		}
		title := "operator adjudication needed for " + candidate.ID + "; inspect applied panel run and keep promotion explicit"
		card := WorkbenchReviewCard{
			ID:                     "memory-review-" + workID + "-" + candidate.ID,
			Kind:                   "memory_candidate_adjudication",
			Title:                  title,
			WorkID:                 workID,
			CandidateID:            candidate.ID,
			CandidateState:         candidate.Summary.State,
			CandidateEntity:        candidate.Summary.Entity,
			CandidatePredicate:     candidate.Summary.Predicate,
			CandidateObject:        candidate.Summary.Object,
			CandidateConfidence:    candidate.Summary.Confidence,
			CandidateVisibility:    candidate.Summary.Visibility,
			CandidateSourceRef:     candidate.Summary.SourceRef,
			CandidateItemRef:       candidate.Summary.ItemRef,
			CandidateExtraction:    candidate.Summary.ExtractionID,
			CandidateExtractedBy:   candidate.Summary.ExtractedBy,
			CandidateSourceKind:    candidate.Summary.SourceKind,
			CandidateSourceTitle:   sourceSummary.Title,
			CandidateSourceExcerpt: sourceSummary.Excerpt,
			PanelRunID:             candidate.Panel.ID,
			PanelRunRef:            candidate.Panel.Ref,
			PanelDecision:          panelSummary.Decision,
			PanelConfidence:        panelSummary.Confidence,
			PanelRationale:         panelSummary.Rationale,
			PanelFindingCount:      panelSummary.FindingCount,
			PanelRiskCount:         panelSummary.RiskCount,
			PanelFailedCount:       panelSummary.FailedCount,
			PromotionSummaryRef:    promotion.Ref,
			PromotionCount:         promotion.Count,
			Command:                command,
			RequiredInputs: []string{
				"operator verdict for exactly one conversation-derived memory candidate",
				"candidate " + candidate.ID + " reviewed against panel run " + reviewartifactOrDash(candidate.Panel.ID),
			},
			ExpectedValidation: []string{
				"candidate " + candidate.ID + " remains parked/proposed until an operator adjudication act",
				"promotion summary remains zero before explicit promotion",
				"panel run " + reviewartifactOrDash(candidate.Panel.ID) + " is inspected as evidence, not treated as promotion",
			},
			StopConditions: []string{
				"candidate already promoted, rejected, or no_promote",
				"operator verdict absent or selects more than one candidate",
				"requested action would run import, extraction, panel run, mass import, or automatic promotion",
			},
			Boundary:         "operator review/adjudication card only; no automatic memory promotion, recall proof, import, extraction, or held-valve clearing",
			Proof:            "Workbench shows candidate id, parked/proposed state, applied panel-run ref, promotion_summary count=0, and explicit stop conditions",
			RequiresOperator: true,
		}
		return []WorkbenchReviewCard{card}
	}
	return nil
}

func (s *Store) workbenchOperatorGateCards(w envelope.WorkEnvelope, item WorkbenchItem) []WorkbenchReviewCard {
	return s.workbenchOperatorGateCardsWithCache(w, item, nil)
}

func (s *Store) workbenchOperatorGateCardsWithCache(w envelope.WorkEnvelope, item WorkbenchItem, cached *ascProductSignalState) []WorkbenchReviewCard {
	if w.State != "held" && w.State != "blocked" {
		return nil
	}
	context := strings.ToLower(strings.Join([]string{
		w.SuccessPredicate,
		w.DesiredOutcome,
		w.CanonicalSurface,
		strings.Join(item.ObservatoryTags, " "),
	}, " "))
	if !(strings.Contains(context, "asc") || strings.Contains(context, "app-store-connect") || strings.Contains(context, "app store connect")) {
		return nil
	}
	ascSignals := cached
	if ascSignals == nil {
		fresh := s.ascProductSignalState()
		ascSignals = &fresh
	}
	if ascSignals.Count > 0 {
		card := WorkbenchReviewCard{
			ID:              "operator-gate-" + w.ID + "-app-store-connect-refresh-decision",
			Kind:            "source_refresh_gate",
			Title:           "ASC product signals observed: " + w.ID + "; choose automatic refresh / manual refresh / close held work",
			NextAction:      "ASC signals observed; choose scheduler load / manual refresh / close held work with proof.",
			ConditionStatus: "observed",
			WorkID:          w.ID,
			GateID:          "asc-daily-refresh",
			SourceKind:      "app_store_connect",
			Command:         []string{"compound", "source", "connections", "--json"},
			RequiredInputs: []string{
				"operator decision: scheduler load / manual refresh / close held work with proof",
				"proof that current ASC product signals are sufficient for this held work's next step",
			},
			ExpectedValidation: []string{
				"source connections shows asc_daily_signals observed from product-signal evidence",
				"product authority shows App Store Connect product signals; no product surface changed",
				"held work remains held until an explicit governed close or operator-approved advance",
			},
			StopConditions: []string{
				"requested action would mutate App Store Connect, publish, spend, or alter product surfaces",
				"operator has not decided scheduler load / manual refresh / close",
				"product signal evidence is missing, stale, or belongs to the wrong product",
			},
			Boundary:         "read-model decision card only; product signals are evidence; scheduler load and held-work close remain explicit operator decisions",
			Proof:            "Workbench sees ASC product signal evidence in ledger/product-signals.jsonl and changes the held card from private-config setup to refresh/close decision without clearing the held work",
			RequiresOperator: true,
		}
		if ascSignals.LastSeen != "" {
			card.TriggeringEvidenceRef = "product-signals:app_store_connect:last_seen:" + ascSignals.LastSeen
		}
		return []WorkbenchReviewCard{card}
	}
	card := WorkbenchReviewCard{
		ID:         "operator-gate-" + w.ID + "-app-store-connect-private-config",
		Kind:       "source_private_config_gate",
		Title:      "operator private ASC config needed for " + w.ID + "; run read-only preflight before apply or scheduler",
		WorkID:     w.ID,
		GateID:     "asc-private-config",
		SourceKind: "app_store_connect",
		Command:    []string{"bun", "tools/asc_preflight.ts", "--config", "<private-asc-config.json>", "--json"},
		RequiredInputs: []string{
			"private ASC product mapping config",
			"operator keeps credential smoke and scheduler checks read-only",
		},
		ExpectedValidation: []string{
			"asc preflight returns status ok or warn",
			"asc_credential_smoke=ok evidence ref names credential readiness",
			"product authority checked for mapped products",
			"no product signal apply and no scheduler load",
		},
		StopConditions: []string{
			"private config missing or invalid",
			"product authority missing mapping",
			"credential smoke fails before scheduler load",
			"requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work",
		},
		Boundary:         "operator supplies private config; read-only ASC preflight only; no live fetch, product signal apply, scheduler load, or held-valve clearing",
		Proof:            "Workbench shows ASC private-config gate with exact preflight command, validation, stop conditions, and no apply/scheduler mutation",
		RequiresOperator: true,
	}
	return []WorkbenchReviewCard{card}
}

type ascProductSignalState struct {
	Count    int
	LastSeen string
}

type workbenchProductSignalRow struct {
	ID           string   `json:"id"`
	MetricName   string   `json:"metric_name"`
	ObservedAt   string   `json:"observed_at"`
	EvidenceRefs []string `json:"evidence_refs"`
}

func (s *Store) ascProductSignalState() ascProductSignalState {
	path := filepath.Join(s.home, "ledger", "product-signals.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return ascProductSignalState{}
	}
	var state ascProductSignalState
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row workbenchProductSignalRow
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.ID == "" || !workbenchProductSignalIsASC(row) {
			continue
		}
		state.Count++
		state.LastSeen = workbenchLatestTimestamp(state.LastSeen, row.ObservedAt)
	}
	return state
}

func workbenchProductSignalIsASC(row workbenchProductSignalRow) bool {
	if strings.HasPrefix(strings.TrimSpace(row.MetricName), "app_store_connect_") {
		return true
	}
	for _, ref := range row.EvidenceRefs {
		if strings.HasPrefix(strings.TrimSpace(ref), "asc:") {
			return true
		}
	}
	return false
}

func workbenchMemoryCandidateRef(refs []string) (string, error) {
	if len(refs) != 1 {
		return "", fmt.Errorf("expected exactly one memory candidate reference, got %d", len(refs))
	}
	if refs[0] == "" {
		return "", errors.New("memory candidate reference is blank")
	}
	return refs[0], nil
}

type memoryCandidateReviewSummary struct {
	State        string
	Entity       string
	Predicate    string
	Object       string
	Confidence   *float64
	Visibility   string
	SourceRef    string
	ItemRef      string
	ExtractionID string
	ExtractedBy  string
	SourceKind   string
}

type panelRunReviewSummary struct {
	Decision     string
	Confidence   *float64
	Rationale    string
	FindingCount int
	RiskCount    int
	FailedCount  int
}

func (s *Store) memoryCandidateReviewSummary(id string) (memoryCandidateReviewSummary, error) {
	record, err := candidates.ReadRecord(s.home, candidates.Memory, id)
	if err != nil {
		return memoryCandidateReviewSummary{}, err
	}
	var payload struct {
		ID             string   `json:"id"`
		State          string   `json:"state"`
		Entity         string   `json:"entity"`
		Predicate      string   `json:"predicate"`
		Object         string   `json:"object"`
		Confidence     *float64 `json:"confidence"`
		Visibility     string   `json:"visibility"`
		SourceTraceRef string   `json:"source_trace_ref"`
		Qualifiers     struct {
			ExtractionID string `json:"extraction_id"`
			ExtractedBy  string `json:"extracted_by"`
			ItemRef      string `json:"item_ref"`
			Source       string `json:"source"`
		} `json:"qualifiers"`
	}
	if err := json.Unmarshal(record.Body, &payload); err != nil {
		return memoryCandidateReviewSummary{}, err
	}
	return memoryCandidateReviewSummary{
		State:        strings.TrimSpace(payload.State),
		Entity:       strings.TrimSpace(payload.Entity),
		Predicate:    strings.TrimSpace(payload.Predicate),
		Object:       strings.TrimSpace(payload.Object),
		Confidence:   payload.Confidence,
		Visibility:   strings.TrimSpace(payload.Visibility),
		SourceRef:    strings.TrimSpace(payload.SourceTraceRef),
		ItemRef:      strings.TrimSpace(payload.Qualifiers.ItemRef),
		ExtractionID: strings.TrimSpace(payload.Qualifiers.ExtractionID),
		ExtractedBy:  strings.TrimSpace(payload.Qualifiers.ExtractedBy),
		SourceKind:   strings.TrimSpace(payload.Qualifiers.Source),
	}, nil
}

type memoryCandidateSourceSummary struct {
	Title   string
	Excerpt string
}

func (s *Store) memoryCandidateSourceSummary(workID, itemRef, claim string) memoryCandidateSourceSummary {
	workID = strings.TrimSpace(workID)
	itemRef = strings.TrimSpace(itemRef)
	if workID == "" || itemRef == "" {
		return memoryCandidateSourceSummary{}
	}
	dir := filepath.Join(s.home, "runs", workID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return memoryCandidateSourceSummary{}
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || workbenchRunArtifactKind(name) != "knowledge_export" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var payload struct {
			Items []struct {
				Ref     string `json:"ref"`
				Title   string `json:"title"`
				Content string `json:"content"`
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		for _, item := range payload.Items {
			if strings.TrimSpace(item.Ref) != itemRef {
				continue
			}
			return memoryCandidateSourceSummary{
				Title:   strings.TrimSpace(item.Title),
				Excerpt: sourceExcerpt(strings.TrimSpace(item.Content), claim),
			}
		}
	}
	return memoryCandidateSourceSummary{}
}

func sourceExcerpt(content, claim string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	lower := strings.ToLower(content)
	var idx = -1
	for _, probe := range sourceExcerptProbes(claim) {
		if probe == "" {
			continue
		}
		if found := strings.Index(lower, strings.ToLower(probe)); found >= 0 {
			idx = found
			break
		}
	}
	if idx < 0 {
		idx = 0
	}
	start := idx
	for start > 0 && content[start-1] != '\n' {
		start--
	}
	if start > 0 {
		prev := strings.LastIndex(content[:start-1], "\n")
		if prev >= 0 {
			start = prev + 1
		}
	}
	end := idx + 420
	if end > len(content) {
		end = len(content)
	}
	for end < len(content) && content[end-1] != '\n' {
		end++
	}
	excerpt := strings.TrimSpace(content[start:end])
	excerpt = strings.Join(strings.Fields(excerpt), " ")
	if len(excerpt) > 520 {
		excerpt = strings.TrimSpace(excerpt[:520]) + "..."
	}
	return excerpt
}

func sourceExcerptProbes(claim string) []string {
	claim = strings.ToLower(claim)
	var probes []string
	if strings.Contains(claim, "personal cognition runtime") {
		probes = append(probes, "personal cognition runtime")
	}
	if strings.Contains(claim, "better memory system") {
		probes = append(probes, "better memory system")
	}
	if strings.Contains(claim, "self-model") {
		probes = append(probes, "self-model")
	}
	if strings.Contains(claim, "evidence loop") {
		probes = append(probes, "evidence loop")
	}
	return append(probes, "")
}

func (s *Store) panelRunReviewSummary(ref string) panelRunReviewSummary {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return panelRunReviewSummary{}
	}
	path := filepath.Join(s.home, ref)
	var payload struct {
		Findings []struct {
			LensFindings []struct {
				Status   string `json:"status"`
				Findings []struct {
					Severity string `json:"severity"`
				} `json:"findings"`
			} `json:"lens_findings"`
			Synthesis struct {
				Decision   string   `json:"decision"`
				Confidence *float64 `json:"confidence"`
				Rationale  string   `json:"rationale"`
			} `json:"synthesis"`
		} `json:"findings"`
	}
	if err := s.readJSON(path, &payload); err != nil || len(payload.Findings) == 0 {
		return panelRunReviewSummary{}
	}
	finding := payload.Findings[0]
	out := panelRunReviewSummary{
		Decision:   strings.TrimSpace(finding.Synthesis.Decision),
		Confidence: finding.Synthesis.Confidence,
		Rationale:  strings.TrimSpace(finding.Synthesis.Rationale),
	}
	for _, lens := range finding.LensFindings {
		if strings.TrimSpace(lens.Status) == "failed" {
			out.FailedCount++
		}
		for _, item := range lens.Findings {
			out.FindingCount++
			switch strings.ToLower(strings.TrimSpace(item.Severity)) {
			case "high", "medium", "error":
				out.RiskCount++
			}
		}
	}
	return out
}

func reviewartifactOrDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

func (s *Store) workbenchDispatchStepRuns(limit int, archiveIndex []home.OperationalArchiveEntry) ([]DispatchStepRunSummary, []string) {
	dir := filepath.Join(s.home, "dispatch-step-runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{"dispatch-step-runs unreadable: " + err.Error()}
	}
	var out []DispatchStepRunSummary
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var result DispatchStepRunResult
		if err := s.readJSON(path, &result); err != nil {
			warnings = append(warnings, "dispatch step run "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		summary := DispatchStepRunSummary{
			ID:          result.ID,
			GeneratedAt: result.GeneratedAt,
			Status:      result.Status,
			Actor:       result.Actor,
			StepCount:   result.StepCount,
			CanContinue: result.CanContinue,
			WaitReason:  result.WaitReason,
			BlockedOn:   append([]string{}, result.BlockedOn...),
			NextAction:  result.NextAction,
			ResultPath:  path,
		}
		for _, step := range result.Steps {
			summary.StepActions = append(summary.StepActions, step.Action)
			if step.WorkID != "" {
				summary.StepWorkIDs = append(summary.StepWorkIDs, step.WorkID)
			}
		}
		summary = s.resolveDispatchStepRunSummary(summary, archiveIndex)
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GeneratedAt != out[j].GeneratedAt {
			return out[i].GeneratedAt > out[j].GeneratedAt
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, warnings
}

func (s *Store) workbenchContinueRuns(limit int, archiveIndex []home.OperationalArchiveEntry) ([]ContinueRunSummary, []string) {
	dir := filepath.Join(s.home, "continue-runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{"continue-runs unreadable: " + err.Error()}
	}
	var out []ContinueRunSummary
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var result ContinueRunResult
		if err := s.readJSON(path, &result); err != nil {
			warnings = append(warnings, "continue run "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		summary := ContinueRunSummary{
			ID:          result.ID,
			GeneratedAt: result.GeneratedAt,
			Status:      result.Status,
			Actor:       result.Actor,
			StepCount:   result.StepCount,
			CanContinue: result.CanContinue,
			WaitReason:  result.WaitReason,
			BlockedOn:   append([]string{}, result.BlockedOn...),
			NextAction:  result.NextAction,
			ResultPath:  path,
		}
		for _, step := range result.Steps {
			summary.StepActions = append(summary.StepActions, step.Action)
			if step.WorkID != "" {
				summary.StepWorkIDs = append(summary.StepWorkIDs, step.WorkID)
			}
		}
		summary = s.resolveContinueRunSummary(summary, archiveIndex)
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GeneratedAt != out[j].GeneratedAt {
			return out[i].GeneratedAt > out[j].GeneratedAt
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, warnings
}

func (s *Store) resolveDispatchStepRunSummary(summary DispatchStepRunSummary, archiveIndex []home.OperationalArchiveEntry) DispatchStepRunSummary {
	if summary.Status != "waiting" || len(summary.BlockedOn) == 0 {
		return summary
	}
	if summary.WaitReason == "no_open_work" && dispatchNoOpenWorkWait(summary.BlockedOn) {
		summary.Status = "resolved"
		summary.CanContinue = false
		summary.NextAction = "no open work at run time; no dispatch action remains for this run"
		return summary
	}
	workIDs, ok := blockedWorkIDs(summary.BlockedOn)
	if !ok {
		return summary
	}
	terminalRefs, ok := s.terminalWorkRefs(workIDs, archiveIndex)
	if !ok {
		return summary
	}
	summary.Status = "resolved"
	summary.CanContinue = false
	summary.WaitReason = "terminal_work"
	summary.BlockedOn = terminalRefs
	summary.NextAction = "referenced work is terminal; no dispatch action required"
	return summary
}

// resolveContinueRunSummary mirrors resolveDispatchStepRunSummary for continue
// runs: when every work id a waiting run references (blocked_on work refs ∪
// step_work_ids) is terminal, the run has no continue action left. Projection
// only — the recorded run file is never mutated.
func (s *Store) resolveContinueRunSummary(summary ContinueRunSummary, archiveIndex []home.OperationalArchiveEntry) ContinueRunSummary {
	if summary.Status != "waiting" {
		return summary
	}
	workIDs, ok := blockedWorkIDs(summary.BlockedOn)
	if !ok {
		return summary
	}
	workIDs = append(workIDs, summary.StepWorkIDs...)
	terminalRefs, ok := s.terminalWorkRefs(workIDs, archiveIndex)
	if !ok {
		return summary
	}
	summary.Status = "resolved"
	summary.CanContinue = false
	summary.WaitReason = "terminal_work"
	summary.BlockedOn = terminalRefs
	summary.NextAction = "referenced work is terminal; no continue action required"
	return summary
}

// blockedWorkIDs extracts work ids from work.<reason>:<id> blocked_on entries.
// ok is false when any entry is not a work reference: a non-work blocker cannot
// be proven terminal, so the caller must stay waiting.
func blockedWorkIDs(blockedOn []string) ([]string, bool) {
	ids := make([]string, 0, len(blockedOn))
	for _, blocked := range blockedOn {
		workID, ok := dispatchBlockedWorkID(blocked)
		if !ok {
			return nil, false
		}
		ids = append(ids, workID)
	}
	return ids, true
}

// terminalWorkRefs returns deduplicated work.terminal:<id> refs when every work
// id resolves to a terminal WorkEnvelope. ok is false when the set is empty or
// any referenced work is missing or non-terminal.
func (s *Store) terminalWorkRefs(workIDs []string, archiveIndex []home.OperationalArchiveEntry) ([]string, bool) {
	seen := map[string]bool{}
	var refs []string
	for _, workID := range workIDs {
		workID = strings.TrimSpace(workID)
		if workID == "" || seen[workID] {
			continue
		}
		seen[workID] = true
		w, err := s.getWithArchiveIndex(workID, archiveIndex)
		if err != nil || !terminalStates[w.State] {
			return nil, false
		}
		refs = append(refs, "work.terminal:"+workID)
	}
	if len(refs) == 0 {
		return nil, false
	}
	return mergeUnique(refs), true
}

func dispatchNoOpenWorkWait(blockedOn []string) bool {
	for _, blocked := range blockedOn {
		if strings.TrimSpace(blocked) == "work.ready_or_source_packet" {
			return true
		}
	}
	return false
}

func dispatchBlockedWorkID(blocked string) (string, bool) {
	blocked = strings.TrimSpace(blocked)
	if !strings.HasPrefix(blocked, "work.") {
		return "", false
	}
	idx := strings.LastIndex(blocked, ":")
	if idx < 0 || idx+1 >= len(blocked) {
		return "", false
	}
	workID := strings.TrimSpace(blocked[idx+1:])
	if workID == "" {
		return "", false
	}
	return workID, true
}

func workbenchDispatchNext(report WorkbenchReport, actor string) WorkbenchDispatchNext {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "codex"
	}
	out := WorkbenchDispatchNext{Actor: actor}
	for _, item := range report.Open {
		if item.State == "verifying" && hasCloseProofEvidence(item) && !dispatchLeaseHeldByOther(item, actor) {
			out.CanRun = true
			out.Action = "close"
			out.Reason = "proof_ready_close"
			out.WorkID = item.WorkID
			break
		}
	}
	if !out.CanRun {
		for _, item := range report.Open {
			if item.State == "claimed" && item.ClaimedBy == actor {
				out.CanRun = true
				out.Action = "advance"
				out.Reason = "actor_claimed_work"
				out.WorkID = item.WorkID
				break
			}
		}
	}
	if !out.CanRun {
		for _, item := range report.Open {
			if item.State == "ready" {
				out.CanRun = true
				out.Action = "claim"
				out.Reason = "ready_work"
				out.WorkID = item.WorkID
				break
			}
		}
	}
	if out.CanRun {
		out.Command = []string{"compound", "work", "continue", "--actor", actor, "--expect-work", out.WorkID, "--until-wait"}
		return out
	}
	if report.Counts.Open == 0 && report.ActivePacketProof != nil {
		out.Action = "none"
		out.WorkID = report.ActivePacketProof.WorkID
		switch report.ActivePacketProof.Status {
		case "ok":
			out.Reason = "active_packet_terminal"
			out.WaitReason = "source_packet_terminal"
			out.BlockedOn = []string{"work.terminal", "work.terminal:" + report.ActivePacketProof.WorkID}
			return out
		case "risk":
			out.Reason = "active_packet_proof_risk"
			out.WaitReason = "active_packet_proof_risk"
			out.BlockedOn = []string{"active_packet_proof:" + report.ActivePacketProof.WorkID}
			return out
		}
	}
	out.WaitReason, out.BlockedOn = dispatchStepWait(DispatchStepResult{Actor: actor}, report)
	out.AdjacentProgress = workbenchAdjacentProgress(report, out)
	return out
}

func workbenchAdjacentProgress(report WorkbenchReport, next WorkbenchDispatchNext) *WorkbenchAdjacentProgress {
	if next.CanRun || next.WaitReason != "needs_review" {
		return nil
	}
	heldWorkIDs := heldValveWorkIDs(next.BlockedOn)
	if len(heldWorkIDs) == 0 {
		return nil
	}
	packetCount, packetRefs := adjacentPacketCandidates(report.OfferedPackets, next.Actor, 5)
	nonPacketActions := adjacentNonPacketActions(report, heldWorkIDs)
	progress := &WorkbenchAdjacentProgress{
		Available:            packetCount > 0 || len(nonPacketActions) > 0,
		Reason:               "direct dispatch is held on operator-only review valves; the broader Compound mission is not blocked",
		NextAction:           "keep held valves visible; move into an adjacent proof-producing packet, preflight, projection, doctor, or source bridge without clearing the held valve",
		PacketCandidateCount: packetCount,
		PacketRefs:           packetRefs,
		HeldWorkIDs:          heldWorkIDs,
		NonPacketActions:     nonPacketActions,
	}
	if packetCount > 0 {
		progress.Command = []string{"compound", "workbench", "--json"}
		if report.OfferedPackets != nil && strings.TrimSpace(report.OfferedPackets.SourceDir) != "" {
			progress.Command = append(progress.Command, "--packet-dir", report.OfferedPackets.SourceDir)
		}
	} else {
		progress.NextAction = "keep held valves visible; no actor-actionable packet candidates are currently offered, so use a listed non-packet proof action such as preflight, projection, doctor, or source bridge"
	}
	return progress
}

func adjacentNonPacketActions(report WorkbenchReport, heldWorkIDs []string) []WorkbenchAdjacentAction {
	held := map[string]bool{}
	for _, id := range heldWorkIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			held[id] = true
		}
	}
	context := strings.ToLower(heldWorkbenchContext(report.Open, held))
	var actions []WorkbenchAdjacentAction
	hasSourcePrivateConfigCard := false
	hasMemoryCandidateReviewCard := false
	add := func(action WorkbenchAdjacentAction) {
		if strings.TrimSpace(action.ID) == "" {
			return
		}
		for _, existing := range actions {
			if existing.ID == action.ID {
				return
			}
		}
		actions = append(actions, action)
	}
	for _, item := range report.Open {
		if !held[item.WorkID] {
			continue
		}
		for _, card := range item.ReviewCards {
			switch card.Kind {
			case "source_private_config_gate":
				hasSourcePrivateConfigCard = true
			case "memory_candidate_adjudication":
				hasMemoryCandidateReviewCard = true
			}
			add(WorkbenchAdjacentAction{
				ID:                 card.ID,
				Kind:               "review_card",
				Title:              card.Title,
				NextAction:         card.NextAction,
				Command:            append([]string{}, card.Command...),
				RequiredInputs:     append([]string{}, card.RequiredInputs...),
				ExpectedValidation: append([]string{}, card.ExpectedValidation...),
				StopConditions:     append([]string{}, card.StopConditions...),
				Boundary:           card.Boundary,
				Proof:              card.Proof,
				RequiresOperator:   card.RequiresOperator,
				ReviewCard:         &card,
			})
		}
	}
	if strings.Contains(context, "app-store-connect") || strings.Contains(context, "asc") {
		if !hasSourcePrivateConfigCard {
			add(WorkbenchAdjacentAction{
				ID:               "asc-preflight-private-config",
				Kind:             "preflight",
				Title:            "Run read-only ASC private mapping preflight",
				Command:          []string{"bun", "tools/asc_preflight.ts", "--config", "<private-asc-config.json>", "--json"},
				RequiredInputs:   []string{"private ASC product mapping config"},
				Boundary:         "operator supplies private config; read-only checks only; no ASC signal apply and no launchd load",
				Proof:            "preflight returns status ok or warn with product authority, credential smoke, and scheduler checks",
				RequiresOperator: true,
			})
		}
		add(WorkbenchAdjacentAction{
			ID:       "asc-sales-series-bridge-proof",
			Kind:     "source_bridge",
			Title:    "Run synthetic ASC sales-series bridge proof",
			Command:  []string{"bun", "test", "tools/asc_sales_series_bridge.test.ts"},
			Boundary: "synthetic fixtures and temp COMPOUND_HOME proof only; no ASC fetch, no private config, no live ProductSignal apply, no scheduler load",
			Proof:    "bridge regression proves apply_results with signal_count and transition_count through the product-signal door",
		})
	}
	if strings.Contains(context, "conversation") || strings.Contains(context, "chatgpt") {
		add(WorkbenchAdjacentAction{
			ID:               "conversation-preflight-private-export",
			Kind:             "preflight",
			Title:            "Run temp-home conversation import preflight",
			Command:          []string{"bun", "tools/chatgpt_conversation_preflight.ts", "<conversation-export-file-or-dir>", "--json"},
			RequiredInputs:   []string{"private conversation export file or directory"},
			Boundary:         "operator supplies private export; temp COMPOUND_HOME proof only; no live import, extraction, or promotion",
			Proof:            "preflight returns status ok, import idempotency and import_safety checks pass, and live actions remain operator-only",
			RequiresOperator: true,
		})
		if !hasMemoryCandidateReviewCard {
			add(WorkbenchAdjacentAction{
				ID:    "conversation-memory-review-gate",
				Kind:  "review_gate",
				Title: "Plan one reviewed conversation-memory candidate before promotion",
				Command: []string{
					"compound", "panel", "plan",
					"--target-kind", "memory_candidate",
					"--target", "<conversation-memory-candidate-id>",
					"--domain", "memory-adjudication",
					"--task", "review one conversation-derived memory candidate for possible promotion; stop before adjudication",
					"--privacy-sensitive",
					"--limit", "1",
					"--max-items", "1",
					"--json",
				},
				RequiredInputs: []string{
					"candidate id from operator-approved bounded live conversation import/extraction",
					"operator selects at most one candidate for promotion review",
				},
				Boundary:         "review-plan only; no live import, extraction execution, panel run, adjudication, promotion, recall proof, or mass import",
				Proof:            "panel plan records one privacy-sensitive memory_candidate review target; no compound memory adjudicate --verdict promote is run",
				RequiresOperator: true,
			})
		}
	}
	add(WorkbenchAdjacentAction{
		ID:               "second-opinion-evidence-recovery",
		Kind:             "source_bridge",
		Title:            "Preflight second-opinion recovery before no-promote import",
		Command:          []string{"bun", "tools/second_opinion_preflight.ts", "--session", "<second-opinion-session-id>", "--answer", "<private-answer.md>", "--source-observation", "<private-source-observation.yaml>", "--json"},
		RequiredInputs:   []string{"second-opinion session id", "private saved answer path", "private source-observation YAML path"},
		Boundary:         "operator supplies session id and private paths; preflight only; second-opinion output remains evidence, not authority; no source observe import",
		Proof:            "preflight reports session, answer, and source-observation readiness plus operator-only harvest/import actions; no-promote import is separate",
		RequiresOperator: true,
	})
	add(WorkbenchAdjacentAction{
		ID:      "codex-pretooluse-native-gate-proof",
		Kind:    "hook_gate",
		Title:   "Prove the Codex native PreToolUse placement gate",
		Command: []string{"compound", "proof-check", "--json"},
		ExpectedValidation: []string{
			"valid apply_patch receives no deny decision",
			"mixed valid and misplaced-canon apply_patch receives permissionDecision deny",
			"every patch target is evaluated through Compound placement policy",
		},
		StopConditions: []string{
			"valid apply_patch is denied",
			"misplaced canon apply_patch is allowed",
			"proof requires live hook installation or harness config mutation",
		},
		Boundary: "synthetic native-protocol proof with temporary Compound home and repo only; no hook install, trust write, harness config mutation, or held-valve clearing",
		Proof:    "proof-check verifies native allow and deny responses, including a deny when any apply_patch target violates Compound placement policy",
	})
	cmd := []string{"compound", "workbench", "--json"}
	if report.OfferedPackets != nil && strings.TrimSpace(report.OfferedPackets.SourceDir) != "" {
		cmd = append(cmd, "--packet-dir", report.OfferedPackets.SourceDir)
	}
	add(WorkbenchAdjacentAction{
		ID:       "workbench-projection-refresh",
		Kind:     "projection",
		Title:    "Refresh Workbench projection and held-valve evidence",
		Command:  cmd,
		Boundary: "read-only projection; does not claim, close, clear, or mutate held work",
		Proof:    "dispatch_next still shows can_run=false for held valves while adjacent_progress lists safe next actions",
	})
	add(WorkbenchAdjacentAction{
		ID:       "architecture-doctor-strict",
		Kind:     "doctor",
		Title:    "Run strict architecture doctor",
		Command:  []string{"compound", "architecture", "doctor", "--strict"},
		Boundary: "read-only architecture/projection gate; no runtime state mutation",
		Proof:    "doctor exits zero and relevant projection/adaptation/hook rows stay green",
	})
	add(WorkbenchAdjacentAction{
		ID:       "proof-check-installed-runtime",
		Kind:     "proof",
		Title:    "Run installed runtime proof check",
		Command:  []string{"compound", "proof-check", "--json"},
		Boundary: "verification only; does not clear operator-only valves",
		Proof:    "proof-check returns status ok on the installed compound binary",
	})
	return withAdjacentActionGates(actions)
}

func (s *Store) applyAdjacentActionProofConsumption(progress *WorkbenchAdjacentProgress) []string {
	if s == nil || progress == nil || len(progress.NonPacketActions) == 0 {
		return nil
	}
	var active []WorkbenchAdjacentAction
	var consumed []WorkbenchAdjacentAction
	var warnings []string
	for _, action := range progress.NonPacketActions {
		refs, actionWarnings := s.adjacentActionProofRefs(action.ID)
		warnings = append(warnings, actionWarnings...)
		if len(refs) == 0 {
			active = append(active, action)
			continue
		}
		action.Consumed = true
		action.ConsumedEvidenceRefs = refs
		action.Command = nil
		consumed = append(consumed, action)
	}
	progress.NonPacketActions = active
	progress.ConsumedNonPacketActions = consumed
	progress.Available = progress.PacketCandidateCount > 0 || len(progress.NonPacketActions) > 0
	if len(consumed) > 0 && len(active) > 0 {
		progress.NextAction = "keep held valves visible; consumed adjacent proofs are recorded, use remaining non-packet actions only when they still produce new proof"
	} else if len(consumed) > 0 {
		progress.NextAction = "keep held valves visible; adjacent proof actions are already consumed, use only new packet/proof surfaces"
	}
	return warnings
}

func (s *Store) adjacentActionProofRefs(actionID string) ([]string, []string) {
	actionID = strings.ToLower(strings.TrimSpace(actionID))
	if actionID == "" {
		return nil, nil
	}
	dir := filepath.Join(s.home, "artifacts", "refs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{"adjacent proof artifacts unreadable: " + err.Error()}
	}
	var refs []string
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, "adjacent proof artifact "+entry.Name()+" unreadable: "+err.Error())
			continue
		}
		body := strings.ToLower(string(data))
		if !strings.Contains(name, actionID) && !strings.Contains(body, actionID) {
			continue
		}
		if !strings.Contains(body, "adjacent workbench action") {
			continue
		}
		if !strings.Contains(body, "verified") && !strings.Contains(body, "do not rerun") {
			continue
		}
		if strings.Contains(body, "proof-check-installed-runtime") &&
			!strings.Contains(body, "publication_complete: true") {
			continue
		}
		refs = append(refs, "artifact:"+filepath.ToSlash(filepath.Join("artifacts", "refs", entry.Name())))
	}
	sort.Strings(refs)
	return refs, warnings
}

func withAdjacentActionGates(actions []WorkbenchAdjacentAction) []WorkbenchAdjacentAction {
	for i := range actions {
		switch actions[i].ID {
		case "asc-preflight-private-config":
			actions[i].ExpectedValidation = []string{
				"asc preflight returns status ok or warn",
				"product authority checked for mapped products",
				"no product signal apply and no scheduler load",
			}
			actions[i].StopConditions = []string{
				"private config missing or invalid",
				"product authority missing mapping",
				"credential smoke fails before scheduler load",
			}
		case "asc-sales-series-bridge-proof":
			actions[i].ExpectedValidation = []string{
				"temp COMPOUND_HOME bridge apply proof passes",
				"apply_results shows signal_count and transition_count",
				"no private collector dependency and no live ASC fetch",
			}
			actions[i].StopConditions = []string{
				"bridge test fails",
				"private/live config required",
				"bridge report lacks apply_results",
			}
		case "conversation-preflight-private-export":
			actions[i].ExpectedValidation = []string{
				"temp COMPOUND_HOME import idempotency passes",
				"import_safety proves private bounded evidence/candidates and zero temp promoted claims",
				"live actions remain operator-only",
				"no live extraction or promotion",
			}
			actions[i].StopConditions = []string{
				"private export missing or invalid",
				"preflight status is not ok",
				"bulk promotion requested",
			}
		case "conversation-memory-review-gate":
			actions[i].ExpectedValidation = []string{
				"panel plan records one privacy-sensitive memory_candidate review target",
				"no panel run, memory adjudication, promotion, recall proof, or mass import",
				"compound memory adjudicate --verdict promote is not run",
			}
			actions[i].StopConditions = []string{
				"candidate id missing",
				"operator selects more than one candidate",
				"review plan would run adjudication or promotion",
			}
		case "second-opinion-evidence-recovery":
			actions[i].ExpectedValidation = []string{
				"session readiness checked",
				"answer/source-observation readiness reported",
				"no source observe import during preflight",
			}
			actions[i].StopConditions = []string{
				"session missing",
				"answer file missing",
				"source observation missing",
				"source observation would promote truth",
			}
		}
	}
	return actions
}

func heldWorkbenchContext(items []WorkbenchItem, held map[string]bool) string {
	var parts []string
	for _, item := range items {
		if !held[item.WorkID] {
			continue
		}
		parts = append(parts,
			item.WorkID,
			item.SuccessPredicate,
			item.DesiredOutcome,
			item.CanonicalSurface,
			item.ReviewTrigger,
		)
		parts = append(parts, item.EvidenceRefs...)
	}
	return strings.Join(parts, " ")
}

func heldValveWorkIDs(blockedOn []string) []string {
	var out []string
	for _, blocked := range blockedOn {
		blocked = strings.TrimSpace(blocked)
		if strings.HasPrefix(blocked, "work.review:") {
			out = append(out, strings.TrimPrefix(blocked, "work.review:"))
		}
	}
	return out
}

func adjacentPacketCandidates(surface *OfferedPacketSurface, actor string, refLimit int) (int, []string) {
	if surface == nil {
		return 0, nil
	}
	count := 0
	var refs []string
	for _, item := range surface.Items {
		if !packetEligibleForAdjacentProgress(item, actor) {
			continue
		}
		count++
		if refLimit <= 0 || len(refs) >= refLimit {
			continue
		}
		ref := strings.TrimSpace(item.SourceRef)
		if ref == "" {
			ref = strings.TrimSpace(item.Path)
		}
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return count, refs
}

func packetEligibleForAdjacentProgress(item OfferedPacketItem, actor string) bool {
	status := strings.ToLower(strings.TrimSpace(item.Status))
	if status == "" {
		return true
	}
	actor = strings.ToLower(strings.TrimSpace(actor))
	owner := strings.ToLower(strings.TrimSpace(item.Owner))
	combined := status + " " + owner
	for _, blocked := range []string{
		"completed",
		"cancelled",
		"held",
		"blocked",
		"after work_",
		"do not run concurrently",
		"low priority",
		"deferred",
		"operator registration",
		"operator-only",
		"one-line registration flip",
		"credential",
		"scheduler",
	} {
		if strings.Contains(combined, blocked) {
			return false
		}
	}
	if strings.Contains(combined, "console lane") && !strings.Contains(actor, "console") {
		return false
	}
	if strings.Contains(combined, "console rendering") && !strings.Contains(actor, "console") {
		return false
	}
	if strings.Contains(combined, "console") && strings.Contains(combined, "remaining") && !strings.Contains(actor, "console") {
		return false
	}
	if !strings.HasPrefix(status, "ready") && !strings.HasPrefix(status, "offered") {
		return false
	}
	return true
}

func lastAttachedWithPrefix(events []Event, prefix string) string {
	for i := len(events) - 1; i >= 0; i-- {
		for j := len(events[i].Attached) - 1; j >= 0; j-- {
			if strings.HasPrefix(events[i].Attached[j], prefix) {
				return events[i].Attached[j]
			}
		}
	}
	return ""
}

func nextWorkAction(state string) string {
	switch state {
	case "proposed":
		return "admit or cancel after owner review"
	case "admitted":
		return "mark ready when context and policy are available"
	case "ready":
		return "claim before touching"
	case "claimed":
		return "advance running or release to ready"
	case "running":
		return "advance verifying, completed, blocked, held, or failed"
	case "verifying":
		return "close completed or failed, or return running"
	case "held":
		return "resolve policy/source-control hold, then ready or cancel"
	case "blocked":
		return "resolve blocker, then running, failed, or cancel"
	case "completed", "failed", "cancelled":
		return "terminal; mine receipt and transition if useful"
	default:
		return "inspect work state"
	}
}

func openReason(state string) string {
	switch state {
	case "proposed", "admitted", "ready":
		return "needs_claim"
	case "held", "blocked":
		return "needs_review"
	case "claimed", "running", "verifying":
		return "in_progress"
	default:
		return ""
	}
}

func workbenchObservatoryTags(item WorkbenchItem) []string {
	var tags []string
	seen := map[string]bool{}
	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			return
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	addFacet := func(facet, value string) {
		if value = workbenchTagValue(value); value != "" {
			add(facet + ":" + value)
		}
	}

	addFacet("state", item.State)
	if item.OpenReason != "" {
		addFacet("attention", item.OpenReason)
	} else if terminalStates[item.State] {
		add("attention:terminal")
	}
	if item.Owner != "" {
		addFacet("owner", item.Owner)
	}
	if item.ClaimedBy != "" {
		addFacet("actor", item.ClaimedBy)
	}
	if item.CanonicalSurface != "" {
		addFacet("surface", item.CanonicalSurface)
	}
	if item.ContextPackID != "" {
		add("context:compiled")
	}
	if item.PolicyDecisionID != "" {
		add("policy:decided")
	}
	if item.MoveType != "" {
		addFacet("move", item.MoveType)
		switch item.MoveType {
		case "memory_delta":
			add("recipient:memory")
		case "rule_delta":
			add("recipient:rule")
		case "outward_delta":
			add("recipient:outward")
		case "control_plane_repair":
			add("recipient:control-plane")
		case "audit_only":
			add("recipient:audit")
		}
		if item.Changed {
			add("change:changed")
		} else {
			add("change:unchanged")
		}
	}
	if item.ReviewTrigger != "" {
		addFacet("review", item.ReviewTrigger)
	}
	if len(item.RunObservations) > 0 {
		add("evidence:run-observed")
	}
	if len(item.RouteOutcomes) > 0 {
		add("route:outcome")
		for _, out := range item.RouteOutcomes {
			if out.ShouldReviewPolicy {
				add("route:review-needed")
				break
			}
		}
	}
	if len(item.EvidenceRefs) > 0 {
		add("evidence:source")
	}
	if len(item.ArtifactIDs) > 0 {
		add("evidence:artifact")
	}
	if len(item.VerificationIDs) > 0 {
		add("evidence:verification")
	}
	if len(item.MemoryCandidateIDs) > 0 {
		add("memory:candidates")
	}
	if terminalProofRequired(item.State) {
		if item.ReceiptID == "" {
			add("proof:missing-receipt")
		} else {
			add("proof:receipt")
		}
		if item.TransitionID == "" {
			add("proof:missing-transition")
		} else {
			add("proof:transition")
		}
		if item.ReceiptID != "" && item.TransitionID != "" {
			add("proof:complete")
		}
	}
	return tags
}

func workbenchTagValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if keep {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func terminalProofRequired(state string) bool {
	return state == "completed" || state == "failed"
}

func workbenchLatestTimestamp(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	if workbenchTimestampAfter(b, a) {
		return b
	}
	return a
}

func workbenchTimestampAfter(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	at, aerr := time.Parse(time.RFC3339Nano, a)
	bt, berr := time.Parse(time.RFC3339Nano, b)
	if aerr == nil && berr == nil {
		return at.After(bt)
	}
	return a > b
}

func statePriority(state string) int {
	switch state {
	case "held":
		return 0
	case "blocked":
		return 1
	case "running", "verifying":
		return 2
	case "claimed":
		return 3
	case "ready":
		return 4
	case "admitted":
		return 5
	case "proposed":
		return 6
	default:
		return 9
	}
}
