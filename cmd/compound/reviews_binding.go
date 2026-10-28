package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const reviewsBindingPlanUsage = "usage: compound reviews binding-plan --item ID --area AREA [--source-work W] [--source-run-kind K --source-run ID [--source-run-work W]] [--fallback-work W] [--supersedes B] [--json]"
const reviewsBindingRecordUsage = "usage: compound reviews binding-record --item ID --area AREA --expected-review-sha256 SHA --governed-work W [--source-work W] [--source-run-kind K --source-run ID [--source-run-work W]] [--fallback-work W] [--supersedes B] [--actor A] [--json]"

type reviewsBindingPlanResult struct {
	SchemaVersion   string                            `json:"schema_version"`
	Operation       string                            `json:"operation"`
	Binding         reviewartifact.ReviewWorkBinding  `json:"binding"`
	BindingPath     string                            `json:"binding_path"`
	Current         *reviewartifact.ReviewWorkBinding `json:"current,omitempty"`
	Recorded        bool                              `json:"recorded"`
	ActionIntent    *envelope.ActionIntent            `json:"action_intent,omitempty"`
	PolicyDecision  *envelope.PolicyDecision          `json:"policy_decision,omitempty"`
	PolicyAuthority *policy.AuthorityFace             `json:"policy_authority,omitempty"`
}

