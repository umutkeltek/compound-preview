package work

import "compound/internal/authority/gitrepository"

const WorktreeRetirementDeliveryCaptureSchema = "compound.worktree_retirement_delivery_capture.v0.1"
const retirementDeliveryEvidenceSchema = "compound.worktree_retirement_delivery.v0.1"

// Inputs name the existing Authority producer's records, never supplied success.
type WorktreeRetirementDeliveryCaptureRequest struct {
	SchemaVersion            string `json:"schema_version"`
	ActionIntentID           string `json:"action_intent_id,omitempty"`
	CaptureID                string `json:"capture_id"`
	PlanPath                 string `json:"plan_path"`
	ReviewReportPath         string `json:"review_report_path"`
	ResultPath               string `json:"result_path"`
	ExpectedWorkState        string `json:"expected_work_state"`
	ExpectedPolicyDecisionID string `json:"expected_policy_decision_id"`
	ExpectedActionIntentID   string `json:"expected_action_intent_id"`
}

type retirementDeliveryEvidence struct {
	SchemaVersion     string                   `json:"schema_version"`
	WorkID            string                   `json:"work_id"`
	ActionIntentID    string                   `json:"action_intent_id"`
	ActionSHA256      string                   `json:"action_sha256"`
	PlanRef           string                   `json:"plan_ref"`
	AuthorizationRef  string                   `json:"authorization_ref"`
	ReviewedSourceRef string                   `json:"reviewed_source_ref"`
	ResultRef         string                   `json:"result_ref"`
	RecoveryArtifacts []string                 `json:"recovery_artifacts"`
	FailedOutcome     *deliveryOutcomeEvidence `json:"failed_outcome,omitempty"`
	Outcome           deliveryOutcomeEvidence  `json:"outcome"`
}

type retirementDeliverySnapshot struct {
	Evidence retirementDeliveryEvidence
	Delivery gitrepository.RetirementDelivery
	Bodies   map[string][]byte
}
