package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/attention"
	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/reviewqueue"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const workbenchCapabilityOfferLimit = 12
const workbenchOperatorStageLimit = 2
const workbenchOperatorOpportunityMaxRunes = 72
const workbenchOperatorMissionMaxRunes = 72
const workbenchOperatorDecisionMaxRunes = 80
const workbenchOperatorProofMaxRunes = 72
const workbenchOperatorSummaryOpportunityMaxRunes = 44
const workbenchOperatorSummaryChoiceMaxRunes = 32

var workbenchNow = time.Now

const workbenchUsage = "usage: compound workbench [--json] [--operator] [--advanced|--operator-advanced] [--operator-limit N] [--recent N] [--actor A] [--active-packet-surface S] [--source-packet packet.md] [--packet-dir D] [--capability-task T] [--capability-harness H] [--capability-limit N] [--route-class C] [--route-privacy-sensitive] [--review-area A] [--include-archived]\n  (branch_debt surfaces the open/covered/stale/dangling/unverifiable transition fold from `internal/work.BranchCoverage`; counts are advisory visibility pressure, never auto-closed; the text render caps at 3 entries when present, the precise counts and full lists are in `compound workbench --json` under `.branch_debt`)"

var (
	operatorAbsolutePathPattern = regexp.MustCompile(`(^|[[:space:]("'=\[])/(?:[^[:space:]|,;)]*)`)
	operatorActionIDPattern     = regexp.MustCompile(`\bact_[A-Za-z0-9][A-Za-z0-9_:-]*\b`)
	operatorPolicyIDPattern     = regexp.MustCompile(`\bpol_[A-Za-z0-9][A-Za-z0-9_:-]*\b`)
)

type workbenchCLIOutput struct {
	work.WorkbenchReport
	CapabilitySurface *capgraph.SurfaceReport            `json:"capability_surface,omitempty"`
	RoutePlan         *routeevidence.CapabilityRoutePlan `json:"route_plan,omitempty"`
	ReviewQueue       *workbenchReviewQueue              `json:"review_queue,omitempty"`
}

// workbenchReviewQueue is the workbench's operator-load headline. It is the
// ONE meter (packet W1e): Counts.ReviewRequired is not computed independently
// from review-artifact status anymore — it is overwritten with the attention
// projection's needs_you count (internal/attention) so this surface never
// disagrees with `compound attention`. The JSON field name stays stable for
// existing consumers (e.g. an absorbed operating system's session-start
// digest hook reads workbench.review_queue.counts.review_required); only
// its source changed.
// BlockedOnYou is additive detail from the same projection, never a
// different total. If the attention projection's inputs cannot be read,
// AttentionUnavailable is set and Counts.ReviewRequired keeps the last-known
// review-artifact-derived value instead of silently recomputing anything.
type workbenchReviewQueue struct {
	SchemaVersion        string                     `json:"schema_version"`
	AuthorityRef         string                     `json:"authority_ref"`
	Area                 string                     `json:"area,omitempty"`
	Counts               reviewartifact.QueueCounts `json:"counts"`
	BlockedOnYou         int                        `json:"blocked_on_you,omitempty"`
	AttentionUnavailable bool                       `json:"attention_unavailable,omitempty"`
	Actionable           []workbenchReviewItem      `json:"actionable,omitempty"`
}

type workbenchReviewItem struct {
	Area       string                  `json:"area"`
	ID         string                  `json:"id"`
	Status     string                  `json:"status"`
	Readiness  string                  `json:"readiness,omitempty"`
	NextAction string                  `json:"next_action,omitempty"`
	Actions    []reviewartifact.Action `json:"actions,omitempty"`
}