func cmdReviewsBinding(args []string, record bool) {
	usage := reviewsBindingPlanUsage
	if record {
		usage = reviewsBindingRecordUsage
	}
	known := map[string]bool{
		"--item": true, "--area": true, "--source-work": true,
		"--source-run-kind": true, "--source-run": true, "--source-run-work": true,
		"--fallback-work": true, "--supersedes": true, "--json": false,
	}
	if record {
		known["--expected-review-sha256"] = true
		known["--governed-work"] = true
		known["--actor"] = true
	}
	for i := 0; i < len(args); i++ {
		takesValue, ok := known[args[i]]
		if !ok {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if takesValue {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			i++
		}
	}
	itemID := strings.TrimSpace(valueFlag(args, "--item"))
	area := strings.TrimSpace(valueFlag(args, "--area"))
	sourceWorkID := strings.TrimSpace(valueFlag(args, "--source-work"))
	sourceRunKind := strings.TrimSpace(valueFlag(args, "--source-run-kind"))
	sourceRunID := strings.TrimSpace(valueFlag(args, "--source-run"))
	sourceRunWorkID := strings.TrimSpace(valueFlag(args, "--source-run-work"))
	fallbackWorkID := strings.TrimSpace(valueFlag(args, "--fallback-work"))
	supersedes := strings.TrimSpace(valueFlag(args, "--supersedes"))
	expectedDigest := strings.TrimSpace(valueFlag(args, "--expected-review-sha256"))
	governedWorkID := strings.TrimSpace(valueFlag(args, "--governed-work"))
	actor := reviewartifact.FirstNonEmpty(strings.TrimSpace(valueFlag(args, "--actor")), strings.TrimSpace(os.Getenv("COMPOUND_OPERATOR_ID")), "operator")
	if itemID == "" || area == "" || (sourceWorkID == "" && fallbackWorkID == "") ||
		(sourceRunKind == "") != (sourceRunID == "") ||
		(record && (expectedDigest == "" || governedWorkID == "")) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviews binding:", err)
		os.Exit(1)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: area}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviews binding:", err)
		os.Exit(1)
	}
	item, err := exactReviewBindingItem(report.Items, area, itemID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviews binding:", err)
		os.Exit(1)
	}
	var sourceRun *reviewartifact.ReviewSourceRun
	if sourceRunID != "" {
		sourceRun = &reviewartifact.ReviewSourceRun{Kind: sourceRunKind, ID: sourceRunID, WorkID: sourceRunWorkID}
	}
	input := reviewartifact.RecordWorkBindingInput{
		Area: area, ReviewID: itemID, CoreReviewPath: item.JSONPath,
		SourceWorkID: sourceWorkID, SourceRun: sourceRun, FallbackWorkID: fallbackWorkID,
		SupersedesBindingID: supersedes,
		EvidenceRefs: reviewartifact.UniqueSorted([]string{
			"review:" + area + "/" + itemID,
			reviewBindingOptionalRef("review-source-work:", sourceWorkID),
			reviewBindingOptionalRef("review-fallback-work:", fallbackWorkID),
			reviewBindingOptionalRef("review-source-run:", sourceRunKind+":"+sourceRunID),
		}),
	}
	planned, err := reviewartifact.PlanWorkBinding(homePath, input, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviews binding:", err)
		os.Exit(1)
	}
	current, err := reviewartifact.LoadWorkBindingHead(homePath, reviewartifact.WorkBindingTarget{
		Area: area, ReviewID: itemID, CoreReviewPath: item.JSONPath,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviews binding:", err)
		os.Exit(1)
	}
	result := reviewsBindingPlanResult{
		SchemaVersion: "compound.reviews_binding_plan.v0.1",
		Operation:     "plan",
		Binding:       planned,
		BindingPath:   reviewBindingSidecarPath(item.JSONPath, planned.ID),
		Current:       current,
	}
	if record {
		if expectedDigest != planned.ReviewSHA256 {
			fmt.Fprintf(os.Stderr, "reviews binding: review digest drift: got %s want %s\n", planned.ReviewSHA256, expectedDigest)
			os.Exit(1)
		}
		intent, decision, authority, err := authorizeReviewBindingRecord(homePath, governedWorkID, actor, item, planned, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "reviews binding:", err)
			os.Exit(1)
		}
		input.EvidenceRefs = reviewartifact.UniqueSorted(append(input.EvidenceRefs,
			"action-intent:"+intent.ID,
			"policy-decision:"+decision.ID,
			"policy-authority:"+authority.Revision,
		))
		if err := persistReviewBindingAuthorization(homePath, intent, decision); err != nil {
			fmt.Fprintln(os.Stderr, "reviews binding:", err)
			os.Exit(1)
		}
		recorded, bindingPath, err := reviewartifact.RecordWorkBinding(homePath, input, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "reviews binding:", err)
			os.Exit(1)
		}
		result.Operation = "record"
		result.Binding = recorded
		result.BindingPath = bindingPath
		result.Recorded = true
		result.ActionIntent = &intent
		result.PolicyDecision = &decision
		result.PolicyAuthority = &authority
	}
	if hasFlag(args, "--json") {
		printJSON(result)
		return
	}
	fmt.Printf("# reviews binding-%s %s/%s\n", result.Operation, area, itemID)
	fmt.Printf("- binding: %s\n", result.Binding.ID)
	fmt.Printf("- review_sha256: %s\n", result.Binding.ReviewSHA256)
	fmt.Printf("- source_work: %s\n", reviewartifact.FirstNonEmpty(result.Binding.SourceWorkID, "none"))
	fmt.Printf("- fallback_work: %s\n", reviewartifact.FirstNonEmpty(result.Binding.FallbackWorkID, "none"))
	fmt.Printf("- path: %s\n", result.BindingPath)
}

func exactReviewBindingItem(items []reviewartifact.QueueItem, area, itemID string) (reviewartifact.QueueItem, error) {
	var matches []reviewartifact.QueueItem
	for _, item := range items {
		if item.Area == area && item.ID == itemID {
			matches = append(matches, item)
		}
	}
	if len(matches) != 1 {
		return reviewartifact.QueueItem{}, fmt.Errorf("review item %s/%s matched %d artifacts", area, itemID, len(matches))
	}
	return matches[0], nil
}

