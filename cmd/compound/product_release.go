package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/work"
)

const productReleasePlanUsage = "usage: compound product release-plan --product <id> --change <summary> --candidate <ref> [--candidate <ref>...] --rollback <text> --metric <post-release-metric> [--action-card <id>] [--evidence <ref>...]"

func cmdProductReleasePlan(args []string, homePath string, store *mem.Store) {
	input, err := parseProductReleasePlanArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	plan, err := product.BuildReleasePlan(homePath, store, input, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-plan:", err)
		os.Exit(1)
	}
	paths, err := product.RecordReleasePlan(homePath, plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-plan:", err)
		os.Exit(1)
	}
	fmt.Printf("# product release-plan %s\n\n", plan.ID)
	fmt.Printf("- status: %s\n", plan.Status)
	fmt.Printf("- product: %s\n", plan.ProductID)
	fmt.Printf("- version: %s\n", plan.ReleaseIdentity.Version)
	fmt.Printf("- build: %s\n", plan.ReleaseIdentity.Build)
	fmt.Printf("- mechanism: %s\n", plan.Mechanism)
	fmt.Printf("- external_mutation_allowed: %t\n", plan.ExternalMutationAllowed)
	fmt.Printf("- post_release_metric: %s\n", plan.PostReleaseMetric)
	fmt.Printf("- plan: %s\n", paths.PlanPath)
	fmt.Printf("- review: %s\n", paths.ReviewPath)
	fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
}

func parseProductReleasePlanArgs(args []string) (product.ReleasePlanInput, error) {
	var input product.ReleasePlanInput
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return product.ReleasePlanInput{}, fmt.Errorf("%s", productReleasePlanUsage)
		}
		value := strings.TrimSpace(args[i+1])
		switch args[i] {
		case "--product":
			input.ProductID = value
		case "--change":
			input.ChangeSummary = value
		case "--candidate":
			input.CandidateRefs = append(input.CandidateRefs, value)
		case "--rollback":
			input.Rollback = value
		case "--metric":
			input.PostReleaseMetric = value
		case "--action-card":
			input.ActionCardID = value
		case "--evidence":
			input.EvidenceRefs = append(input.EvidenceRefs, value)
		default:
			return product.ReleasePlanInput{}, fmt.Errorf("%s", productReleasePlanUsage)
		}
		i++
	}
	if input.ProductID == "" || input.ChangeSummary == "" || len(input.CandidateRefs) == 0 || input.Rollback == "" || input.PostReleaseMetric == "" {
		return product.ReleasePlanInput{}, fmt.Errorf("%s", productReleasePlanUsage)
	}
	return input, nil
}

func cmdProductReleaseReview(args []string, homePath string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: compound product release-review <review.yaml>")
		os.Exit(2)
	}
	review, err := product.LoadReleaseReview(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-review:", err)
		os.Exit(1)
	}
	report, err := product.ApplyReleaseReview(homePath, review, args[0], time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-review:", err)
		os.Exit(1)
	}
	fmt.Printf("# product release-review %s\n\n", report.ReviewID)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- product: %s\n", report.ProductID)
	fmt.Printf("- decision: %s\n", report.Decision)
	fmt.Printf("- authorized: %t\n", report.Authorized)
	fmt.Printf("- executed: %t\n", report.Executed)
	fmt.Printf("- transition: %s\n", report.TransitionID)
	fmt.Printf("- next_action: %s\n", report.NextAction)
}