func defaultWorkbenchPacketDir() string {
	if dir := strings.TrimSpace(os.Getenv("COMPOUND_PACKET_DIR")); dir != "" {
		return dir
	}
	for _, dir := range []string{
		filepath.Join("docs_private", "plan", "packets"),
		filepath.Join("..", "docs_private", "plan", "packets"),
	} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

func cmdWorkbench(args []string) {
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	// Read-only by registry contract: the workbench inspects the ledger and
	// must not materialize work/ or events/ just by looking (see
	// work.OpenReadOnly). Every projection it composes — Workbench,
	// ExecutorsProjection, the review queue, capability surface, route plan —
	// is a read; a missing home projects empty rather than scaffolded.
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	jsonOut := false
	operatorOut := false
	operatorAdvanced := false
	operatorLimit := workbenchOperatorStageLimit
	recent := 5
	actor := "codex"
	activePacketSurface := ""
	activePacketExpectedRef := ""
	capabilityTask := ""
	capabilityHarness := ""
	capabilityLimit := workbenchCapabilityOfferLimit
	routeClass := ""
	routePrivacySensitive := false
	reviewArea := ""
	includeArchived := false
	packetDir := defaultWorkbenchPacketDir()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--operator":
			operatorOut = true
		case "--advanced", "--operator-advanced":
			operatorAdvanced = true
		case "--operator-limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --operator-limit must be a non-negative integer")
				os.Exit(2)
			}
			operatorLimit = n
		case "--include-archived":
			includeArchived = true
		case "--active-packet-surface":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			activePacketSurface = strings.TrimSpace(args[i])
			if activePacketSurface == "" {
				fmt.Fprintln(os.Stderr, "error: --active-packet-surface must be non-empty")
				os.Exit(2)
			}
		case "--source-packet":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			proof, err := work.PacketSourceProofForPath(args[i])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			activePacketExpectedRef = proof.EvidenceRef
			if activePacketSurface == "" {
				activePacketSurface = proof.CanonicalSurface
			}
		case "--packet-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			packetDir = strings.TrimSpace(args[i])
			if packetDir == "" {
				fmt.Fprintln(os.Stderr, "error: --packet-dir must be non-empty")
				os.Exit(2)
			}
		case "--capability-task":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			capabilityTask = strings.TrimSpace(args[i])
			if capabilityTask == "" {
				fmt.Fprintln(os.Stderr, "error: --capability-task must be non-empty")
				os.Exit(2)
			}
		case "--capability-harness":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			capabilityHarness = strings.TrimSpace(args[i])
			if capabilityHarness == "" {
				fmt.Fprintln(os.Stderr, "error: --capability-harness must be non-empty")
				os.Exit(2)
			}
		case "--capability-limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --capability-limit must be a non-negative integer")
				os.Exit(2)
			}
			capabilityLimit = n
		case "--route-class":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			routeClass = strings.TrimSpace(args[i])
			if routeClass == "" {
				fmt.Fprintln(os.Stderr, "error: --route-class must be non-empty")
				os.Exit(2)
			}
		case "--route-privacy-sensitive":
			routePrivacySensitive = true
		case "--review-area":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			reviewArea = strings.TrimSpace(args[i])
			if reviewArea == "" {
				fmt.Fprintln(os.Stderr, "error: --review-area must be non-empty")
				os.Exit(2)
			}
		case "--actor":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			actor = strings.TrimSpace(args[i])
			if actor == "" {
				fmt.Fprintln(os.Stderr, "error: --actor must be non-empty")
				os.Exit(2)
			}
		case "--recent":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, workbenchUsage)
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --recent must be a non-negative integer")
				os.Exit(2)
			}
			recent = n
		default:
			fmt.Fprintln(os.Stderr, workbenchUsage)
			os.Exit(2)
		}
	}
	now := workbenchNow()
	report, err := store.Workbench(now, work.WorkbenchOptions{
		RecentLimit:                     recent,
		Actor:                           actor,
		ActivePacketSurface:             activePacketSurface,
		ActivePacketExpectedEvidenceRef: activePacketExpectedRef,
		IncludeArchived:                 includeArchived,
		PacketDir:                       packetDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var capabilitySurface *capgraph.SurfaceReport
	if capabilityTask != "" && capgraph.Exists(homePath) {
		if capabilityHarness == "" {
			capabilityHarness = actor
		}
		// OpenReadOnly, not Open: the workbench is read-classified and must not
		// MkdirAll registries/ just because --capability-task was passed. An
		// absent graph (fresh home) skips the surface with a visible warning,
		// mirroring doctor's agentLaneRows Exists guard.
		capStore, err := capgraph.OpenReadOnly(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		surface, err := buildCapabilitySurfaceReport(capStore, homePath, capabilityTask, capabilityHarness)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		surface = surface.LimitOffers(capabilityLimit)
		capabilitySurface = &surface
	} else if capabilityTask != "" {
		// The operator asked for a capability surface explicitly, but this home
		// has no graph yet. Say so instead of silently rendering nothing — and
		// never scaffold registries/ from a read-classified command.
		fmt.Fprintln(os.Stderr, "warning: no capability graph under this home; --capability-task surface skipped")
	}
	var routePlan *routeevidence.CapabilityRoutePlan
	if routeClass != "" {
		plan, err := routeevidence.PlanCapabilityRoute(homePath, routeClass, routeevidence.CapabilityRoutePlanOptions{
			PrivacySensitive: routePrivacySensitive,
		}, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		routePlan = &plan
	}
	reviewReport, err := buildReviewQueueReport(homePath, reviewArea, includeArchived)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	reviewQueue := buildWorkbenchReviewQueue(reviewReport)
	// The attention meter's executor projection reuses this displayed report only
	// when it was built non-archived (the meter has always been computed from the
	// non-archived board); an --include-archived report would fold archived work
	// into the executor counts, so that path still builds its own board.
	applyAttentionMeter(&reviewQueue, store, reviewReport, report, !includeArchived, now)
	output := workbenchCLIOutput{WorkbenchReport: report, CapabilitySurface: capabilitySurface, RoutePlan: routePlan, ReviewQueue: &reviewQueue}
	if jsonOut {
		printJSON(output)
		return
	}
	if operatorOut {
		printWorkbenchOperator(output, workbenchOperatorOptions{StageLimit: operatorLimit, ShowAdvanced: operatorAdvanced})
		return
	}
	fmt.Printf("workbench open=%d needs_claim=%d needs_review=%d in_progress=%d terminal=%d\n",
		report.Counts.Open, report.Counts.NeedsClaim, report.Counts.NeedsReview, report.Counts.InProgress, report.Counts.Terminal)
	if report.Counts.RunObservations > 0 {
		fmt.Printf("run_observations=%d\n", report.Counts.RunObservations)
	}
	if report.Counts.RunArtifacts > 0 {
		fmt.Printf("run_artifacts=%d\n", report.Counts.RunArtifacts)
	}
	if report.Counts.DispatchStepRuns > 0 {
		fmt.Printf("dispatch_step_runs=%d\n", report.Counts.DispatchStepRuns)
	}
	if report.Counts.ContinueRuns > 0 {
		fmt.Printf("continue_runs=%d\n", report.Counts.ContinueRuns)
	}
	if report.Counts.AdmissionPreviews > 0 {
		fmt.Printf("admission_previews=%d\n", report.Counts.AdmissionPreviews)
		for i, preview := range report.AdmissionPreviews {
			if i >= 3 {
				break
			}
			fmt.Printf("admission_preview: %s owner=%s state=%s staleness=%s age=%s plan=%s\n", preview.WorkID, preview.WorkOwner, preview.WorkState, emptyDash(preview.Staleness), emptyDash(preview.Age), preview.PlanPath)
			if preview.NextAction != "" {
				fmt.Printf("  next_action: %s\n", preview.NextAction)
			}
			if preview.RequiresOperator {
				fmt.Printf("  operator_required: true\n")
			}
			if preview.AdmissionBlockedReason != "" {
				fmt.Printf("  blocked_reason: %s\n", preview.AdmissionBlockedReason)
			}
			if preview.ActionSummary != "" {
				fmt.Printf(" action_summary: %s\n", preview.ActionSummary)
			}
			if len(preview.AdmitCommand) > 0 {
				fmt.Printf("  admit: %s\n", strings.Join(preview.AdmitCommand, " "))
			}
			if len(preview.RetireDryRun) > 0 {
				fmt.Printf("  retire_dry_run: %s\n", strings.Join(preview.RetireDryRun, " "))
			}
			if len(preview.RetireCommand) > 0 {
				fmt.Printf("  retire: %s\n", strings.Join(preview.RetireCommand, " "))
			}
			if preview.ActionContext != nil && len(preview.ActionContext.Command) > 0 {
				fmt.Printf("  action_context: %s proof=%s\n", strings.Join(preview.ActionContext.Command, " "), emptyDash(preview.ActionContext.Proof))
			}
		}
	}
	if report.BranchDebt != nil && (report.BranchDebt.Counts.Open > 0 || report.BranchDebt.Counts.DanglingCovers > 0 || report.BranchDebt.Counts.UnverifiableChanged > 0) {
		bd := report.BranchDebt
		fmt.Printf("branch_debt open=%d stale=%d covered=%d expansions=%d unverifiable=%d dangling=%d window=%dd\n",
			bd.Counts.Open, bd.Counts.Stale, bd.Counts.Covered, bd.Counts.Expansions, bd.Counts.UnverifiableChanged, bd.Counts.DanglingCovers, bd.WindowDays)
		for i, entry := range bd.Debt {
			if i >= work.BranchDebtTextRenderCap() {
				break
			}
			marker := ""
			if entry.Stale {
				marker = " STALE"
			}
			fmt.Printf("  branch: %s owner=%s age=%dd%s opened_by=%s\n", entry.Branch, entry.Owner, entry.AgeDays, marker, entry.OpenedBy)
		}
	}
	if capabilitySurface != nil {
		fmt.Printf("capability_surface status=%s offers=%d showing=%d omitted=%d task=%q harness=%q authority=%s\n",
			capabilitySurface.Status,
			capabilitySurface.OfferCount,
			len(capabilitySurface.OfferSummaries),
			capabilitySurface.OmittedOfferCount,
			capabilitySurface.Task,
			capabilitySurface.Harness,
			capabilitySurface.AuthorityRef,
		)
		for i, offer := range capabilitySurface.OfferSummaries {
			if i >= 3 {
				break
			}
			fmt.Printf("capability_offer: %s %-28s score %.1f [%s]\n", offer.Kind, offer.ID, offer.Score, offer.Why)
		}
	}
	if routePlan != nil {
		fmt.Printf("route_plan status=%s class=%q privacy_sensitive=%t lanes=%d omitted=%d authority=%s\n",
			routePlan.Status,
			routePlan.CapabilityClass,
			routePlan.PrivacySensitive,
			len(routePlan.Chain),
			len(routePlan.Omitted),
			routePlan.AuthorityRef,
		)
		for i, lane := range routePlan.Chain {
			if i >= 3 {
				break
			}
			fmt.Printf("route_lane: %d %s tier=%s cost_class=%s role=%s harness=%s\n",
				lane.Position,
				lane.ModelID,
				lane.Tier,
				lane.CostClass,
				lane.GateRole,
				lane.Harness,
			)
		}
	}
	fmt.Printf("review_queue total=%d review_required=%d ready_to_apply=%d ok=%d authority=%s\n",
		reviewQueue.Counts.Total,
		reviewQueue.Counts.ReviewRequired,
		reviewQueue.Counts.ReadyToApply,
		reviewQueue.Counts.OK,
		reviewQueue.AuthorityRef,
	)
	for i, item := range reviewQueue.Actionable {
		if i >= 3 {
			break
		}
		fmt.Printf("review_action: %s %-28s %s %s\n", item.Area, item.ID, item.Readiness, item.NextAction)
	}
	if report.ActivePacketProof != nil {
		fmt.Printf("active_packet_proof status=%s work=%s context=%s receipt=%s transition=%s\n",
			report.ActivePacketProof.Status,
			report.ActivePacketProof.WorkID,
			report.ActivePacketProof.ContextPackID,
			report.ActivePacketProof.ReceiptID,
			report.ActivePacketProof.TransitionID)
	}
	if report.DispatchNext.CanRun {
		fmt.Printf("dispatch_next: %s\n", strings.Join(report.DispatchNext.Command, " "))
	} else if report.DispatchNext.WaitReason != "" {
		fmt.Printf("dispatch_wait: %s", report.DispatchNext.WaitReason)
		if report.DispatchNext.WorkID != "" {
			fmt.Printf(" work=%s", report.DispatchNext.WorkID)
		}
		fmt.Println()
		if adjacent := report.DispatchNext.AdjacentProgress; adjacent != nil {
			fmt.Printf("adjacent_progress: available=%t packet_candidates=%d consumed_non_packet_actions=%d reason=%s\n", adjacent.Available, adjacent.PacketCandidateCount, len(adjacent.ConsumedNonPacketActions), adjacent.Reason)
			if adjacent.NextAction != "" {
				fmt.Printf("adjacent_next: %s\n", adjacent.NextAction)
			}
			if len(adjacent.Command) > 0 {
				fmt.Printf("adjacent_command: %s\n", strings.Join(adjacent.Command, " "))
			}
			for i, action := range adjacent.ConsumedNonPacketActions {
				if i >= 3 {
					break
				}
				fmt.Printf("consumed_adjacent_action: %s kind=%s evidence=%s\n", action.ID, action.Kind, strings.Join(action.ConsumedEvidenceRefs, ","))
			}
			for i, action := range adjacent.NonPacketActions {
				if i >= 3 {
					break
				}
				fmt.Printf("adjacent_non_packet_action: %s kind=%s operator=%t proof=%s\n", action.ID, action.Kind, action.RequiresOperator, action.Proof)
				if action.NextAction != "" {
					fmt.Printf(" action_next: %s\n", action.NextAction)
				}
				if action.ReviewCard != nil && action.ReviewCard.ConditionStatus != "" {
					fmt.Printf(" action_condition_status: %s evidence=%s\n", action.ReviewCard.ConditionStatus, action.ReviewCard.TriggeringEvidenceRef)
				}
				if len(action.Command) > 0 {
					fmt.Printf(" action_command: %s\n", strings.Join(action.Command, " "))
				}
				if len(action.RequiredInputs) > 0 {
					fmt.Printf(" action_required_inputs: %s\n", strings.Join(action.RequiredInputs, ", "))
				}
				if len(action.ExpectedValidation) > 0 {
					fmt.Printf(" action_expected_validation: %s\n", strings.Join(action.ExpectedValidation, ", "))
				}
				if len(action.StopConditions) > 0 {
					fmt.Printf(" action_stop_conditions: %s\n", strings.Join(action.StopConditions, ", "))
				}
			}
		}
	}
	if len(report.Integrity) > 0 {
		fmt.Println("integrity:")
		for _, msg := range report.Integrity {
			fmt.Println("- " + msg)
		}
	}
	if len(report.Open) == 0 {
		fmt.Println("open: none")
	} else {
		fmt.Println("open:")
		for _, item := range report.Open {
			fmt.Printf("- %s %-10s %-14s %s\n", item.WorkID, item.State, item.OpenReason, item.SuccessPredicate)
			fmt.Printf("  next: %s\n", item.NextAction)
			if item.ActionContext != nil && len(item.ActionContext.Command) > 0 {
				fmt.Printf("  action_context: %s proof=%s\n", strings.Join(item.ActionContext.Command, " "), emptyDash(item.ActionContext.Proof))
			}
			if item.ClaimedBy != "" {
				fmt.Printf("  claimed_by: %s\n", item.ClaimedBy)
			}
			if len(item.RunObservations) > 0 {
				latest := item.RunObservations[0]
				fmt.Printf("  observed_run: %s trace=%s policy=%s\n", latest.ID, latest.TraceID, latest.PolicyDecisionID)
			}
			if len(item.RunArtifacts) > 0 {
				artifact := item.RunArtifacts[0]
				count := "-"
				if artifact.Count != nil {
					count = strconv.Itoa(*artifact.Count)
				}
				target := "-"
				if len(artifact.TargetRefs) > 0 {
					target = artifact.TargetRefs[0]
				} else if len(artifact.CandidateIDs) > 0 {
					target = artifact.CandidateIDs[0]
				}
				fmt.Printf("  run_artifact: %s kind=%s status=%s target=%s count=%s\n", artifact.Ref, artifact.Kind, emptyDash(artifact.Status), target, count)
			}
		}
	}
	if len(report.Recent) > 0 {
		fmt.Println("recent:")
		for _, item := range report.Recent {
			fmt.Printf("- %s %-10s receipt=%s transition=%s move=%s changed=%v\n",
				item.WorkID, item.State, emptyDash(item.ReceiptID), emptyDash(item.TransitionID), emptyDash(item.MoveType), item.Changed)
		}
	}
	if len(report.DispatchStepRuns) > 0 {
		fmt.Println("dispatch_step_runs:")
		for _, run := range report.DispatchStepRuns {
			fmt.Printf("- %s status=%s steps=%d wait=%s next=%s\n",
				run.ID, run.Status, run.StepCount, emptyDash(run.WaitReason), emptyDash(run.NextAction))
		}
	}
	if len(report.ContinueRuns) > 0 {
		fmt.Println("continue_runs:")
		for _, run := range report.ContinueRuns {
			fmt.Printf("- %s status=%s steps=%d wait=%s next=%s\n",
				run.ID, run.Status, run.StepCount, emptyDash(run.WaitReason), emptyDash(run.NextAction))
		}
	}
}

type workbenchOperatorOptions struct {
	StageLimit   int
	ShowAdvanced bool
}

type operatorWorkbenchRow struct {
	Item        work.WorkbenchItem
	Status      string
	Authority   string
	Opportunity string
	Mission     string
	Assignee    string
	Proof       string
	Decision    string
	Learning    string
}

type operatorWorkbenchGroup struct {
	Representative operatorWorkbenchRow
	Count          int
}

func printWorkbenchOperator(output workbenchCLIOutput, opts workbenchOperatorOptions) {
	if opts.StageLimit <= 0 {
		opts.StageLimit = workbenchOperatorStageLimit
	}
	rows := operatorWorkbenchRows(output.WorkbenchReport)
	groups := operatorWorkbenchGroups(rows)
	fmt.Println("Operator workbench")
	fmt.Println("Flow: Needs you → Working → Ready")
	fmt.Println("Narrative: highest-priority mission first; raw proof fields require --operator-advanced")
	fmt.Println("Summary: " + operatorFlowSummary(output, rows))
	fmt.Println()
	if len(rows) == 0 {
		fmt.Println("Needs you (0)")
		fmt.Println("- none")
		fmt.Println("Working (0)")
		fmt.Println("- none")
		fmt.Println("Ready (0)")
		fmt.Println("- none")
		if opts.ShowAdvanced {
			printWorkbenchOperatorAdvanced(output, rows)
		}
		return
	}

	printOperatorStage("Needs you", groups["Needs you"], opts.StageLimit)
	printOperatorStage("Working", groups["Working"], opts.StageLimit)
	printOperatorStage("Ready", groups["Ready"], opts.StageLimit)
	printOperatorStage("Recent proof", groups["Recent proof"], opts.StageLimit)
	if other := groups["Other"]; len(other) > 0 {
		printOperatorStage("Other", other, opts.StageLimit)
	}
	fmt.Printf("Counts: open=%d needs_you=%d working=%d ready=%d recent=%d\n",
		output.Counts.Open,
		operatorStageRowCount(groups["Needs you"]),
		operatorStageRowCount(groups["Working"]),
		operatorStageRowCount(groups["Ready"]),
		len(output.Recent),
	)
	if opts.ShowAdvanced {
		printWorkbenchOperatorAdvanced(output, rows)
	}
}

func printOperatorStage(title string, groups []operatorWorkbenchGroup, limit int) {
	total := operatorStageRowCount(groups)
	fmt.Printf("%s (%d)\n", title, total)
	if total == 0 {
		fmt.Println("- none")
		return
	}
	shownRows := 0
	for i, group := range groups {
		if limit > 0 && i >= limit {
			break
		}
		shownRows += group.Count
		printOperatorGroup(group)
	}
	if omitted := total - shownRows; omitted > 0 {
		fmt.Printf("- %s omitted\n", operatorCount(omitted, "item", "items"))
	}
}

func printOperatorGroup(group operatorWorkbenchGroup) {
	row := group.Representative
	similar := ""
	if group.Count > 1 {
		similar = fmt.Sprintf(" (+%d similar)", group.Count-1)
	}
	fmt.Printf("- %s%s | Mission: %s | Authority: %s\n", row.Opportunity, similar, row.Mission, row.Authority)
	fmt.Printf("  work: %s; status=%s; assigned=%s; next=%s\n", row.Item.WorkID, row.Status, row.Assignee, operatorDecisionText(row.Decision))
	if row.Proof != "unknown/not yet proven" || row.Learning != "unknown/not yet proven" || row.Item.ReceiptID != "" || row.Item.TransitionID != "" {
		fmt.Printf("  proof: %s; receipt=%s; transition=%s; learning=%s\n",
			operatorValue(row.Proof),
			operatorValue(row.Item.ReceiptID),
			operatorValue(row.Item.TransitionID),
			operatorDefaultText(operatorValue(row.Learning), workbenchOperatorProofMaxRunes),
		)
	}
}

func printWorkbenchOperatorAdvanced(output workbenchCLIOutput, rows []operatorWorkbenchRow) {
	fmt.Println("Advanced")
	fmt.Printf("- generated_at=%s counts_open=%d counts_terminal=%d dispatch_wait=%s\n",
		operatorValue(output.GeneratedAt),
		output.Counts.Open,
		output.Counts.Terminal,
		operatorValue(output.DispatchNext.WaitReason),
	)
	for _, row := range rows {
		fmt.Printf("- work_id=%s state=%s status=%s authority=%s owner=%s claimed_by=%s policy=%s receipt=%s transition=%s move=%s changed=%t evidence=%s artifacts=%s verifications=%s\n",
			row.Item.WorkID,
			operatorValue(row.Item.State),
			row.Status,
			row.Authority,
			operatorValue(row.Item.Owner),
			operatorValue(row.Item.ClaimedBy),
			operatorValue(row.Item.PolicyDecisionID),
			operatorValue(row.Item.ReceiptID),
			operatorValue(row.Item.TransitionID),
			operatorValue(row.Item.MoveType),
			row.Item.Changed,
			operatorList(row.Item.EvidenceRefs),
			operatorList(row.Item.ArtifactIDs),
			operatorList(row.Item.VerificationIDs),
		)
	}
}

func operatorWorkbenchRows(report work.WorkbenchReport) []operatorWorkbenchRow {
	items := append([]work.WorkbenchItem{}, report.Open...)
	items = append(items, report.Recent...)
	rows := make([]operatorWorkbenchRow, 0, len(items))
	for _, item := range items {
		status := operatorStatusForItem(item)
		authority := operatorAuthorityForItem(item)
		rows = append(rows, operatorWorkbenchRow{
			Item:        item,
			Status:      status,
			Authority:   authority,
			Opportunity: operatorOpportunityForItem(item),
			Mission:     operatorMissionForItem(item),
			Assignee:    operatorAssigneeForItem(item),
			Proof:       operatorProofForItem(item),
			Decision:    operatorDecisionForItem(item),
			Learning:    operatorLearningForItem(item),
		})
	}
	return rows
}

func operatorWorkbenchGroups(rows []operatorWorkbenchRow) map[string][]operatorWorkbenchGroup {
	groups := map[string][]operatorWorkbenchGroup{
		"Needs you":    {},
		"Working":      {},
		"Ready":        {},
		"Recent proof": {},
		"Other":        {},
	}
	indexes := map[string]map[string]int{}
	for _, row := range rows {
		stage := operatorStageForRow(row)
		if indexes[stage] == nil {
			indexes[stage] = map[string]int{}
		}
		key := operatorGroupKey(row)
		if idx, ok := indexes[stage][key]; ok {
			groups[stage][idx].Count++
			continue
		}
		indexes[stage][key] = len(groups[stage])
		groups[stage] = append(groups[stage], operatorWorkbenchGroup{Representative: row, Count: 1})
	}
	sortOperatorWorkbenchGroups(groups)
	return groups
}

func sortOperatorWorkbenchGroups(groups map[string][]operatorWorkbenchGroup) {
	for stage := range groups {
		sort.SliceStable(groups[stage], func(i, j int) bool {
			pi := operatorGroupPriority(groups[stage][i])
			pj := operatorGroupPriority(groups[stage][j])
			return pi < pj
		})
	}
}

func operatorGroupPriority(group operatorWorkbenchGroup) int {
	return operatorRowPriority(group.Representative)
}

func operatorRowPriority(row operatorWorkbenchRow) int {
	if operatorIsSourceControlToolWriteHold(row.Item) {
		return 30
	}
	if operatorIsMeaningfulDecisionRow(row) {
		return 0
	}
	return 10
}

func operatorStageForRow(row operatorWorkbenchRow) string {
	switch row.Status {
	case "Needs you", "Blocked":
		return "Needs you"
	case "Working", "Proving":
		return "Working"
	case "Ready":
		return "Ready"
	case "Integrated", "Verified":
		return "Recent proof"
	default:
		return "Other"
	}
}

func operatorGroupKey(row operatorWorkbenchRow) string {
	if operatorIsSourceControlToolWriteHold(row.Item) {
		return operatorStageForRow(row) + "|held-source-control-tool-write-gates"
	}
	return strings.Join([]string{
		operatorStageForRow(row),
		operatorNormalizeGroupText(row.Status),
		operatorNormalizeGroupText(row.Authority),
		operatorNormalizeGroupText(row.Opportunity),
		operatorNormalizeGroupText(row.Mission),
		operatorNormalizeGroupText(operatorDecisionText(row.Decision)),
	}, "|")
}

func operatorNormalizeGroupText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(operatorWords(value)), " "))
}

