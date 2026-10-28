package work

// ActionDeliveryRequest asks the Work owner to bind one original action to
// immutable non-source evidence. BindingActionIntentID is deliberately absent:
// only the same-state advance may mint that provenance.
type ActionDeliveryRequest struct {
	ActionIntentID string `json:"action_intent_id"`
	Kind           string `json:"kind"`
	EvidenceRef    string `json:"evidence_ref"`
}

const privateRuntimeDeliveryEvidenceSchema = "compound.private_runtime_delivery.v0.1"

const (
	privateDeliveryMetadataLimit = 1 << 20
	privateDeliveryBodyLimit     = 64 << 20
	privateDeliveryReportLimit   = 1 << 20
)

type privateRuntimeDeliveryEvidence struct {
	SchemaVersion         string                        `json:"schema_version"`
	WorkID                string                        `json:"work_id"`
	ActionIntentID        string                        `json:"action_intent_id"`
	ActionSHA256          string                        `json:"action_sha256"`
	RepositoryRoot        string                        `json:"repository_root"`
	RepositoryRef         string                        `json:"repository_ref"`
	RepositoryFingerprint string                        `json:"repository_fingerprint"`
	TargetPath            string                        `json:"target_path"`
	Files                 []privateRuntimeDeliveryEntry `json:"files"`
	Consumer              deliveryOutcomeEvidence       `json:"consumer"`
}

type privateRuntimeDeliveryEntry struct {
	Path           string `json:"path"`
	BeforeArtifact string `json:"before_artifact,omitempty"`
	BeforeAbsent   bool   `json:"before_absent,omitempty"`
	AfterArtifact  string `json:"after_artifact"`
}

// This is a reference to the existing receipted-action transaction, not another
// outcome authority. Canonical envelope/event digests use that owner's existing
// exactReceiptedActionDigest grammar. The original mutation uses captured bytes.
type deliveryOutcomeEvidence struct {
	ActionID         string `json:"action_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
	ReceiptID        string `json:"receipt_id"`
	TransitionID     string `json:"transition_id"`
	ActionSHA256     string `json:"action_sha256"`
	PolicySHA256     string `json:"policy_sha256"`
	ReceiptSHA256    string `json:"receipt_sha256"`
	TransitionSHA256 string `json:"transition_sha256"`
	EventSHA256      string `json:"event_sha256"`
	ReportRef        string `json:"report_ref"`
}
