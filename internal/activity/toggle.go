package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/policy"
)

// Governed capabilities. Grants are matched capability-exact, so these strings ARE the security
// boundary: a grant for one never authorizes the other.
const (
	ToggleCapability       = "activity.sensor.toggle"
	DrainCapability        = "activity.ingest.drain"
	Tier1CollectCapability = "activity.tier1.collect"
	ActionRecordVersion    = "compound.activity_sensor_action.v0.1"
)

// ToggleResult reports the outcome of an enable/disable attempt.
type ToggleResult struct {
	Enabled      bool
	Applied      bool
	Refused      bool
	Reason       string
	ReceiptID    string
	TransitionID string
}

// actionRecord is the append-only governed-toggle ledger row (mirrors lensmodulator.actionRecord).
type actionRecord struct {
	SchemaVersion  string `json:"schema_version"`
	Kind           string `json:"kind"` // "enable" | "disable"
	WorkID         string `json:"work_id"`
	Capability     string `json:"capability"`
	ActionIntentID string `json:"action_intent_id"`
	PreImageRef    string `json:"pre_image_ref"`
	PostImageRef   string `json:"post_image_ref"`
	ReceiptID      string `json:"receipt_id"`
	TransitionID   string `json:"transition_id"`
	CreatedAt      string `json:"created_at"`
}

// SetEnabled runs the governed enable/disable toggle through the policy+act seam. It mirrors the lens
// modulator exactly: GATE BEFORE ANY WRITE, so a refused (hold/deny) decision leaves the config key
// unchanged (F9). Only an allowing PolicyDecision materializes the baseline key and flips it; the
// flip lands a pre/post-image + Receipt/Transition via act.Execute and an action-ledger row.
func SetEnabled(home string, cfg policy.Config, workID string, enable bool, now time.Time) (ToggleResult, error) {
	kind := "disable"
	newValue := "false"
	if enable {
		kind = "enable"
		newValue = "true"
	}
	target := envelope.ActionTarget{Kind: "config_key", ID: EnabledKey, Path: ConfigPath(home)}
	ai := buildToggleIntent(workID, target, kind, enable, now)
	pd := policy.Evaluate(ai, cfg, now)

	// Gate before materializing anything. A refused decision returns with the config untouched.
	if gateErr := act.Gate(ai, pd); gateErr != nil {
		return ToggleResult{Refused: true, Enabled: false, Reason: gateErr.Error()}, nil
	}

	store, err := act.NewArtifactStore(home)
	if err != nil {
		return ToggleResult{}, err
	}
	adapter := act.NewConfigKeyAdapter()

	// Admitted: materialize the baseline the repoint-only adapter needs, tracking whether WE created
	// it so a failed act can restore the prior state.
	created, err := ensureConfig(home)
	if err != nil {
		return ToggleResult{}, err
	}
	result, execErr := act.Execute(store, adapter, ai, pd, []byte(newValue), now)
	if execErr != nil {
		if created {
			if delErr := removeConfigIfPresent(home); delErr != nil {
				return ToggleResult{}, fmt.Errorf("activity toggle act failed (%w); compensating config removal ALSO failed: %v", execErr, delErr)
			}
			return ToggleResult{}, fmt.Errorf("activity toggle act failed (compensated: removed the config this pass created): %w", execErr)
		}
		return ToggleResult{}, fmt.Errorf("activity toggle act: %w", execErr)
	}

	rec := actionRecord{
		SchemaVersion: ActionRecordVersion, Kind: kind, WorkID: workID,
		Capability: ToggleCapability, ActionIntentID: ai.ID,
		PreImageRef: result.PreImageRef, PostImageRef: result.PostImageRef,
		ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
	if err := appendActionRecord(home, rec); err != nil {
		return ToggleResult{}, err
	}
	return ToggleResult{
		Enabled: enable, Applied: true,
		ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID,
	}, nil
}

func buildToggleIntent(workID string, target envelope.ActionTarget, kind string, enable bool, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actID("act_", workID, ToggleCapability, target.Path, kind),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "activity",
		Capability:    ToggleCapability,
		Action:        "toggle_activity_sensor",
		Target:        target,
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("%s the Tier-0 activity sensor (config key %q)", kind, EnabledKey),
		RollbackPlan:  fmt.Sprintf("repoint config key %q back to its prior value via act.RevertIntent", EnabledKey),
	}
}

func appendActionRecord(home string, rec actionRecord) error {
	path := ActionLedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// actID builds a deterministic act_-prefixed id from identity parts.
func actID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:24]
}