func authorizeReviewBindingRecord(homePath, workID, actor string, item reviewartifact.QueueItem, binding reviewartifact.ReviewWorkBinding, now time.Time) (envelope.ActionIntent, envelope.PolicyDecision, policy.AuthorityFace, error) {
	workStore, err := work.Open(homePath)
	if err != nil {
		return envelope.ActionIntent{}, envelope.PolicyDecision{}, policy.AuthorityFace{}, err
	}
	owner, err := workStore.Get(workID)
	if err != nil || owner.ID != workID || owner.Validate() != nil {
		return envelope.ActionIntent{}, envelope.PolicyDecision{}, policy.AuthorityFace{}, fmt.Errorf("governed work %s missing or invalid", workID)
	}
	switch owner.State {
	case "completed", "failed", "cancelled":
		return envelope.ActionIntent{}, envelope.PolicyDecision{}, policy.AuthorityFace{}, fmt.Errorf("governed work %s is terminal", workID)
	}
	intent := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            reviewartifact.ID("act_", "review-work-binding", workID, item.Area, item.ID, binding.ReviewSHA256, binding.SourceWorkID, binding.FallbackWorkID, now.UTC().Format(time.RFC3339Nano)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound reviews binding-record"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs: reviewartifact.UniqueSorted([]string{
				"review:" + item.Area + "/" + item.ID,
				binding.ReviewSHA256,
				reviewBindingOptionalRef("review-source-work:", binding.SourceWorkID),
				reviewBindingOptionalRef("review-fallback-work:", binding.FallbackWorkID),
			}),
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor:         actor,
		Capability:    "review.bind_owner_work",
		Action:        "record_owner_work_binding",
		Target:        envelope.ActionTarget{Kind: "review_work_binding", ID: item.Area + "/" + item.ID, Path: item.JSONPath},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "Append an owner-work binding for sealed review " + item.Area + "/" + item.ID + "; core review bytes remain unchanged.",
		RollbackPlan:  "Append an exact superseding binding; never rewrite or delete prior binding history.",
	}
	if err := intent.Validate(); err != nil {
		return intent, envelope.PolicyDecision{}, policy.AuthorityFace{}, err
	}
	cfg, source, err := policy.LoadConfig(homePath)
	if err != nil {
		return intent, envelope.PolicyDecision{}, policy.AuthorityFace{}, err
	}
	context := &policy.AuthorityContext{
		TargetScope: "private", VerifiedRecoverability: policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "Recorded an immutable owner-work binding for review " + item.Area + "/" + item.ID + ".",
		ExpectedProof:             []string{"review:" + item.Area + "/" + item.ID, binding.ReviewSHA256, "work:" + workID},
		EvidenceRefs:              intent.EvidenceRefs,
	}
	evaluation := policy.EvaluateAuthority(intent, cfg, policy.EvaluationContext{Authority: context}, source, now)
	if err := policy.ValidateAuthorityBundle(intent, evaluation.Decision, evaluation.Authority); err != nil {
		return intent, evaluation.Decision, evaluation.Authority, err
	}
	if !evaluation.Authority.Allowed {
		return intent, evaluation.Decision, evaluation.Authority, fmt.Errorf("current policy holds owner-work binding record: %s", strings.Join(evaluation.Decision.Reasons, "; "))
	}
	return intent, evaluation.Decision, evaluation.Authority, nil
}

func persistReviewBindingAuthorization(homePath string, intent envelope.ActionIntent, decision envelope.PolicyDecision) error {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	if err := store.Ensure(); err != nil {
		return err
	}
	if _, err := store.PutJSON(intent.ID, intent, "  "); err != nil {
		return fmt.Errorf("record binding ActionIntent: %w", err)
	}
	if _, err := store.PutJSON(decision.ID, decision, "  "); err != nil {
		return fmt.Errorf("record binding PolicyDecision: %w", err)
	}
	return nil
}

func reviewBindingSidecarPath(corePath, bindingID string) string {
	return strings.TrimSuffix(filepath.Clean(corePath), ".json") + ".work-binding." + bindingID + ".json"
}

func reviewBindingOptionalRef(prefix, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasSuffix(value, ":") {
		return ""
	}
	return prefix + value
}
