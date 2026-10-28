package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

const (
	eventStoreRepairCapability             = "eventstore.post_completion_repair.apply"
	eventStoreRepairSupersessionCapability = "eventstore.post_completion_repair.supersede"
)

var eventStoreRepairObservationInterval = 250 * time.Millisecond
var eventStoreRepairSupersessionAfterStage = func(string) error { return nil }

const eventStoreRepairUsage = "usage:\n" +
	"  compound event-store repair status [--home P] [--plan PLAN_ID] [--json]\n" +
	"  compound event-store repair plan [--home P] [--json]\n" +
	"  compound event-store repair review PLAN.json --reviewer operator:ID --decision authorize --attest-writer-stopped [--home P] [--json]\n" +
	"  compound event-store repair supersede PLAN.json --successor PLAN.json --review REVIEW.json --work W --actor LEASE_HOLDER [--home P] [--json]\n" +
	"  compound event-store repair apply PLAN.json --review REVIEW.json --work W --actor LEASE_HOLDER [--home P] [--json]\n" +
	"    Plan idempotently classifies every post-completion loose record against segment authority and writes immutable source evidence.\n" +
	"    Review binds the exact plan and bounded writer-quiescence evidence to an explicit operator decision.\n" +
	"    Supersede terminalizes only a zero-progress reviewed plan in favor of one exact broader immutable plan through the same work/policy/receipt seam.\n" +
	"    Apply revalidates that review and derives readiness from the current PolicyDecision for any remaining mutation,\n" +
	"    reserves ActionIntent -> PolicyDecision on the supplied running work, archives every original byte content-addressably,\n" +
	"    imports only ids absent from segments, and records Receipt + Transition proof. Segment authority is never overwritten.\n" +
	"    Status composes the existing immutable plan and lifecycle readers without taking a lease or writing any state."

type eventStoreRepairApplyReport struct {
	SchemaVersion     string                                `json:"schema_version"`
	Status            string                                `json:"status"`
	Changed           bool                                  `json:"changed"`
	Replayed          bool                                  `json:"replayed"`
	WorkID            string                                `json:"work_id"`
	PlanID            string                                `json:"plan_id"`
	PlanDigest        string                                `json:"plan_digest"`
	ActionIntentID    string                                `json:"action_intent_id"`
	PolicyDecisionID  string                                `json:"policy_decision_id"`
	ReceiptID         string                                `json:"receipt_id"`
	TransitionID      string                                `json:"transition_id"`
	RepairProofDigest string                                `json:"repair_proof_digest"`
	PolicyAuthority   policy.AuthorityFace                  `json:"policy_authority"`
	Result            eventstore.PostCompletionRepairResult `json:"result"`
}

type eventStoreRepairSupersessionReport struct {
	SchemaVersion           string               `json:"schema_version"`
	Status                  string               `json:"status"`
	WorkID                  string               `json:"work_id"`
	PlanID                  string               `json:"plan_id"`
	PlanDigest              string               `json:"plan_digest"`
	SuccessorPlanID         string               `json:"successor_plan_id"`
	SuccessorPlanDigest     string               `json:"successor_plan_digest"`
	ActionIntentID          string               `json:"action_intent_id"`
	PolicyDecisionID        string               `json:"policy_decision_id"`
	ReceiptID               string               `json:"receipt_id"`
	TransitionID            string               `json:"transition_id"`
	SupersessionProofDigest string               `json:"supersession_proof_digest"`
	PolicyAuthority         policy.AuthorityFace `json:"policy_authority"`
}

type eventStoreRepairStatusReport struct {
	SchemaVersion string `json:"schema_version"`
	Needed        bool   `json:"needed"`

	LooseAfterCompletion int `json:"loose_after_completion"`

	PlanID     string `json:"plan_id,omitempty"`
	PlanPath   string `json:"plan_path,omitempty"`
	PlanStatus string `json:"plan_status"`

	ReviewID     string `json:"review_id,omitempty"`
	ReviewPath   string `json:"review_path,omitempty"`
	ReviewStatus string `json:"review_status"`

	ApplyStatus    string         `json:"apply_status"`
	ProgressPhases map[string]int `json:"progress_phases,omitempty"`
	ResultID       string         `json:"result_id,omitempty"`
	ProofStatus    string         `json:"proof_status"`
	ProofDigest    string         `json:"proof_digest,omitempty"`

	RootAnchorConsistent bool   `json:"root_anchor_consistent"`
	RootAnchorIssue      string `json:"root_anchor_issue,omitempty"`

	SupersessionStatus          string `json:"supersession_status"`
	SupersessionID              string `json:"supersession_id,omitempty"`
	SupersessionSuccessorPlanID string `json:"supersession_successor_plan_id,omitempty"`

	ActivePlanIDs []string `json:"active_plan_ids"`

	NextAction              string   `json:"next_action"`
	NextCommand             string   `json:"next_command"`
	NextCommandArgv         []string `json:"next_command_argv,omitempty"`
	NextCommandRequirements []string `json:"next_command_requirements,omitempty"`
}

type eventStoreRepairPlanProjection struct {
	report   eventStoreRepairStatusReport
	terminal bool
}

func runEventStoreRepair(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stdout, eventStoreRepairUsage)
		return 0
	}
	switch args[0] {
	case "status":
		values, jsonOut, err := parseEventStoreRepairFlags(args[1:], map[string]bool{"--home": true, "--plan": true})
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair status:", err)
			return 2
		}
		homePath, err := home.Resolve(values["--home"])
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair status:", err)
			return 1
		}
		report, err := readEventStoreRepairStatus(homePath, strings.TrimSpace(values["--plan"]))
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair status:", err)
			return 1
		}
		if jsonOut {
			writeEventStoreRepairJSON(stdout, report)
		} else {
			writeEventStoreRepairStatus(stdout, report)
		}
		return 0

	case "plan":
		values, jsonOut, err := parseEventStoreRepairFlags(args[1:], map[string]bool{"--home": true})
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair plan:", err)
			return 2
		}
		homePath, err := home.Resolve(values["--home"])
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair plan:", err)
			return 1
		}
		store, err := eventstore.Open(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair plan:", err)
			return 1
		}
		record, err := store.CreatePostCompletionRepairPlan(time.Time{}, eventStoreRepairObservationInterval)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair plan:", err)
			return 1
		}
		if jsonOut {
			writeEventStoreRepairJSON(stdout, record)
		} else {
			fmt.Fprintf(stdout, "event-store repair plan %s status=%s total=%d counts=%v\n", record.Plan.ID, record.Plan.Status, record.Plan.Total, record.Plan.Counts)
			fmt.Fprintf(stdout, "plan: %s\n", record.PlanPath)
		}
		return 0

	case "review":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(stderr, eventStoreRepairUsage)
			return 2
		}
		values, jsonOut, err := parseEventStoreRepairFlags(args[2:], map[string]bool{
			"--home": true, "--reviewer": true, "--decision": true, "--attest-writer-stopped": false,
		})
		if err != nil || values["--reviewer"] == "" || values["--decision"] == "" || values["--attest-writer-stopped"] != "true" {
			fmt.Fprintln(stderr, "event-store repair review: --reviewer, --decision, and --attest-writer-stopped are required")
			return 2
		}
		homePath, err := home.Resolve(values["--home"])
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair review:", err)
			return 1
		}
		store, err := eventstore.Open(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair review:", err)
			return 1
		}
		record, err := store.ReviewPostCompletionRepair(args[1], values["--reviewer"], values["--decision"], true, time.Time{}, eventStoreRepairObservationInterval)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair review:", err)
			return 1
		}
		if jsonOut {
			writeEventStoreRepairJSON(stdout, record.Review)
		} else {
			fmt.Fprintf(stdout, "event-store repair review %s decision=%s reviewer=%s\nreview: %s\n", record.Review.ID, record.Review.Decision, record.Review.Reviewer, record.ReviewPath)
		}
		return 0

	case "supersede":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(stderr, eventStoreRepairUsage)
			return 2
		}
		values, jsonOut, err := parseEventStoreRepairFlags(args[2:], map[string]bool{"--home": true, "--work": true, "--actor": true, "--review": true, "--successor": true})
		if err != nil || strings.TrimSpace(values["--work"]) == "" || strings.TrimSpace(values["--actor"]) == "" || strings.TrimSpace(values["--review"]) == "" || strings.TrimSpace(values["--successor"]) == "" {
			fmt.Fprintln(stderr, "event-store repair supersede: --successor, --review, --work, and --actor are required")
			return 2
		}
		homePath, err := home.Resolve(values["--home"])
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede:", err)
			return 1
		}
		return supersedeEventStoreRepair(homePath, args[1], strings.TrimSpace(values["--successor"]), strings.TrimSpace(values["--review"]), strings.TrimSpace(values["--work"]), strings.TrimSpace(values["--actor"]), jsonOut, stdout, stderr)

	case "apply":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			fmt.Fprintln(stderr, eventStoreRepairUsage)
			return 2
		}
		values, jsonOut, err := parseEventStoreRepairFlags(args[2:], map[string]bool{"--home": true, "--work": true, "--actor": true, "--review": true})
		if err != nil || strings.TrimSpace(values["--work"]) == "" || strings.TrimSpace(values["--actor"]) == "" || strings.TrimSpace(values["--review"]) == "" {
			fmt.Fprintln(stderr, "event-store repair apply: --review, --work, and --actor are required")
			return 2
		}
		homePath, err := home.Resolve(values["--home"])
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply:", err)
			return 1
		}
		return applyEventStoreRepair(homePath, args[1], strings.TrimSpace(values["--review"]), strings.TrimSpace(values["--work"]), strings.TrimSpace(values["--actor"]), jsonOut, stdout, stderr)
	default:
		fmt.Fprintln(stderr, eventStoreRepairUsage)
		return 2
	}
}