func operatorStageRowCount(groups []operatorWorkbenchGroup) int {
	total := 0
	for _, group := range groups {
		total += group.Count
	}
	return total
}

func operatorTopPriorityRow(rows []operatorWorkbenchRow) *operatorWorkbenchRow {
	if len(rows) == 0 {
		return nil
	}
	best := 0
	for i := 1; i < len(rows); i++ {
		if operatorRowLess(rows[i], rows[best]) {
			best = i
		}
	}
	return &rows[best]
}

func operatorRowLess(a, b operatorWorkbenchRow) bool {
	as := operatorStagePriority(a)
	bs := operatorStagePriority(b)
	if as != bs {
		return as < bs
	}
	ap := operatorRowPriority(a)
	bp := operatorRowPriority(b)
	return ap < bp
}

func operatorStagePriority(row operatorWorkbenchRow) int {
	switch operatorStageForRow(row) {
	case "Needs you":
		return 0
	case "Working":
		return 1
	case "Ready":
		return 2
	case "Recent proof":
		return 3
	default:
		return 4
	}
}

// operatorStatusForItem is a deterministic translation of existing
// WorkEnvelope/proof/review state. It does not add a second lifecycle.
func operatorStatusForItem(item work.WorkbenchItem) string {
	switch strings.TrimSpace(item.State) {
	case "proposed", "admitted", "ready":
		return "Ready"
	case "claimed", "running":
		return "Working"
	case "verifying":
		return "Proving"
	case "held":
		return "Needs you"
	case "blocked":
		return "Blocked"
	case "completed":
		if item.ReceiptID == "" || item.TransitionID == "" {
			return "Blocked"
		}
		if item.Changed {
			return "Integrated"
		}
		return "Verified"
	case "cancelled":
		return "Parked"
	case "failed":
		return "Discarded"
	default:
		return "Parked"
	}
}

