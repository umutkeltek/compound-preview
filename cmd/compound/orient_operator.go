package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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

	"compound/internal/authority"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/narrate"
	"compound/internal/observation"
	panelpkg "compound/internal/panel"
	"compound/internal/product"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const orientOperatorCursorSchemaVersion = "compound.orient_operator_cursor.v0.1"
const orientOperatorReportSchemaVersion = "compound.orient_operator.v0.1"

type orientOperatorReport struct {
	SchemaVersion  string                `json:"schema_version"`
	GeneratedAt    string                `json:"generated_at"`
	NeedsMe        orientNeedsMe         `json:"needs_me"`
	Running        orientRunning         `json:"running"`
	SessionContext *orientSessionContext `json:"session_context,omitempty"`
	ProductLoops   *orientProductLoops   `json:"product_loops,omitempty"`
	Changed        orientChanged         `json:"changed"`
	// Narrate embeds the served compound.narrate.v0.1 correspondent script,
	// produced by the SAME builder `compound narrate` uses (buildNarrateScript),
	// so orient stays the named consumer surface of the narration law (spec §1).
	// Additive + omitempty: absent on any build error (recorded in Warnings).
	Narrate   *narrate.Script   `json:"narrate,omitempty"`
	DrillDown []orientDrillDown `json:"drill_down"`
	Warnings  []string          `json:"warnings,omitempty"`
}

type orientSessionContext struct {
	Status   string   `json:"status"`
	Source   string   `json:"source"`
	Harness  string   `json:"harness"`
	RepoKind string   `json:"repo_kind"`
	Task     string   `json:"task"`
	Command  []string `json:"command"`
	Boundary string   `json:"boundary"`
	Proof    string   `json:"proof"`
}

type orientNeedsMe struct {
	CandidateTotal         int                       `json:"candidate_total"`
	CandidateActionable    int                       `json:"candidate_actionable"`
	CandidateInboxes       []orientCandidateInbox    `json:"candidate_inboxes"`
	ReviewRequired         int                       `json:"review_required"`
	ReadyToApply           int                       `json:"ready_to_apply"`
	PendingPanelNeedsHuman int                       `json:"pending_panel_needs_human"`
	ObservationReviews     *orientObservationReviews `json:"observation_reviews,omitempty"`
	TensionCards           *orientTensionCards       `json:"tension_cards,omitempty"`
	MemoryReviewCardCount  int                       `json:"memory_review_card_count,omitempty"`
	MemoryReviewCards      []orientMemoryReviewCard  `json:"memory_review_cards,omitempty"`
	OperatorGateCardCount  int                       `json:"operator_gate_card_count,omitempty"`
	OperatorGateCards      []orientOperatorGateCard  `json:"operator_gate_cards,omitempty"`
	AdaptationIntake       *orientAdaptationIntake   `json:"adaptation_intake,omitempty"`
	CapabilityFeedback     *orientCapabilityFeedback `json:"capability_feedback,omitempty"`
	SourceControl          *orientSourceControl      `json:"source_control,omitempty"`
	SourceIntake           *orientSourceIntake       `json:"source_intake,omitempty"`
	OldestItem             *orientAgedItem           `json:"oldest_item,omitempty"`
	OldestActionableItem   *orientAgedItem           `json:"oldest_actionable_item,omitempty"`
}

type orientTensionCards struct {
	Items    []orientTensionCard `json:"items"`
	Commands []orientDrillDown   `json:"commands"`
}

type orientTensionCard struct {
	ID      string   `json:"id"`
	Summary string   `json:"summary"`
	Refs    []string `json:"refs"`
	Command []string `json:"command"`
}

type orientMemoryReviewCard struct {
	ID                  string   `json:"id"`
	WorkID              string   `json:"work_id"`
	CandidateID         string   `json:"candidate_id"`
	CandidateState      string   `json:"candidate_state,omitempty"`
	PanelRunID          string   `json:"panel_run_id,omitempty"`
	PanelRunRef         string   `json:"panel_run_ref,omitempty"`
	PromotionSummaryRef string   `json:"promotion_summary_ref,omitempty"`
	PromotionCount      *int     `json:"promotion_count,omitempty"`
	Command             []string `json:"command,omitempty"`
	Boundary            string   `json:"boundary"`
	NextAction          string   `json:"next_action"`
}

type orientOperatorGateCard struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	WorkID             string   `json:"work_id"`
	GateID             string   `json:"gate_id,omitempty"`
	SourceKind         string   `json:"source_kind,omitempty"`
	Command            []string `json:"command,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary"`
	Proof              string   `json:"proof"`
	NextAction         string   `json:"next_action"`
}

type orientCandidateInbox struct {
	Kind            string         `json:"kind"`
	Dir             string         `json:"dir"`
	Count           int            `json:"count"`
	CorruptCount    int            `json:"corrupt_count,omitempty"`
	ActionableCount int            `json:"actionable_count"`
	StateCounts     map[string]int `json:"state_counts,omitempty"`
	Warnings        []string       `json:"warnings,omitempty"`
}

type orientSourceIntake struct {
	RecordCount         int                                  `json:"record_count"`
	RowCount            int                                  `json:"row_count"`
	AppliedCount        int                                  `json:"applied_count"`
	ReviewRecordedCount int                                  `json:"review_recorded_count"`
	SkippedCount        int                                  `json:"skipped_count"`
	BlockedPrivateCount int                                  `json:"blocked_private_count"`
	UnsupportedCount    int                                  `json:"unsupported_count"`
	ActionCardCount     int                                  `json:"action_card_count"`
	ActionCards         []observation.SourceIntakeActionCard `json:"action_cards,omitempty"`
	LastSource          string                               `json:"last_source,omitempty"`
	LastSubject         string                               `json:"last_subject,omitempty"`
	LastRoute           string                               `json:"last_route,omitempty"`
	LastOwner           string                               `json:"last_owner,omitempty"`
	LastCapturedAt      string                               `json:"last_captured_at,omitempty"`
	NextAction          string                               `json:"next_action"`
	Commands            []orientDrillDown                    `json:"commands"`
}

type orientSourceControl struct {
	LiveStatus             string            `json:"live_status,omitempty"`
	GitRoot                string            `json:"git_root,omitempty"`
	Branch                 string            `json:"branch,omitempty"`
	Head                   string            `json:"head,omitempty"`
	Ahead                  int               `json:"ahead,omitempty"`
	Behind                 int               `json:"behind,omitempty"`
	ChangedPaths           int               `json:"changed_paths"`
	Staged                 int               `json:"staged"`
	Unstaged               int               `json:"unstaged"`
	Untracked              int               `json:"untracked"`
	EvidenceRef            string            `json:"evidence_ref,omitempty"`
	ReviewCount            int               `json:"review_count"`
	ReviewOpenCount        int               `json:"review_open_count"`
	ReviewRecordedCount    int               `json:"review_recorded_count"`
	LatestReviewID         string            `json:"latest_review_id,omitempty"`
	LatestReviewNextAction string            `json:"latest_review_next_action,omitempty"`
	HeadReviewRecorded     bool              `json:"head_review_recorded"`
	NextAction             string            `json:"next_action"`
	Commands               []orientDrillDown `json:"commands"`
}

type orientProductLoops struct {
	ActionCardCount         int                     `json:"action_card_count"`
	ProductCount            int                     `json:"product_count"`
	OpenHypothesisDebtCount int                     `json:"open_hypothesis_debt_count"`
	ClosedReviewActionCount int                     `json:"closed_review_action_count"`
	Products                []orientProductLoopItem `json:"products"`
	DrillDownCommand        []string                `json:"drill_down_command"`
}

type orientProductLoopItem struct {
	ActionCardCount                     int                             `json:"action_card_count"`
	ActionCards                         []orientLoopActionCard          `json:"action_cards,omitempty"`
	ProductID                           string                          `json:"product_id"`
	Name                                string                          `json:"name,omitempty"`
	OpenHypothesisDebtCount             int                             `json:"open_hypothesis_debt_count"`
	ClosedReviewActionCount             int                             `json:"closed_review_action_count"`
	OpenHypothesisCount                 int                             `json:"open_hypothesis_count"`
	ReviewCount                         int                             `json:"review_count"`
	LoopGaps                            []string                        `json:"loop_gaps,omitempty"`
	OpenHypothesesByMissingSignalSource []orientLoopMissingSignalSource `json:"open_hypotheses_by_missing_signal_source,omitempty"`
	SignalsWithoutHypothesis            []orientLoopSignalHypothesisGap `json:"signals_without_hypothesis,omitempty"`
	ClosedReviewsByNextAction           []orientLoopReviewNextAction    `json:"closed_reviews_by_next_action,omitempty"`
	// ReleaseAwareness is the product-owned evidence-only summary.
	ReleaseAwareness *product.ReleaseAwarenessSummary `json:"release_awareness,omitempty"`
}

type orientLoopMissingSignalSource struct {
	Source        string   `json:"source"`
	HypothesisIDs []string `json:"hypothesis_ids,omitempty"`
	Metrics       []string `json:"metrics,omitempty"`
	NextActions   []string `json:"next_actions,omitempty"`
	Count         int      `json:"count"`
}

type orientLoopSignalHypothesisGap struct {
	ChangeEvent          string   `json:"change_event"`
	SignalIDs            []string `json:"signal_ids,omitempty"`
	Metrics              []string `json:"metrics,omitempty"`
	Providers            []string `json:"providers,omitempty"`
	Consumed             bool     `json:"consumed,omitempty"`
	ConsumedByWorkIDs    []string `json:"consumed_by_work_ids,omitempty"`
	ConsumedEvidenceRefs []string `json:"consumed_evidence_refs,omitempty"`
	Count                int      `json:"count"`
}

type orientLoopReviewNextAction struct {
	NextAction           string   `json:"next_action"`
	ReviewIDs            []string `json:"review_ids,omitempty"`
	HypothesisIDs        []string `json:"hypothesis_ids,omitempty"`
	SignalIDs            []string `json:"signal_ids,omitempty"`
	Consumed             bool     `json:"consumed,omitempty"`
	ConsumedByWorkIDs    []string `json:"consumed_by_work_ids,omitempty"`
	ConsumedEvidenceRefs []string `json:"consumed_evidence_refs,omitempty"`
	Count                int      `json:"count"`
}

type orientObservationReviews struct {
	ObservationCandidateReviewCount int               `json:"observation_candidate_review_count"`
	ObservationPanelVerdictCount    int               `json:"observation_panel_verdict_count"`
	TotalReviewDebt                 int               `json:"total_review_debt"`
	NextAction                      string            `json:"next_action"`
	Commands                        []orientDrillDown `json:"commands"`
}

type orientCapabilityFeedback struct {
	Status             string                                         `json:"status"`
	PendingReviewCount int                                            `json:"pending_review_count"`
	AppliedReviewCount int                                            `json:"applied_review_count"`
	RejectedCount      int                                            `json:"rejected_count"`
	KeptReviewCount    int                                            `json:"kept_review_count"`
	AppliedEdges       int                                            `json:"applied_edges"`
	EdgeKindCounts     map[string]int                                 `json:"edge_kind_counts,omitempty"`
	Recent             []panelpkg.PanelCapabilityFeedbackImpactRecord `json:"recent,omitempty"`
	NextAction         string                                         `json:"next_action"`
	Commands           []orientDrillDown                              `json:"commands"`
}