func readEventStoreRepairStatus(homePath, requestedPlanID string) (eventStoreRepairStatusReport, error) {
	if _, err := home.LoadManifest(homePath); err != nil {
		return eventStoreRepairStatusReport{}, fmt.Errorf("read home manifest: %w", err)
	}
	if requestedPlanID != "" && (filepath.Base(requestedPlanID) != requestedPlanID || !strings.HasPrefix(requestedPlanID, "esrp_")) {
		return eventStoreRepairStatusReport{}, fmt.Errorf("post-completion repair plan id invalid")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return eventStoreRepairStatusReport{}, err
	}
	verification, err := events.Migrate(eventstore.MigrateOptions{Mode: eventstore.ModeVerify})
	if err != nil {
		return eventStoreRepairStatusReport{}, fmt.Errorf("read migration verification: %w", err)
	}
	plans, err := events.ReadPostCompletionRepairPlans()
	if err != nil {
		return eventStoreRepairStatusReport{}, err
	}
	planByID := make(map[string]eventstore.PostCompletionRepairPlanRecord, len(plans))
	for _, record := range plans {
		planByID[record.Plan.ID] = record
	}

	projections := make(map[string]eventStoreRepairPlanProjection, len(plans))
	activePlanIDs := make([]string, 0, len(plans))
	for _, record := range plans {
		projection, err := projectEventStoreRepairPlanStatus(events, homePath, record, planByID)
		if err != nil {
			return eventStoreRepairStatusReport{}, fmt.Errorf("read plan %s status: %w", record.Plan.ID, err)
		}
		projections[record.Plan.ID] = projection
		if !projection.terminal {
			activePlanIDs = append(activePlanIDs, record.Plan.ID)
		}
	}

	looseAfterCompletion := 0
	if events.MigrationComplete() {
		looseAfterCompletion = verification.Completion.RemainingLoose
	}
	rootAnchorConsistent, rootAnchorIssue := eventStoreRepairRootAnchorStatus(verification.Completion.Reasons)
	needed := looseAfterCompletion > 0 || len(activePlanIDs) > 0
	applyCommon := func(report eventStoreRepairStatusReport) eventStoreRepairStatusReport {
		report.SchemaVersion = "compound.eventstore.post_completion_repair_status.v0.1"
		report.Needed = needed
		report.LooseAfterCompletion = looseAfterCompletion
		report.RootAnchorConsistent = rootAnchorConsistent
		report.RootAnchorIssue = rootAnchorIssue
		report.ActivePlanIDs = append([]string(nil), activePlanIDs...)
		return report
	}

	if requestedPlanID != "" {
		projection, ok := projections[requestedPlanID]
		if !ok {
			return eventStoreRepairStatusReport{}, fmt.Errorf("post-completion repair plan %s not found", requestedPlanID)
		}
		return applyCommon(projection.report), nil
	}

	if len(activePlanIDs) > 1 {
		report := newEventStoreRepairStatusReport()
		report.PlanStatus = "multiple_active"
		report.NextAction = "inspect_active_repairs"
		commands := make([]string, 0, len(activePlanIDs))
		for _, planID := range activePlanIDs {
			commands = append(commands, eventStoreRepairCommand([]string{
				"compound", "event-store", "repair", "status", "--home", homePath, "--plan", planID,
			}))
		}
		report.NextCommand = strings.Join(commands, "\n")
		return applyCommon(report), nil
	}
	if len(activePlanIDs) == 1 {
		return applyCommon(projections[activePlanIDs[0]].report), nil
	}
	if looseAfterCompletion > 0 {
		report := newEventStoreRepairStatusReport()
		setEventStoreRepairNextCommand(&report, "plan", []string{
			"compound", "event-store", "repair", "plan", "--home", homePath,
		})
		return applyCommon(report), nil
	}
	if len(plans) > 0 {
		latest := plans[len(plans)-1]
		return applyCommon(projections[latest.Plan.ID].report), nil
	}
	report := newEventStoreRepairStatusReport()
	report.NextAction = "none"
	return applyCommon(report), nil
}

func newEventStoreRepairStatusReport() eventStoreRepairStatusReport {
	return eventStoreRepairStatusReport{
		PlanStatus:         "not_created",
		ReviewStatus:       "not_recorded",
		ApplyStatus:        "not_started",
		ProofStatus:        "not_recorded",
		SupersessionStatus: "not_recorded",
		ActivePlanIDs:      []string{},
	}
}