func cmdProductReleaseWorkPlan(args []string, homePath string) {
	reviewedPath := strings.TrimSpace(valueFlag(args, "--reviewed"))
	actor := strings.TrimSpace(valueFlag(args, "--actor"))
	jsonOut := hasFlag(args, "--json")
	if reviewedPath == "" || actor == "" || !releaseContinuationFlagsValid(args, map[string]bool{"--reviewed": true, "--actor": true}, map[string]bool{"--json": true}) {
		fmt.Fprintln(os.Stderr, "usage: compound product release-work-plan --reviewed <reviewed.json> --actor <actor> [--json]")
		os.Exit(2)
	}
	draft, err := product.BuildReleaseWorkDraft(homePath, reviewedPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-work-plan:", err)
		os.Exit(1)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-work-plan:", err)
		os.Exit(1)
	}
	requiredInputs := []string{
		"exact release authorization " + draft.AuthorizationID,
		"destination App Store Connect app " + draft.ReleaseIdentity.AppStoreConnectAppID + " and bundle " + draft.ReleaseIdentity.BundleID,
		"candidate version " + draft.ReleaseIdentity.Version + " build " + draft.ReleaseIdentity.Build,
		"candidate refs " + strings.Join(draft.CandidateRefs, ", "),
		"separately allowing PolicyDecision for the external store mutation",
	}
	expectedValidation := append([]string{}, draft.RequiredChecks...)
	expectedValidation = append(expectedValidation,
		"completed outward_delta Receipt linked to work "+draft.WorkID,
		"post-release ProductSignal metric_name equals "+draft.PostReleaseMetric,
	)
	stopConditions := []string{
		"resolved product, app, bundle, version, build, or candidate differs from the authorization",
		"any required release check is stale or fails",
		"external mutation PolicyDecision is not allowing",
		"provider read-back cannot prove the resulting release state",
	}
	actionSummary := fmt.Sprintf("%s Release exactly %s version %s build %s through %s; rollback: %s; then observe %s.", draft.ChangeSummary, draft.ProductID, draft.ReleaseIdentity.Version, draft.ReleaseIdentity.Build, draft.Mechanism, draft.Rollback, draft.PostReleaseMetric)
	plan, err := workStore.CreatePlan(work.CreateInput{
		WorkID:           draft.WorkID,
		Owner:            draft.WorkOwner,
		Actor:            actor,
		SuccessPredicate: draft.SuccessPredicate,
		DesiredOutcome:   draft.DesiredOutcome,
		NonGoals:         draft.NonGoals,
		EvidenceRefs:     draft.EvidenceRefs,
		State:            "proposed",
		Risk:             "tier3",
		Privacy:          "private",
	}, time.Now(), work.CreatePlanOptions{
		Actor:         actor,
		Capability:    "product.release_authorization",
		Action:        "plan_work_from_authorized_product_release",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		TargetKind:    "product_release_authorization",
		TargetID:      draft.AuthorizationID,
		TargetPath:    reviewedPath,
		ActionContext: &work.WorkActionContext{
			Kind:               "product_release_authorization",
			SourceRef:          "compound://reviews/" + product.ProductReleaseReviewArea + "/" + filepath.Base(reviewedPath),
			SourceID:           draft.AuthorizationID,
			Title:              "Execute authorized release for " + draft.ProductID,
			ActionSummary:      actionSummary,
			RequiredInputs:     requiredInputs,
			ExpectedValidation: expectedValidation,
			StopConditions:     stopConditions,
			Boundary:           draft.Boundary,
			Proof:              draft.Proof,
			RequiresOperator:   true,
			EvidenceRefs:       draft.EvidenceRefs,
		},
		Record: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-work-plan:", err)
		os.Exit(1)
	}
	out := struct {
		SchemaVersion  string                          `json:"schema_version"`
		Status         string                          `json:"status"`
		ProductRelease product.ProductReleaseWorkDraft `json:"product_release"`
		WorkPlan       work.CreatePlan                 `json:"work_plan"`
	}{
		SchemaVersion:  "compound.product_release_work_plan_report.v0.1",
		Status:         "recorded",
		ProductRelease: draft,
		WorkPlan:       plan,
	}
	if jsonOut {
		printJSON(out)
		return
	}
	fmt.Printf("# product release-work-plan %s\n\n", draft.ID)
	fmt.Printf("- status: %s\n", out.Status)
	fmt.Printf("- product: %s\n", draft.ProductID)
	fmt.Printf("- authorization: %s\n", draft.AuthorizationID)
	fmt.Printf("- work: %s\n", plan.WorkID)
	fmt.Printf("- work_state: %s\n", plan.WorkState)
	fmt.Printf("- risk: %s\n", plan.Work.Risk)
	fmt.Printf("- requires_operator: %t\n", plan.ActionContext != nil && plan.ActionContext.RequiresOperator)
	fmt.Printf("- policy: %s allowed=%t\n", plan.Policy.Outcome, plan.Policy.Allowed)
	fmt.Printf("- plan: %s\n", plan.PlanPath)
}

func cmdProductReleaseObserve(args []string, homePath string) {
	reviewedPath := strings.TrimSpace(valueFlag(args, "--reviewed"))
	executionRef := strings.TrimSpace(valueFlag(args, "--execution"))
	signalID := strings.TrimSpace(valueFlag(args, "--signal"))
	jsonOut := hasFlag(args, "--json")
	if reviewedPath == "" || executionRef == "" || signalID == "" || !releaseContinuationFlagsValid(args, map[string]bool{"--reviewed": true, "--execution": true, "--signal": true}, map[string]bool{"--json": true}) {
		fmt.Fprintln(os.Stderr, "usage: compound product release-observe --reviewed <reviewed.json> --execution <receipt_id> --signal <signal_id> [--json]")
		os.Exit(2)
	}
	report, err := product.ObserveProductRelease(homePath, product.ReleaseObservationInput{
		ReviewedPath: reviewedPath,
		ExecutionRef: executionRef,
		SignalID:     signalID,
	}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "product release-observe:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("# product release-observe %s\n\n", report.ID)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- product: %s\n", report.ProductID)
	fmt.Printf("- authorization: %s\n", report.AuthorizationID)
	fmt.Printf("- execution_receipt: %s\n", report.ExecutionReceiptID)
	fmt.Printf("- signal: %s\n", report.SignalID)
	fmt.Printf("- metric: %s\n", report.MetricName)
	fmt.Printf("- transition: %s\n", report.TransitionID)
	fmt.Printf("- report: %s\n", report.ReportPath)
}

func releaseContinuationFlagsValid(args []string, valueFlags, boolFlags map[string]bool) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if boolFlags[arg] {
			continue
		}
		if !valueFlags[arg] || i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return false
		}
		i++
	}
	return true
}