type orientAdaptationIntake struct {
	ExternalRepoCount                 int                          `json:"external_repo_count"`
	ExternalRepoOpenCount             int                          `json:"external_repo_open_count"`
	ExternalRepoReviewedCount         int                          `json:"external_repo_reviewed_count"`
	CapabilityEvaluationCount         int                          `json:"capability_evaluation_count"`
	CapabilityEvaluationOpenCount     int                          `json:"capability_evaluation_open_count"`
	CapabilityEvaluationReviewedCount int                          `json:"capability_evaluation_reviewed_count"`
	ActionableCount                   int                          `json:"actionable_count"`
	Recent                            []orientAdaptationIntakeItem `json:"recent,omitempty"`
	NextAction                        string                       `json:"next_action"`
	Commands                          []orientDrillDown            `json:"commands"`
}

type orientAdaptationIntakeItem struct {
	Area         string   `json:"area"`
	ID           string   `json:"id"`
	Status       string   `json:"status,omitempty"`
	Readiness    string   `json:"readiness,omitempty"`
	TargetPath   string   `json:"target_path,omitempty"`
	Owner        string   `json:"owner,omitempty"`
	DecisionHint string   `json:"decision_hint,omitempty"`
	NextAction   string   `json:"next_action,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type orientLoopActionCard struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Command  []string `json:"command,omitempty"`
	Boundary string   `json:"boundary,omitempty"`
	Proof    string   `json:"proof,omitempty"`
}

type orientAgedItem struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	Path       string `json:"path,omitempty"`
	At         string `json:"at"`
	AgeSeconds int64  `json:"age_seconds"`
	Age        string `json:"age"`
}

type orientRunning struct {
	AdmissionPreviewCount int                      `json:"admission_preview_count,omitempty"`
	AdmissionPreviews     []orientAdmissionPreview `json:"admission_previews,omitempty"`
	OpenCount             int                      `json:"open_count"`
	InProgress            int                      `json:"in_progress"`
	NonStaleLeaseCount    int                      `json:"non_stale_lease_count"`
	LastDispatchStepRun   *orientAgedItem          `json:"last_dispatch_step_run,omitempty"`
	OpenWork              []orientOpenWorkItem     `json:"open_work"`
	AdjacentProgress      *orientAdjacentProgress  `json:"adjacent_progress,omitempty"`
}

type orientAdmissionPreview struct {
	PlanID                 string                  `json:"plan_id"`
	WorkID                 string                  `json:"work_id"`
	WorkOwner              string                  `json:"work_owner,omitempty"`
	WorkState              string                  `json:"work_state,omitempty"`
	Staleness              string                  `json:"staleness,omitempty"`
	Age                    string                  `json:"age,omitempty"`
	NextAction             string                  `json:"next_action,omitempty"`
	ActionSummary          string                  `json:"action_summary,omitempty"`
	PlanPath               string                  `json:"plan_path,omitempty"`
	AdmitCommand           []string                `json:"admit_command,omitempty"`
	RetireDryRun           []string                `json:"retire_dry_run_command,omitempty"`
	PolicyAllowed          bool                    `json:"policy_allowed"`
	PolicyOutcome          string                  `json:"policy_outcome,omitempty"`
	RequiresOperator       bool                    `json:"requires_operator,omitempty"`
	AdmissionBlockedReason string                  `json:"admission_blocked_reason,omitempty"`
	ActionContext          *work.WorkActionContext `json:"action_context,omitempty"`
	SourceReviewCardID     string                  `json:"source_review_card_id,omitempty"`
}

type orientAdjacentProgress struct {
	Available                    bool     `json:"available"`
	Reason                       string   `json:"reason,omitempty"`
	NextAction                   string   `json:"next_action,omitempty"`
	HeldWorkIDs                  []string `json:"held_work_ids,omitempty"`
	PacketCandidateCount         int      `json:"packet_candidate_count,omitempty"`
	NonPacketActionCount         int      `json:"non_packet_action_count,omitempty"`
	NonPacketActionIDs           []string `json:"non_packet_action_ids,omitempty"`
	ConsumedNonPacketActionCount int      `json:"consumed_non_packet_action_count,omitempty"`
	ConsumedNonPacketActionIDs   []string `json:"consumed_non_packet_action_ids,omitempty"`
}

type orientOpenWorkItem struct {
	WorkID            string   `json:"work_id"`
	State             string   `json:"state"`
	Actor             string   `json:"actor,omitempty"`
	ClaimedAt         string   `json:"claimed_at,omitempty"`
	ClaimedAgeSeconds int64    `json:"claimed_age_seconds,omitempty"`
	ClaimedAge        string   `json:"claimed_age,omitempty"`
	NextAction        string   `json:"next_action,omitempty"`
	ObservatoryTags   []string `json:"observatory_tags,omitempty"`
	Surface           string   `json:"surface,omitempty"`
}

type orientChanged struct {
	CurrentClaimSeq int64                    `json:"current_claim_seq"`
	LastSeenSeq     int64                    `json:"last_seen_seq"`
	ClaimsSeqDelta  int64                    `json:"claims_seq_delta"`
	FirstRun        bool                     `json:"first_run"`
	CursorPath      string                   `json:"cursor_path"`
	Recent          []orientRecentChangeItem `json:"recent"`
}

type orientRecentChangeItem struct {
	WorkID       string   `json:"work_id"`
	ReceiptID    string   `json:"receipt_id,omitempty"`
	TransitionID string   `json:"transition_id,omitempty"`
	MoveType     string   `json:"move_type,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Changed      bool     `json:"changed"`
	CreatedAt    string   `json:"created_at,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type orientDrillDown struct {
	Label   string   `json:"label"`
	Command []string `json:"command"`
}

type orientOperatorCursor struct {
	SchemaVersion string `json:"schema_version"`
	UpdatedAt     string `json:"updated_at"`
	LastSeenSeq   int64  `json:"last_seen_seq"`
}

func cmdOrientOperator(homePath string, args []string) {
	jsonOut := false
	recent := 5
	strangerExcludeTopic := ""
	usage := "usage: compound orient --operator [--json] [--recent N] [--stranger-exclude-topic SLUG]"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--operator":
		case "--json":
			jsonOut = true
		case "--recent":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --recent must be a non-negative integer")
				os.Exit(2)
			}
			recent = n
		case "--stranger-exclude-topic":
			// D4: v1 "current topic" signal — excludes this topic's subtree from the S3b
			// stranger pool. Absent => no topic exclusion (dealer still restricts to
			// proposed|parked candidates and the dealt-log cooldown).
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			i++
			strangerExcludeTopic = args[i]
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	report, err := buildOrientOperatorReport(homePath, recent, time.Now(), strangerExcludeTopic)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orient --operator:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	printOrientOperatorReport(report)
}

func buildOrientOperatorReport(homePath string, recent int, now time.Time, strangerExcludeTopic string) (orientOperatorReport, error) {
	return buildOrientOperatorReportWithCursorWriter(homePath, recent, now, strangerExcludeTopic, writeOrientOperatorCursor)
}

func buildOrientOperatorReportWithCursorWriter(homePath string, recent int, now time.Time, strangerExcludeTopic string, cursorWriter func(string, int64, time.Time) error) (orientOperatorReport, error) {
	if recent <= 0 {
		recent = 5
	}
	store, err := work.Open(homePath)
	if err != nil {
		return orientOperatorReport{}, err
	}
	workbench, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: recent, Actor: "codex"})
	if err != nil {
		return orientOperatorReport{}, err
	}
	reviews, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		return orientOperatorReport{}, err
	}
	currentSeq, err := maxClaimSeq(homePath)
	if err != nil {
		return orientOperatorReport{}, err
	}
	cursor, cursorPath, firstRun, err := readOrientOperatorCursor(homePath)
	if err != nil {
		return orientOperatorReport{}, err
	}
	needsMe, pendingStranger := buildOrientNeedsMeWithPendingStranger(homePath, reviews, workbench, now, strangerExcludeTopic)
	report := orientOperatorReport{
		SchemaVersion:  orientOperatorReportSchemaVersion,
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		NeedsMe:        needsMe,
		Running:        buildOrientRunning(homePath, workbench, now),
		SessionContext: buildOrientSessionContext(workbench),
		Changed:        buildOrientChanged(homePath, workbench, currentSeq, cursor.LastSeenSeq, firstRun, cursorPath),
		DrillDown: []orientDrillDown{
			{Label: "workbench", Command: []string{"compound", "workbench"}},
			{Label: "reviews", Command: []string{"compound", "reviews"}},
			{Label: "memory doctor", Command: []string{"compound", "memory", "doctor"}},
		},
		Warnings: append([]string(nil), workbench.Integrity...),
	}
	for _, inbox := range report.NeedsMe.CandidateInboxes {
		for _, warning := range inbox.Warnings {
			report.Warnings = append(report.Warnings, fmt.Sprintf("candidate_inbox %s: %s", inbox.Kind, warning))
		}
	}
	report.Warnings = reviewartifact.Unique(report.Warnings)
	if report.NeedsMe.ObservationReviews != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.ObservationReviews.Commands...)
	}
	if report.NeedsMe.TensionCards != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.TensionCards.Commands...)
	}
	if report.NeedsMe.CapabilityFeedback != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.CapabilityFeedback.Commands...)
	}
	if report.NeedsMe.AdaptationIntake != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.AdaptationIntake.Commands...)
	}
	if report.NeedsMe.SourceControl != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.SourceControl.Commands...)
	}
	if report.NeedsMe.SourceIntake != nil {
		report.DrillDown = append(report.DrillDown, report.NeedsMe.SourceIntake.Commands...)
	}
	productLoops, err := buildOrientProductLoops(homePath, recent, now)
	if err != nil {
		report.Warnings = append(report.Warnings, "product_loops: "+err.Error())
	} else if productLoops.ProductCount > 0 {
		report.ProductLoops = &productLoops
		report.DrillDown = append(report.DrillDown, orientDrillDown{
			Label:   "product loops",
			Command: productLoops.DrillDownCommand,
		})
	}
	// Embed the correspondent script (additive, best-effort). buildNarrateScript
	// reads the cursor/changed-set read-only — it must run BEFORE the cursor
	// advance below so it sees the same last-seen watermark buildOrientChanged
	// used, and it never persists the narrate emphasis sidecar from this path.
	if narrateRes, err := buildNarrateScript(homePath, narrate.MoodDecide, recent, now); err != nil {
		report.Warnings = append(report.Warnings, "narrate: "+err.Error())
	} else {
		script := narrateRes.Script
		report.Narrate = &script
	}
	if pendingStranger != nil {
		finalized, finalizeErr := pendingStranger.store.FinalizeStrangerDeal(pendingStranger.preparation, now, strangerCardCooldown, func() error {
			return cursorWriter(homePath, currentSeq, now)
		})
		switch {
		case errors.Is(finalizeErr, mem.ErrStrangerDealPairedCommit):
			return orientOperatorReport{}, finalizeErr
		case finalizeErr != nil:
			report.Warnings = append(report.Warnings, "stranger_card: "+finalizeErr.Error())
		case finalized:
			attachOrientStrangerCard(&report, pendingStranger.card)
		default:
			if err := cursorWriter(homePath, currentSeq, now); err != nil {
				return orientOperatorReport{}, err
			}
		}
	} else if err := cursorWriter(homePath, currentSeq, now); err != nil {
		return orientOperatorReport{}, err
	}
	return report, nil
}

func buildOrientSessionContext(workbench work.WorkbenchReport) *orientSessionContext {
	const (
		harness  = "codex"
		repoKind = "control-plane"
	)
	source := "workbench"
	task := "inspect Compound operator state and choose next proof-producing move"
	switch {
	case workbench.DispatchNext.AdjacentProgress != nil && workbench.DispatchNext.AdjacentProgress.Available:
		source = "workbench.adjacent_progress"
		task = "continue held-valve-adjacent Compound work without clearing operator-only valves"
	case workbench.DispatchNext.CanRun:
		source = "workbench.dispatch_next"
		task = "claim next ready Compound work through Workbench dispatch"
	case workbench.Counts.AdmissionPreviews > 0:
		source = "workbench.admission_previews"
		task = "review Workbench admission previews before admitting work"
	case workbench.Counts.Open > 0:
		source = "workbench.open_work"
		task = "inspect Compound open work and preserve active gates"
	}
	return &orientSessionContext{
		Status:   "ready",
		Source:   source,
		Harness:  harness,
		RepoKind: repoKind,
		Task:     task,
		Command:  []string{"compound", "orient", "--harness", harness, "--repo-kind", repoKind, "--task", task},
		Boundary: "context-pack compilation only; records EffectiveContract and ContextPack without claiming, clearing held valves, promoting memory, calling external accounts, or mutating public surfaces",
		Proof:    "compound orient writes an EffectiveContract plus ContextPack artifact and records included/excluded refs for the selected task",
	}
}

func buildOrientProductLoops(homePath string, recent int, now time.Time) (orientProductLoops, error) {
	if recent <= 0 {
		recent = 5
	}
	model, err := product.BuildLoopModel(homePath, product.LoopOptions{
		Recent: recent,
		Now:    now,
	})
	if err != nil {
		return orientProductLoops{}, err
	}
	out := orientProductLoops{
		Products:         []orientProductLoopItem{},
		DrillDownCommand: []string{"compound", "product", "loops"},
	}
	for _, loop := range model.Products {
		if len(loop.OpenHypothesesByMissingSignalSource) == 0 && len(loop.SignalsWithoutHypothesis) == 0 && len(loop.ClosedReviewsByNextAction) == 0 && loop.ReleaseAwareness == nil {
			continue
		}
		item := orientProductLoopItem{
			ProductID:                           loop.ProductID,
			Name:                                loop.Name,
			OpenHypothesisCount:                 len(loop.OpenHypotheses),
			ReviewCount:                         len(loop.Reviews),
			ActionCardCount:                     len(loop.ActionCards),
			ActionCards:                         orientLoopActionCards(loop.ActionCards),
			LoopGaps:                            append([]string{}, loop.LoopGaps...),
			OpenHypothesesByMissingSignalSource: orientMissingSignalSources(loop.OpenHypothesesByMissingSignalSource),
			SignalsWithoutHypothesis:            orientSignalHypothesisGaps(loop.SignalsWithoutHypothesis),
			ClosedReviewsByNextAction:           orientReviewNextActions(loop.ClosedReviewsByNextAction),
			ReleaseAwareness:                    loop.ReleaseAwareness.Summary(),
		}
		for _, group := range item.OpenHypothesesByMissingSignalSource {
			item.OpenHypothesisDebtCount += group.Count
			out.OpenHypothesisDebtCount += group.Count
		}
		for _, group := range item.ClosedReviewsByNextAction {
			item.ClosedReviewActionCount += group.Count
			out.ClosedReviewActionCount += group.Count
		}
		out.ActionCardCount += item.ActionCardCount
		out.Products = append(out.Products, item)
	}
	out.ProductCount = len(out.Products)
	return out, nil
}

func orientMissingSignalSources(groups []product.LoopMissingSignalSource) []orientLoopMissingSignalSource {
	out := make([]orientLoopMissingSignalSource, 0, len(groups))
	for _, group := range groups {
		out = append(out, orientLoopMissingSignalSource{
			Source:        group.Source,
			HypothesisIDs: append([]string{}, group.HypothesisIDs...),
			Metrics:       append([]string{}, group.Metrics...),
			NextActions:   append([]string{}, group.NextActions...),
			Count:         len(group.HypothesisIDs),
		})
	}
	return out
}

func orientSignalHypothesisGaps(groups []product.LoopSignalHypothesisGap) []orientLoopSignalHypothesisGap {
	out := make([]orientLoopSignalHypothesisGap, 0, len(groups))
	for _, group := range groups {
		out = append(out, orientLoopSignalHypothesisGap{
			ChangeEvent:          group.ChangeEvent,
			SignalIDs:            append([]string{}, group.SignalIDs...),
			Metrics:              append([]string{}, group.Metrics...),
			Providers:            append([]string{}, group.Providers...),
			Consumed:             group.Consumed,
			ConsumedByWorkIDs:    append([]string{}, group.ConsumedByWorkIDs...),
			ConsumedEvidenceRefs: append([]string{}, group.ConsumedEvidenceRefs...),
			Count:                len(group.SignalIDs),
		})
	}
	return out
}

func orientReviewNextActions(groups []product.LoopReviewNextAction) []orientLoopReviewNextAction {
	out := make([]orientLoopReviewNextAction, 0, len(groups))
	for _, group := range groups {
		out = append(out, orientLoopReviewNextAction{
			NextAction:           group.NextAction,
			ReviewIDs:            append([]string{}, group.ReviewIDs...),
			HypothesisIDs:        append([]string{}, group.HypothesisIDs...),
			SignalIDs:            append([]string{}, group.SignalIDs...),
			Consumed:             group.Consumed,
			ConsumedByWorkIDs:    append([]string{}, group.ConsumedByWorkIDs...),
			ConsumedEvidenceRefs: append([]string{}, group.ConsumedEvidenceRefs...),
			Count:                len(group.ReviewIDs),
		})
	}
	return out
}

func orientLoopActionCards(cards []product.LoopActionCard) []orientLoopActionCard {
	out := make([]orientLoopActionCard, 0, len(cards))
	for _, card := range cards {
		out = append(out, orientLoopActionCard{
			ID:       card.ID,
			Kind:     card.Kind,
			Title:    card.Title,
			Command:  append([]string{}, card.Command...),
			Boundary: card.Boundary,
			Proof:    card.Proof,
		})
	}
	return out
}

func buildOrientNeedsMe(homePath string, reviews reviewartifact.QueueReport, workbench work.WorkbenchReport, now time.Time, strangerExcludeTopic string) orientNeedsMe {
	out, _ := buildOrientNeedsMeWithPendingStranger(homePath, reviews, workbench, now, strangerExcludeTopic)
	return out
}

func buildOrientNeedsMeWithPendingStranger(homePath string, reviews reviewartifact.QueueReport, workbench work.WorkbenchReport, now time.Time, strangerExcludeTopic string) (orientNeedsMe, *orientPendingStrangerDeal) {
	out := orientNeedsMe{
		CandidateInboxes:   []orientCandidateInbox{},
		ReviewRequired:     reviews.Counts.ReviewRequired,
		ReadyToApply:       reviews.Counts.ReadyToApply,
		ObservationReviews: buildOrientObservationReviews(reviews),
	}
	out.CapabilityFeedback = buildOrientCapabilityFeedback(homePath, reviews, now)
	var pendingStranger *orientPendingStrangerDeal
	out.TensionCards, pendingStranger = buildOrientTensionCards(homePath, reviews, now, strangerExcludeTopic)
	out.AdaptationIntake = buildOrientAdaptationIntake(reviews)
	out.SourceControl = buildOrientSourceControl(reviews)
	out.SourceIntake = buildOrientSourceIntake(homePath, now)
	out.MemoryReviewCards = orientMemoryReviewCards(workbench)
	out.MemoryReviewCardCount = len(out.MemoryReviewCards)
	out.OperatorGateCards = orientOperatorGateCards(workbench)
	out.OperatorGateCardCount = len(out.OperatorGateCards)
	var oldest *orientAgedItem
	var oldestActionable *orientAgedItem
	for _, kind := range candidates.AllKinds() {
		dir := candidates.InboxDir(kind)
		summary := candidateInboxSummary(homePath, dir, string(kind), now)
		if kind == candidates.Memory {
			summary = memoryCandidateInboxSummary(homePath, now)
		}
		out.CandidateTotal += summary.Count
		out.CandidateActionable += summary.ActionableCount
		out.CandidateInboxes = append(out.CandidateInboxes, orientCandidateInbox{
			Kind:            string(kind),
			Dir:             dir,
			Count:           summary.Count,
			CorruptCount:    summary.CorruptCount,
			ActionableCount: summary.ActionableCount,
			StateCounts:     summary.StateCounts,
			Warnings:        summary.Warnings,
		})
		oldest = olderAgedItem(oldest, summary.Oldest)
		oldestActionable = olderAgedItem(oldestActionable, summary.OldestActionable)
	}
	for _, item := range reviews.Items {
		if item.Area == "panel-verdicts" && item.Readiness == "needs_review" && strings.Contains(item.NextAction, "needs_human") {
			out.PendingPanelNeedsHuman++
		}
		if reviewOldest := reviewQueueAgedItem(item, now); reviewOldest != nil {
			oldest = olderAgedItem(oldest, reviewOldest)
		}
	}
	out.OldestItem = oldest
	out.OldestActionableItem = oldestActionable
	return out, pendingStranger
}

func orientMemoryReviewCards(workbench work.WorkbenchReport) []orientMemoryReviewCard {
	var out []orientMemoryReviewCard
	for _, item := range workbench.Open {
		for _, card := range item.ReviewCards {
			if strings.TrimSpace(card.CandidateID) == "" {
				continue
			}
			out = append(out, orientMemoryReviewCard{
				ID:                  card.ID,
				WorkID:              card.WorkID,
				CandidateID:         card.CandidateID,
				CandidateState:      card.CandidateState,
				PanelRunID:          card.PanelRunID,
				PanelRunRef:         card.PanelRunRef,
				PromotionSummaryRef: card.PromotionSummaryRef,
				PromotionCount:      card.PromotionCount,
				Command:             append([]string{}, card.Command...),
				Boundary:            card.Boundary,
				NextAction:          card.Title,
			})
		}
	}
	return out
}

func orientOperatorGateCards(workbench work.WorkbenchReport) []orientOperatorGateCard {
	var out []orientOperatorGateCard
	for _, item := range workbench.Open {
		for _, card := range item.ReviewCards {
			if strings.TrimSpace(card.CandidateID) != "" {
				continue
			}
			out = append(out, orientOperatorGateCard{
				ID:                 card.ID,
				Kind:               card.Kind,
				WorkID:             card.WorkID,
				GateID:             card.GateID,
				SourceKind:         card.SourceKind,
				Command:            append([]string{}, card.Command...),
				RequiredInputs:     append([]string{}, card.RequiredInputs...),
				ExpectedValidation: append([]string{}, card.ExpectedValidation...),
				StopConditions:     append([]string{}, card.StopConditions...),
				Boundary:           card.Boundary,
				Proof:              card.Proof,
				NextAction:         card.Title,
			})
		}
	}
	return out
}

func buildOrientSourceControl(reviews reviewartifact.QueueReport) *orientSourceControl {
	out := &orientSourceControl{
		Commands: []orientDrillDown{
			{Label: "source-control cleanup reviews", Command: []string{"compound", "reviews", "--area", authority.SourceControlCleanupReviewArea, "--json"}},
			{Label: "source-control snapshot", Command: []string{"compound", "authority", "git", ".", "--json"}},
		},
	}
	var latest *reviewartifact.QueueItem
	for _, item := range reviews.Items {
		if item.Area != authority.SourceControlCleanupReviewArea {
			continue
		}
		out.ReviewCount++
		if sourceControlReviewItemOpen(item) {
			out.ReviewOpenCount++
		} else if sourceControlReviewItemRecorded(item) {
			out.ReviewRecordedCount++
		}
		if latest == nil || queueItemNewer(item, *latest) {
			copyItem := item
			latest = &copyItem
		}
	}
	if latest != nil {
		out.LatestReviewID = latest.ID
		out.LatestReviewNextAction = latest.NextAction
	}
	wd, err := os.Getwd()
	if err == nil {
		if snap, snapErr := authority.ReadGitSnapshot(wd); snapErr == nil {
			out.LiveStatus = snap.Status
			out.GitRoot = snap.GitRoot
			out.Branch = snap.Branch
			out.Head = snap.Head
			out.Ahead = snap.Ahead
			out.Behind = snap.Behind
			out.ChangedPaths = snap.ChangedPaths
			out.Staged = snap.Staged
			out.Unstaged = snap.Unstaged
			out.Untracked = snap.Untracked
			out.EvidenceRef = snap.EvidenceReference()
			out.HeadReviewRecorded = sourceControlHeadReviewRecorded(reviews, out.EvidenceRef)
		}
	}
	if out.LiveStatus == "" && out.ReviewCount == 0 {
		return nil
	}
	out.NextAction = orientSourceControlNextAction(*out)
	if out.LiveStatus == "dirty" && !out.HeadReviewRecorded {
		out.Commands = append(out.Commands, orientDrillDown{Label: "source-control dirty plan", Command: []string{"compound", "authority", "dirty-plan", ".", "--json"}})
	}
	return out
}

func sourceControlReviewItemOpen(item reviewartifact.QueueItem) bool {
	if reviewItemNeedsOwner(item) {
		return true
	}
	switch strings.TrimSpace(item.Readiness) {
	case "blocked", "ready_to_record":
		return true
	}
	return false
}

func sourceControlReviewItemRecorded(item reviewartifact.QueueItem) bool {
	switch strings.TrimSpace(item.Readiness) {
	case "review_recorded", "no_action":
		return true
	}
	// Terminal words are owned by reviewartifact.TerminalStatus.
	return reviewartifact.TerminalStatus(item.Status)
}

func sourceControlHeadReviewRecorded(reviews reviewartifact.QueueReport, evidenceRef string) bool {
	evidenceRef = strings.TrimSpace(evidenceRef)
	if evidenceRef == "" {
		return false
	}
	for _, item := range reviews.Items {
		if item.Area != authority.SourceControlCleanupReviewArea || !sourceControlReviewItemRecorded(item) {
			continue
		}
		for _, ref := range item.EvidenceRefs {
			if strings.TrimSpace(ref) == evidenceRef {
				return true
			}
		}
	}
	return false
}

func queueItemNewer(candidate, current reviewartifact.QueueItem) bool {
	candidateTime, candidateOK := parseCompoundTime(candidate.CreatedAt)
	currentTime, currentOK := parseCompoundTime(current.CreatedAt)
	if candidateOK && currentOK {
		return candidateTime.After(currentTime)
	}
	if candidateOK != currentOK {
		return candidateOK
	}
	return candidate.ID > current.ID
}

func orientSourceControlNextAction(source orientSourceControl) string {
	if source.LiveStatus == "dirty" {
		if source.ReviewOpenCount > 0 {
			return "source-control dirty and cleanup review is open; record owner decision before staging, cleaning, committing, or discarding"
		}
		if source.HeadReviewRecorded {
			return "source-control dirty with review receipt for current head; inspect cleanup receipts before touching git, create fresh dirty-plan only if changed paths drifted"
		}
		return "source-control dirty without a cleanup receipt for current head; run non-repo-mutating dirty-plan before mutation work"
	}
	if source.LiveStatus == "clean" {
		if source.ReviewRecordedCount > 0 {
			return "source-control clean; recorded cleanup receipts are historical evidence, not current operator debt"
		}
		return "source-control clean"
	}
	if source.ReviewRecordedCount > 0 || source.ReviewOpenCount > 0 {
		return "source-control live snapshot unavailable; inspect cleanup review receipts before git mutation"
	}
	return "source-control state unavailable"
}

func buildOrientSourceIntake(homePath string, now time.Time) *orientSourceIntake {
	projection, err := observation.LoadSourceIntakeProjection(homePath, now, 12)
	if err != nil || projection.RowCount == 0 {
		return nil
	}
	out := &orientSourceIntake{
		RecordCount:         projection.RecordCount,
		RowCount:            projection.RowCount,
		AppliedCount:        projection.Counts.Applied,
		ReviewRecordedCount: projection.Counts.ReviewRecorded,
		SkippedCount:        projection.Counts.Skipped,
		BlockedPrivateCount: projection.Counts.BlockedPrivate,
		UnsupportedCount:    projection.Counts.Unsupported,
		ActionCardCount:     projection.ActionCardCount,
		ActionCards:         append([]observation.SourceIntakeActionCard{}, projection.ActionCards...),
		NextAction:          "inspect source-intake projection before chasing owner-specific ledgers",
		Commands: []orientDrillDown{
			{Label: "source intake", Command: []string{"compound", "source", "intake", "--json"}},
		},
	}
	if len(projection.Rows) > 0 {
		row := projection.Rows[0]
		out.LastSource = reviewartifact.FirstNonEmpty(row.SourceKind+":"+row.SourceRef, row.SourceKind)
		out.LastSubject = reviewartifact.FirstNonEmpty(row.SubjectKind+":"+row.SubjectID, row.SubjectKind)
		out.LastRoute = row.Route
		out.LastOwner = row.Owner
		out.LastCapturedAt = row.SourceCapturedAt
	}
	return out
}

func buildOrientObservationReviews(reviews reviewartifact.QueueReport) *orientObservationReviews {
	out := orientObservationReviews{
		Commands: []orientDrillDown{
			{Label: "observation candidates", Command: []string{"compound", "reviews", "--area", observation.ObservationReviewArea}},
			{Label: "observation panel verdicts", Command: []string{"compound", "reviews", "--area", "panel-verdicts"}},
		},
	}
	for _, item := range reviews.Items {
		if !reviewItemNeedsOwner(item) {
			continue
		}
		switch item.Area {
		case observation.ObservationReviewArea:
			out.ObservationCandidateReviewCount++
		case "panel-verdicts":
			if item.DecisionCard != nil && item.DecisionCard.Meta["target_kind"] == string(candidates.CapabilityObs) {
				out.ObservationPanelVerdictCount++
			}
		}
	}
	out.TotalReviewDebt = out.ObservationCandidateReviewCount + out.ObservationPanelVerdictCount
	if out.TotalReviewDebt == 0 {
		return nil
	}
	if out.ObservationCandidateReviewCount > 0 {
		out.NextAction = "plan observation lens review from observation-candidates, then run bounded panel verdict proof"
	} else {
		out.NextAction = "review observation panel verdicts; keep lens output evidence/candidate material"
	}
	return &out
}

func buildOrientTensionCards(homePath string, reviews reviewartifact.QueueReport, now time.Time, strangerExcludeTopic string) (*orientTensionCards, *orientPendingStrangerDeal) {
	store, err := mem.Open(homePath)
	if err != nil {
		return nil, nil
	}
	claims, err := store.AllCurrent()
	if err != nil {
		return nil, nil
	}
	liveClaims, superseded := map[string]bool{}, map[string]bool{}
	for _, claim := range claims {
		liveClaims[claim.ID] = true
		if claim.Supersedes != "" {
			superseded[claim.Supersedes] = true
		}
	}
	view, err := decisions.StandingLaw(store, homePath, now)
	if err != nil {
		return nil, nil
	}
	cards := selectOrientTensionCards(reviews, liveClaims, superseded, view)
	// S3b is prepared read-only here. The report builder writes its cursor first, then
	// atomically finalizes and attaches the card so a failed report never records a deal.
	pending := prepareOrientStrangerDeal(store, homePath, strangerExcludeTopic, now)
	return cards, pending
}

// strangerCardCooldown is D5's re-deal cooldown: a candidate already dealt as a stranger card
// within this window is not dealt again, so one untouched candidate cannot monopolize every
// session's one weak-tie slot.
const strangerCardCooldown = 7 * 24 * time.Hour

// orientStrangerDealPreparedObserver is a test seam invoked after a candidate passes the
// pool/cooldown filters but before the deal is committed. Production leaves it nil.
var orientStrangerDealPreparedObserver func()

type orientPendingStrangerDeal struct {
	store       *mem.Store
	card        orientTensionCard
	preparation mem.StrangerDealPreparation
}

// prepareOrientStrangerDeal selects at most one cross-topic, proposed/parked candidate without
// mutating the dealt-log. Selection is provisional: Store.FinalizeStrangerDeal rechecks current
// state and cooldown under the memory lock after the operator report's cursor is durable.
func prepareOrientStrangerDeal(store *mem.Store, homePath, excludeTopic string, now time.Time) *orientPendingStrangerDeal {
	pool, err := strangerCandidatePool(store, now)
	if err != nil {
		return nil
	}
	context, err := store.NewStrangerDealContext(excludeTopic)
	if err != nil {
		return nil
	}
	dealt, err := mem.ReadStrangerDealtLog(homePath)
	if err != nil {
		// Unreadable/corrupt dealt-log: fail closed (F7) rather than deal blind — a
		// cooldown we cannot verify must not be silently treated as expired.
		return nil
	}
	cooled := map[string]bool{}
	for _, row := range dealt {
		dealtAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(row.Ts))
		if perr != nil {
			continue
		}
		if now.Sub(dealtAt) < strangerCardCooldown {
			cooled[row.CandidateRef] = true
		}
	}
	var preparation *mem.StrangerDealPreparation
	for i := range pool {
		c := pool[i]
		if cooled[c.ID] {
			continue
		}
		prepared, err := store.PrepareStrangerDeal(c.ID, context, now)
		if err != nil {
			return nil
		}
		if prepared == nil {
			continue
		}
		preparation = prepared
		break // pool is sorted oldest-first, so the first currently eligible candidate wins
	}
	if preparation == nil {
		return nil
	}
	chosen := preparation.CardSnapshot()
	cardID := "stranger:" + chosen.CandidateRef
	relationship := "un-adjudicated weak tie"
	if strings.TrimSpace(excludeTopic) != "" {
		relationship = "outside the excluded topic subtree, un-adjudicated"
	}
	summary := fmt.Sprintf("stranger: %s.%s = %s (topic: %s, state: %s) — %s", chosen.Entity, chosen.Predicate, chosen.Object, chosen.Topic, chosen.State, relationship)
	if orientStrangerDealPreparedObserver != nil {
		orientStrangerDealPreparedObserver()
	}
	return &orientPendingStrangerDeal{
		store:       store,
		preparation: *preparation,
		card: orientTensionCard{
			ID:      cardID,
			Summary: summary,
			Refs:    []string{chosen.CandidateRef},
			Command: []string{"compound", "memory", "adjudicate", chosen.CandidateRef, "--verdict", "park"},
		},
	}
}

func attachOrientStrangerCard(report *orientOperatorReport, card orientTensionCard) {
	if report.NeedsMe.TensionCards == nil {
		report.NeedsMe.TensionCards = &orientTensionCards{Items: []orientTensionCard{}, Commands: []orientDrillDown{}}
	}
	command := orientDrillDown{Label: "stranger card", Command: append([]string{}, card.Command...)}
	report.NeedsMe.TensionCards.Items = append(report.NeedsMe.TensionCards.Items, card)
	report.NeedsMe.TensionCards.Commands = append(report.NeedsMe.TensionCards.Commands, command)
	report.DrillDown = append(report.DrillDown, command)
}

// strangerCandidatePool returns every proposed|parked memory candidate via the packet's named
// query surface (Store.ListCandidates — authority-admitted, not the raw inbox state; see
// TestOrientOperatorUsesMemoryCandidateAuthorityForNeedsMe for why raw state is misleading),
// sorted oldest-first so the dealer's selection is deterministic given equal inputs.
func strangerCandidatePool(store *mem.Store, now time.Time) ([]mem.CandidateListItem, error) {
	var pool []mem.CandidateListItem
	for _, state := range []string{"proposed", "parked"} {
		report, err := store.ListCandidates(now, mem.CandidateListOptions{State: state})
		if err != nil {
			return nil, err
		}
		pool = append(pool, report.Items...)
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].CreatedAt != pool[j].CreatedAt {
			return pool[i].CreatedAt < pool[j].CreatedAt
		}
		return pool[i].ID < pool[j].ID
	})
	return pool, nil
}

func selectOrientTensionCards(reviews reviewartifact.QueueReport, liveClaims, superseded map[string]bool, view decisions.StandingLawView) *orientTensionCards {
	items := make([]orientTensionCard, 0, 3)
	for _, item := range reviews.Items {
		if len(items) == 3 || item.Area != mem.ReconcileContestedArea || !reviewItemNeedsOwner(item) {
			continue
		}
		refs := claimEvidenceRefs(item.EvidenceRefs)
		live := len(refs) >= 2
		for _, ref := range refs {
			live = live && liveClaims[ref] && !superseded[ref]
		}
		if live {
			items = append(items, orientTensionCard{ID: item.ID, Summary: item.OperatorSummary, Refs: refs, Command: []string{"compound", "reviews", "--area", mem.ReconcileContestedArea}})
		}
	}
	liveClaim, liveDecision := map[string]bool{}, map[string]bool{}
	for _, record := range view.Records {
		liveClaim[record.ClaimID], liveDecision[record.DecisionID] = true, true
	}
	for _, conflict := range view.Conflicts {
		if len(items) == 3 || !liveClaim[conflict.ConflictsWithClaim] || !liveDecision[conflict.ConflictsWithDecision] {
			continue
		}
		refs := []string{conflict.CandidateID, conflict.ConflictsWithClaim, conflict.ConflictsWithDecision}
		summary := fmt.Sprintf("%s conflicts with standing law %s (%s) — supersede %s?", reviewartifact.FirstNonEmpty(conflict.Restatement, conflict.Verbatim, conflict.CandidateID), reviewartifact.FirstNonEmpty(conflict.ConflictingTitle, conflict.ConflictsWithClaim), conflict.ConflictsWithClaim, conflict.ConflictsWithDecision)
		items = append(items, orientTensionCard{ID: conflict.CandidateID, Summary: summary, Refs: refs, Command: []string{"compound", "decisions", "standing-law"}})
	}
	if len(items) == 0 {
		return nil
	}
	// Deal-time gate deliberately verifies cited claim/decision IDs against the
	// current fold and queue disposition. It does not rerun the reconcile
	// detector or validate non-claim evidence references.
	return &orientTensionCards{Items: items, Commands: []orientDrillDown{{Label: "tension reviews", Command: []string{"compound", "reviews", "--area", mem.ReconcileContestedArea}}, {Label: "standing-law conflicts", Command: []string{"compound", "decisions", "standing-law"}}}}
}

func claimEvidenceRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref, "claim_") {
			out = append(out, ref)
		}
	}
	return out
}

func buildOrientCapabilityFeedback(homePath string, reviews reviewartifact.QueueReport, now time.Time) *orientCapabilityFeedback {
	impact, err := panelpkg.BuildPanelCapabilityFeedbackImpact(homePath, reviews, 5, now)
	if err != nil {
		return nil
	}
	if impact.PendingReviewCount == 0 && impact.AppliedReviewCount == 0 && impact.RejectedCount == 0 && impact.KeptReviewCount == 0 {
		return nil
	}
	out := &orientCapabilityFeedback{
		Status:             impact.Status,
		PendingReviewCount: impact.PendingReviewCount,
		AppliedReviewCount: impact.AppliedReviewCount,
		RejectedCount:      impact.RejectedCount,
		KeptReviewCount:    impact.KeptReviewCount,
		AppliedEdges:       impact.AppliedEdges,
		EdgeKindCounts:     impact.EdgeKindCounts,
		Recent:             impact.Recent,
		Commands: []orientDrillDown{
			{Label: "capability feedback", Command: []string{"compound", "reviews", "--area", panelpkg.PanelCapabilityFeedbackReviewArea}},
			{Label: "capability authority", Command: []string{"compound", "capability", "authority"}},
		},
	}
	if impact.PendingReviewCount > 0 {
		out.NextAction = "review panel capability feedback; edited review is required before CapabilityGraph learning"
	} else {
		out.NextAction = "capability feedback already reviewed; inspect capability authority when lens surfacing looks surprising"
	}
	return out
}

func buildOrientAdaptationIntake(reviews reviewartifact.QueueReport) *orientAdaptationIntake {
	out := &orientAdaptationIntake{
		Commands: []orientDrillDown{
			{Label: "external repo intake", Command: []string{"compound", "reviews", "--area", capgraph.ExternalRepoIntakeReviewArea}},
			{Label: "capability evaluation intake", Command: []string{"compound", "reviews", "--area", "capability-evaluation-intake"}},
			{Label: "capability authority", Command: []string{"compound", "capability", "authority"}},
		},
	}
	for _, item := range reviews.Items {
		switch item.Area {
		case capgraph.ExternalRepoIntakeReviewArea:
			out.ExternalRepoCount++
			if adaptationReviewItemOpen(item) {
				out.ExternalRepoOpenCount++
			} else if adaptationReviewItemRecorded(item) {
				out.ExternalRepoReviewedCount++
			}
			appendOrientAdaptationRecent(out, item)
		case "capability-evaluation-intake":
			out.CapabilityEvaluationCount++
			if adaptationReviewItemOpen(item) {
				out.CapabilityEvaluationOpenCount++
			} else if adaptationReviewItemRecorded(item) {
				out.CapabilityEvaluationReviewedCount++
			}
			appendOrientAdaptationRecent(out, item)
		}
	}
	out.ActionableCount = out.ExternalRepoOpenCount + out.CapabilityEvaluationOpenCount
	total := out.ExternalRepoCount + out.CapabilityEvaluationCount
	if total == 0 {
		return nil
	}
	if out.ActionableCount > 0 {
		out.NextAction = "review adaptation intake before capability evaluation, manifest import, or owner adoption"
	} else {
		out.NextAction = "adaptation intake already reviewed/recorded; use these receipts before copying skills, repos, or creating new capability owners"
	}
	return out
}

func appendOrientAdaptationRecent(out *orientAdaptationIntake, item reviewartifact.QueueItem) {
	if out == nil || len(out.Recent) >= 5 {
		return
	}
	out.Recent = append(out.Recent, orientAdaptationIntakeItem{
		Area:         item.Area,
		ID:           item.ID,
		Status:       item.Status,
		Readiness:    item.Readiness,
		TargetPath:   reviewartifact.FirstNonEmpty(item.TargetPath, item.CandidateID, item.SourcePath),
		Owner:        item.Owner,
		DecisionHint: item.GraphDecisionHint,
		NextAction:   item.NextAction,
		EvidenceRefs: orientLimitStrings(item.EvidenceRefs, 6),
	})
}

func orientLimitStrings(values []string, max int) []string {
	if max <= 0 || len(values) == 0 {
		return nil
	}
	if len(values) > max {
		values = values[:max]
	}
	return append([]string(nil), values...)
}

func adaptationReviewItemOpen(item reviewartifact.QueueItem) bool {
	if reviewItemNeedsOwner(item) {
		return true
	}
	switch strings.TrimSpace(item.Readiness) {
	case "blocked", "ready_to_record":
		return true
	}
	return false
}

func adaptationReviewItemRecorded(item reviewartifact.QueueItem) bool {
	switch strings.TrimSpace(item.Readiness) {
	case "review_recorded", "no_action":
		return true
	}
	// Terminal words are owned by reviewartifact.TerminalStatus.
	return reviewartifact.TerminalStatus(item.Status)
}

func reviewItemNeedsOwner(item reviewartifact.QueueItem) bool {
	switch strings.TrimSpace(item.Readiness) {
	case "needs_review", "ready_to_apply":
		return true
	}
	switch strings.TrimSpace(item.Status) {
	case "review_required", "ready_to_apply":
		return true
	}
	return false
}

func orientAdmissionPreviews(previews []work.WorkbenchAdmissionPreview) []orientAdmissionPreview {
	out := make([]orientAdmissionPreview, 0, len(previews))
	for _, preview := range previews {
		item := orientAdmissionPreview{
			PlanID:                 preview.PlanID,
			WorkID:                 preview.WorkID,
			WorkOwner:              preview.WorkOwner,
			WorkState:              preview.WorkState,
			Staleness:              preview.Staleness,
			Age:                    preview.Age,
			NextAction:             preview.NextAction,
			ActionSummary:          preview.ActionSummary,
			PlanPath:               preview.PlanPath,
			AdmitCommand:           append([]string{}, preview.AdmitCommand...),
			RetireDryRun:           append([]string{}, preview.RetireDryRun...),
			PolicyAllowed:          preview.PolicyAllowed,
			PolicyOutcome:          preview.PolicyOutcome,
			RequiresOperator:       preview.RequiresOperator,
			AdmissionBlockedReason: preview.AdmissionBlockedReason,
			ActionContext:          cloneOrientWorkActionContext(preview.ActionContext),
		}
		if item.ActionContext != nil && item.ActionContext.Kind == "workbench_review_card" {
			item.SourceReviewCardID = item.ActionContext.SourceID
		}
		out = append(out, item)
	}
	return out
}

func cloneOrientWorkActionContext(ctx *work.WorkActionContext) *work.WorkActionContext {
	if ctx == nil {
		return nil
	}
	out := *ctx
	out.Command = append([]string{}, ctx.Command...)
	out.RequiredInputs = append([]string{}, ctx.RequiredInputs...)
	out.ExpectedValidation = append([]string{}, ctx.ExpectedValidation...)
	out.StopConditions = append([]string{}, ctx.StopConditions...)
	out.EvidenceRefs = append([]string{}, ctx.EvidenceRefs...)
	return &out
}

func orientAdmissionPreviewContextKind(preview orientAdmissionPreview) string {
	if preview.ActionContext == nil {
		return "-"
	}
	return reviewartifact.FirstNonEmpty(preview.ActionContext.Kind, "-")
}

func buildOrientRunning(homePath string, report work.WorkbenchReport, now time.Time) orientRunning {
	openIDs := map[string]bool{}
	adjacent := buildOrientAdjacentProgress(report.DispatchNext.AdjacentProgress)
	out := orientRunning{
		OpenCount:             report.Counts.Open,
		InProgress:            report.Counts.InProgress,
		AdmissionPreviewCount: len(report.AdmissionPreviews),
		AdmissionPreviews:     orientAdmissionPreviews(report.AdmissionPreviews),
		OpenWork:              []orientOpenWorkItem{},
		AdjacentProgress:      adjacent,
	}
	for _, item := range report.Open {
		openIDs[item.WorkID] = true
		actor := strings.TrimSpace(item.ClaimedBy)
		open := orientOpenWorkItem{
			WorkID:          item.WorkID,
			State:           item.State,
			Actor:           actor,
			ClaimedAt:       item.ClaimedAt,
			NextAction:      orientOpenWorkNextAction(item, adjacent),
			ObservatoryTags: append([]string{}, item.ObservatoryTags...),
			Surface:         item.CanonicalSurface,
		}
		if claimedAt, ok := parseCompoundTime(item.ClaimedAt); ok {
			open.ClaimedAgeSeconds = ageSeconds(now, claimedAt)
			open.ClaimedAge = formatDuration(open.ClaimedAgeSeconds)
		}
		out.OpenWork = append(out.OpenWork, open)
	}
	out.NonStaleLeaseCount = activeLeaseCount(homePath, openIDs)
	for _, run := range report.DispatchStepRuns {
		if !orientOperatorDispatchStepRunActive(run) {
			continue
		}
		if at, ok := parseCompoundTime(run.GeneratedAt); ok {
			out.LastDispatchStepRun = &orientAgedItem{
				Kind:       "dispatch_step_run",
				ID:         run.ID,
				Path:       run.ResultPath,
				At:         at.UTC().Format(time.RFC3339),
				AgeSeconds: ageSeconds(now, at),
				Age:        formatDuration(ageSeconds(now, at)),
			}
		}
		break
	}
	return out
}

func orientOpenWorkNextAction(item work.WorkbenchItem, adjacent *orientAdjacentProgress) string {
	if adjacent == nil || !adjacent.Available || !slices.Contains(adjacent.HeldWorkIDs, item.WorkID) {
		return item.NextAction
	}
	if len(item.ReviewCards) > 0 {
		card := item.ReviewCards[0]
		if strings.TrimSpace(card.ID) != "" {
			surface := "needs_me.operator_gate_cards"
			if strings.TrimSpace(card.CandidateID) != "" {
				surface = "needs_me.memory_review_cards"
			}
			return item.NextAction + "; use " + surface + " action " + card.ID
		}
		return item.NextAction
	}
	actionIDs := orientAdjacentActionIDsForOpenWork(item, adjacent.NonPacketActionIDs)
	if len(actionIDs) == 0 {
		actionIDs = append([]string{}, adjacent.NonPacketActionIDs...)
	}
	if len(actionIDs) > 0 {
		return "held on operator valve; use running.adjacent_progress actions: " + strings.Join(actionIDs, ", ")
	}
	if strings.TrimSpace(adjacent.NextAction) != "" {
		return "held on operator valve; use running.adjacent_progress: " + adjacent.NextAction
	}
	return "held on operator valve; inspect running.adjacent_progress"
}

func buildOrientAdjacentProgress(adjacent *work.WorkbenchAdjacentProgress) *orientAdjacentProgress {
	if adjacent == nil {
		return nil
	}
	out := &orientAdjacentProgress{
		Available:                    adjacent.Available,
		Reason:                       adjacent.Reason,
		NextAction:                   adjacent.NextAction,
		HeldWorkIDs:                  append([]string{}, adjacent.HeldWorkIDs...),
		PacketCandidateCount:         adjacent.PacketCandidateCount,
		NonPacketActionCount:         len(adjacent.NonPacketActions),
		ConsumedNonPacketActionCount: len(adjacent.ConsumedNonPacketActions),
		NonPacketActionIDs:           []string{},
		ConsumedNonPacketActionIDs:   []string{},
	}
	for _, action := range adjacent.NonPacketActions {
		if strings.TrimSpace(action.ID) == "" {
			continue
		}
		out.NonPacketActionIDs = append(out.NonPacketActionIDs, action.ID)
	}
	for _, action := range adjacent.ConsumedNonPacketActions {
		if strings.TrimSpace(action.ID) == "" {
			continue
		}
		out.ConsumedNonPacketActionIDs = append(out.ConsumedNonPacketActionIDs, action.ID)
	}
	return out
}

func orientOperatorDispatchStepRunActive(run work.DispatchStepRunSummary) bool {
	if strings.TrimSpace(run.Status) == "resolved" {
		return false
	}
	switch strings.TrimSpace(run.WaitReason) {
	case "no_open_work", "source_packet_terminal", "terminal_work":
		return false
	default:
		return true
	}
}

func buildOrientChanged(homePath string, report work.WorkbenchReport, currentSeq, lastSeenSeq int64, firstRun bool, cursorPath string) orientChanged {
	delta := currentSeq - lastSeenSeq
	if firstRun {
		delta = currentSeq
	}
	if delta < 0 {
		delta = 0
	}
	out := orientChanged{
		CurrentClaimSeq: currentSeq,
		LastSeenSeq:     lastSeenSeq,
		ClaimsSeqDelta:  delta,
		FirstRun:        firstRun,
		CursorPath:      cursorPath,
		Recent:          []orientRecentChangeItem{},
	}
	for _, item := range report.Recent {
		change := orientRecentChangeItem{
			WorkID:       item.WorkID,
			ReceiptID:    item.ReceiptID,
			TransitionID: item.TransitionID,
			MoveType:     item.MoveType,
			Changed:      item.Changed,
			EvidenceRefs: append([]string{}, item.EvidenceRefs...),
		}
		if item.ReceiptID != "" {
			if rec, err := readReceipt(homePath, item.ReceiptID); err == nil {
				change.MoveType = rec.MoveType
				change.Summary = rec.StateDelta.Summary
				change.Changed = rec.EffectiveChanged()
				change.CreatedAt = rec.CreatedAt
			}
		}
		out.Recent = append(out.Recent, change)
	}
	return out
}

type orientCandidateInboxSummary struct {
	Count            int
	CorruptCount     int
	ActionableCount  int
	StateCounts      map[string]int
	Warnings         []string
	Oldest           *orientAgedItem
	OldestActionable *orientAgedItem
}

type candidateReadinessPayload struct {
	State          string            `json:"state"`
	Status         string            `json:"status"`
	Verdict        string            `json:"verdict"`
	Kind           string            `json:"kind"`
	Subject        string            `json:"subject"`
	Detail         string            `json:"detail"`
	PromoteAllowed *bool             `json:"promote_allowed"`
	EvidenceRefs   []string          `json:"evidence_refs"`
	Generated      string            `json:"generated"`
	Remaining      *int              `json:"remaining"`
	Proposals      []json.RawMessage `json:"proposals"`
	AllMisses      []json.RawMessage `json:"all_misses"`
	FalseTriggers  []json.RawMessage `json:"false_triggers"`
	PerReceipt     []json.RawMessage `json:"perReceipt"`
}

func candidateInboxSummary(homePath, dir, kind string, now time.Time) orientCandidateInboxSummary {
	out := orientCandidateInboxSummary{StateCounts: map[string]int{}}
	page, err := candidates.ReadPage(homePath, candidates.Kind(kind), "", 0)
	if err != nil {
		out.Warnings = append(out.Warnings, err.Error())
		return out
	}
	for _, entry := range page.Entries {
		filename := string(entry.EntryCursor)
		if entry.Corruption != nil {
			out.CorruptCount++
			out.Warnings = append(out.Warnings, entry.Corruption.Error())
			continue
		}
		var payload candidateReadinessPayload
		if err := json.Unmarshal(entry.Record.Body, &payload); err != nil {
			out.CorruptCount++
			out.Warnings = append(out.Warnings, fmt.Sprintf("candidate entry %q is corrupt: invalid_json (%v)", entry.EntryCursor, err))
			continue
		}
		out.Count++
		path := filepath.Join(dir, filename)
		fullPath := filepath.Join(homePath, path)
		state := candidateReadinessStateFromRecord(homePath, fullPath, entry.Record.Body, payload, page, entry.EntryCursor)
		out.StateCounts[state]++
		actionable := candidateStateActionable(state)
		if actionable {
			out.ActionableCount++
		}
		at := entry.Record.ModifiedAt()
		if at.IsZero() {
			at = now
		}
		aged := &orientAgedItem{
			Kind:       "candidate:" + kind,
			ID:         entry.Record.ID,
			Path:       filepath.ToSlash(path),
			At:         at.UTC().Format(time.RFC3339),
			AgeSeconds: ageSeconds(now, at),
			Age:        formatDuration(ageSeconds(now, at)),
		}
		out.Oldest = olderAgedItem(out.Oldest, aged)
		if actionable {
			out.OldestActionable = olderAgedItem(out.OldestActionable, aged)
		}
	}
	return out
}

func memoryCandidateInboxSummary(homePath string, now time.Time) orientCandidateInboxSummary {
	out := orientCandidateInboxSummary{StateCounts: map[string]int{}}
	store, err := mem.Open(homePath)
	if err != nil {
		out.Warnings = append(out.Warnings, "memory candidate projection unavailable: "+err.Error())
		return out
	}
	report, err := store.ListCandidates(now, mem.CandidateListOptions{})
	if err != nil {
		out.Warnings = append(out.Warnings, "memory candidate projection unavailable: "+err.Error())
		return out
	}
	out.Count = report.Counts.Total
	out.CorruptCount = report.Counts.Corrupt
	out.Warnings = append(out.Warnings, report.Warnings...)
	for state, count := range report.Counts.ByState {
		out.StateCounts[state] = count
		if candidateStateActionable(state) {
			out.ActionableCount += count
		}
	}
	for _, item := range report.Items {
		aged := memoryCandidateAgedItem(item, now)
		out.Oldest = olderAgedItem(out.Oldest, aged)
		if candidateStateActionable(item.State) {
			out.OldestActionable = olderAgedItem(out.OldestActionable, aged)
		}
	}
	return out
}

func memoryCandidateAgedItem(item mem.CandidateListItem, now time.Time) *orientAgedItem {
	path := filepath.Join(candidates.InboxDir(candidates.Memory), item.ID+".json")
	var at time.Time
	for _, raw := range []string{item.CreatedAt, item.ObservationDate} {
		if parsed, ok := parseCompoundTime(raw); ok {
			at = parsed
			break
		}
	}
	if at.IsZero() {
		if parsed, ok := parseCompoundTime(item.ModifiedAt); ok {
			at = parsed
		}
	}
	if at.IsZero() {
		at = now
	}
	age := ageSeconds(now, at)
	return &orientAgedItem{
		Kind:       "candidate:" + string(candidates.Memory),
		ID:         item.ID,
		Path:       filepath.ToSlash(path),
		At:         at.UTC().Format(time.RFC3339),
		AgeSeconds: age,
		Age:        formatDuration(age),
	}
}

func candidateReadinessStateFromRecord(homePath, path string, data []byte, payload candidateReadinessPayload, page candidates.Page, current candidates.EntryCursor) string {
	if verdict, ok := terminalCandidateVerdict(payload.Verdict); ok {
		return verdict
	}
	state := strings.TrimSpace(payload.State)
	if state == "" {
		state = strings.TrimSpace(payload.Status)
	}
	if state == "" {
		state = strings.TrimSpace(payload.Verdict)
	}
	if state == "" && payload.PromoteAllowed != nil && !*payload.PromoteAllowed {
		state = candidates.NoPromote
	}
	if state == "" && consumedSkillSurfacingFeedback(path, data, payload, page, current) {
		state = candidates.NoPromote
	}
	if state == "" {
		return "unspecified"
	}
	if candidateStateActionable(state) && resolvedPanelLensSelectionGap(homePath, payload) {
		return "resolved"
	}
	return state
}

func terminalCandidateVerdict(verdict string) (string, bool) {
	verdict = strings.TrimSpace(verdict)
	switch verdict {
	case "promote", "promoted", "reject", "rejected", "park", "parked", "operationalize", "retire", "done", "recorded", "resolved", "accepted", "applied", "ok", "no_promote", "merge_existing":
		return verdict, true
	default:
		return "", false
	}
}

func resolvedPanelLensSelectionGap(homePath string, payload candidateReadinessPayload) bool {
	if strings.TrimSpace(payload.Kind) != string(candidates.CapabilityObs) || strings.TrimSpace(payload.Subject) != "panel_lens_selection" {
		return false
	}
	targetKind := candidateDetailValue(payload.Detail, "target_kind")
	domain := candidateDetailValue(payload.Detail, "domain")
	task := candidateDetailValue(payload.Detail, "task")
	if task == "" {
		for _, ref := range payload.EvidenceRefs {
			if value, ok := strings.CutPrefix(ref, "task:"); ok {
				task = strings.TrimSpace(value)
				break
			}
		}
	}
	if targetKind == "" || domain == "" {
		return false
	}
	selection, err := panelpkg.SelectLensesWithContext(homePath, targetKind, panelpkg.LensContext{Domain: domain, Task: task})
	return err == nil && len(selection.MissingDomainLens) == 0
}

func candidateDetailValue(detail, key string) string {
	needle := key + "="
	if key == "task" {
		if _, value, ok := strings.Cut(detail, " "+needle); ok {
			return strings.TrimSpace(value)
		}
	}
	for _, field := range strings.Fields(detail) {
		value, ok := strings.CutPrefix(field, needle)
		if ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func consumedSkillSurfacingFeedback(path string, data []byte, payload candidateReadinessPayload, page candidates.Page, current candidates.EntryCursor) bool {
	if strings.TrimSpace(payload.Generated) == "" || payload.Remaining == nil || *payload.Remaining != 0 {
		return false
	}
	if len(payload.Proposals) > 0 || len(payload.AllMisses) > 0 {
		return false
	}
	if len(payload.FalseTriggers) == 0 && len(payload.PerReceipt) == 0 {
		return false
	}
	return hasSourceNoPromoteForSkillSurfacingReport(path, data, page, current)
}

func hasSourceNoPromoteForSkillSurfacingReport(path string, data []byte, page candidates.Page, current candidates.EntryCursor) bool {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	hashRef := "skill_surfacing_misses:" + hash
	shortSubject := "skill_surfacing_misses:" + hash[:12]
	sourceRef := "source:" + path + "#sha256:" + hash
	for _, entry := range page.Entries {
		if entry.EntryCursor == current || entry.Corruption != nil || entry.Record == nil {
			continue
		}
		var candidate struct {
			Subject        string   `json:"subject"`
			SourceRef      string   `json:"source_ref"`
			Verdict        string   `json:"verdict"`
			PromoteAllowed *bool    `json:"promote_allowed"`
			EvidenceRefs   []string `json:"evidence_refs"`
		}
		if err := json.Unmarshal(entry.Record.Body, &candidate); err != nil {
			continue
		}
		if !candidateNoPromote(candidate.Verdict, candidate.PromoteAllowed) {
			continue
		}
		if candidate.Subject == shortSubject || candidate.SourceRef == path ||
			slices.Contains(candidate.EvidenceRefs, hashRef) || slices.Contains(candidate.EvidenceRefs, sourceRef) {
			return true
		}
	}
	return false
}

func candidateNoPromote(verdict string, promoteAllowed *bool) bool {
	if strings.TrimSpace(verdict) == candidates.NoPromote {
		return true
	}
	return promoteAllowed != nil && !*promoteAllowed
}

func candidateStateActionable(state string) bool {
	switch strings.TrimSpace(state) {
	case "", "unspecified", "proposed", "pending", "needs_review", "review_required", "blocked":
		return true
	case "promote", "promoted", "reject", "rejected", "park", "parked", "operationalize", "retire", "done", "recorded", "resolved", "accepted", "applied", "ok", "no_promote":
		return false
	default:
		return true
	}
}

func reviewQueueAgedItem(item reviewartifact.QueueItem, now time.Time) *orientAgedItem {
	if item.Status == "ok" || item.Status == "applied" || item.Status == "resolved" || item.Status == "accepted" || item.Status == "recorded" {
		return nil
	}
	var at time.Time
	if parsed, ok := parseCompoundTime(item.CreatedAt); ok {
		at = parsed
	} else if item.JSONPath != "" {
		if info, err := os.Stat(item.JSONPath); err == nil {
			at = info.ModTime()
		}
	}
	if at.IsZero() {
		return nil
	}
	return &orientAgedItem{
		Kind:       "review:" + item.Area,
		ID:         item.ID,
		Path:       item.JSONPath,
		At:         at.UTC().Format(time.RFC3339),
		AgeSeconds: ageSeconds(now, at),
		Age:        formatDuration(ageSeconds(now, at)),
	}
}

func activeLeaseCount(homePath string, openIDs map[string]bool) int {
	entries, err := os.ReadDir(filepath.Join(homePath, "work", "leases"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if openIDs[id] {
			count++
		}
	}
	return count
}

func maxClaimSeq(homePath string) (int64, error) {
	path := filepath.Join(homePath, "ledger", "claims.jsonl")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var max int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var claim mem.Claim
		if err := json.Unmarshal(scanner.Bytes(), &claim); err != nil {
			return 0, fmt.Errorf("parse %s: %w", path, err)
		}
		if claim.Seq > max {
			max = claim.Seq
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return max, nil
}

func readOrientOperatorCursor(homePath string) (orientOperatorCursor, string, bool, error) {
	path := orientOperatorCursorPath(homePath)
	var cursor orientOperatorCursor
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cursor, path, true, nil
	}
	if err != nil {
		return cursor, path, false, err
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		return cursor, path, false, err
	}
	return cursor, path, false, nil
}

func writeOrientOperatorCursor(homePath string, seq int64, now time.Time) error {
	path := orientOperatorCursorPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cursor := orientOperatorCursor{
		SchemaVersion: orientOperatorCursorSchemaVersion,
		UpdatedAt:     now.UTC().Format(time.RFC3339),
		LastSeenSeq:   seq,
	}
	data, err := json.MarshalIndent(cursor, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func orientOperatorCursorPath(homePath string) string {
	return filepath.Join(homePath, "projections", "orient-operator-last-seq.json")
}

func readReceipt(homePath, id string) (envelope.Receipt, error) {
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

func olderAgedItem(current, candidate *orientAgedItem) *orientAgedItem {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if candidate.AgeSeconds > current.AgeSeconds {
		return candidate
	}
	return current
}

func parseCompoundTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(0, n), true
	}
	return time.Time{}, false
}

func ageSeconds(now, at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	seconds := int64(now.Sub(at).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

func formatDuration(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	if hours < 48 {
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	return fmt.Sprintf("%dd", days)
}

func printOrientOperatorReport(report orientOperatorReport) {
	fmt.Printf("# operator orient %s\n", report.GeneratedAt)
	fmt.Println()
	fmt.Println("NEEDS-ME")
	fmt.Printf("- candidates: actionable=%d total=%d oldest_actionable=%s oldest_raw=%s\n",
		report.NeedsMe.CandidateActionable,
		report.NeedsMe.CandidateTotal,
		agedLabel(report.NeedsMe.OldestActionableItem),
		agedLabel(report.NeedsMe.OldestItem),
	)
	for _, inbox := range report.NeedsMe.CandidateInboxes {
		if inbox.Count == 0 {
			continue
		}
		fmt.Printf("  - %s: actionable=%d total=%d (%s)\n", inbox.Kind, inbox.ActionableCount, inbox.Count, inbox.Dir)
	}
	fmt.Printf("- reviews: review_required=%d ready_to_apply=%d pending_panel_needs_human=%d\n",
		report.NeedsMe.ReviewRequired,
		report.NeedsMe.ReadyToApply,
		report.NeedsMe.PendingPanelNeedsHuman,
	)
	if report.NeedsMe.MemoryReviewCardCount > 0 {
		fmt.Printf("- memory_review_cards: %d\n", report.NeedsMe.MemoryReviewCardCount)
		for _, card := range report.NeedsMe.MemoryReviewCards {
			count := "-"
			if card.PromotionCount != nil {
				count = strconv.Itoa(*card.PromotionCount)
			}
			fmt.Printf(" - memory_review_card: %s work=%s candidate=%s state=%s panel=%s promoted=%s next=%s\n",
				card.ID,
				card.WorkID,
				card.CandidateID,
				emptyDash(card.CandidateState),
				emptyDash(card.PanelRunID),
				count,
				card.NextAction,
			)
		}
	}
	if report.NeedsMe.OperatorGateCardCount > 0 {
		fmt.Printf("- operator_gate_cards: %d\n", report.NeedsMe.OperatorGateCardCount)
		for _, card := range report.NeedsMe.OperatorGateCards {
			fmt.Printf(" - operator_gate_card: %s kind=%s work=%s source=%s gate=%s next=%s\n",
				card.ID,
				card.Kind,
				card.WorkID,
				emptyDash(card.SourceKind),
				emptyDash(card.GateID),
				card.NextAction,
			)
		}
	}
	if obs := report.NeedsMe.ObservationReviews; obs != nil {
		fmt.Printf("- observation_reviews: candidate_reviews=%d panel_verdicts=%d total=%d next=%s\n",
			obs.ObservationCandidateReviewCount,
			obs.ObservationPanelVerdictCount,
			obs.TotalReviewDebt,
			obs.NextAction,
		)
	}
	if tensions := report.NeedsMe.TensionCards; tensions != nil && len(tensions.Items) > 0 {
		card := tensions.Items[0]
		fmt.Printf("- tension_cards: %d headline=%s refs=%s command=%s\n", len(tensions.Items), card.Summary, strings.Join(card.Refs, ","), strings.Join(card.Command, " "))
		for _, card := range tensions.Items {
			fmt.Printf(" - tension_card: %s refs=%s question=%s command=%s\n", card.ID, strings.Join(card.Refs, ","), card.Summary, strings.Join(card.Command, " "))
		}
	}
	if feedback := report.NeedsMe.CapabilityFeedback; feedback != nil {
		fmt.Printf("- capability_feedback: pending=%d applied_reviews=%d applied_edges=%d edge_kinds=%s next=%s\n",
			feedback.PendingReviewCount,
			feedback.AppliedReviewCount,
			feedback.AppliedEdges,
			formatIntMap(feedback.EdgeKindCounts),
			feedback.NextAction,
		)
		for _, item := range feedback.Recent {
			fmt.Printf(" - capability_feedback_review: %s status=%s verdict=%s edges=%d kinds=%s\n",
				item.ID,
				item.Status,
				emptyDash(item.VerdictID),
				item.AppliedEdges,
				formatIntMap(item.EdgeKindCounts),
			)
		}
	}
	if adaptation := report.NeedsMe.AdaptationIntake; adaptation != nil {
		fmt.Printf("- adaptation_intake: external_repos=%d open=%d reviewed=%d capability_evaluations=%d open=%d reviewed=%d actionable=%d next=%s\n",
			adaptation.ExternalRepoCount,
			adaptation.ExternalRepoOpenCount,
			adaptation.ExternalRepoReviewedCount,
			adaptation.CapabilityEvaluationCount,
			adaptation.CapabilityEvaluationOpenCount,
			adaptation.CapabilityEvaluationReviewedCount,
			adaptation.ActionableCount,
			adaptation.NextAction,
		)
		for _, item := range adaptation.Recent {
			fmt.Printf(" - adaptation_item: %s/%s status=%s readiness=%s target=%s owner=%s decision=%s next=%s\n",
				item.Area,
				item.ID,
				emptyDash(item.Status),
				emptyDash(item.Readiness),
				emptyDash(item.TargetPath),
				emptyDash(item.Owner),
				emptyDash(item.DecisionHint),
				emptyDash(item.NextAction),
			)
		}
	}
	if source := report.NeedsMe.SourceControl; source != nil {
		fmt.Printf("- source_control: live=%s changed=%d staged=%d unstaged=%d untracked=%d reviews=%d open=%d recorded=%d head_review_recorded=%v next=%s\n",
			emptyDash(source.LiveStatus),
			source.ChangedPaths,
			source.Staged,
			source.Unstaged,
			source.Untracked,
			source.ReviewCount,
			source.ReviewOpenCount,
			source.ReviewRecordedCount,
			source.HeadReviewRecorded,
			source.NextAction,
		)
		if source.EvidenceRef != "" {
			fmt.Printf(" - source_control_ref: %s head=%s branch=%s root=%s\n", source.EvidenceRef, emptyDash(source.Head), emptyDash(source.Branch), emptyDash(source.GitRoot))
		}
		if source.LatestReviewID != "" {
			fmt.Printf(" - source_control_review: %s next=%s\n", source.LatestReviewID, emptyDash(source.LatestReviewNextAction))
		}
	}
	if intake := report.NeedsMe.SourceIntake; intake != nil {
		fmt.Printf("- source_intake: records=%d rows=%d applied=%d review_recorded=%d blocked_private=%d last=%s/%s owner=%s next=%s\n",
			intake.RecordCount,
			intake.RowCount,
			intake.AppliedCount,
			intake.ReviewRecordedCount,
			intake.BlockedPrivateCount,
			intake.LastSource,
			intake.LastRoute,
			intake.LastOwner,
			intake.NextAction,
		)
	}
	printOrientProductLoops(report.ProductLoops)
	if ctx := report.SessionContext; ctx != nil {
		fmt.Println()
		fmt.Println("SESSION-CONTEXT")
		fmt.Printf("- status=%s source=%s harness=%s repo_kind=%s task=%s\n", ctx.Status, ctx.Source, ctx.Harness, ctx.RepoKind, ctx.Task)
		fmt.Printf("- command: %s\n", strings.Join(ctx.Command, " "))
		fmt.Printf("- boundary: %s\n", ctx.Boundary)
		fmt.Printf("- proof: %s\n", ctx.Proof)
	}
	fmt.Println()
	fmt.Println("RUNNING")
	fmt.Printf("- open=%d in_progress=%d non_stale_leases=%d last_dispatch_step_run=%s\n",
		report.Running.OpenCount,
		report.Running.InProgress,
		report.Running.NonStaleLeaseCount,
		agedLabel(report.Running.LastDispatchStepRun),
	)
	if adjacent := report.Running.AdjacentProgress; adjacent != nil {
		fmt.Printf("- adjacent_progress: available=%t held=%d non_packet_actions=%d consumed_non_packet_actions=%d packet_candidates=%d next=%s\n",
			adjacent.Available,
			len(adjacent.HeldWorkIDs),
			adjacent.NonPacketActionCount,
			adjacent.ConsumedNonPacketActionCount,
			adjacent.PacketCandidateCount,
			adjacent.NextAction,
		)
	}
	if report.Running.AdmissionPreviewCount > 0 {
		fmt.Printf("- admission_previews: %d\n", report.Running.AdmissionPreviewCount)
		for _, preview := range report.Running.AdmissionPreviews {
			fmt.Printf(" - admission_preview: %s staleness=%s action_context=%s source_review_card=%s operator_required=%t admit=%s\n",
				preview.WorkID,
				emptyDash(preview.Staleness),
				orientAdmissionPreviewContextKind(preview),
				emptyDash(preview.SourceReviewCardID),
				preview.RequiresOperator,
				strings.Join(preview.AdmitCommand, " "),
			)
			if preview.AdmissionBlockedReason != "" {
				fmt.Printf("   blocked_reason: %s\n", preview.AdmissionBlockedReason)
			}
		}
	}
	if len(report.Running.OpenWork) == 0 {
		fmt.Println("- open_work: none")
	} else {
		for _, item := range report.Running.OpenWork {
			actor := reviewartifact.FirstNonEmpty(item.Actor, "-")
			claimed := "-"
			if item.ClaimedAge != "" {
				claimed = item.ClaimedAge
			}
			fmt.Printf("- %s state=%s actor=%s claimed_age=%s next=%s\n", item.WorkID, item.State, actor, claimed, item.NextAction)
		}
	}
	fmt.Println()
	fmt.Println("CHANGED")
	fmt.Printf("- claims_seq: current=%d delta=%d last_seen=%d first_run=%t\n",
		report.Changed.CurrentClaimSeq,
		report.Changed.ClaimsSeqDelta,
		report.Changed.LastSeenSeq,
		report.Changed.FirstRun,
	)
	if len(report.Changed.Recent) == 0 {
		fmt.Println("- recent: none")
	} else {
		for _, item := range report.Changed.Recent {
			fmt.Printf("- %s receipt=%s transition=%s move=%s changed=%t summary=%s\n",
				item.WorkID,
				emptyDash(item.ReceiptID),
				emptyDash(item.TransitionID),
				emptyDash(item.MoveType),
				item.Changed,
				reviewartifact.FirstNonEmpty(item.Summary, "-"),
			)
		}
	}
	fmt.Println()
	if len(report.Warnings) > 0 {
		fmt.Println("WARNINGS")
		for _, warning := range report.Warnings {
			fmt.Println("- " + warning)
		}
		fmt.Println()
	}
	fmt.Println("DRILL-DOWN")
	for _, drill := range report.DrillDown {
		fmt.Printf("- %s: %s\n", drill.Label, strings.Join(drill.Command, " "))
	}
}

func printOrientProductLoops(loops *orientProductLoops) {
	if loops == nil || loops.ProductCount == 0 {
		return
	}
	fmt.Printf("- product_loops: products=%d open_hypothesis_debt=%d closed_review_actions=%d action_cards=%d drill_down=%s\n",
		loops.ProductCount,
		loops.OpenHypothesisDebtCount,
		loops.ClosedReviewActionCount,
		loops.ActionCardCount,
		strings.Join(loops.DrillDownCommand, " "),
	)
	for _, product := range loops.Products {
		for _, group := range product.OpenHypothesesByMissingSignalSource {
			fmt.Printf("  - %s missing_signal_source=%s hypotheses=%s next=%s\n",
				product.ProductID,
				group.Source,
				strings.Join(group.HypothesisIDs, ","),
				emptyDash(strings.Join(group.NextActions, "; ")),
			)
		}
		for _, group := range product.SignalsWithoutHypothesis {
			consumed := ""
			if group.Consumed {
				consumed = " consumed=true work=" + strings.Join(group.ConsumedByWorkIDs, ",")
			}
			fmt.Printf(" - %s signals_without_hypothesis=%s signals=%s metrics=%s%s\n",
				product.ProductID,
				group.ChangeEvent,
				strings.Join(group.SignalIDs, ","),
				emptyDash(strings.Join(group.Metrics, ",")),
				consumed,
			)
		}
		for _, group := range product.ClosedReviewsByNextAction {
			consumed := ""
			if group.Consumed {
				consumed = " consumed=true work=" + strings.Join(group.ConsumedByWorkIDs, ",")
			}
			fmt.Printf("  - %s closed_review_next=%s reviews=%s%s\n",
				product.ProductID,
				group.NextAction,
				strings.Join(group.ReviewIDs, ","),
				consumed,
			)
		}
		for _, card := range product.ActionCards {
			fmt.Printf("  - %s action_card=%s kind=%s command=%s\n",
				product.ProductID,
				card.ID,
				card.Kind,
				strings.Join(card.Command, " "),
			)
		}
	}
}

func agedLabel(item *orientAgedItem) string {
	if item == nil {
		return "none"
	}
	parts := []string{item.Age}
	if item.Kind != "" {
		parts = append(parts, item.Kind)
	}
	if item.ID != "" {
		parts = append(parts, item.ID)
	}
	return strings.Join(parts, " ")
}

func formatIntMap(values map[string]int) string {
	if len(values) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(values))
	for _, key := range sortedMapKeys(values) {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ",")
}