func projectEventStoreRepairPlanStatus(events *eventstore.Store, homePath string, record eventstore.PostCompletionRepairPlanRecord, plans map[string]eventstore.PostCompletionRepairPlanRecord) (eventStoreRepairPlanProjection, error) {
	report := newEventStoreRepairStatusReport()
	report.PlanID = record.Plan.ID
	report.PlanPath = record.PlanPath
	report.PlanStatus = record.Plan.Status
	report.ReviewPath = filepath.Join(filepath.Dir(record.PlanPath), record.Plan.ID+".review.json")

	review, reviewErr := events.PostCompletionRepairReviewForFinalization(report.ReviewPath, record.Plan)
	reviewExists := reviewErr == nil
	if reviewErr != nil && !errors.Is(reviewErr, os.ErrNotExist) {
		return eventStoreRepairPlanProjection{}, reviewErr
	}
	if reviewExists {
		report.ReviewID = review.ID
		report.ReviewStatus = review.Decision
	} else {
		report.ReviewPath = ""
	}

	pending, pendingExists, err := events.ReadPostCompletionRepairPending(record.Plan.ID)
	if err != nil {
		return eventStoreRepairPlanProjection{}, err
	}
	result, resultExists, err := events.ReadPostCompletionRepairResult(record.Plan.ID)
	if err != nil {
		return eventStoreRepairPlanProjection{}, err
	}
	supersession, supersessionExists, err := events.ReadPostCompletionRepairSupersession(record.Plan.ID)
	if err != nil {
		return eventStoreRepairPlanProjection{}, err
	}
	if resultExists && supersessionExists {
		return eventStoreRepairPlanProjection{}, fmt.Errorf("conflicting result and supersession terminal owners")
	}

	if pendingExists {
		report.ProgressPhases = map[string]int{}
		for _, entry := range pending.Entries {
			report.ProgressPhases[entry.Phase]++
		}
	}
	if resultExists {
		report.ResultID = result.ID
	}

	var proofExists bool
	if resultExists {
		proof, exists, err := events.ReadPostCompletionRepairProof(record.Plan.ID)
		if err != nil {
			return eventStoreRepairPlanProjection{}, err
		}
		proofExists = exists
		if exists {
			report.ProofStatus = "recorded"
			report.ProofDigest = proof.ProofDigest
		}
	}
	var supersessionProofExists bool
	if supersessionExists {
		proof, exists, err := events.ReadPostCompletionRepairSupersessionProof(record.Plan.ID)
		if err != nil {
			return eventStoreRepairPlanProjection{}, err
		}
		supersessionProofExists = exists
		if exists {
			report.ProofStatus = "recorded"
			report.ProofDigest = proof.ProofDigest
		}
	}

	projection := eventStoreRepairPlanProjection{report: report}
	switch {
	case supersessionExists:
		projection.report.ApplyStatus = "not_applicable"
		projection.report.SupersessionStatus = "recorded"
		projection.report.SupersessionID = supersession.ID
		projection.report.SupersessionSuccessorPlanID = supersession.SuccessorPlanID
		if supersessionProofExists {
			projection.report.SupersessionStatus = "finalized"
			projection.report.NextAction = "complete"
			projection.terminal = true
			break
		}
		successor, ok := plans[supersession.SuccessorPlanID]
		if !ok {
			return eventStoreRepairPlanProjection{}, fmt.Errorf("successor plan %s not found", supersession.SuccessorPlanID)
		}
		setEventStoreRepairNextCommand(&projection.report, "finalize_supersession", []string{
			"compound", "event-store", "repair", "supersede", record.PlanPath,
			"--successor", successor.PlanPath,
			"--review", filepath.Join(filepath.Dir(record.PlanPath), record.Plan.ID+".review.json"),
			"--work", supersession.Authorization.WorkID,
			"--actor", supersession.Authorization.Actor,
			"--home", homePath,
		})
	case resultExists:
		switch {
		case proofExists && !pendingExists:
			projection.report.ApplyStatus = "finalized"
			projection.report.NextAction = "complete"
			projection.terminal = true
			return projection, nil
		case proofExists:
			projection.report.ApplyStatus = "finalization_pending"
		default:
			projection.report.ApplyStatus = "result_recorded"
		}
		setEventStoreRepairApplyCommand(&projection.report, homePath, record.PlanPath, filepath.Join(filepath.Dir(record.PlanPath), record.Plan.ID+".review.json"), result.Authorization, nil)
	case pendingExists:
		projection.report.ApplyStatus = "pending"
		setEventStoreRepairApplyCommand(&projection.report, homePath, record.PlanPath, filepath.Join(filepath.Dir(record.PlanPath), record.Plan.ID+".review.json"), pending.Authorization, nil)
	case reviewExists:
		setEventStoreRepairApplyCommand(&projection.report, homePath, record.PlanPath, filepath.Join(filepath.Dir(record.PlanPath), record.Plan.ID+".review.json"), eventstore.PostCompletionRepairAuthorization{}, []string{"WORK_ID", "LEASE_HOLDER"})
	default:
		setEventStoreRepairNextCommand(&projection.report, "review", []string{
			"compound", "event-store", "repair", "review", record.PlanPath,
			"--reviewer", "operator:${OPERATOR_ID}",
			"--decision", "authorize",
			"--attest-writer-stopped",
			"--home", homePath,
		}, "OPERATOR_ID")
	}
	return projection, nil
}

func setEventStoreRepairApplyCommand(report *eventStoreRepairStatusReport, homePath, planPath, reviewPath string, authorization eventstore.PostCompletionRepairAuthorization, requirements []string) {
	workID := authorization.WorkID
	actor := authorization.Actor
	if workID == "" {
		workID = "${WORK_ID}"
	}
	if actor == "" {
		actor = "${LEASE_HOLDER}"
	}
	setEventStoreRepairNextCommand(report, "apply", []string{
		"compound", "event-store", "repair", "apply", planPath,
		"--review", reviewPath,
		"--work", workID,
		"--actor", actor,
		"--home", homePath,
	}, requirements...)
}

func setEventStoreRepairNextCommand(report *eventStoreRepairStatusReport, action string, argv []string, requirements ...string) {
	report.NextAction = action
	report.NextCommandArgv = append([]string(nil), argv...)
	report.NextCommand = eventStoreRepairCommand(argv)
	report.NextCommandRequirements = append([]string(nil), requirements...)
}

func eventStoreRepairCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = eventStoreRepairShellArg(arg)
	}
	return strings.Join(quoted, " ")
}

