package work

import "encoding/json"

const InstalledPackageDeliveryCaptureSchema = "compound.installed_package_delivery_capture.v0.1"
const installedPackageDeliverySchema = "compound.installed_package_delivery.v0.1"

// This door observes an already installed package; it never runs an installer,
// accepts a caller's success report, or grants permission for another update.
type InstalledPackageDeliveryCaptureRequest struct {
	SchemaVersion            string `json:"schema_version"`
	CaptureID                string `json:"capture_id"`
	ActionIntentID           string `json:"action_intent_id"`
	Manager                  string `json:"manager"`
	Prefix                   string `json:"prefix"`
	Package                  string `json:"package"`
	Version                  string `json:"version"`
	ExpectedWorkState        string `json:"expected_work_state"`
	ExpectedPolicyDecisionID string `json:"expected_policy_decision_id"`
	ExpectedActionIntentID   string `json:"expected_action_intent_id"`
}

type installedPackageSnapshot struct {
	Manager               string `json:"manager"`
	Prefix                string `json:"prefix"`
	Package               string `json:"package"`
	Version               string `json:"version"`
	TargetPath            string `json:"target_path"`
	RepositoryRef         string `json:"repository_ref"`
	RepositoryFingerprint string `json:"repository_fingerprint"`
	ReceiptRef            string `json:"receipt_ref"`
	TreeSHA256            string `json:"tree_sha256"`
	FileCount             int    `json:"file_count"`
	Bytes                 int64  `json:"bytes"`
}

type installedPackageEvidence struct {
	SchemaVersion  string                   `json:"schema_version"`
	WorkID         string                   `json:"work_id"`
	ActionIntentID string                   `json:"action_intent_id"`
	ActionSHA256   string                   `json:"action_sha256"`
	Snapshot       installedPackageSnapshot `json:"snapshot"`
	Consumer       deliveryOutcomeEvidence  `json:"consumer"`
}

type installedPackageReport struct {
	SchemaVersion string                   `json:"schema_version"`
	RequestRef    string                   `json:"request_ref"`
	Snapshot      installedPackageSnapshot `json:"snapshot"`
}

type homebrewInstalledReceipt struct {
	HomebrewVersion string `json:"homebrew_version"`
	Source          struct {
		Tap     string `json:"tap"`
		Version string `json:"version"`
	} `json:"source"`
	Artifacts []map[string]json.RawMessage `json:"uninstall_artifacts"`
}
