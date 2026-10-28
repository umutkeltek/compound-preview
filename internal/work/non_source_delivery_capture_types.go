package work

const PrivateRuntimeDeliveryCaptureSchema = "compound.private_runtime_delivery_capture.v0.1"

// A capture request supplies verifiable file expectations, never a success report
// or consumer receipt. The expected generation remains part of exact retry identity.
type PrivateRuntimeDeliveryCaptureRequest struct {
	SchemaVersion            string                              `json:"schema_version"`
	CaptureID                string                              `json:"capture_id"`
	ActionIntentID           string                              `json:"action_intent_id"`
	RepositoryRoot           string                              `json:"repository_root"`
	RepositoryRef            string                              `json:"repository_ref"`
	ExpectedWorkState        string                              `json:"expected_work_state"`
	ExpectedPolicyDecisionID string                              `json:"expected_policy_decision_id"`
	ExpectedActionIntentID   string                              `json:"expected_action_intent_id"`
	Files                    []PrivateRuntimeDeliveryCaptureFile `json:"files"`
}

type PrivateRuntimeDeliveryCaptureFile struct {
	Path         string `json:"path"`
	AfterSHA256  string `json:"after_sha256"`
	BeforeAbsent bool   `json:"before_absent,omitempty"`
	BeforePath   string `json:"before_path,omitempty"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
}

type NonSourceDeliveryCaptureResult struct {
	Status        string                `json:"status"`
	ActionID      string                `json:"action_id,omitempty"`
	ReceiptID     string                `json:"receipt_id,omitempty"`
	TransitionID  string                `json:"transition_id,omitempty"`
	CustodiedRefs []string              `json:"custodied_refs,omitempty"`
	RetryCommand  []string              `json:"retry_command,omitempty"`
	Delivery      ActionDeliveryRequest `json:"delivery"`
}

type PrivateRuntimeDeliveryCaptureResult = NonSourceDeliveryCaptureResult