func eventStoreRepairShellArg(arg string) string {
	switch arg {
	case "${WORK_ID}", "${LEASE_HOLDER}", "operator:${OPERATOR_ID}":
		return `"` + arg + `"`
	}
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-./:", r))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

func eventStoreRepairRootAnchorStatus(reasons []string) (bool, string) {
	issues := make([]string, 0)
	for _, reason := range reasons {
		if strings.Contains(reason, "post-completion repair root") || strings.Contains(reason, "post-completion repair authority anchor") {
			issues = append(issues, reason)
		}
	}
	return len(issues) == 0, strings.Join(issues, "; ")
}

func writeEventStoreRepairStatus(w io.Writer, report eventStoreRepairStatusReport) {
	fmt.Fprintln(w, "event-store repair status")
	fmt.Fprintf(w, "needed: %t\n", report.Needed)
	fmt.Fprintf(w, "loose_after_completion: %d\n", report.LooseAfterCompletion)
	if report.PlanID == "" {
		fmt.Fprintf(w, "plan: %s\n", report.PlanStatus)
	} else {
		fmt.Fprintf(w, "plan: %s (%s)\n", report.PlanID, report.PlanStatus)
	}
	fmt.Fprintf(w, "review: %s\n", report.ReviewStatus)
	fmt.Fprintf(w, "apply: %s\n", report.ApplyStatus)
	fmt.Fprintf(w, "proof: %s\n", report.ProofStatus)
	fmt.Fprintf(w, "root_anchor_consistent: %t\n", report.RootAnchorConsistent)
	if report.RootAnchorIssue != "" {
		fmt.Fprintf(w, "root_anchor_issue: %s\n", report.RootAnchorIssue)
	}
	fmt.Fprintf(w, "supersession: %s\n", report.SupersessionStatus)
	if len(report.ActivePlanIDs) > 1 {
		fmt.Fprintf(w, "active_plans: %s\n", strings.Join(report.ActivePlanIDs, ", "))
	}
	fmt.Fprintf(w, "next_action: %s\n", report.NextAction)
	if len(report.NextCommandRequirements) > 0 {
		fmt.Fprintf(w, "next_command_requires: %s\n", strings.Join(report.NextCommandRequirements, ", "))
	}
	if report.NextCommand != "" {
		fmt.Fprintf(w, "next_command: %s\n", report.NextCommand)
	}
}

func supersedeEventStoreRepair(homePath, oldPlanPath, successorPlanPath, reviewPath, workID, actor string, jsonOut bool, stdout, stderr io.Writer) int {
	events, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	oldPlan, err := events.PostCompletionRepairPlanForApply(oldPlanPath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: valid old plan required:", err)
		return 1
	}
	successorPlan, err := events.PostCompletionRepairPlanForApply(successorPlanPath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: valid successor plan required:", err)
		return 1
	}
	review, err := events.PostCompletionRepairReviewForFinalization(reviewPath, oldPlan)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: exact old-plan review required:", err)
		return 1
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	w, err := workStore.Get(workID)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	oldCreatedAt, err := time.Parse(time.RFC3339Nano, oldPlan.CreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	successorCreatedAt, err := time.Parse(time.RFC3339Nano, successorPlan.CreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	workCreatedAt, err := time.Parse(time.RFC3339Nano, w.CreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede:", err)
		return 1
	}
	executionNow := time.Now().UTC()
	actionAt, err := eventStoreRepairActionTime(review.CreatedAt, executionNow, oldCreatedAt, successorCreatedAt, workCreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: refused:", err)
		return 1
	}
	var (
		action        envelope.ActionIntent
		decision      envelope.PolicyDecision
		evaluation    policy.AuthorityEvaluation
		authorization eventstore.PostCompletionRepairAuthorization
		reservationAt time.Time
	)
	existing, superseded, err := events.ReadPostCompletionRepairSupersession(oldPlan.ID)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: existing terminal invalid:", err)
		return 1
	}
	if superseded {
		if existing.SuccessorPlanID != successorPlan.ID || existing.SuccessorPlanDigest != successorPlan.PlanDigest || existing.Authorization.WorkID != workID || existing.Authorization.Actor != actor || existing.ReviewID != review.ID || existing.ReviewDigest != review.ReviewDigest {
			fmt.Fprintln(stderr, "event-store repair supersede: supplied successor/work/actor/review do not match sealed supersession")
			return 1
		}
		authorization = existing.Authorization
		action, decision, err = loadEventStoreRepairReservedTuple(events, authorization)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede:", err)
			return 1
		}
		reservationAt, err = time.Parse(time.RFC3339Nano, authorization.AuthorizedAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: sealed authorization time invalid:", err)
			return 1
		}
		completedProof, completed, err := events.ReadPostCompletionRepairSupersessionProof(oldPlan.ID)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: completed proof invalid:", err)
			return 1
		}
		if completed {
			if _, err := events.FinalizePostCompletionRepairSupersession(oldPlan.ID, authorization, completedProof.ReceiptID, completedProof.TransitionID, time.Now()); err != nil {
				fmt.Fprintln(stderr, "event-store repair supersede: completed proof invalid:", err)
				return 1
			}
		} else if err := events.SupersedePostCompletionRepair(oldPlanPath, successorPlanPath, authorization, time.Now(), func() error {
			return fmt.Errorf("sealed supersession replay cannot authorize a replacement terminal artifact")
		}); err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: terminal artifact root recovery pending:", err)
			return 1
		}
		face := policy.ProjectAuthority(action, decision, policy.DefaultConfig(), eventStoreRepairAuthorityContext(action, oldPlan), "sealed:"+authorization.AuthorityRevision)
		face.Current = false
		face.Allowed = policy.Allowed(decision)
		face.Revision = authorization.AuthorityRevision
		face.Wording = "Supersession durable · finalizing sealed work proof"
		evaluation = policy.AuthorityEvaluation{Decision: decision, Authority: face}
	} else {
		action = eventStoreRepairSupersessionIntent(w, actor, oldPlan, review, successorPlan, actionAt)
		reservationAt = time.Now().UTC()
		evaluation, err = evaluateEventStoreRepairCurrentAuthority(homePath, action, oldPlan, reservationAt, reservationAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: refused:", err)
			return 1
		}
		decision = evaluation.Decision
		authorization = eventstore.PostCompletionRepairAuthorization{
			WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: decision.ID,
			AuthorityRevision: evaluation.Authority.Revision, PlanDigest: oldPlan.PlanDigest,
			ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
			AuthorizedAt: reservationAt.Format(time.RFC3339Nano),
		}
		authorization.SealedAction, err = json.Marshal(action)
		if err == nil {
			authorization.SealedDecision, err = json.Marshal(decision)
		}
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: seal authorization:", err)
			return 1
		}
		validateMutation := func() error {
			return validateEventStoreRepairSupersessionAuthority(events, homePath, oldPlanPath, successorPlanPath, reviewPath, action, oldPlan, successorPlan, reservationAt, decision.ID, authorization.AuthorityRevision)
		}
		receiptInput := eventStoreRepairSupersessionReceiptedInput(action, decision, oldPlan, successorPlan, authorization.AuthorityRevision)
		if err := workStore.ReserveReceiptedAction(workID, receiptInput, reservationAt, validateMutation); err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede:", err)
			return 1
		}
		if err := events.SupersedePostCompletionRepair(oldPlanPath, successorPlanPath, authorization, time.Now(), validateMutation); err != nil {
			if _, durable, readErr := events.ReadPostCompletionRepairSupersession(oldPlan.ID); readErr != nil || !durable {
				_ = workStore.AbortReceiptedActionReservation(workID, action.ID)
			}
			fmt.Fprintln(stderr, "event-store repair supersede:", err)
			return 1
		}
		if err := eventStoreRepairSupersessionAfterStage("terminal"); err != nil {
			fmt.Fprintln(stderr, "event-store repair supersede: terminal artifact durable; work proof finalization pending:", err)
			return 1
		}
	}
	receiptInput := eventStoreRepairSupersessionReceiptedInput(action, decision, oldPlan, successorPlan, authorization.AuthorityRevision)
	receipted, err := workStore.RecordReservedReceiptedAction(workID, receiptInput, reservationAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: terminal artifact durable; work proof finalization pending:", err)
		return 1
	}
	if err := eventStoreRepairSupersessionAfterStage("receipt"); err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: work receipt durable; supersession proof finalization pending:", err)
		return 1
	}
	proof, err := events.FinalizePostCompletionRepairSupersession(oldPlan.ID, authorization, receipted.Receipt.ID, receipted.Transition.ID, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair supersede: work proof recorded; supersession finalization pending:", err)
		return 1
	}
	report := eventStoreRepairSupersessionReport{
		SchemaVersion: "compound.eventstore.post_completion_repair_supersession_report.v0.1", Status: "completed",
		WorkID: workID, PlanID: oldPlan.ID, PlanDigest: oldPlan.PlanDigest,
		SuccessorPlanID: successorPlan.ID, SuccessorPlanDigest: successorPlan.PlanDigest,
		ActionIntentID: receipted.Action.ID, PolicyDecisionID: receipted.Decision.ID,
		ReceiptID: receipted.Receipt.ID, TransitionID: receipted.Transition.ID,
		SupersessionProofDigest: proof.ProofDigest,
		PolicyAuthority:         evaluation.Authority,
	}
	if jsonOut {
		writeEventStoreRepairJSON(stdout, report)
	} else {
		fmt.Fprintf(stdout, "event-store repair supersede %s status=completed successor=%s work=%s receipt=%s transition=%s\n", oldPlan.ID, successorPlan.ID, workID, receipted.Receipt.ID, receipted.Transition.ID)
	}
	return 0
}