func operatorAuthorityForItem(item work.WorkbenchItem) string {
	if operatorItemNeedsOperator(item) {
		return "Needs operator"
	}
	if strings.TrimSpace(item.State) == "verifying" ||
		strings.TrimSpace(item.ReviewTrigger) != "" ||
		(item.ActionContext != nil && len(item.ActionContext.ExpectedValidation) > 0) {
		return "Review before landing"
	}
	return "Free to act"
}

func operatorItemNeedsOperator(item work.WorkbenchItem) bool {
	if item.State == "held" || item.State == "blocked" {
		return true
	}
	if item.ActionContext != nil && item.ActionContext.RequiresOperator {
		return true
	}
	for _, card := range item.ReviewCards {
		if card.RequiresOperator {
			return true
		}
	}
	return false
}

func operatorOpportunityForItem(item work.WorkbenchItem) string {
	if operatorIsSourceControlToolWriteHold(item) {
		return "held source-control tool-write gates"
	}
	return operatorDefaultText(firstNonEmpty(item.SuccessPredicate, item.CanonicalSurface, item.WorkID, "unknown/not yet proven"), workbenchOperatorOpportunityMaxRunes)
}

func operatorMissionForItem(item work.WorkbenchItem) string {
	if operatorIsSourceControlToolWriteHold(item) {
		return "review current source-control write gate before clearing held work"
	}
	return operatorDefaultText(firstNonEmpty(item.DesiredOutcome, item.SuccessPredicate, "unknown/not yet proven"), workbenchOperatorMissionMaxRunes)
}

