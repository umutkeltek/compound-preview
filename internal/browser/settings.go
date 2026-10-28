package browser

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

const (
	settingsSchemaVersion       = "compound.browser_settings.v0.2"
	legacySettingsSchemaVersion = "compound.browser_settings.v0.1"
	toggleCapability            = "browser.backend.toggle"
	providerPricingCapability   = "browser.provider.pricing"
	// Jev 1.13 accepts at most 64k input tokens at $0.042 per million.
	// https://docs.typesafe.ai/models
	minPricedJevUSDPerRequest = 0.002688
)

// Settings is Compound's sole browser-backend switch. Missing settings mean
// the established Playwright route; malformed settings fail closed.
type Settings struct {
	SchemaVersion            string  `json:"schema_version"`
	JevEnabled               bool    `json:"jev_enabled"`
	ProviderModel            string  `json:"provider_model,omitempty"`
	ProviderMaxUSDPerRequest float64 `json:"provider_max_usd_per_request,omitempty"`
}

type SettingsChange struct {
	Settings     Settings `json:"settings"`
	Changed      bool     `json:"changed"`
	Refused      bool     `json:"refused,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	ActionID     string   `json:"action_intent_id,omitempty"`
	DecisionID   string   `json:"policy_decision_id,omitempty"`
	ReceiptID    string   `json:"receipt_id,omitempty"`
	TransitionID string   `json:"transition_id,omitempty"`
}

type Route struct {
	Backend    string `json:"backend"`
	JevEnabled bool   `json:"jev_enabled"`
	ProfileID  string `json:"profile_id,omitempty"`
	Reason     string `json:"reason"`
}

func SettingsPath(home string) string {
	return filepath.Join(home, "config", "browser-task.json")
}

func LoadSettings(home string) (Settings, error) {
	defaultSettings := Settings{SchemaVersion: settingsSchemaVersion}
	data, err := os.ReadFile(SettingsPath(home))
	if os.IsNotExist(err) {
		return defaultSettings, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read browser settings: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Settings{}, fmt.Errorf("decode browser settings: %w", err)
	}
	if fields["schema_version"] == nil || fields["jev_enabled"] == nil {
		return Settings{}, fmt.Errorf("browser settings have unrecognized shape")
	}
	for name := range fields {
		switch name {
		case "schema_version", "jev_enabled", "provider_model", "provider_max_usd_per_request":
		default:
			return Settings{}, fmt.Errorf("browser settings have unrecognized shape")
		}
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode browser settings: %w", err)
	}
	if settings.SchemaVersion != settingsSchemaVersion && settings.SchemaVersion != legacySettingsSchemaVersion {
		return Settings{}, fmt.Errorf("unsupported browser settings schema %q", settings.SchemaVersion)
	}
	if (settings.ProviderModel == "") != (settings.ProviderMaxUSDPerRequest == 0) ||
		(settings.ProviderModel != "" && (settings.ProviderModel != "jev-1.13.0" ||
			settings.ProviderMaxUSDPerRequest < minPricedJevUSDPerRequest || settings.ProviderMaxUSDPerRequest > 0.003)) {
		return Settings{}, fmt.Errorf("browser provider settings need pinned Jev model and bounded request ceiling")
	}
	if settings.SchemaVersion == legacySettingsSchemaVersion && len(fields) != 2 {
		return Settings{}, fmt.Errorf("legacy browser settings have unrecognized shape")
	}
	return settings, nil
}

// SelectRoute never assigns Jev an authenticated profile: upstream Browser
// Harness connects to an existing Chrome and cannot bind user_data_dir.
func SelectRoute(settings Settings, profileID string) Route {
	profileID = strings.TrimSpace(profileID)
	if profileID != "" {
		return Route{Backend: "playwright", JevEnabled: settings.JevEnabled, ProfileID: profileID, Reason: "profile-bound tasks use the registered Playwright backend"}
	}
	if settings.JevEnabled {
		return Route{Backend: "jev", JevEnabled: true, Reason: "Jev enabled for public unprofiled tasks"}
	}
	return Route{Backend: "playwright", Reason: "Jev disabled in Compound settings"}
}

// SetJevEnabled changes the switch through act's policy-enforced file adapter,
// recording pre/post images, Receipt, and Transition under COMPOUND_HOME.
func SetJevEnabled(home, workID string, enabled bool, cfg policy.Config, now time.Time) (SettingsChange, error) {
	current, err := LoadSettings(home)
	if err != nil {
		return SettingsChange{}, err
	}
	if current.JevEnabled == enabled {
		return SettingsChange{Settings: current}, nil
	}
	if !strings.HasPrefix(workID, "work_") {
		return SettingsChange{}, fmt.Errorf("browser setting requires a work_id")
	}
	updated := current
	updated.SchemaVersion = settingsSchemaVersion
	updated.JevEnabled = enabled
	newValue, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return SettingsChange{}, err
	}
	newValue = append(newValue, '\n')
	intent := browserSettingIntent(home, workID, enabled, now)
	decision := policy.Evaluate(intent, cfg, now)
	if err := act.Gate(intent, decision); err != nil {
		return SettingsChange{Settings: current, Refused: true, Reason: err.Error(), ActionID: intent.ID, DecisionID: decision.ID}, nil
	}
	store, err := act.NewArtifactStore(home)
	if err != nil {
		return SettingsChange{}, err
	}
	result, err := act.Execute(store, act.NewFileArtifactAdapter(), intent, decision, newValue, now)
	if err != nil {
		return SettingsChange{}, err
	}
	return SettingsChange{Settings: updated, Changed: true, ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID}, nil
}

// SetProviderPricing pins the model and request ceiling without enabling Jev
// or authorizing provider spend. The configured model cannot drift via alias.
func SetProviderPricing(home, workID, model string, perRequestUSD float64, cfg policy.Config, now time.Time) (SettingsChange, error) {
	if model != "jev-1.13.0" || perRequestUSD < minPricedJevUSDPerRequest || perRequestUSD > 0.003 {
		return SettingsChange{}, fmt.Errorf("provider pricing requires pinned Jev 1.13 and USD ceiling between 0.002688 and 0.003")
	}
	if !strings.HasPrefix(workID, "work_") {
		return SettingsChange{}, fmt.Errorf("browser provider pricing requires work_id")
	}
	current, err := LoadSettings(home)
	if err != nil {
		return SettingsChange{}, err
	}
	if current.ProviderModel == model && current.ProviderMaxUSDPerRequest == perRequestUSD {
		return SettingsChange{Settings: current}, nil
	}
	updated := current
	updated.SchemaVersion = settingsSchemaVersion
	updated.ProviderModel = model
	updated.ProviderMaxUSDPerRequest = perRequestUSD
	newValue, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return SettingsChange{}, err
	}
	newValue = append(newValue, '\n')
	intent := browserSettingIntent(home, workID, false, now)
	digest := sha256.Sum256([]byte(workID + "\x00provider-pricing\x00" + now.UTC().Format(time.RFC3339Nano)))
	intent.ID = "act_" + hex.EncodeToString(digest[:8])
	intent.Capability = providerPricingCapability
	intent.Action = "set_provider_pricing"
	intent.Preview = "pin Jev provider pricing"
	decision := policy.Evaluate(intent, cfg, now)
	if err := act.Gate(intent, decision); err != nil {
		return SettingsChange{Settings: current, Refused: true, Reason: err.Error(), ActionID: intent.ID, DecisionID: decision.ID}, nil
	}
	store, err := act.NewArtifactStore(home)
	if err != nil {
		return SettingsChange{}, err
	}
	result, err := act.Execute(store, act.NewFileArtifactAdapter(), intent, decision, newValue, now)
	if err != nil {
		return SettingsChange{}, err
	}
	return SettingsChange{Settings: updated, Changed: true, ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID}, nil
}

func browserSettingIntent(home, workID string, enabled bool, now time.Time) envelope.ActionIntent {
	action := "disable"
	if enabled {
		action = "enable"
	}
	digest := sha256.Sum256([]byte(workID + "\x00" + action + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_" + hex.EncodeToString(digest[:8]),
			CreatedAt:     now.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound.browser",
		Capability:    toggleCapability,
		Action:        "set_jev_enabled",
		Target:        envelope.ActionTarget{Kind: "config_file", ID: "browser-task", Path: SettingsPath(home)},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       action + " Jev browser backend",
		RollbackPlan:  "restore the captured config pre-image through act.ExecuteRevert",
	}
}