func applyEventStoreRepair(homePath, planPath, reviewPath, workID, actor string, jsonOut bool, stdout, stderr io.Writer) int {
	events, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	plan, err := events.PostCompletionRepairPlanForApply(planPath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply: valid plan required:", err)
		return 1
	}
	planCreatedAt, err := time.Parse(time.RFC3339Nano, plan.CreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	w, err := workStore.Get(workID)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	workCreatedAt, err := time.Parse(time.RFC3339Nano, w.CreatedAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	repairResult, resultExists, err := events.ReadPostCompletionRepairResult(plan.ID)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	pending, pendingExists, err := events.ReadPostCompletionRepairPending(plan.ID)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}

	var (
		action        envelope.ActionIntent
		decision      envelope.PolicyDecision
		evaluation    policy.AuthorityEvaluation
		authorization eventstore.PostCompletionRepairAuthorization
		actionAt      time.Time
		reservationAt time.Time
		review        eventstore.PostCompletionRepairReview
	)
	if resultExists || pendingExists {
		if resultExists {
			authorization = repairResult.Authorization
			review, err = events.PostCompletionRepairReviewForFinalization(reviewPath, plan)
		} else {
			authorization = pending.Authorization
			review, err = events.PostCompletionRepairReviewForApply(reviewPath, plan, true)
		}
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: exact operator review required:", err)
			return 1
		}
		if authorization.WorkID != workID || authorization.Actor != actor || authorization.ReviewID != review.ID || authorization.ReviewDigest != review.ReviewDigest || authorization.WriterFenceDigest != review.WriterFenceDigest {
			fmt.Fprintln(stderr, "event-store repair apply: supplied work/actor/review do not match sealed repair authorization")
			return 1
		}
		action, decision, err = loadEventStoreRepairReservedTuple(events, authorization)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply:", err)
			return 1
		}
		actionAt, err = time.Parse(time.RFC3339Nano, action.CreatedAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply:", err)
			return 1
		}
		reservationAt, err = time.Parse(time.RFC3339Nano, authorization.AuthorizedAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: sealed authorization time invalid:", err)
			return 1
		}
		sealedFinalization := false
		if !resultExists {
			sealedFinalization, err = events.PostCompletionRepairPhysicalCompletionForFinalization(planPath, authorization)
			if err != nil {
				fmt.Fprintln(stderr, "event-store repair apply: sealed finalization proof failed:", err)
				return 1
			}
		}
		if resultExists || sealedFinalization {
			// No physical mutation remains. Render the persisted sealed tuple as
			// historical authority without consulting (or re-authorizing through)
			// the current policy file, which may be unreadable during recovery.
			face := policy.ProjectAuthority(action, decision, policy.DefaultConfig(), eventStoreRepairAuthorityContext(action, plan), "sealed:"+authorization.AuthorityRevision)
			face.Current = false
			face.Allowed = policy.Allowed(decision)
			face.Revision = authorization.AuthorityRevision
			face.Wording = "Physical repair complete · finalizing sealed authority"
			evaluation = policy.AuthorityEvaluation{Decision: decision, Authority: face}
		} else {
			previousAuthorization := authorization
			previousDecision := decision
			previousReservationAt := reservationAt
			if reservedInput, reservedAt, reserved, readErr := workStore.ReadReceiptedActionReservationInput(workID, action.ID); readErr != nil {
				fmt.Fprintln(stderr, "event-store repair apply: read unpublished authorization:", readErr)
				return 1
			} else if reserved {
				reservedRevision, revisionErr := eventStoreRepairInputAuthorityRevision(reservedInput)
				if revisionErr != nil {
					fmt.Fprintln(stderr, "event-store repair apply: read unpublished authorization:", revisionErr)
					return 1
				}
				reservedDecision := reservedInput.PreEvaluatedDecision
				if reservedDecision == nil {
					fmt.Fprintln(stderr, "event-store repair apply: unpublished authorization lacks policy decision")
					return 1
				}
				if reservedDecision.ID != previousAuthorization.PolicyDecisionID || reservedRevision != previousAuthorization.AuthorityRevision {
					intermediate := previousAuthorization
					intermediate.PolicyDecisionID = reservedDecision.ID
					intermediate.AuthorityRevision = reservedRevision
					intermediate.AuthorizedAt = reservedAt.UTC().Format(time.RFC3339Nano)
					intermediate.SealedAction, err = json.Marshal(action)
					if err == nil {
						intermediate.SealedDecision, err = json.Marshal(reservedDecision)
					}
					if err != nil {
						fmt.Fprintln(stderr, "event-store repair apply: seal unpublished authorization:", err)
						return 1
					}
					validateInstalled := func() error {
						installed, installedAt, present, err := workStore.ReadReceiptedActionReservationInput(workID, action.ID)
						if err != nil || !present || installed.PreEvaluatedDecision == nil {
							return errors.Join(fmt.Errorf("unpublished receipted action reservation changed before journal recovery"), err)
						}
						installedRevision, err := eventStoreRepairInputAuthorityRevision(installed)
						if err != nil || installed.PreEvaluatedDecision.ID != reservedDecision.ID || installedRevision != reservedRevision || !installedAt.Equal(reservedAt) {
							return errors.Join(fmt.Errorf("unpublished receipted action reservation changed before journal recovery"), err)
						}
						return nil
					}
					if err := events.ReauthorizePostCompletionRepair(planPath, previousAuthorization, intermediate, validateInstalled); err != nil {
						fmt.Fprintln(stderr, "event-store repair apply: interrupted policy reauthorization recovery failed:", err)
						return 1
					}
					previousAuthorization = intermediate
					previousDecision = *reservedDecision
					previousReservationAt = reservedAt
				}
			}

			authorization = previousAuthorization
			decision = previousDecision
			reservationAt = previousReservationAt
			current, currentErr := evaluateEventStoreRepairCurrentAuthority(homePath, action, plan, reservationAt, time.Now())
			if currentErr != nil {
				fmt.Fprintln(stderr, "event-store repair apply: remaining mutation refused:", currentErr)
				return 1
			}
			if current.Authority.Revision != authorization.AuthorityRevision {
				evaluationAt := time.Now().UTC()
				current, currentErr = evaluateEventStoreRepairCurrentAuthority(homePath, action, plan, evaluationAt, evaluationAt)
				if currentErr != nil {
					fmt.Fprintln(stderr, "event-store repair apply: remaining mutation refused:", currentErr)
					return 1
				}
				nextAuthorization := authorization
				nextAuthorization.PolicyDecisionID = current.Decision.ID
				nextAuthorization.AuthorityRevision = current.Authority.Revision
				nextAuthorization.AuthorizedAt = evaluationAt.Format(time.RFC3339Nano)
				nextAuthorization.SealedAction, err = json.Marshal(action)
				if err == nil {
					nextAuthorization.SealedDecision, err = json.Marshal(current.Decision)
				}
				if err != nil {
					fmt.Fprintln(stderr, "event-store repair apply: seal current authorization:", err)
					return 1
				}
				validateNext := func() error {
					return validateEventStoreRepairMutationAuthority(events, homePath, planPath, reviewPath, action, plan, evaluationAt, current.Decision.ID, current.Authority.Revision, true)
				}
				previousInput := eventStoreRepairReceiptedInput(action, decision, plan, authorization.AuthorityRevision)
				nextInput := eventStoreRepairReceiptedInput(action, current.Decision, plan, nextAuthorization.AuthorityRevision)
				if err := workStore.ReauthorizeReceiptedActionReservation(workID, previousInput, reservationAt, nextInput, evaluationAt, validateNext); err != nil {
					fmt.Fprintln(stderr, "event-store repair apply: current policy reauthorization failed:", err)
					return 1
				}
				if err := events.ReauthorizePostCompletionRepair(planPath, authorization, nextAuthorization, validateNext); err != nil {
					fmt.Fprintln(stderr, "event-store repair apply: current policy reauthorization journal failed:", err)
					return 1
				}
				authorization = nextAuthorization
				decision = current.Decision
				reservationAt = evaluationAt
			}
			evaluation = current
		}
	} else {
		executionNow := time.Now().UTC()
		review, err = events.PostCompletionRepairReviewForApply(reviewPath, plan, false)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: exact operator review required:", err)
			return 1
		}
		actionAt, err = eventStoreRepairActionTime(review.CreatedAt, executionNow, planCreatedAt, workCreatedAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: refused:", err)
			return 1
		}
		action = eventStoreRepairIntent(w, actor, plan, review, actionAt)
		reservationAt = time.Now().UTC()
		evaluation, err = evaluateEventStoreRepairCurrentAuthority(homePath, action, plan, reservationAt, reservationAt)
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: refused:", err)
			return 1
		}
		decision = evaluation.Decision
		authorization = eventstore.PostCompletionRepairAuthorization{
			WorkID: workID, Actor: actor, ActionIntentID: action.ID, PolicyDecisionID: decision.ID,
			AuthorityRevision: evaluation.Authority.Revision, PlanDigest: plan.PlanDigest,
			ReviewID: review.ID, ReviewDigest: review.ReviewDigest, WriterFenceDigest: review.WriterFenceDigest,
			AuthorizedAt: reservationAt.Format(time.RFC3339Nano),
		}
		authorization.SealedAction, err = json.Marshal(action)
		if err == nil {
			authorization.SealedDecision, err = json.Marshal(decision)
		}
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply: seal authorization:", err)
			return 1
		}
	}
	receiptInput := eventStoreRepairReceiptedInput(action, decision, plan, authorization.AuthorityRevision)
	if !resultExists && !pendingExists {
		// Reserve the exact work transaction before the physical owner can append,
		// archive, or retire one loose byte. The callback repeats current policy
		// and exact-plan validation while holding the work lifecycle lock.
		err = workStore.ReserveReceiptedAction(workID, receiptInput, reservationAt, func() error {
			decisionAt, decisionErr := eventStoreRepairDecisionInstant(decision)
			if decisionErr != nil {
				return decisionErr
			}
			return validateEventStoreRepairMutationAuthority(events, homePath, planPath, reviewPath, action, plan, decisionAt, decision.ID, authorization.AuthorityRevision, false)
		})
		if err != nil {
			fmt.Fprintln(stderr, "event-store repair apply:", err)
			return 1
		}
	}

	// Identity validation must re-evaluate at the sealed decision's own minted
	// instant: a policy decision's identity binds its evaluation time, so any
	// other instant would re-derive a different id and refuse a legitimately
	// installed authorization (recovery paths authorize later than they mint).
	validateMutation := func() error {
		decisionAt, decisionErr := eventStoreRepairDecisionInstant(decision)
		if decisionErr != nil {
			return decisionErr
		}
		return validateEventStoreRepairMutationAuthority(events, homePath, planPath, reviewPath, action, plan, decisionAt, decision.ID, authorization.AuthorityRevision, true)
	}
	applyOutcome, err := events.ApplyPostCompletionRepairOutcome(planPath, authorization, eventstore.PostCompletionRepairApplyOptions{ValidateAuthority: validateMutation})
	if err != nil {
		if _, pending, pendingErr := events.ReadPostCompletionRepairPending(plan.ID); pendingErr == nil && !pending {
			_ = workStore.AbortReceiptedActionReservation(workID, action.ID)
		}
		fmt.Fprintln(stderr, "event-store repair apply:", err)
		return 1
	}
	repairResult = applyOutcome.Result
	receiptInput = eventStoreRepairReceiptedInput(action, decision, plan, authorization.AuthorityRevision)
	receipted, err := workStore.RecordReservedReceiptedAction(workID, receiptInput, reservationAt)
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply: physical repair complete; work proof finalization pending:", err)
		return 1
	}
	repairProof, err := events.FinalizePostCompletionRepair(plan.ID, authorization, receipted.Receipt.ID, receipted.Transition.ID, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "event-store repair apply: work proof recorded; repair journal finalization pending:", err)
		return 1
	}
	report := eventStoreRepairApplyReport{
		SchemaVersion: "compound.eventstore.post_completion_repair_apply_report.v0.1", Status: "completed",
		Changed: applyOutcome.Changed, Replayed: applyOutcome.Replayed, WorkID: workID,
		PlanID: plan.ID, PlanDigest: plan.PlanDigest,
		ActionIntentID: receipted.Action.ID, PolicyDecisionID: receipted.Decision.ID,
		ReceiptID: receipted.Receipt.ID, TransitionID: receipted.Transition.ID,
		RepairProofDigest: repairProof.ProofDigest,
		PolicyAuthority:   evaluation.Authority, Result: repairResult,
	}
	if jsonOut {
		writeEventStoreRepairJSON(stdout, report)
	} else {
		fmt.Fprintf(stdout, "event-store repair apply %s status=completed changed=%t replayed=%t work=%s receipt=%s transition=%s\n", repairResult.ID, applyOutcome.Changed, applyOutcome.Replayed, workID, receipted.Receipt.ID, receipted.Transition.ID)
	}
	return 0
}