func operatorAssigneeForItem(item work.WorkbenchItem) string {
	return firstNonEmpty(item.ClaimedBy, item.Owner, "unknown")
}

func operatorProofForItem(item work.WorkbenchItem) string {
	if proof := firstNonEmpty(item.EvidenceRefs...); proof != "" {
		return operatorHumanProofRef(proof)
	}
	if proof := firstNonEmpty(item.VerificationIDs...); proof != "" {
		return operatorHumanProofRef(proof)
	}
	if proof := firstNonEmpty(item.ArtifactIDs...); proof != "" {
		return operatorHumanProofRef(proof)
	}
	for _, obs := range item.RunObservations {
		if obs.TraceID != "" {
			return operatorHumanProofRef(obs.TraceID)
		}
	}
	if item.ReceiptID != "" && item.TransitionID != "" {
		return "receipt and transition recorded"
	}
	return "unknown/not yet proven"
}

func operatorDecisionForItem(item work.WorkbenchItem) string {
	if item.NextAction != "" {
		return "next: " + item.NextAction
	}
	if item.ReceiptID != "" && item.TransitionID != "" {
		return "receipt and transition recorded"
	}
	return "unknown/not yet proven"
}

func operatorLearningForItem(item work.WorkbenchItem) string {
	if item.TransitionID != "" {
		return "transition " + item.TransitionID + "; move=" + operatorValue(item.MoveType)
	}
	if len(item.MemoryCandidateIDs) > 0 {
		return "memory candidates " + operatorList(item.MemoryCandidateIDs)
	}
	return "unknown/not yet proven"
}

