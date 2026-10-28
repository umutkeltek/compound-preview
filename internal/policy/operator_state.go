package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	homepkg "compound/internal/home"
)

const (
	OperatorStateAuthorityRef          = "compound://registries/operator-state"
	OperatorStateSourceAuthoritySchema = "compound.operator_state_source_authority.v0.1"
	operatorStateFile                  = "operator-state.json"
	operatorStateSourceAuthorityFile   = "operator-state-source-authority.jsonl"
	operatorStateImportedVia           = "policy import-operator-state"
	defaultAutonomyLevel               = "L2"
)

type OperatorState struct {
	Mode            string                 `json:"mode,omitempty"`
	CurrentPriority string                 `json:"current_priority,omitempty"`
	AutonomyLever   OperatorAutonomyLever  `json:"autonomy_lever,omitempty"`
	NeedsMe         []string               `json:"needs_me,omitempty"`
	Presence        *OperatorPresence      `json:"presence,omitempty"`
	Blocked         []map[string]any       `json:"blocked,omitempty"`
	Raw             map[string]interface{} `json:"-"`
}

type OperatorAutonomyLever struct {
	Level   string `json:"level,omitempty"`
	SetAt   string `json:"set_at,omitempty"`
	SetBy   string `json:"set_by,omitempty"`
	Meaning string `json:"meaning,omitempty"`
}

type OperatorStateImportReport struct {
	AuthorityRef      string                             `json:"authority_ref"`
	AuthorityPath     string                             `json:"authority_path"`
	SourceAuthorityID string                             `json:"source_authority_id"`
	SourceAuthority   OperatorStateSourceAuthorityRecord `json:"source_authority"`
	AutonomyLevel     string                             `json:"autonomy_level"`
	Mode              string                             `json:"mode,omitempty"`
	NeedsMeCount      int                                `json:"needs_me_count"`
	BlockedCount      int                                `json:"blocked_count"`
}