func eventStoreRepairActionTime(reviewCreatedAt string, executionNow time.Time, causalPredecessors ...time.Time) (time.Time, error) {
	reviewAt, err := time.Parse(time.RFC3339Nano, reviewCreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("review created_at invalid: %w", err)
	}
	executionNow = executionNow.UTC()
	if reviewAt.After(executionNow) {
		return time.Time{}, fmt.Errorf("review created_at is after current execution time")
	}
	causal := reviewAt.UTC()
	for index, predecessor := range causalPredecessors {
		predecessor = predecessor.UTC()
		if predecessor.After(executionNow) {
			return time.Time{}, fmt.Errorf("causal predecessor is after current execution time (index %d)", index+1)
		}
		if predecessor.After(causal) {
			causal = predecessor
		}
	}
	return causal, nil
}

func eventStoreRepairDecisionInstant(decision envelope.PolicyDecision) (time.Time, error) {
	decisionAt, err := time.Parse(time.RFC3339Nano, decision.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("sealed policy decision created_at invalid: %w", err)
	}
	return decisionAt, nil
}

func evaluateEventStoreRepairCurrentAuthority(homePath string, action envelope.ActionIntent, plan eventstore.PostCompletionRepairPlan, evaluatedAt, checkedAt time.Time) (policy.AuthorityEvaluation, error) {
	// Clock rollback must fail closed before any policy evaluation: a check
	// instant earlier than the evaluation instant would let future-dated
	// authority govern a mutation happening in its past.
	if checkedAt.UTC().Before(evaluatedAt.UTC()) {
		return policy.AuthorityEvaluation{}, fmt.Errorf("event-store repair authority check time precedes evaluation time")
	}
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		return policy.AuthorityEvaluation{}, err
	}
	evaluation := policy.EvaluateAuthority(action, cfg, eventStoreRepairAuthorityContext(action, plan), source, evaluatedAt)
	// This adapter owns a nanosecond causal chain. Policy's compatibility
	// projection uses whole seconds, which can otherwise place the decision
	// before the review/action it governs. Preserve the exact evaluation instant
	// and rebuild the authority face from that exact decision.
	evaluation.Decision.CreatedAt = evaluatedAt.UTC().Format(time.RFC3339Nano)
	evaluation.Authority = policy.ProjectAuthority(action, evaluation.Decision, cfg, eventStoreRepairAuthorityContext(action, plan), source)
	if err := validateEventStoreRepairCurrentAuthority(action, evaluation, checkedAt); err != nil {
		return policy.AuthorityEvaluation{}, err
	}
	return evaluation, nil
}