func operatorFlowSummary(output workbenchCLIOutput, rows []operatorWorkbenchRow) string {
	return fmt.Sprintf("We noticed %s, chose %s, assigned %s, proved it with %s, and now need %s.",
		operatorSummaryOpportunity(output, rows),
		operatorSummaryChoice(output, rows),
		operatorSummaryAssignee(output, rows),
		operatorSummaryProof(rows),
		operatorSummaryNeed(output, rows),
	)
}

func operatorSummaryOpportunity(output workbenchCLIOutput, rows []operatorWorkbenchRow) string {
	if top := operatorTopPriorityRow(rows); top != nil {
		return "top priority " + top.Status + ": " + operatorTruncateRunes(top.Opportunity, workbenchOperatorSummaryOpportunityMaxRunes)
	}
	if output.Counts.Open > 0 {
		return operatorCount(output.Counts.Open, "open work item", "open work items")
	}
	if output.Counts.Terminal > 0 {
		return operatorCount(output.Counts.Terminal, "terminal work item", "terminal work items")
	}
	return "no active work"
}

func operatorSummaryChoice(output workbenchCLIOutput, rows []operatorWorkbenchRow) string {
	if top := operatorTopPriorityRow(rows); top != nil && top.Decision != "unknown/not yet proven" {
		return operatorTruncateRunes(operatorDecisionText(top.Decision), workbenchOperatorSummaryChoiceMaxRunes)
	}
	if output.DispatchNext.CanRun {
		return "the next governed dispatch step"
	}
	if output.DispatchNext.WaitReason != "" {
		return "to wait on " + operatorDefaultText(operatorWords(output.DispatchNext.WaitReason), workbenchOperatorDecisionMaxRunes)
	}
	return "to inspect current work"
}

