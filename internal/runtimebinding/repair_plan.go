package runtimebinding

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const (
	RuntimeRepairReviewArea = "runtime-repair"
	RepairPlanSchemaVersion = "compound.runtime_repair_plan.v0.1"
)

type RepairPlan struct {
	SchemaVersion   string         `json:"schema_version"`
	ID              string         `json:"id"`
	CreatedAt       string         `json:"created_at"`
	ServiceID       string         `json:"service_id"`
	Owner           string         `json:"owner"`
	Status          string         `json:"status"`
	FailedCount     int            `json:"failed_count"`
	FailedChecks    []Check        `json:"failed_checks"`
	WritebackGateID string         `json:"writeback_gate_id"`
	WorkID          string         `json:"work_id,omitempty"`
	ReceiptID       string         `json:"receipt_id,omitempty"`
	TransitionID    string         `json:"transition_id,omitempty"`
	EvidenceRefs    []string       `json:"evidence_refs"`
	OwnerTarget     string         `json:"owner_target"`
	ReviewActions   []RepairAction `json:"review_actions"`
}

type RepairAction struct {
	Check      string `json:"check"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion"`
}

type RepairRefs struct {
	WritebackGateID string
	WorkID          string
	ReceiptID       string
	TransitionID    string
}

type RepairArtifactPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

func BuildRepairPlan(report DoctorReport, refs RepairRefs, now time.Time) (RepairPlan, error) {
	if !NeedsWriteback(report) {
		return RepairPlan{}, fmt.Errorf("runtime report %s is ok; no repair plan needed", report.ServiceID)
	}
	plan := RepairPlan{
		SchemaVersion:   RepairPlanSchemaVersion,
		CreatedAt:       now.UTC().Format(time.RFC3339),
		ServiceID:       report.ServiceID,
		Owner:           report.Owner,
		Status:          "review_required",
		WritebackGateID: refs.WritebackGateID,
		WorkID:          refs.WorkID,
		ReceiptID:       refs.ReceiptID,
		TransitionID:    refs.TransitionID,
		EvidenceRefs:    EvidenceRefs(report),
		OwnerTarget:     "runtime_binding:" + report.ServiceID,
	}
	for _, check := range report.Checks {
		if check.Status == "fail" {
			plan.FailedChecks = append(plan.FailedChecks, check)
			plan.ReviewActions = append(plan.ReviewActions, RepairAction{
				Check:      check.Name,
				Status:     check.Status,
				Detail:     check.Detail,
				Suggestion: runtimeRepairSuggestion(check),
			})
		}
	}
	plan.FailedCount = len(plan.FailedChecks)
	if plan.FailedCount == 0 {
		return RepairPlan{}, fmt.Errorf("runtime report %s has no failed checks", report.ServiceID)
	}
	plan.ID = "rrp_" + reviewartifact.Digest12(plan.ServiceID+"\n"+plan.Owner+"\n"+plan.WritebackGateID+"\n"+runtimeRepairDigest(plan))
	return plan, nil
}

func RecordRepairPlan(homePath string, plan RepairPlan) (RepairArtifactPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return RepairArtifactPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	paths, err := reviewartifact.Write(homePath, RuntimeRepairReviewArea, plan.ID, plan, "  ", RenderRepairReview(plan))
	if err != nil {
		return RepairArtifactPaths{}, err
	}
	reviewYAMLPath, err := reviewartifact.WriteRaw(homePath, RuntimeRepairReviewArea, plan.ID+".review.yaml", []byte(RenderRepairReviewYAMLDraft(plan)))
	if err != nil {
		return RepairArtifactPaths{}, err
	}
	return RepairArtifactPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: reviewYAMLPath}, nil
}

func RenderRepairReview(plan RepairPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Runtime Repair Review: %s\n\n", plan.ServiceID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- owner: %s\n", plan.Owner)
	fmt.Fprintf(&b, "- failed_checks: %d\n", plan.FailedCount)
	fmt.Fprintf(&b, "- writeback_gate: %s\n", plan.WritebackGateID)
	if plan.WorkID != "" {
		fmt.Fprintf(&b, "- work: %s\n", plan.WorkID)
	}
	if plan.ReceiptID != "" {
		fmt.Fprintf(&b, "- receipt: %s\n", plan.ReceiptID)
	}
	if plan.TransitionID != "" {
		fmt.Fprintf(&b, "- transition: %s\n", plan.TransitionID)
	}
	b.WriteString("\nThis review artifact does not mutate runtime owner files, regenerate contracts, or promote runtime truth. Apply any owner change through an explicit reviewed writeback action.\n\n")
	b.WriteString("## Failed Checks\n\n")
	for _, action := range plan.ReviewActions {
		fmt.Fprintf(&b, "- `%s`: %s", action.Check, action.Suggestion)
		if action.Detail != "" {
			fmt.Fprintf(&b, " (current: %s)", action.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func runtimeRepairSuggestion(check Check) string {
	switch check.Name {
	case "service":
		return "align runtime_binding.service_id and service_contract.service_id"
	case "owner":
		return "align runtime and service owner entity"
	case "host":
		return "declare the host or runtime authority for this service"
	case "deploy":
		return "declare deployment binding and rollback surface"
	case "data":
		return "declare data binding or state owner"
	case "health":
		return "declare at least one health check reference"
	case "interface":
		return "declare at least one service interface"
	case "risk":
		return "align runtime and service risk tiers"
	case "evidence":
		return "attach evidence refs for both runtime binding and service contract"
	case "status":
		return "set a valid lifecycle status on runtime binding and service contract"
	default:
		return "review and repair this runtime contract check"
	}
}

func runtimeRepairDigest(plan RepairPlan) string {
	var parts []string
	for _, action := range plan.ReviewActions {
		parts = append(parts, action.Check+"="+action.Suggestion+"="+action.Detail)
	}
	return strings.Join(parts, "\n")
}