func validateEventStoreRepairMutationAuthority(events *eventstore.Store, homePath, planPath, reviewPath string, action envelope.ActionIntent, plan eventstore.PostCompletionRepairPlan, evaluatedAt time.Time, decisionID, authorityRevision string, allowProgress bool) error {
	currentPlan, err := events.PostCompletionRepairPlanForApply(planPath)
	if err != nil || currentPlan.PlanDigest != plan.PlanDigest {
		return errors.Join(fmt.Errorf("repair plan drifted before mutation"), err)
	}
	// The event-store apply owns the one full writer/source census. Adapter
	// revalidation reloads the immutable review only, then rechecks policy; a
	// second writer snapshot here would restore per-mutation O(N) scans.
	if _, err := events.PostCompletionRepairReviewForFinalization(reviewPath, plan); err != nil {
		return err
	}
	current, err := evaluateEventStoreRepairCurrentAuthority(homePath, action, plan, evaluatedAt, time.Now())
	if err != nil {
		return err
	}
	// The stable authority revision intentionally excludes decision identity, so
	// revision equality alone would let a replacement decision at the same
	// revision govern the mutation. Bind the exact current PolicyDecision.
	if current.Decision.ID != decisionID || current.Authority.Revision != authorityRevision {
		return fmt.Errorf("event-store repair policy authority changed before mutation")
	}
	// A fresh reservation (allowProgress=false) forbids any physical repair
	// progress; a mutation revalidation (true) runs while progress exists.
	if !allowProgress {
		if _, present, err := events.ReadPostCompletionRepairPending(plan.ID); err != nil {
			return err
		} else if present {
			return fmt.Errorf("event-store repair already progressed before reservation")
		}
	}
	return nil
}

func validateEventStoreRepairSupersessionAuthority(events *eventstore.Store, homePath, oldPlanPath, successorPlanPath, reviewPath string, action envelope.ActionIntent, oldPlan, successorPlan eventstore.PostCompletionRepairPlan, evaluatedAt time.Time, decisionID, authorityRevision string) error {
	currentOld, err := events.PostCompletionRepairPlanForApply(oldPlanPath)
	if err != nil || currentOld.PlanDigest != oldPlan.PlanDigest {
		return errors.Join(fmt.Errorf("old repair plan drifted before supersession"), err)
	}
	currentSuccessor, err := events.PostCompletionRepairPlanForApply(successorPlanPath)
	if err != nil || currentSuccessor.PlanDigest != successorPlan.PlanDigest {
		return errors.Join(fmt.Errorf("successor repair plan drifted before supersession"), err)
	}
	review, err := events.PostCompletionRepairReviewForFinalization(reviewPath, oldPlan)
	if err != nil {
		return err
	}
	if !containsRepairEvidence(action.EvidenceRefs, "eventstore-repair-review:"+review.ReviewDigest) {
		return fmt.Errorf("repair supersession action does not bind exact review")
	}
	current, err := evaluateEventStoreRepairCurrentAuthority(homePath, action, oldPlan, evaluatedAt, time.Now())
	if err != nil {
		return err
	}
	if current.Decision.ID != decisionID || current.Authority.Revision != authorityRevision {
		return fmt.Errorf("event-store repair supersession policy authority changed before mutation")
	}
	return nil
}

func loadEventStoreRepairReservedTuple(events *eventstore.Store, authorization eventstore.PostCompletionRepairAuthorization) (envelope.ActionIntent, envelope.PolicyDecision, error) {
	var action envelope.ActionIntent
	if len(authorization.SealedAction) > 0 {
		if err := json.Unmarshal(authorization.SealedAction, &action); err != nil {
			return action, envelope.PolicyDecision{}, fmt.Errorf("sealed repair action intent is invalid: %w", err)
		}
	} else {
		actionRecord, err := events.Read(authorization.ActionIntentID)
		if err != nil {
			return action, envelope.PolicyDecision{}, fmt.Errorf("sealed repair action intent is unavailable: %w", err)
		}
		if err := json.Unmarshal(actionRecord.Bytes, &action); err != nil {
			return action, envelope.PolicyDecision{}, err
		}
	}
	var decision envelope.PolicyDecision
	if len(authorization.SealedDecision) > 0 {
		if err := json.Unmarshal(authorization.SealedDecision, &decision); err != nil {
			return action, decision, fmt.Errorf("sealed repair policy decision is invalid: %w", err)
		}
	} else {
		decisionRecord, err := events.Read(authorization.PolicyDecisionID)
		if err != nil {
			return action, decision, fmt.Errorf("sealed repair policy decision is unavailable: %w", err)
		}
		if err := json.Unmarshal(decisionRecord.Bytes, &decision); err != nil {
			return action, decision, err
		}
	}
	if action.ID != authorization.ActionIntentID || action.IDs.WorkID != authorization.WorkID || decision.ID != authorization.PolicyDecisionID || decision.ActionIntentID != action.ID || decision.IDs.WorkID != authorization.WorkID || !policy.Allowed(decision) {
		return action, decision, fmt.Errorf("sealed repair action/policy tuple is invalid")
	}
	return action, decision, nil
}

func eventStoreRepairInputAuthorityRevision(input work.ReceiptedActionInput) (string, error) {
	revision := ""
	for _, ref := range input.ProofRefs {
		if !strings.HasPrefix(ref, "policy-authority:") {
			continue
		}
		if revision != "" {
			return "", fmt.Errorf("unpublished receipted action reservation has multiple policy authority refs")
		}
		revision = strings.TrimPrefix(ref, "policy-authority:")
	}
	if revision == "" {
		return "", fmt.Errorf("unpublished receipted action reservation lacks policy authority ref")
	}
	return revision, nil
}

func eventStoreRepairIntent(w envelope.WorkEnvelope, actor string, plan eventstore.PostCompletionRepairPlan, review eventstore.PostCompletionRepairReview, createdAt time.Time) envelope.ActionIntent {
	evidence := []string{
		"eventstore-repair-plan:" + plan.PlanDigest,
		"eventstore-repair-source:" + plan.SourceDigest,
		"eventstore-repair-review:" + review.ReviewDigest,
		"eventstore-repair-writer-fence:" + review.WriterFenceDigest,
	}
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", w.ID, eventStoreRepairCapability, plan.ID, plan.PlanDigest, review.ReviewDigest),
			CreatedAt:     createdAt.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: actor},
			IDs:           envelope.IDs{WorkID: w.ID, ContextPackID: w.IDs.ContextPackID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:       "private", Risk: "tier1", EvidenceRefs: evidence,
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: eventStoreRepairCapability, Action: "repair_post_completion_loose_records",
		Target:        envelope.ActionTarget{Kind: "eventstore_post_completion_repair", ID: plan.ID, Path: "compound://event-store"},
		MutationClass: "local_reversible", Reversibility: "manual", BlastRadius: "bounded_event_store",
		Preview:      fmt.Sprintf("apply governed repair %s to %d post-completion loose event records", plan.ID, plan.Total),
		RollbackPlan: "restore exact source bodies from verified content-addressed recovery evidence while retaining the append-only authority record",
	}
}