func operatorSummaryAssignee(output workbenchCLIOutput, rows []operatorWorkbenchRow) string {
	if output.DispatchNext.Actor != "" {
		return output.DispatchNext.Actor
	}
	for _, row := range rows {
		if row.Assignee != "" && row.Assignee != "unknown" {
			return row.Assignee
		}
	}
	return "unknown"
}

func operatorSummaryProof(rows []operatorWorkbenchRow) string {
	if top := operatorTopPriorityRow(rows); top != nil && top.Proof != "unknown/not yet proven" {
		return top.Proof
	}
	for _, row := range rows {
		if (row.Status == "Integrated" || row.Status == "Verified") && row.Proof != "unknown/not yet proven" {
			return row.Proof
		}
	}
	for _, row := range rows {
		if row.Proof != "unknown/not yet proven" {
			return row.Proof
		}
	}
	return "unknown/not yet proven"
}

func operatorSummaryNeed(output workbenchCLIOutput, rows []operatorWorkbenchRow) string {
	if top := operatorTopPriorityRow(rows); top != nil {
		switch operatorStageForRow(*top) {
		case "Needs you":
			return "review the top Needs you mission"
		case "Working":
			return "move the top Working mission toward proof"
		case "Ready":
			return "claim or dispatch the top Ready mission"
		case "Recent proof":
			return "mine the latest proof only if it changes future work"
		}
	}
	if output.ReviewQueue != nil && output.ReviewQueue.Counts.ReviewRequired > 0 {
		return "review for " + operatorCount(output.ReviewQueue.Counts.ReviewRequired, "item", "items")
	}
	if output.Counts.NeedsReview > 0 {
		return "review for " + operatorCount(output.Counts.NeedsReview, "work item", "work items")
	}
	if output.Counts.NeedsClaim > 0 {
		return "a claim or dispatch for " + operatorCount(output.Counts.NeedsClaim, "work item", "work items")
	}
	if output.Counts.InProgress > 0 {
		return "proof for " + operatorCount(output.Counts.InProgress, "work item", "work items")
	}
	if output.Counts.MissingReceipt > 0 || output.Counts.MissingTransition > 0 {
		return "receipt/transition proof"
	}
	if output.DispatchNext.WaitReason != "" {
		return operatorDefaultText(operatorWords(output.DispatchNext.WaitReason), workbenchOperatorDecisionMaxRunes)
	}
	return "nothing; visible work is integrated"
}

func operatorDecisionText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "next: ")
	if value == "" {
		return "unknown/not yet proven"
	}
	return operatorDefaultText(value, workbenchOperatorDecisionMaxRunes)
}

func operatorHumanProofRef(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if operatorActionIDPattern.MatchString(value) || operatorPolicyIDPattern.MatchString(value) {
		return "governed action/policy proof recorded"
	}
	if operatorAbsolutePathPattern.MatchString(value) {
		return "local proof file recorded"
	}
	return operatorDefaultText(value, workbenchOperatorProofMaxRunes)
}

func operatorDefaultText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = operatorRedactDefaultText(value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "unknown"
	}
	return operatorTruncateRunes(value, maxRunes)
}

func operatorRedactDefaultText(value string) string {
	value = operatorAbsolutePathPattern.ReplaceAllString(value, "${1}local path")
	value = operatorActionIDPattern.ReplaceAllString(value, "governed action id")
	value = operatorPolicyIDPattern.ReplaceAllString(value, "policy decision id")
	return value
}

func operatorTruncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

