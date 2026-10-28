package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/work"
)

const capabilityLifecycleReviewUsage = "usage: compound capability lifecycle-review --proposal <clp_id> --expected-hash <sha256> --decision <open_work|defer|reject> --rationale <text> [--defer-until <RFC3339>] [--actor <actor>] [--preview] [--json]"

type CapabilityLifecycleReviewStateDelta struct {
	Changed bool   `json:"changed"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Summary string `json:"summary"`
}

type CapabilityLifecycleReviewResult struct {
	SchemaVersion string                                      `json:"schema_version"`
	Status        string                                      `json:"status"`
	Preview       bool                                        `json:"preview"`
	Plan          capgraph.CapabilityLifecycleDispositionPlan `json:"plan"`
	WorkPlan      *work.CreatePlan                            `json:"work_plan,omitempty"`
	Admission     *work.AdmitCreatePlanReport                 `json:"admission,omitempty"`
	Disposition   *capgraph.CapabilityLifecycleDisposition    `json:"disposition,omitempty"`
	StateDelta    CapabilityLifecycleReviewStateDelta         `json:"state_delta"`
	ProofRefs     []string                                    `json:"proof_refs"`
}

type CapabilityLifecycleReviewErrorResult struct {
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	ErrorCode     string `json:"error_code"`
	Error         string `json:"error"`
}

type capabilityLifecycleReviewArgs struct {
	Input   capgraph.CapabilityLifecycleDispositionInput
	Preview bool
	JSON    bool
}

func cmdCapabilityLifecycleReview(homePath string, args []string) {
	parsed, err := parseCapabilityLifecycleReviewArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, capabilityLifecycleReviewUsage)
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	result, err := runCapabilityLifecycleReview(homePath, args, time.Now())
	if err != nil {
		if parsed.JSON {
			code := capgraph.CapabilityLifecycleReviewErrorCode(err)
			if code == "" {
				code = "command_failed"
			}
			printJSON(CapabilityLifecycleReviewErrorResult{
				SchemaVersion: "compound.capability.lifecycle_review_error.v0.1",
				Status:        "error",
				ErrorCode:     code,
				Error:         err.Error(),
			})
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
	if parsed.JSON {
		printJSON(result)
		return
	}
	printCapabilityLifecycleReviewResult(result)
}

func runCapabilityLifecycleReview(homePath string, args []string, now time.Time) (CapabilityLifecycleReviewResult, error) {
	parsed, err := parseCapabilityLifecycleReviewArgs(args)
	if err != nil {
		return CapabilityLifecycleReviewResult{}, err
	}
	plan, err := capgraph.PlanCapabilityLifecycleDisposition(homePath, parsed.Input, now)
	if err != nil {
		return CapabilityLifecycleReviewResult{}, err
	}
	result := CapabilityLifecycleReviewResult{
		SchemaVersion: "compound.capability.lifecycle_review_result.v0.1",
		Status:        "preview",
		Preview:       parsed.Preview,
		Plan:          plan,
		StateDelta: CapabilityLifecycleReviewStateDelta{
			Changed: false,
			Before:  plan.BeforeBand,
			After:   plan.AfterBand,
			Summary: capabilityLifecycleReviewPreviewSummary(plan),
		},
		ProofRefs: lifecycleReviewProofRefs(plan, nil),
	}

	store, err := work.Open(homePath)
	if err != nil {
		return CapabilityLifecycleReviewResult{}, err
	}
	if plan.Input.Decision == capgraph.CapabilityLifecycleOpenWork {
		workPlan, err := store.PrepareCapabilityLifecycleCreatePlan(plan.Snapshot, now, work.CapabilityLifecycleCreatePlanOptions{
			Actor:  plan.Input.Actor,
			Record: false,
		})
		if err != nil {
			return CapabilityLifecycleReviewResult{}, err
		}
		result.WorkPlan = &workPlan
	}
	if parsed.Preview {
		if existing, ok, err := capgraph.ExistingCapabilityLifecycleDisposition(homePath, plan); err != nil {
			return CapabilityLifecycleReviewResult{}, err
		} else if ok {
			result.Status = existing.Status
			result.Disposition = &existing
			result.StateDelta = CapabilityLifecycleReviewStateDelta{
				Changed: false,
				Before:  existing.AfterBand,
				After:   existing.AfterBand,
				Summary: existing.StateDelta,
			}
			result.ProofRefs = lifecycleReviewProofRefs(plan, &existing)
		}
		return result, nil
	}

	var createWork capgraph.CapabilityLifecycleWorkCreator
	if result.WorkPlan != nil {
		createWork = func() (capgraph.CapabilityLifecycleDispositionLinks, error) {
			workPlan := *result.WorkPlan
			planPath := workPlan.PlanPath
			if strings.TrimSpace(planPath) == "" {
				var err error
				planPath, err = store.RecordCreatePlan(workPlan)
				if err != nil {
					return capgraph.CapabilityLifecycleDispositionLinks{}, err
				}
			}
			workPlan.PlanPath = planPath
			result.WorkPlan = &workPlan
			admission, err := store.AdmitCapabilityLifecycleCreatePlan(workPlan, planPath, now, work.AdmitCreatePlanOptions{
				Actor: plan.Input.Actor,
				Note:  "capability_lifecycle_disposition_open_work",
			})
			if err != nil {
				return capgraph.CapabilityLifecycleDispositionLinks{}, err
			}
			result.Admission = &admission
			return capabilityLifecycleAdmissionLinks(store, workPlan, admission), nil
		}
	}
	disposition, err := capgraph.ApplyCapabilityLifecycleDisposition(homePath, plan, createWork)
	if err != nil {
		return CapabilityLifecycleReviewResult{}, err
	}
	result.Status = disposition.Status
	result.Disposition = &disposition
	result.StateDelta = CapabilityLifecycleReviewStateDelta{
		Changed: disposition.Changed,
		Before:  disposition.BeforeBand,
		After:   disposition.AfterBand,
		Summary: disposition.StateDelta,
	}
	result.ProofRefs = lifecycleReviewProofRefs(plan, &disposition)
	return result, nil
}

func capabilityLifecycleReviewPreviewSummary(plan capgraph.CapabilityLifecycleDispositionPlan) string {
	switch plan.Input.Decision {
	case capgraph.CapabilityLifecycleOpenWork:
		return fmt.Sprintf("would move proposal %s from needs_you to filed and create one proposed WorkEnvelope", plan.Input.ProposalID)
	case capgraph.CapabilityLifecycleDefer:
		return fmt.Sprintf("would move proposal %s from needs_you to watching until %s", plan.Input.ProposalID, plan.Input.DeferUntil)
	default:
		return fmt.Sprintf("would move proposal %s from needs_you to filed as rejected", plan.Input.ProposalID)
	}
}

func parseCapabilityLifecycleReviewArgs(args []string) (capabilityLifecycleReviewArgs, error) {
	out := capabilityLifecycleReviewArgs{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--preview":
			if seen[flag] {
				return out, fmt.Errorf("duplicate %s", flag)
			}
			seen[flag], out.Preview = true, true
		case "--json":
			if seen[flag] {
				return out, fmt.Errorf("duplicate %s", flag)
			}
			seen[flag], out.JSON = true, true
		case "--proposal", "--expected-hash", "--decision", "--rationale", "--defer-until", "--actor":
			if seen[flag] {
				return out, fmt.Errorf("duplicate %s", flag)
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return out, fmt.Errorf("%s requires a value", flag)
			}
			seen[flag] = true
			i++
			value := args[i]
			switch flag {
			case "--proposal":
				out.Input.ProposalID = value
			case "--expected-hash":
				out.Input.ExpectedHash = value
			case "--decision":
				out.Input.Decision = value
			case "--rationale":
				out.Input.Rationale = value
			case "--defer-until":
				out.Input.DeferUntil = value
			case "--actor":
				out.Input.Actor = value
			}
		default:
			return out, fmt.Errorf("unknown lifecycle-review argument %q", flag)
		}
	}
	if strings.TrimSpace(out.Input.Actor) == "" {
		out.Input.Actor = "operator-console"
	}
	for flag, value := range map[string]string{
		"--proposal": out.Input.ProposalID, "--expected-hash": out.Input.ExpectedHash,
		"--decision": out.Input.Decision, "--rationale": out.Input.Rationale,
	} {
		if strings.TrimSpace(value) == "" {
			return out, fmt.Errorf("%s is required", flag)
		}
	}
	return out, nil
}

func capabilityLifecycleAdmissionLinks(store *work.Store, plan work.CreatePlan, admission work.AdmitCreatePlanReport) capgraph.CapabilityLifecycleDispositionLinks {
	links := capgraph.CapabilityLifecycleDispositionLinks{
		WorkID: admission.WorkID, WorkState: admission.WorkState, WorkPlanID: plan.ID,
		ActionIntentID: plan.ActionIntent.ID, PolicyDecisionID: plan.Decision.ID,
	}
	if admission.Status != "already_admitted" {
		return links
	}
	w, events, err := store.Show(admission.WorkID)
	if err != nil {
		return links
	}
	if len(w.ActionIntentIDs) > 0 {
		links.ActionIntentID = w.ActionIntentIDs[0]
	}
	links.PolicyDecisionID = w.PolicyDecisionID
	for _, event := range events {
		for _, attached := range event.Attached {
			if strings.HasPrefix(attached, "create_plan:") {
				links.WorkPlanID = strings.TrimPrefix(attached, "create_plan:")
			}
		}
	}
	return links
}

func lifecycleReviewProofRefs(plan capgraph.CapabilityLifecycleDispositionPlan, disposition *capgraph.CapabilityLifecycleDisposition) []string {
	refs := []string{"capability_lifecycle_proposal:" + plan.Input.ProposalID, "sha256:" + plan.Snapshot.Hash}
	if disposition == nil {
		return refs
	}
	refs = append(refs, "decision:"+disposition.ID)
	for _, pair := range [][2]string{
		{"work:", disposition.WorkID}, {"create_plan:", disposition.WorkPlanID},
		{"action_intent:", disposition.ActionIntentID}, {"policy_decision:", disposition.PolicyDecisionID},
	} {
		if pair[1] != "" {
			refs = append(refs, pair[0]+pair[1])
		}
	}
	return refs
}

func printCapabilityLifecycleReviewResult(result CapabilityLifecycleReviewResult) {
	fmt.Printf("# capability lifecycle-review %s\n", result.Plan.Input.ProposalID)
	fmt.Printf("- status: %s\n", result.Status)
	fmt.Printf("- decision: %s\n", result.Plan.Input.Decision)
	fmt.Printf("- changed: %t\n", result.StateDelta.Changed)
	fmt.Printf("- state_delta: %s\n", result.StateDelta.Summary)
	if result.Disposition != nil {
		fmt.Printf("- decision_id: %s\n", result.Disposition.ID)
		if result.Disposition.WorkID != "" {
			fmt.Printf("- work_id: %s\n", result.Disposition.WorkID)
			fmt.Printf("- action_intent_id: %s\n", result.Disposition.ActionIntentID)
			fmt.Printf("- policy_decision_id: %s\n", result.Disposition.PolicyDecisionID)
		}
	}
	for _, ref := range result.ProofRefs {
		fmt.Printf("- proof_ref: %s\n", ref)
	}
}