func eventStoreRepairSupersessionIntent(w envelope.WorkEnvelope, actor string, oldPlan eventstore.PostCompletionRepairPlan, review eventstore.PostCompletionRepairReview, successor eventstore.PostCompletionRepairPlan, createdAt time.Time) envelope.ActionIntent {
	evidence := []string{
		"eventstore-repair-plan:" + oldPlan.PlanDigest,
		"eventstore-repair-source:" + oldPlan.SourceDigest,
		"eventstore-repair-review:" + review.ReviewDigest,
		"eventstore-repair-successor-plan:" + successor.PlanDigest,
		"eventstore-repair-successor-source:" + successor.SourceDigest,
	}
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", w.ID, eventStoreRepairSupersessionCapability, oldPlan.ID, oldPlan.PlanDigest, review.ReviewDigest, successor.ID, successor.PlanDigest),
			CreatedAt:     createdAt.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: actor},
			IDs:           envelope.IDs{WorkID: w.ID, ContextPackID: w.IDs.ContextPackID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:       "private", Risk: "tier1", EvidenceRefs: evidence,
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: eventStoreRepairSupersessionCapability, Action: "supersede_zero_progress_repair_plan",
		Target:        envelope.ActionTarget{Kind: "eventstore_post_completion_repair", ID: oldPlan.ID, Path: "compound://event-store"},
		MutationClass: "local_reversible", Reversibility: "manual", BlastRadius: "bounded_event_store",
		Preview:      fmt.Sprintf("terminalize zero-progress repair %s in favor of broader plan %s", oldPlan.ID, successor.ID),
		RollbackPlan: "preserve both immutable plans and record any later correction as another governed lifecycle transition",
	}
}

func containsRepairEvidence(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func eventStoreRepairAuthorityContext(action envelope.ActionIntent, plan eventstore.PostCompletionRepairPlan) policy.EvaluationContext {
	return policy.EvaluationContext{Authority: &policy.AuthorityContext{
		TargetScope: policyTargetScopePrivate, VerifiedRecoverability: policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "Receipt binding the exact repair plan and policy authority to the running work",
		ExpectedProof: []string{
			"eventstore-repair-plan:" + plan.PlanDigest,
			postCompletionRepairCLIProofRef,
		},
		EvidenceRefs: append([]string(nil), action.EvidenceRefs...),
	}}
}

const (
	policyTargetScopePrivate        = "private"
	postCompletionRepairCLIProofRef = "falsifier:cmd/compound/eventstore_cli_test.go#repair-plan-policy-work-replay"
)

func validateEventStoreRepairCurrentAuthority(action envelope.ActionIntent, evaluation policy.AuthorityEvaluation, now time.Time) error {
	if err := policy.ValidateAuthorityBundle(action, evaluation.Decision, evaluation.Authority); err != nil {
		return err
	}
	if !evaluation.Authority.Current || !evaluation.Authority.Allowed {
		return fmt.Errorf("%s", evaluation.Authority.Wording)
	}
	if expiresAt := strings.TrimSpace(evaluation.Decision.ExpiresAt); expiresAt != "" {
		expires, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil || !now.Before(expires) {
			return fmt.Errorf("policy decision standing scope expired at %s", expiresAt)
		}
	}
	return act.Gate(action, evaluation.Decision)
}

func eventStoreRepairReceiptedInput(action envelope.ActionIntent, decision envelope.PolicyDecision, plan eventstore.PostCompletionRepairPlan, authorityRevision string) work.ReceiptedActionInput {
	// The reservation must be byte-identical before and after the physical
	// transaction. Bind the deterministic immutable result id here; the result's
	// own digest is validated by eventstore and migrate --verify.
	resultRef := "eventstore-repair-result:esrm_" + strings.TrimPrefix(plan.PlanDigest, "sha256:")[:12]
	planRef := "eventstore-repair-plan:" + plan.PlanDigest
	reviewRef := ""
	for _, ref := range action.EvidenceRefs {
		if strings.HasPrefix(ref, "eventstore-repair-review:") {
			reviewRef = ref
			break
		}
	}
	authorityRef := "policy-authority:" + authorityRevision
	historyRef := "eventstore-repair-authority-history:" + plan.PlanDigest
	return work.ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true,
		Summary:    fmt.Sprintf("post-completion event-store repair %s archived %d loose records and preserved segment authority", plan.ID, plan.Total),
		MoveType:   "control_plane_repair",
		ProofRefs:  []string{planRef, reviewRef, authorityRef, historyRef, resultRef, postCompletionRepairCLIProofRef},
		BeforeRefs: []string{planRef}, AfterRefs: []string{resultRef},
		FilesChanged: []string{action.Target.Path},
	}
}

func eventStoreRepairSupersessionReceiptedInput(action envelope.ActionIntent, decision envelope.PolicyDecision, oldPlan, successor eventstore.PostCompletionRepairPlan, authorityRevision string) work.ReceiptedActionInput {
	planRef := "eventstore-repair-plan:" + oldPlan.PlanDigest
	successorRef := "eventstore-repair-successor-plan:" + successor.PlanDigest
	supersessionRef := "eventstore-repair-supersession:esrs_" + strings.TrimPrefix(oldPlan.PlanDigest, "sha256:")[:12]
	reviewRef := ""
	for _, ref := range action.EvidenceRefs {
		if strings.HasPrefix(ref, "eventstore-repair-review:") {
			reviewRef = ref
			break
		}
	}
	return work.ReceiptedActionInput{
		Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
		Outcome: "completed", Changed: true,
		Summary:    fmt.Sprintf("post-completion event-store repair %s was terminalized at zero progress in favor of broader plan %s", oldPlan.ID, successor.ID),
		MoveType:   "control_plane_repair",
		ProofRefs:  []string{planRef, reviewRef, successorRef, supersessionRef, "policy-authority:" + authorityRevision, postCompletionRepairCLIProofRef},
		BeforeRefs: []string{planRef}, AfterRefs: []string{successorRef, supersessionRef},
		FilesChanged: []string{action.Target.Path},
	}
}

func parseEventStoreRepairFlags(args []string, allowed map[string]bool) (map[string]string, bool, error) {
	values := map[string]string{}
	jsonOut := false
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if flag == "--json" {
			jsonOut = true
			continue
		}
		if !allowed[flag] {
			if expectsValue, ok := allowed[flag]; ok && !expectsValue {
				if values[flag] != "" {
					return nil, false, fmt.Errorf("duplicate flag %s", flag)
				}
				values[flag] = "true"
				continue
			}
			return nil, false, fmt.Errorf("unknown flag %s", flag)
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return nil, false, fmt.Errorf("%s requires a value", flag)
		}
		if _, duplicate := values[flag]; duplicate {
			return nil, false, fmt.Errorf("duplicate flag %s", flag)
		}
		values[flag] = args[i+1]
		i++
	}
	return values, jsonOut, nil
}

func writeEventStoreRepairJSON(w io.Writer, value any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