func operatorIsMeaningfulDecisionRow(row operatorWorkbenchRow) bool {
	text := operatorItemSearchText(row.Item)
	for _, needle := range []string{"product", "review", "operator decision", "adjudicat", "approval", "pricing", "release"} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func operatorIsSourceControlToolWriteHold(item work.WorkbenchItem) bool {
	if item.State != "held" && item.State != "blocked" {
		return false
	}
	text := operatorItemSearchText(item)
	hasSourceControl := strings.Contains(text, "source-control") || strings.Contains(text, "source control")
	hasToolWriteGate := strings.Contains(text, "tool-write") ||
		strings.Contains(text, "tool write") ||
		strings.Contains(text, "policy-gate") ||
		strings.Contains(text, "policy gate")
	return hasSourceControl && hasToolWriteGate
}

func operatorItemSearchText(item work.WorkbenchItem) string {
	parts := []string{
		item.State,
		item.SuccessPredicate,
		item.DesiredOutcome,
		item.CanonicalSurface,
		item.NextAction,
		strings.Join(item.EvidenceRefs, " "),
	}
	for _, card := range item.ReviewCards {
		parts = append(parts, card.Kind, card.Title, card.NextAction, card.GateID, card.SourceKind)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func operatorCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

func operatorValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func operatorWords(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = strings.ReplaceAll(value, "_", " ")
	value = strings.ReplaceAll(value, "-", " ")
	return value
}

func operatorList(values []string) string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 {
		return "unknown"
	}
	return strings.Join(cleaned, ",")
}

// buildReviewQueueReport is the cmd/compound-side wrapper that gives callers
// the same "fully annotated" report the previous shape produced, but
// splits the assembly between internal/reviewqueue (the canonical
// 24-Annotate assembly) and synthesis (the synthesis-specific
// operator-card pass). All 8 callers (workbench, narrate, attention,
// reviews-resolve, experiential-smoke, the two main.go workbench
// commands, and the orient report) keep this signature unchanged.
func buildReviewQueueReport(homePath, area string, includeArchived bool) (reviewartifact.QueueReport, error) {
	report, err := reviewqueue.Build(homePath, reviewqueue.Options{
		Area:            area,
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return reviewartifact.QueueReport{}, err
	}
	// Synthesis annotator: lives in synthesis.go (a cmd/compound
	// concern, not a queue-assembly concern). The canonical builder
	// does not run it; this wrapper preserves the legacy
	// "synthesis for free" behavior by chaining it on top of Build.
	annotateSynthesisReviewQueue(&report)
	// Re-run the count rollup after the synthesis annotator. The
	// prior shape called Recount once, after both the 24-Annotate
	// pass and the synthesis annotator; the canonical builder
	// (internal/reviewqueue) only sees the 24-Annotate shape, so the
	// wrapper re-rolls counts here to capture the synthesis-driven
	// status changes.
	reviewartifact.Recount(&report)
	if err := reviewqueue.StampReviewGroupActionAuthority(homePath, &report, time.Now()); err != nil {
		return reviewartifact.QueueReport{}, err
	}
	reviewartifact.RefreshSystemicBlocker(&report)
	return report, nil
}

// noticeReviewQueue adapts buildReviewQueueReport to operatornotice's injected
// builder. The notifier cannot assemble this itself without importing six
// organs into a leaf package; this is the whole reason the seam exists.
func noticeReviewQueue(homePath string, _ time.Time) (*reviewartifact.QueueReport, error) {
	report, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		return nil, err
	}
	return &report, nil
}

func buildWorkbenchReviewQueue(report reviewartifact.QueueReport) workbenchReviewQueue {
	out := workbenchReviewQueue{
		SchemaVersion: reviewartifact.QueueSchemaVersion,
		AuthorityRef:  "compound://reviews",
		Area:          report.Area,
		Counts:        report.Counts,
	}
	for _, item := range report.Items {
		if len(out.Actionable) >= 5 {
			break
		}
		if len(item.Actions) == 0 && !workbenchReviewReadinessActionable(item.Readiness) {
			continue
		}
		out.Actionable = append(out.Actionable, workbenchReviewItem{
			Area:       item.Area,
			ID:         item.ID,
			Status:     item.Status,
			Readiness:  item.Readiness,
			NextAction: item.NextAction,
			Actions:    item.Actions,
		})
	}
	return out
}

// applyAttentionMeter is the ONE meter seam (packet W1e, work_246942fca52c):
// it overwrites reviewQueue's independently-computed review_required with
// the attention projection's needs_you count, and adds blocked_on_you as
// additive detail, so workbench never reports a different "what needs the
// operator" total than `compound attention`. attention.Build itself never
// errors; the only failure mode is an unreadable executor source feeding its
// Inputs (e.g. a corrupt hook-events or route-evidence file), surfaced here
// as a *work.Store.ExecutorsProjection error. On that path this function
// sets AttentionUnavailable and returns without touching reviewQueue.Counts
// again — the review-artifact-derived value already in Counts.ReviewRequired
// stands as the last-known count. It never falls back to recomputing an
// independent total silently; the flag makes the degradation visible.
func applyAttentionMeter(reviewQueue *workbenchReviewQueue, store *work.Store, reviews reviewartifact.QueueReport, bench work.WorkbenchReport, reuseBenchExecutors bool, now time.Time) {
	// Reuse the already-built displayed board for the executor projection instead
	// of walking the whole event store a second time (the executor projection's
	// dominant cost). The meter reads only report.Counts.NeedsYou/BlockedOnYou,
	// which attention derives from executor lease-hygiene and Open-derived burden
	// -- never the Recent-derived LastSuccess/LastFailure -- so a RecentLimit<8
	// board here does not move the meter. Reuse is gated on a non-archived board
	// so the meter's counts stay computed from the same non-archived work set as
	// before; the archived path keeps building its own board.
	var opts []work.ExecutorsProjectionOptions
	if reuseBenchExecutors {
		opts = append(opts, work.ExecutorsProjectionOptions{Workbench: &bench})
	}
	executors, err := store.ExecutorsProjection(now, opts...)
	if err != nil {
		reviewQueue.AttentionUnavailable = true
		return
	}
	report := attention.Build(attention.Inputs{
		Reviews:   &reviews,
		Workbench: &bench,
		Executors: &executors,
	}, now)
	reviewQueue.Counts.ReviewRequired = report.Counts.NeedsYou
	reviewQueue.BlockedOnYou = report.Counts.BlockedOnYou
}

func workbenchReviewReadinessActionable(readiness string) bool {
	switch readiness {
	case "needs_review", "ready_to_record", "ready_to_apply", "blocked":
		return true
	default:
		return false
	}
}