type OperatorStateSourceAuthorityRecord struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	AutonomyLevel       string   `json:"autonomy_level"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func LoadOperatorState(path string) (OperatorState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OperatorState{}, err
	}
	return parseOperatorState(data)
}

func parseOperatorState(data []byte) (OperatorState, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return OperatorState{}, fmt.Errorf("parse operator state: %w", err)
	}
	var state OperatorState
	if err := json.Unmarshal(data, &state); err != nil {
		return OperatorState{}, fmt.Errorf("parse operator state: %w", err)
	}
	state.Raw = raw
	if _, err := normalizeAutonomyLevel(state.AutonomyLever.Level); err != nil {
		return OperatorState{}, err
	}
	// The operator-state source view has long written the legacy key
	// needs_umut while this reader looked only for needs_me, so the list was
	// always empty here. Read either; needs_me wins whenever its key is
	// present, even empty, because it is the canonical name.
	if _, canonical := raw["needs_me"]; !canonical {
		if _, legacy := raw["needs_umut"]; legacy {
			var legacyState struct {
				NeedsUmut []string `json:"needs_umut"`
			}
			if err := json.Unmarshal(data, &legacyState); err != nil {
				return OperatorState{}, fmt.Errorf("parse operator state needs_umut: %w", err)
			}
			state.NeedsMe = legacyState.NeedsUmut
		}
	}
	if err := normalizeOperatorPresence(state.Presence); err != nil {
		return OperatorState{}, err
	}
	return state, nil
}

func ImportOperatorState(homePath, sourcePath string, now time.Time) (OperatorStateImportReport, error) {
	var report OperatorStateImportReport
	var importErr error
	err := homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		report, importErr = importOperatorStateLocked(homePath, sourcePath, now)
		return importErr
	})
	return report, err
}

func importOperatorStateLocked(homePath, sourcePath string, now time.Time) (OperatorStateImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OperatorStateImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return OperatorStateImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return OperatorStateImportReport{}, err
	}
	state, err := parseOperatorState(data)
	if err != nil {
		return OperatorStateImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return OperatorStateImportReport{}, err
	}
	authorityPath := OperatorStatePath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return OperatorStateImportReport{}, err
	}
	sourceRevision := digestHex(data)
	level := OperatorStateLevel(state)
	rec := OperatorStateSourceAuthorityRecord{
		SchemaVersion:       OperatorStateSourceAuthoritySchema,
		ID:                  operatorStateSourceAuthorityID(sourcePath, sourceRevision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         operatorStateImportedVia,
		AuthorityRef:        OperatorStateAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "Policy evaluation reads compound://registries/operator-state from COMPOUND_HOME; source view remains rollback/evidence input",
		Rollback:            "re-import source operator-state.json or remove COMPOUND_HOME registries/operator-state.json",
		AutonomyLevel:       level,
		EvidenceRefs:        []string{"sha256:" + sourceRevision, OperatorStateAuthorityRef},
	}
	if err := appendOperatorStateSourceAuthority(homePath, rec); err != nil {
		return OperatorStateImportReport{}, err
	}
	return OperatorStateImportReport{
		AuthorityRef:      OperatorStateAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		AutonomyLevel:     level,
		Mode:              state.Mode,
		NeedsMeCount:      len(state.NeedsMe),
		BlockedCount:      len(state.Blocked),
	}, nil
}

func LoadOperatorStateFromHome(homePath string) (OperatorState, error) {
	return LoadOperatorState(OperatorStatePath(homePath))
}

// CurrentOperatorAuthorityUnresolved is the read seam for governed consumers
// that need the operator-state owner's current answer for one exact policy
// gate. Historical hold/deny decisions are immutable evidence; only a matching
// row in the current blocked projection establishes that the authority question
// remains unresolved now.
func CurrentOperatorAuthorityUnresolved(homePath, workID, actionID, decisionID string) (bool, error) {
	workID = strings.TrimSpace(workID)
	actionID = strings.TrimSpace(actionID)
	decisionID = strings.TrimSpace(decisionID)
	if workID == "" || actionID == "" || decisionID == "" {
		return false, fmt.Errorf("current operator authority requires exact work, action, and policy decision")
	}
	state, err := LoadOperatorStateFromHome(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, blocked := range state.Blocked {
		if operatorStateString(blocked, "work_id") != workID ||
			operatorStateString(blocked, "action_intent_id") != actionID ||
			operatorStateString(blocked, "policy_decision_id") != decisionID {
			continue
		}
		switch strings.ToLower(firstOperatorStateString(blocked, "state", "status", "resolution")) {
		case "resolved", "cleared", "approved", "allowed", "completed", "dismissed":
			return false, nil
		default:
			return true, nil
		}
	}
	for _, ref := range state.NeedsMe {
		if strings.TrimSpace(ref) == decisionID {
			return true, nil
		}
	}
	return false, nil
}

func operatorStateString(row map[string]any, key string) string {
	value, ok := row[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func firstOperatorStateString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := operatorStateString(row, key); value != "" {
			return value
		}
	}
	return ""
}

func OperatorStatePath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorStateFile)
}

func OperatorStateSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", operatorStateSourceAuthorityFile)
}

func LoadOperatorStateSourceAuthority(homePath string) ([]OperatorStateSourceAuthorityRecord, error) {
	data, err := os.ReadFile(OperatorStateSourceAuthorityPath(homePath))
	if err != nil {
		return nil, err
	}
	var records []OperatorStateSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec OperatorStateSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse operator state source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreatedAt < records[j].CreatedAt
	})
	return records, nil
}

func ApplyOperatorStateFromHome(cfg *Config, homePath string) error {
	if cfg == nil || strings.TrimSpace(homePath) == "" {
		return nil
	}
	state, err := LoadOperatorStateFromHome(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	ApplyOperatorState(cfg, state)
	return nil
}

func ApplyOperatorState(cfg *Config, state OperatorState) {
	if cfg == nil {
		return
	}
	level := OperatorStateLevel(state)
	cfg.OperatorAutonomyLevel = level
	cfg.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
	// The window, not its effective value: presence is evaluated at decision
	// time against now, so a long-lived config cannot carry a stale "away".
	if state.Presence != nil {
		presence := *state.Presence
		cfg.OperatorPresence = &presence
	}
	cfg.PolicyEvidenceRefs = mergeStrings(cfg.PolicyEvidenceRefs, OperatorStateAuthorityRef)
	if !autonomyAllowsLocalSourceMutation(level) {
		if cfg.ClassOutcome == nil {
			cfg.ClassOutcome = map[string]ClassRule{}
		}
		cfg.ClassOutcome["local_reversible"] = ClassRule{Outcome: "hold"}
	}
}

func OperatorStateLevel(state OperatorState) string {
	level, err := normalizeAutonomyLevel(state.AutonomyLever.Level)
	if err != nil {
		return defaultAutonomyLevel
	}
	return level
}

func normalizeAutonomyLevel(level string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(level))
	if normalized == "" {
		return defaultAutonomyLevel, nil
	}
	switch normalized {
	case "L0", "L1", "L2", "L3":
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid autonomy_lever.level %q", level)
	}
}

func autonomyAllowsLocalSourceMutation(level string) bool {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "L2", "L3":
		return true
	default:
		return false
	}
}

func appendOperatorStateSourceAuthority(homePath string, rec OperatorStateSourceAuthorityRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := OperatorStateSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func operatorStateSourceAuthorityID(path, revision string) string {
	return "opsrc_" + digestHex([]byte(strings.Join([]string{path, revision, operatorStateImportedVia}, "|")))[:12]
}
